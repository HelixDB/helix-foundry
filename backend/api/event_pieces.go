package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/events"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// event_pieces.go reads an event source's pieces back: what people and their
// agents sent it, from the moment a send is stored, whatever any ontology has
// made of them. Nothing indexes a source's pieces, so a list reads them: its
// batches no sync has read, then its table's snapshot, within the bounds
// below.

const (
	// defaultEventPieceLimit and maxEventPieceLimit bound a list of pieces,
	// and maxEventPieceTextBytes the text they hold together.
	defaultEventPieceLimit = 20
	maxEventPieceLimit     = 50
	maxEventPieceTextBytes = 1 << 20
	// maxEventPieceQueryBytes and maxEventPieceQueryWords bound a query.
	maxEventPieceQueryBytes = 200
	maxEventPieceQueryWords = 10
	// eventPieceBatches is how many of a source's batches no sync has read
	// one list reads, the newest, eventPieceBatchReads at a time, and
	// eventPieceTableBytes how much of its table's snapshot, uncompressed.
	eventPieceBatches    = 100
	eventPieceBatchReads = 8
	eventPieceTableBytes = 64 << 20
	// eventPieceLists is how many lists the backend reads at once, across
	// the deployment: each reads up to eventPieceTableBytes, and one past it
	// waits.
	eventPieceLists = 4
)

// pieceList keeps the newest limit pieces of those offered that hold every
// word, in memory for at most twice that however many it is offered. Each
// id is offered newest send first, so its first offer is the piece and any
// later one is a send it replaced.
type pieceList struct {
	limit     int
	words     [][]byte
	schema    events.Schema
	textField string
	seen      map[string]bool
	kept      []listedPiece
	// floor is the arrival of the oldest piece kept once limit are: nothing
	// that arrived no later can be among the newest limit.
	floor time.Time
	// left is set once a piece was left out: one of more than limit that
	// were found, or one too old to be read for its words.
	left bool
}

// listedPiece is a piece of a list, with its arrival and its text's size.
type listedPiece struct {
	at    time.Time
	bytes int64
	piece *foundryv1.FoundryEventSourcePiece
}

// offer takes row, a row of the source's table or a waiting piece's line as
// one (events.Line.Row), with its id and arrival. A row that does not render
// is events.ErrCorruptRow, or an error of the source's settings.
func (l *pieceList) offer(id string, at time.Time, synced bool, row []byte) error {
	if l.seen[id] {
		return nil
	}
	l.seen[id] = true
	if !l.floor.IsZero() && !at.After(l.floor) {
		l.left = true
		return nil
	}
	doc, err := events.Render(row, l.schema, l.textField)
	if err != nil {
		return err
	}
	text := bytes.ToLower(doc.Body)
	if slices.ContainsFunc(l.words, func(word []byte) bool { return !bytes.Contains(text, word) }) {
		return nil
	}
	l.kept = append(l.kept, listedPiece{at: at, bytes: int64(len(doc.Body)), piece: &foundryv1.FoundryEventSourcePiece{
		Id: id, ReceivedAt: timestamppb.New(at), Title: doc.Title, Text: string(doc.Body), Synced: synced,
	}})
	if len(l.kept) == 2*l.limit {
		l.trim()
	}
	return nil
}

// trim cuts the list to its newest limit pieces, in that order.
func (l *pieceList) trim() {
	slices.SortStableFunc(l.kept, func(a, b listedPiece) int { return b.at.Compare(a.at) })
	if len(l.kept) > l.limit {
		l.kept, l.left = l.kept[:l.limit], true
	}
	if len(l.kept) == l.limit {
		l.floor = l.kept[l.limit-1].at
	}
}

// ListFoundryEventPieces lists the pieces of a live event source of the
// workspace, newest first by arrival, each id once and as the document a sync
// makes of it (events.Render): with a query, those whose document holds every
// word of it, whatever its case. The source must have a text field, which
// the document's text is.
//
// It reads, in this order, the source's batches no sync has read (above its
// cursor), newest first, with its schema from the same consistent read, then
// its table's snapshot: a sync writes the snapshot before it moves the
// cursor, so a piece is in one of them, or in both, where the batch's is the
// newer send. Nothing indexes a source's pieces, so a list costs what the
// source holds, within bounds: the eventPieceBatches newest batches, and
// eventPieceTableBytes of the snapshot, eventPieceLists lists at once.
//
// The list holds at most the request's limit and maxEventPieceTextBytes of
// text (always its newest piece), and says when pieces were left out: more
// were found than it holds, or the source holds more than the list read.
func (s *Service) ListFoundryEventPieces(ctx context.Context, req *foundryv1.ListFoundryEventPiecesRequest) (*foundryv1.ListFoundryEventPiecesResponse, error) {
	e := s.deps.EventSends
	if e == nil {
		return nil, status.Error(codes.Unavailable, "API sources need the Foundry data bucket, which this installation does not have")
	}
	ws, cid := req.GetWorkspaceId(), req.GetConnectorId()
	if ws == "" || cid == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and connector_id are required")
	}
	words := bytes.Fields(bytes.ToLower([]byte(req.GetQuery())))
	if len(req.GetQuery()) > maxEventPieceQueryBytes || len(words) > maxEventPieceQueryWords {
		return nil, status.Errorf(codes.InvalidArgument, "query is at most %d bytes and %d words", maxEventPieceQueryBytes, maxEventPieceQueryWords)
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultEventPieceLimit
	}
	limit = min(limit, maxEventPieceLimit)
	// A source's batches no sync has read are its newest.
	source, err := s.deps.DB.ListFoundryEventBatches(ctx, ws, cid, eventPieceBatches)
	if err != nil {
		return nil, eventSourceError("list the API source's batches", cid, err)
	}
	var stored struct {
		TextField string `json:"text_field"`
	}
	if err := json.Unmarshal(source.State.Connector.Config, &stored); err != nil {
		return nil, status.Errorf(codes.Internal, "read the API source's settings: %v", err)
	}
	if stored.TextField == "" {
		return nil, eventPrecondition(reasonEventSourceNoText,
			"this API source has no text field, so its pieces are rows of its table with no text to list: choose its text field, then list its pieces")
	}
	select {
	case e.reading <- struct{}{}:
		defer func() { <-e.reading }()
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}

	waiting := source.Batches
	if synced := slices.IndexFunc(waiting, func(b *foundrymodel.FoundryEventBatch) bool { return b.Seq <= source.State.Cursor }); synced >= 0 {
		waiting = waiting[:synced]
	}
	list := &pieceList{limit: limit, words: words, schema: source.State.Schema, textField: stored.TextField, seen: map[string]bool{}}
	var read int64
	// A few batches at a time, so a list holds that many in memory however
	// many wait.
	for batches := range slices.Chunk(waiting, eventPieceBatchReads) {
		lines := make([][]events.Line, len(batches))
		g, gctx := errgroup.WithContext(ctx)
		for i, b := range batches {
			g.Go(func() error {
				body, err := e.objects.Open(gctx, b.S3Key)
				if err == nil {
					lines[i], err = events.Lines(body, events.Batch{ID: b.ID, Seq: b.Seq, Key: b.S3Key, Pieces: b.Pieces, Bytes: b.Bytes})
					_ = body.Close() // best-effort: only read
				}
				switch {
				case errors.Is(err, snapshot.ErrMissing), errors.Is(err, events.ErrCorrupt):
					return status.Errorf(codes.Internal, "read batch %d of this API source: %v", b.Seq, err)
				case err != nil:
					return status.Errorf(codes.Unavailable, "read batch %d of this API source: %v", b.Seq, err)
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
		for i, b := range batches {
			read += int64(b.Pieces)
			for _, l := range lines[i] {
				row, err := l.Row(list.schema)
				if err == nil {
					err = list.offer(l.ID(), l.ReceivedAt(), false, row)
				}
				if err != nil {
					return nil, status.Errorf(codes.Internal, "render piece %q of batch %d of this API source: %v", l.ID(), b.Seq, err)
				}
			}
		}
	}
	// More batches wait than one list reads: their pieces were left out.
	list.left = list.left || read < source.State.PendingPieces

	if key := source.State.TableKey; key != "" {
		whole, err := e.tableRows(ctx, key, list)
		if err != nil {
			return nil, err
		}
		list.left = list.left || !whole
	}
	list.trim()
	kept, text := 0, int64(0)
	for kept < len(list.kept) && (kept == 0 || text+list.kept[kept].bytes <= maxEventPieceTextBytes) {
		text += list.kept[kept].bytes
		kept++
	}
	resp := &foundryv1.ListFoundryEventPiecesResponse{Truncated: list.left || kept < len(list.kept), Pieces: make([]*foundryv1.FoundryEventSourcePiece, kept)}
	for i, p := range list.kept[:kept] {
		resp.Pieces[i] = p.piece
	}
	return resp, nil
}

// tableRows offers list the rows of the events table's snapshot at key, in
// the order it holds them, and reports whether it read the snapshot whole:
// it stops once it has read eventPieceTableBytes of it, uncompressed. A row
// no sync writes, or one the source's settings can't render, is INTERNAL; a
// snapshot that can't be read, such as one a sync replaced and its grace
// has removed, is UNAVAILABLE: the next list reads the source's new one.
func (e *eventSends) tableRows(ctx context.Context, key string, list *pieceList) (bool, error) {
	body, err := e.objects.Open(ctx, key)
	if err != nil {
		return false, status.Errorf(codes.Unavailable, "read the API source's table: %v", err)
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	gz, err := gzip.NewReader(bufio.NewReaderSize(body, 1<<16))
	if err != nil {
		return false, status.Errorf(codes.Unavailable, "read the API source's table: %v", err)
	}
	rows := bufio.NewReaderSize(gz, 1<<16)
	for read := 0; read < eventPieceTableBytes; {
		line, err := rows.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, status.Errorf(codes.Unavailable, "read the API source's table: %v", err)
		}
		read += len(line)
		if row := bytes.TrimSuffix(line, []byte("\n")); len(row) > 0 {
			var piece struct {
				ID         string    `json:"id"`
				ReceivedAt time.Time `json:"received_at"`
			}
			rerr := json.Unmarshal(row, &piece)
			if rerr == nil {
				rerr = list.offer(piece.ID, piece.ReceivedAt, true, row)
			}
			if rerr != nil {
				return false, status.Errorf(codes.Internal, "read the API source's table: %v", rerr)
			}
		}
		if err != nil {
			return true, nil
		}
	}
	// Whole only if nothing follows what was read.
	_, err = rows.Peek(1)
	return errors.Is(err, io.EOF), nil
}
