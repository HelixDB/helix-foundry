package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/time/rate"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryconfig "github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/events"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/keytoken"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// foundry_event_sources.go serves Foundry's event sources (connector kind
// events; people call them API sources). People and their agents send them
// pieces of JSON through SendFoundryEvents, as themselves or with a source's
// send-only token. A source is made, renamed, described, synced and deleted
// through the connector RPCs, which take the event-source paths here; its
// tokens and its batches have RPCs of their own. Package events holds every
// rule of a send, and store.SendFoundryEvents commits one under the
// source's row lock.

const (
	// eventStateIdle is how long nothing holds a source's or workspace's
	// send state before it is dropped, and eventSweepEvery how often idle
	// state is looked for.
	eventStateIdle  = 10 * time.Minute
	eventSweepEvery = time.Minute
	// eventPutTimeout bounds the write of a batch object, and its delete.
	eventPutTimeout = 10 * time.Second
	// eventCommitTimeout bounds a send's transaction, under the database's
	// 20 s transaction limit, so it ends by its own deadline before the database kills it.
	eventCommitTimeout = 15 * time.Second
	// maxEventSourceTokens is how many tokens a list shows, revoked ones included.
	maxEventSourceTokens = 50
	// defaultEventBatchLimit and maxEventBatchLimit bound a batch list.
	defaultEventBatchLimit = 20
	maxEventBatchLimit     = 100
	// eventErrorDomain is the domain of the reasons an event source's
	// refusals carry.
	eventErrorDomain = "foundry.helix-db.com"
)

// The reasons front ends tell an event source's FAILED_PRECONDITION refusals apart by.
const (
	reasonNotEventSource    = "FOUNDRY_NOT_EVENT_SOURCE"
	reasonEventSourceFull   = "FOUNDRY_EVENT_SOURCE_FULL"
	reasonEventSourcePaused = "FOUNDRY_EVENT_SOURCE_PAUSED"
	reasonEventSourceFields = "FOUNDRY_EVENT_SOURCE_FIELDS"
)

var (
	eventSendsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_event_sends_total",
		Help: "Sends to event sources by outcome (stored, replayed, invalid, denied, not_found, limited, not_event_source, paused, " +
			"fields, full, unavailable or failed) and by the front end they came through (via: mcp, http or other).",
	}, []string{"outcome", "via"})
	eventPiecesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "helix_foundry_event_pieces_total",
		Help: "Pieces stored in event sources' batches; a replayed send stores none.",
	})
	eventNearFullTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "helix_foundry_event_sends_near_full_total",
		Help: "Sends answered with a warning that their event source is at least 80% full.",
	})
)

// eventObjects is the Foundry data bucket as event sources use it: their
// batch objects, and the gzip trailers of their tables' snapshots.
// *snapshot.S3 implements it.
type eventObjects interface {
	Put(ctx context.Context, key string, body io.ReadSeeker) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	Tail(ctx context.Context, key string, n int64) ([]byte, error)
}

// eventSends is what the backend holds in memory for event sources' sends:
// per source, a commit lock, a rate bucket and the size of its
// table's snapshot; per workspace, a rate bucket and its UTC day's counters;
// and the deployment-wide semaphores of sends in progress and of send
// transactions. The backend runs one replica, so the limits are exact; N
// replicas would allow N times as much until the limits move to shared
// state. State nothing has held for eventStateIdle is dropped: by then its
// bucket has refilled, and its counters are seeded again from the day's
// batches.
type eventSends struct {
	cfg     foundryconfig.EventsConfig
	limits  events.Limits
	caps    events.Caps
	spacing foundrymodel.EventSyncSpacing
	objects eventObjects
	now     func() time.Time
	// checking is the deployment-wide semaphore of sends in progress
	// (max_sends_in_progress), each of which holds its pieces several times
	// over while it is checked, encoded and stored, before its commit.
	checking chan struct{}
	// slots is the deployment-wide semaphore of send transactions, each of which
	// holds one of the pool's connections while it runs.
	slots chan struct{}

	mu sync.Mutex
	// sources are by workspace and connector id, so a send naming another
	// workspace's connector never touches that source's state.
	sources    map[string]*eventSource
	workspaces map[string]*eventWorkspace
	swept      time.Time
}

func newEventSends(cfg *foundryconfig.Config, objects eventObjects, now func() time.Time) *eventSends {
	ev := cfg.Foundry.Events
	return &eventSends{
		cfg:     ev,
		limits:  events.Limits{MaxPieces: ev.MaxPiecesPerSend, MaxBytes: ev.MaxSendBytes, MaxPieceBytes: ev.MaxPieceBytes},
		caps:    events.Caps{MaxRows: cfg.Foundry.Sync.MaxRowsPerTable, MaxBytes: cfg.Foundry.Sync.MaxTableBytes},
		spacing: foundrymodel.EventSyncSpacing{After: ev.SyncAfter, PerMillionRows: ev.SyncAfterPerMillionRows},
		objects: objects, now: now,
		checking: make(chan struct{}, ev.MaxSendsInProgress), slots: make(chan struct{}, ev.SendConcurrency),
		sources: map[string]*eventSource{}, workspaces: map[string]*eventWorkspace{},
	}
}

// eventHolders counts the sends and lists holding a source's or a
// workspace's state, which is never dropped while held. eventSends.mu
// guards it.
type eventHolders struct {
	holders int
	// idleSince is when the last holder let go.
	idleSince time.Time
}

func (h eventHolders) idle(now time.Time) bool {
	return h.holders == 0 && now.Sub(h.idleSince) >= eventStateIdle
}

type eventSource struct {
	eventHolders
	// commit is the source's commit lock, a one-slot semaphore: the
	// source's sends queue here holding no pool connection, and a waiting
	// send gives up with its context.
	commit chan struct{}
	bucket *rate.Limiter
	// replays is the bucket of the retries a stored send answers, which
	// spend none of the send limits; it is as large as bucket.
	replays *rate.Limiter
	// key is the source's events table's snapshot, and size its
	// uncompressed size (ISIZE); eventSends.mu guards both. A key is never
	// reused, so a size never goes stale.
	key  string
	size int64
}

type eventWorkspace struct {
	eventHolders
	bucket *rate.Limiter
	// mu guards the day's counters, and is held while they are seeded.
	mu sync.Mutex
	// day is the UTC day pieces and bytes count, the zero time until they
	// are seeded from that day's batches; they include the reservations of
	// sends in flight. Sends in flight across midnight count on the day
	// they reserved.
	day           time.Time
	pieces, bytes int64
}

// enter holds a source's and its workspace's state, made when missing, until
// leave. It drops idle state, at most once every eventSweepEvery.
func (e *eventSends) enter(workspaceID, connectorID string) (*eventSource, *eventWorkspace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if now.Sub(e.swept) >= eventSweepEvery {
		maps.DeleteFunc(e.sources, func(_ string, s *eventSource) bool { return s.idle(now) })
		maps.DeleteFunc(e.workspaces, func(_ string, w *eventWorkspace) bool { return w.idle(now) })
		e.swept = now
	}
	key := workspaceID + "/" + connectorID
	src := e.sources[key]
	if src == nil {
		src = &eventSource{commit: make(chan struct{}, 1), bucket: rate.NewLimiter(rate.Limit(e.cfg.SourceSendsPerSecond), e.cfg.SourceSendsBurst),
			replays: rate.NewLimiter(rate.Limit(e.cfg.SourceSendsPerSecond), e.cfg.SourceSendsBurst)}
		e.sources[key] = src
	}
	ws := e.workspaces[workspaceID]
	if ws == nil {
		ws = &eventWorkspace{bucket: rate.NewLimiter(rate.Limit(e.cfg.WorkspaceSendsPerSecond), e.cfg.WorkspaceSendsBurst)}
		e.workspaces[workspaceID] = ws
	}
	src.holders++
	ws.holders++
	return src, ws
}

func (e *eventSends) leave(src *eventSource, ws *eventWorkspace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	src.holders, src.idleSince = src.holders-1, now
	ws.holders, ws.idleSince = ws.holders-1, now
}

// eventHold is what a send holds of its limits until it stores its batch: a
// token from its source's bucket and one from its workspace's, taken at at,
// and its share of the workspace's UTC day. A send that stores nothing gives
// it back.
type eventHold struct {
	src, ws       *rate.Reservation
	at            time.Time
	workspace     *eventWorkspace
	day           time.Time
	pieces, bytes int64
}

// limit takes a send's token from its source's bucket, then its workspace's,
// then reserves its pieces and bytes of the workspace's UTC day, checking and
// adding in one step so concurrent sends never pass the caps together. The
// day's counters are seeded from the day's batches when first needed. A
// refusal is RESOURCE_EXHAUSTED saying when to send again, and gives back
// what the send had taken.
func (e *eventSends) limit(ctx context.Context, db foundrystore.ReadStore, workspaceID string, src *eventSource, ws *eventWorkspace,
	send events.Send) (eventHold, error) {
	now := e.now()
	hold := eventHold{at: now, workspace: ws, pieces: int64(send.Pieces()), bytes: send.Bytes()}
	hold.src = src.bucket.ReserveN(now, 1)
	if wait := hold.src.DelayFrom(now); wait > 0 {
		hold.src.CancelAt(now)
		return eventHold{}, eventLimited(wait, "an API source takes at most %d sends a second, %d at once: send again in %s",
			e.cfg.SourceSendsPerSecond, e.cfg.SourceSendsBurst, wait.Round(time.Millisecond))
	}
	hold.ws = ws.bucket.ReserveN(now, 1)
	if wait := hold.ws.DelayFrom(now); wait > 0 {
		hold.ws.CancelAt(now)
		hold.src.CancelAt(now)
		return eventHold{}, eventLimited(wait, "a workspace's API sources take at most %d sends a second, %d at once: send again in %s",
			e.cfg.WorkspaceSendsPerSecond, e.cfg.WorkspaceSendsBurst, wait.Round(time.Millisecond))
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	today := now.UTC().Truncate(24 * time.Hour)
	if !ws.day.Equal(today) {
		totals, err := db.SumFoundryEventBatches(ctx, workspaceID, today)
		if err != nil {
			hold.ws.CancelAt(now)
			hold.src.CancelAt(now)
			return eventHold{}, status.Errorf(codes.Unavailable, "read the workspace's sends today: %v", err)
		}
		ws.day, ws.pieces, ws.bytes = today, totals.Pieces, totals.Bytes
	}
	reset := today.Add(24 * time.Hour).Sub(now)
	switch {
	case ws.pieces+hold.pieces > e.cfg.WorkspacePiecesPerDay:
		hold.ws.CancelAt(now)
		hold.src.CancelAt(now)
		return eventHold{}, eventLimited(reset, "this workspace has sent %d pieces today, and a workspace sends at most %d a day: "+
			"the count resets at 00:00 UTC", ws.pieces, e.cfg.WorkspacePiecesPerDay)
	case ws.bytes+hold.bytes > e.cfg.WorkspaceBytesPerDay:
		hold.ws.CancelAt(now)
		hold.src.CancelAt(now)
		return eventHold{}, eventLimited(reset, "this workspace has sent %d bytes of pieces today, and a workspace sends at most %d a day: "+
			"the count resets at 00:00 UTC", ws.bytes, e.cfg.WorkspaceBytesPerDay)
	}
	ws.pieces += hold.pieces
	ws.bytes += hold.bytes
	hold.day = today
	return hold, nil
}

// release gives back the hold of a send that stored nothing. A token is given
// back as of when it was taken: a rate.Reservation cancelled after the time
// it acted on gives nothing back. A day that has ended since was counted
// afresh from its batches, so it keeps its counts.
func (e *eventSends) release(h eventHold) {
	h.src.CancelAt(h.at)
	h.ws.CancelAt(h.at)
	h.workspace.mu.Lock()
	defer h.workspace.mu.Unlock()
	if h.workspace.day.Equal(h.day) {
		h.workspace.pieces -= h.pieces
		h.workspace.bytes -= h.bytes
	}
}

// tableBytes is the uncompressed size of the source's events table's
// snapshot at key, 0 for none: from the source's cache, or from one ranged
// GET of the snapshot's gzip trailer for a key it has not seen.
func (e *eventSends) tableBytes(ctx context.Context, src *eventSource, key string) (int64, error) {
	if key == "" {
		return 0, nil
	}
	e.mu.Lock()
	cached, size := src.key == key, src.size
	e.mu.Unlock()
	if cached {
		return size, nil
	}
	tail, err := e.objects.Tail(ctx, key, 4)
	if err != nil {
		return 0, err
	}
	if size, err = events.ISIZE(tail); err != nil {
		return 0, err
	}
	e.mu.Lock()
	src.key, src.size = key, size
	e.mu.Unlock()
	return size, nil
}

// errNotCommitted is a send that gave up before its transaction began: it
// stored nothing.
var errNotCommitted = errors.New("the send gave up before its commit")

// commit runs a send's transaction holding its source's commit lock and then
// a slot of the deployment-wide semaphore. A send waiting for either gives up with
// its context, and errNotCommitted. Once it holds both, its transaction runs
// to the end whatever its caller does, within eventCommitTimeout, so a caller
// that leaves never cuts a commit off part way.
func (e *eventSends) commit(ctx context.Context, db foundrystore.Store, src *eventSource, send foundrymodel.FoundryEventSend) (foundrymodel.FoundryEventsSent, error) {
	select {
	case src.commit <- struct{}{}:
	case <-ctx.Done():
		return foundrymodel.FoundryEventsSent{}, fmt.Errorf("%w: %w", errNotCommitted, ctx.Err())
	}
	defer func() { <-src.commit }()
	select {
	case e.slots <- struct{}{}:
	case <-ctx.Done():
		return foundrymodel.FoundryEventsSent{}, fmt.Errorf("%w: %w", errNotCommitted, ctx.Err())
	}
	defer func() { <-e.slots }()
	txCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventCommitTimeout)
	defer cancel()
	return db.SendFoundryEvents(txCtx, send)
}

// drop deletes the batch object of a send that stored nothing, best effort:
// an object left behind is an orphan no row names, which the purge of its
// source deletes.
func (e *eventSends) drop(ctx context.Context, key string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventPutTimeout)
	defer cancel()
	if err := e.objects.Delete(ctx, key); err != nil {
		log.From(ctx).Warn("delete the object of an unstored event batch failed", "key", key, "err", err)
	}
}

// SendFoundryEvents stores one send of pieces to an event source as one batch,
// in this order:
//  1. The actor. A send token is read again, and must still be live and of
//     this source, since the front end validated it before; a person was
//     authorized by the front end. The HTTP door (FOUNDRY_EVENTS) sends only
//     with a send token. Anyone else is PERMISSION_DENIED.
//     1b. A place among max_sends_in_progress (eventSends.checking), waited
//     for under the request's context: DEADLINE_EXCEEDED or CANCELED when it
//     ends first, having stored nothing. A send holds its pieces several
//     times over while it is checked, encoded and stored, so steps 2 to 6,
//     and a replay in step 3, run in the place. It is given up before step 7,
//     so a send queued behind its source's commit lock holds no place
//     another workspace's send needs.
//  2. The rules that need no state (events.Validate), and the client
//     token's form. A refusal of one piece names it in a BadRequest field
//     violation (eventSourceError).
//  3. A retry. When a batch of the source holds the client token, the send
//     is compared with it and answered, replayed or refused, before any
//     send limit, so a retry after a timeout always learns what happened.
//     Retries have a bucket of their own per source, as large as its sends':
//     each reads the batch's object and the source, so one send replayed in
//     a loop, by a leaked send token say, would otherwise hold the pool's
//     connections without end.
//  4. The limits, in memory: the source's rate, the workspace's, then a
//     reservation against the workspace's UTC day.
//  5. A pre-check with no lock: the kind, a pause, the schema and the caps,
//     so a refusal comes before anything is written.
//  6. The batch object, at events.BatchKey under a new batch id, before any
//     lock. A failed write is UNAVAILABLE.
//  7. The commit (store.SendFoundryEvents) under the source's commit lock
//     and a deployment-wide slot (eventSends.commit), which checks the client
//     token, the pause, the schema and the caps again against what other
//     sends committed meanwhile.
//
// A refusal from the commit deletes the object; an error whose outcome is
// unknown keeps it, since its row may have committed. A send refused or
// failed after step 4 gives its limits back. Every send is logged and
// counted, by its outcome and by the front end it came through (via: the MCP,
// the HTTP door or another), never with its pieces' content.
func (s *Service) SendFoundryEvents(ctx context.Context, req *foundryv1.SendFoundryEventsRequest) (*foundryv1.SendFoundryEventsResponse, error) {
	resp, err := s.sendFoundryEvents(ctx, req)
	outcome := eventSendOutcome(resp, err)
	caller := actor(ctx)
	via := "other"
	switch caller.Source {
	case ports.SourceMCP:
		via = "mcp"
	case ports.SourceEventSource:
		via = "http"
	}
	eventSendsTotal.WithLabelValues(outcome, via).Inc()
	sender := "none"
	switch {
	case caller.EventSourceTokenID != "":
		sender = "token:" + caller.EventSourceTokenID
	case caller.UserID != "":
		sender = "user:" + caller.UserID
	}
	attrs := []any{"workspace", req.GetWorkspaceId(), "source", req.GetConnectorId(), "sender", sender, "via", via, "outcome", outcome,
		"code", status.Code(err).String()}
	if b := resp.GetBatch(); b != nil {
		attrs = append(attrs, "batch", b.GetId(), "seq", b.GetSeq(), "pieces", b.GetPieces(), "bytes", b.GetBytes(), "replayed", resp.GetReplayed())
		if !resp.GetReplayed() {
			eventPiecesTotal.Add(float64(b.GetPieces()))
		}
	} else {
		attrs = append(attrs, "pieces", len(req.GetPieces()))
	}
	if resp.GetWarning() != "" {
		eventNearFullTotal.Inc()
	}
	log.From(ctx).Info("foundry event send", attrs...)
	return resp, err
}

func (s *Service) sendFoundryEvents(ctx context.Context, req *foundryv1.SendFoundryEventsRequest) (*foundryv1.SendFoundryEventsResponse, error) {
	e := s.deps.EventSends
	if e == nil {
		return nil, status.Error(codes.Unavailable, "API sources need the Foundry data bucket, which this installation does not have")
	}
	ws, cid, token := req.GetWorkspaceId(), req.GetConnectorId(), req.GetClientToken()
	if ws == "" || cid == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and connector_id are required")
	}

	// 1. The actor.
	switch caller := actor(ctx); {
	case caller.EventSourceTokenID != "":
		_, err := s.deps.DB.GetLiveFoundryEventSourceToken(ctx, ws, cid, caller.EventSourceTokenID)
		if errors.Is(err, foundrystore.ErrNotFound) {
			return nil, status.Error(codes.PermissionDenied, "this send token is revoked, or sends to another API source")
		}
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "read the send token: %v", err)
		}
	case caller.Source == ports.SourceEventSource:
		return nil, status.Error(codes.PermissionDenied, "a send over HTTP needs a send token of this API source")
	case caller.UserID == "":
		return nil, status.Error(codes.PermissionDenied, "only a person, or a send token of this API source, sends to it")
	}

	// 1b. A place among the sends in progress, given up before the commit.
	select {
	case e.checking <- struct{}{}:
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	checking := true
	defer func() {
		if checking {
			<-e.checking
		}
	}()

	// 2. The rules that need no state.
	if token != "" && !validClientToken(token) {
		return nil, status.Error(codes.InvalidArgument,
			"client_token must be 16 to 64 printable ASCII characters with no spaces: a new random value, such as a UUID, for each send")
	}
	pieces := make([]events.Piece, len(req.GetPieces()))
	for i, p := range req.GetPieces() {
		pieces[i] = events.Piece{ID: p.GetId(), Data: []byte(p.GetDataJson())}
		if p.GetOccurredAt() != nil {
			at := p.GetOccurredAt().AsTime()
			pieces[i].OccurredAt = &at
		}
	}
	send, err := events.Validate(pieces, e.limits)
	if err != nil {
		return nil, eventSourceError("check the pieces", cid, err)
	}
	src, wsState := e.enter(ws, cid)
	defer e.leave(src, wsState)

	// 3. A retry.
	if token != "" {
		switch batch, err := s.deps.DB.EventBatchByToken(ctx, ws, cid, token); {
		case err == nil:
			now := e.now()
			r := src.replays.ReserveN(now, 1)
			if wait := r.DelayFrom(now); wait > 0 {
				r.CancelAt(now)
				return nil, eventLimited(wait, "an API source answers at most %d retries of its sends a second, %d at once: send again in %s",
					e.cfg.SourceSendsPerSecond, e.cfg.SourceSendsBurst, wait.Round(time.Millisecond))
			}
			return s.replayFoundryEvents(ctx, src, send, batch)
		case !errors.Is(err, foundrystore.ErrNotFound):
			return nil, status.Errorf(codes.Unavailable, "read the send of this client_token: %v", err)
		}
	}

	// 4. The limits.
	hold, err := e.limit(ctx, s.deps.DB, ws, src, wsState, send)
	if err != nil {
		return nil, err
	}
	stored := false
	defer func() {
		if !stored {
			e.release(hold)
		}
	}()

	// 5. The pre-check.
	state, err := s.deps.DB.EventSendState(ctx, ws, cid)
	if err != nil {
		return nil, eventSourceError("read the API source", cid, err)
	}
	if paused := state.Connector.SchedulePaused; paused != nil {
		return nil, eventSourceError("", cid, &foundrystore.EventSourcePausedError{Reason: *paused})
	}
	merged, err := state.Schema.Merge(send, e.cfg.MaxFields)
	if err != nil {
		return nil, eventSourceError("", cid, err)
	}
	size, err := e.tableBytes(ctx, src, state.TableKey)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read the size of the API source's table: %v", err)
	}
	if _, err := state.Holding(size).Project(send, merged.Schema, e.caps); err != nil {
		return nil, eventSourceError("", cid, err)
	}

	// 6. The batch object.
	batchID := id.New()
	object, err := events.EncodeLines(send, e.now())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode the pieces: %v", err)
	}
	key := events.BatchKey(ws, cid, batchID)
	putCtx, cancel := context.WithTimeout(ctx, eventPutTimeout)
	err = e.objects.Put(putCtx, key, bytes.NewReader(object.Body))
	cancel()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "store the pieces: %v; send again with the same client_token", err)
	}

	// 7. The commit.
	commit := foundrymodel.FoundryEventSend{
		ID: batchID, WorkspaceID: ws, ConnectorID: cid, Key: key, Send: send, Object: object,
		MaxFields: e.cfg.MaxFields, Caps: e.caps, Spacing: e.spacing,
		TableBytes: func(ctx context.Context, key string) (int64, error) { return e.tableBytes(ctx, src, key) },
	}
	if token != "" {
		commit.ClientToken = &token
	}
	<-e.checking
	checking = false
	sent, err := e.commit(ctx, s.deps.DB, src, commit)
	var refusal *events.Refusal
	var paused *foundrystore.EventSourcePausedError
	switch {
	case err == nil:
	case errors.Is(err, foundrystore.ErrConflict):
		// A retry of this send committed first: this one answers as it.
		e.drop(ctx, key)
		won, err := s.deps.DB.EventBatchByToken(ctx, ws, cid, token)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "read the send of this client_token: %v", err)
		}
		return s.replayFoundryEvents(ctx, src, send, won)
	case errors.Is(err, errNotCommitted):
		e.drop(ctx, key)
		return nil, status.FromContextError(ctx.Err()).Err()
	case errors.Is(err, foundrystore.ErrNotFound), errors.Is(err, foundrystore.ErrNotEventSource), errors.As(err, &refusal), errors.As(err, &paused):
		e.drop(ctx, key)
		return nil, eventSourceError("", cid, err)
	default:
		// The commit may have landed, so its object stays: the retry this
		// asks for finds the batch if it did, and an orphan costs storage
		// only, until the source's purge.
		return nil, status.Errorf(codes.Unavailable, "commit the pieces: %v; send again with the same client_token", err)
	}
	stored = true
	return &foundryv1.SendFoundryEventsResponse{
		Batch: eventBatchToProto(&sent.Batch, 0), NewFields: eventFieldsToProto(sent.New),
		Capacity: eventCapacityToProto(sent.Capacity), Warning: sent.Capacity.Warning(),
	}, nil
}

// replayFoundryEvents answers a send whose client token a stored batch holds.
// The same pieces are that send, answered as stored then, with the source as
// it is now; other pieces are INVALID_ARGUMENT. The batch's object is read
// only when its count and size equal the send's, since the same pieces always
// encode to the same size. It stores nothing and spends no send limit.
func (s *Service) replayFoundryEvents(ctx context.Context, src *eventSource, send events.Send, batch *foundrymodel.FoundryEventBatch) (
	*foundryv1.SendFoundryEventsResponse, error) {
	e := s.deps.EventSends
	objectError := func(err error) error {
		if errors.Is(err, snapshot.ErrMissing) || errors.Is(err, events.ErrCorrupt) {
			return status.Errorf(codes.Internal, "read batch %d of this API source: %v", batch.Seq, err)
		}
		return status.Errorf(codes.Unavailable, "read batch %d of this API source: %v", batch.Seq, err)
	}
	same := batch.Pieces == send.Pieces() && batch.Bytes == send.Bytes()
	if same {
		body, err := e.objects.Open(ctx, batch.S3Key)
		if err != nil {
			return nil, objectError(err)
		}
		same, err = events.SameSend(body, send)
		_ = body.Close() // best-effort: only read
		if err != nil {
			return nil, objectError(err)
		}
	}
	if !same {
		return nil, status.Errorf(codes.InvalidArgument, "client_token %q was already used for a different send; use a new client_token for new pieces",
			*batch.ClientToken)
	}
	state, err := s.deps.DB.EventSendState(ctx, batch.WorkspaceID, batch.ConnectorID)
	if err != nil {
		return nil, eventSourceError("read the API source", batch.ConnectorID, err)
	}
	size, err := e.tableBytes(ctx, src, state.TableKey)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read the size of the API source's table: %v", err)
	}
	capacity := state.Holding(size).Capacity(state.Schema, e.caps)
	return &foundryv1.SendFoundryEventsResponse{
		Batch: eventBatchToProto(batch, state.Cursor), Replayed: true, Capacity: eventCapacityToProto(capacity), Warning: capacity.Warning(),
	}, nil
}

// validClientToken is 16 to 64 bytes, each printable ASCII other than space:
// a UUID fits, and the column's ascii_bin never pads two tokens equal.
func validClientToken(token string) bool {
	return len(token) >= 16 && len(token) <= 64 && strings.IndexFunc(token, func(r rune) bool { return r < 0x21 || r > 0x7E }) < 0
}

// eventSendOutcome is the outcome a send is counted and logged by.
func eventSendOutcome(resp *foundryv1.SendFoundryEventsResponse, err error) string {
	switch {
	case err == nil && resp.GetReplayed():
		return "replayed"
	case err == nil:
		return "stored"
	}
	st := status.Convert(err)
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			switch info.GetReason() {
			case reasonNotEventSource:
				return "not_event_source"
			case reasonEventSourcePaused:
				return "paused"
			case reasonEventSourceFields:
				return "fields"
			case reasonEventSourceFull:
				return "full"
			}
		}
	}
	switch st.Code() {
	case codes.InvalidArgument:
		return "invalid"
	case codes.PermissionDenied:
		return "denied"
	case codes.NotFound:
		return "not_found"
	case codes.ResourceExhausted:
		return "limited"
	case codes.Unavailable:
		return "unavailable"
	}
	return "failed"
}

// eventSourceError answers err, from reading or writing event source
// connectorID, or from a rule of a send: NOT_FOUND for no live connector of
// the workspace with this id, FAILED_PRECONDITION with its reason for one of
// another kind, a paused source, the field cap and a full source, and
// INVALID_ARGUMENT for any other rule, with a BadRequest field violation
// pieces[i] (from 0) when one piece broke it, so a front end can name the
// piece apart from the message. Anything else is UNAVAILABLE, doing op.
func eventSourceError(op, connectorID string, err error) error {
	var refusal *events.Refusal
	var paused *foundrystore.EventSourcePausedError
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return status.Error(codes.NotFound, "connector not found")
	case errors.Is(err, foundrystore.ErrNotEventSource):
		return eventPrecondition(reasonNotEventSource, fmt.Sprintf("connector %s is not an API source", connectorID))
	case errors.As(err, &paused):
		return eventPrecondition(reasonEventSourcePaused, fmt.Sprintf("this API source is paused: %s. Resume it from its page in Foundry, then send again",
			paused.Reason))
	case errors.As(err, &refusal) && refusal.Kind == events.TooManyFields:
		return eventPrecondition(reasonEventSourceFields, refusal.Message)
	case errors.As(err, &refusal) && refusal.Kind == events.Full:
		return eventPrecondition(reasonEventSourceFull, refusal.Message)
	case errors.As(err, &refusal) && refusal.Piece > 0:
		st := status.New(codes.InvalidArgument, refusal.Message)
		detail, err := st.WithDetails(&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{
			{Field: fmt.Sprintf("pieces[%d]", refusal.Piece-1)},
		}})
		if err != nil {
			return st.Err()
		}
		return detail.Err()
	case errors.As(err, &refusal):
		return status.Error(codes.InvalidArgument, refusal.Message)
	}
	return status.Errorf(codes.Unavailable, "%s: %v", op, err)
}

// eventPrecondition is a FAILED_PRECONDITION with the reason front ends tell
// it apart by.
func eventPrecondition(reason, msg string) error {
	st := status.New(codes.FailedPrecondition, msg)
	detail, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: eventErrorDomain})
	if err != nil {
		return st.Err()
	}
	return detail.Err()
}

// eventLimited is a RESOURCE_EXHAUSTED saying, in its RetryInfo, when to send again.
func eventLimited(after time.Duration, format string, args ...any) error {
	st := status.Newf(codes.ResourceExhausted, format, args...)
	detail, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(after)})
	if err != nil {
		return st.Err()
	}
	return detail.Err()
}

// ListFoundryEventBatches lists an event source's newest batches, each with
// whether a sync has merged it, with its pieces no sync has read yet and how
// full it is, from one consistent read.
func (s *Service) ListFoundryEventBatches(ctx context.Context, req *foundryv1.ListFoundryEventBatchesRequest) (*foundryv1.ListFoundryEventBatchesResponse, error) {
	e := s.deps.EventSends
	if e == nil {
		return nil, status.Error(codes.Unavailable, "API sources need the Foundry data bucket, which this installation does not have")
	}
	ws, cid := req.GetWorkspaceId(), req.GetConnectorId()
	if ws == "" || cid == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and connector_id are required")
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultEventBatchLimit
	}
	list, err := s.deps.DB.ListFoundryEventBatches(ctx, ws, cid, min(limit, maxEventBatchLimit))
	if err != nil {
		return nil, eventSourceError("list the API source's batches", cid, err)
	}
	src, wsState := e.enter(ws, cid)
	defer e.leave(src, wsState)
	size, err := e.tableBytes(ctx, src, list.State.TableKey)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read the size of the API source's table: %v", err)
	}
	resp := &foundryv1.ListFoundryEventBatchesResponse{
		PendingPieces: list.State.PendingPieces,
		Capacity:      eventCapacityToProto(list.State.Holding(size).Capacity(list.State.Schema, e.caps)),
	}
	for _, b := range list.Batches {
		resp.Batches = append(resp.Batches, eventBatchToProto(b, list.State.Cursor))
	}
	return resp, nil
}

// CreateFoundryEventSourceToken makes a send-only token of a live event
// source of the workspace, for the person asking: its secret is returned
// this once, and only its SHA-256 is stored. A source has at most
// foundry.events.max_tokens_per_source live tokens.
func (s *Service) CreateFoundryEventSourceToken(ctx context.Context, req *foundryv1.CreateFoundryEventSourceTokenRequest) (
	*foundryv1.CreateFoundryEventSourceTokenResponse, error) {
	userID := actor(ctx).UserID
	if userID == "" {
		return nil, status.Error(codes.PermissionDenied, "only a person makes send tokens")
	}
	c, err := s.liveEventSource(ctx, req.GetWorkspaceId(), req.GetConnectorId())
	if err != nil {
		return nil, err
	}
	secret, err := keytoken.NewEventSource()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "make send token: %v", err)
	}
	t := &foundrymodel.FoundryEventSourceToken{
		ID: id.New(), WorkspaceID: c.WorkspaceID, ConnectorID: c.ID, Hash: hashToken(secret), Prefix: secret[:12], CreatedBy: userID,
	}
	maxLive := s.deps.Cfg.Foundry.Events.MaxTokensPerSource
	switch err := s.deps.DB.CreateFoundryEventSourceToken(ctx, t, maxLive); {
	case errors.Is(err, foundrystore.ErrLimitExceeded):
		return nil, status.Errorf(codes.ResourceExhausted, "an API source has at most %d live send tokens: revoke one first", maxLive)
	case err != nil:
		return nil, eventSourceError("store the send token", c.ID, err)
	}
	log.From(ctx).Info("foundry event source token created", "workspace", c.WorkspaceID, "source", c.ID, "token", t.ID, "user", userID)
	return &foundryv1.CreateFoundryEventSourceTokenResponse{Token: eventTokenToProto(t), Secret: secret}, nil
}

// ListFoundryEventSourceTokens lists a live event source's send tokens,
// newest first, revoked ones included.
func (s *Service) ListFoundryEventSourceTokens(ctx context.Context, req *foundryv1.ListFoundryEventSourceTokensRequest) (
	*foundryv1.ListFoundryEventSourceTokensResponse, error) {
	c, err := s.liveEventSource(ctx, req.GetWorkspaceId(), req.GetConnectorId())
	if err != nil {
		return nil, err
	}
	tokens, err := s.deps.DB.ListFoundryEventSourceTokens(ctx, c.WorkspaceID, c.ID, maxEventSourceTokens)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list send tokens: %v", err)
	}
	resp := &foundryv1.ListFoundryEventSourceTokensResponse{}
	for _, t := range tokens {
		resp.Tokens = append(resp.Tokens, eventTokenToProto(t))
	}
	return resp, nil
}

// RevokeFoundryEventSourceToken revokes a send token of a live event source
// of the workspace: its next send is refused. Revoking it again is no error,
// and a token not matching all three ids is NOT_FOUND.
func (s *Service) RevokeFoundryEventSourceToken(ctx context.Context, req *foundryv1.RevokeFoundryEventSourceTokenRequest) (
	*foundryv1.FoundryEventSourceTokenResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	c, err := s.liveEventSource(ctx, req.GetWorkspaceId(), req.GetConnectorId())
	if err != nil {
		return nil, err
	}
	t, err := s.deps.DB.RevokeFoundryEventSourceToken(ctx, c.WorkspaceID, c.ID, req.GetId())
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "send token not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "revoke send token: %v", err)
	}
	log.From(ctx).Info("foundry event source token revoked", "workspace", c.WorkspaceID, "source", c.ID, "token", t.ID,
		"user", actor(ctx).UserID)
	return &foundryv1.FoundryEventSourceTokenResponse{Token: eventTokenToProto(t)}, nil
}

// ValidateFoundryEventSourceToken finds the live send token whose SHA-256 is
// hash, for a front end authenticating a sender, and records its use. A
// token unknown, revoked, or of a deleted source or workspace is not valid.
func (s *Service) ValidateFoundryEventSourceToken(ctx context.Context, req *foundryv1.ValidateFoundryEventSourceTokenRequest) (
	*foundryv1.ValidateFoundryEventSourceTokenResponse, error) {
	hash := req.GetHash()
	if len(hash) != 64 || strings.IndexFunc(hash, func(r rune) bool { return (r < '0' || r > '9') && (r < 'a' || r > 'f') }) >= 0 {
		return nil, status.Error(codes.InvalidArgument, "hash must be the lowercase hex SHA-256 of the token")
	}
	t, err := s.deps.DB.ValidateFoundryEventSourceToken(ctx, hash)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return &foundryv1.ValidateFoundryEventSourceTokenResponse{}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "validate send token: %v", err)
	}
	return &foundryv1.ValidateFoundryEventSourceTokenResponse{Valid: true, TokenId: t.ID, WorkspaceId: t.WorkspaceID, ConnectorId: t.ConnectorID}, nil
}

// liveEventSource is the workspace's live connector with this id, which
// must be an event source: NOT_FOUND for no such connector in the workspace,
// another workspace's included, and FOUNDRY_NOT_EVENT_SOURCE for another kind.
func (s *Service) liveEventSource(ctx context.Context, workspaceID, connectorID string) (*foundrymodel.Connector, error) {
	c, err := s.loadConnector(ctx, workspaceID, connectorID)
	if err != nil {
		return nil, err
	}
	if c.Kind != foundrymodel.ConnectorKindEvents {
		return nil, eventSourceError("", c.ID, foundrystore.ErrNotEventSource)
	}
	return c, nil
}

// updateEventSource is UpdateConnector for an event source: a new name,
// description or text field, each only when the request carries it, nothing
// else. Its schema, change cursor, next sync and sealed secret are its
// sends' and syncs', so store.UpdateEventSource sets the description and
// the text field in place under the row lock a send takes: a field a
// concurrent send learns, and the sync it schedules, both stay. A text field
// other than the stored one is locked as a string and refused as a send
// would be (INVALID_ARGUMENT for a field held as another type, the field
// cap's FAILED_PRECONDITION for one past it), and queues one full sync that
// renders the newest pieces by it; while a sync of the source is live it is
// FAILED_PRECONDITION and nothing is written. It has no interval to set, and
// no discovery to answer: its table's columns are its sends'.
func (s *Service) updateEventSource(ctx context.Context, catalog *connector.Catalog, current *foundrymodel.Connector, req *foundryv1.UpdateConnectorRequest) (
	*foundryv1.ConnectorResponse, error) {
	if req.SyncEverySeconds != nil {
		return nil, status.Error(codes.InvalidArgument, "an API source syncs when pieces arrive; it has no sync interval to set")
	}
	var update foundrymodel.EventSourceUpdate
	if req.Name != nil {
		name, err := connectorName(req.GetName())
		if err != nil {
			return nil, err
		}
		update.Name = &name
	}
	var prepared connector.EventsSettings
	if req.GetConfig() != nil {
		kind, src, _, err := sourceFromProto(catalog, req.GetConfig())
		if err != nil {
			return nil, err
		}
		if kind.Name() != current.Kind {
			return nil, status.Error(codes.InvalidArgument, "a connector's kind cannot change; create a new connector")
		}
		settings, _, err := s.testSource(ctx, current.WorkspaceID, kind, src)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(settings, &prepared); err != nil {
			return nil, status.Errorf(codes.Internal, "read API source settings: %v", err)
		}
		update.Description = prepared.Description
	}
	if update.Name == nil && update.Description == nil && prepared.TextField == nil {
		return nil, status.Error(codes.InvalidArgument, "set a name, description or text field to update")
	}
	var stored struct {
		TextField string `json:"text_field"`
	}
	if err := json.Unmarshal(current.Config, &stored); err != nil {
		return nil, status.Errorf(codes.Internal, "read API source %s settings: %v", current.ID, err)
	}
	if field := prepared.TextField; field != nil && *field != stored.TextField {
		update.Text = &foundrymodel.EventSourceText{Field: *field, MaxFields: s.deps.Cfg.Foundry.Events.MaxFields}
		if userID := actor(ctx).UserID; userID != "" {
			update.Text.RequestedByUserID = &userID
		}
	}
	// A text field the source already has, alone, leaves nothing to write.
	if update.Name != nil || update.Description != nil || update.Text != nil {
		var taken *foundrystore.EventSourceNameTakenError
		var refusal *events.Refusal
		switch err := s.deps.DB.UpdateEventSource(ctx, current.WorkspaceID, current.ID, update); {
		case errors.As(err, &taken):
			return nil, status.Errorf(codes.AlreadyExists, "the API source %s already has this name; choose another", taken.ConnectorID)
		case errors.Is(err, foundrystore.ErrConflict):
			return nil, status.Error(codes.FailedPrecondition, "a sync of this API source is running; change its text field in a minute")
		case errors.As(err, &refusal):
			return nil, eventSourceError("", current.ID, err)
		case errors.Is(err, foundrystore.ErrNotFound):
			return nil, status.Error(codes.NotFound, "connector not found")
		case err != nil:
			return nil, status.Errorf(codes.Internal, "update API source: %v", err)
		}
	}
	updated, err := s.loadConnector(ctx, current.WorkspaceID, current.ID)
	if err != nil {
		return nil, err
	}
	return s.connectorResponse(ctx, updated, nil)
}

// createEventSource stores c, a new event source, under its workspace's
// lock: a name a live event source of the workspace has is ALREADY_EXISTS
// naming that source, and a workspace has at most
// foundry.events.max_sources_per_workspace live sources. A text field in
// c's settings is locked as a string in its schema (events.Schema.WithText),
// its first field, so the source's first pieces must comply.
func (s *Service) createEventSource(ctx context.Context, c *foundrymodel.Connector) error {
	var settings connector.EventsSettings
	if err := json.Unmarshal(c.Config, &settings); err != nil {
		return status.Errorf(codes.Internal, "read API source settings: %v", err)
	}
	if field := settings.TextField; field != nil && *field != "" {
		schema, err := events.Schema{}.WithText(*field, s.deps.Cfg.Foundry.Events.MaxFields)
		if err != nil {
			return eventSourceError("", c.ID, err)
		}
		if c.Config, err = json.Marshal(struct {
			connector.EventsSettings
			Schema events.Schema `json:"schema"`
		}{settings, schema}); err != nil {
			return status.Errorf(codes.Internal, "encode API source settings: %v", err)
		}
	}
	maxSources := s.deps.Cfg.Foundry.Events.MaxSourcesPerWorkspace
	var taken *foundrystore.EventSourceNameTakenError
	switch err := s.deps.DB.CreateEventSource(ctx, c, maxSources); {
	case errors.As(err, &taken):
		return status.Errorf(codes.AlreadyExists, "the API source %s already has this name; choose another, or send to that source", taken.ConnectorID)
	case errors.Is(err, foundrystore.ErrLimitExceeded):
		return status.Errorf(codes.ResourceExhausted, "a workspace has at most %d API sources: delete one first", maxSources)
	case errors.Is(err, foundrystore.ErrNotFound):
		return status.Error(codes.NotFound, "workspace not found")
	case err != nil:
		return status.Errorf(codes.Internal, "create API source: %v", err)
	}
	return nil
}

// eventsConfigToProto rebuilds an event source's config from its stored
// settings: its description, its text field ("" for none) and the fields its
// sends learned, the text field's lock included.
func eventsConfigToProto(settings json.RawMessage) (*foundryv1.ConnectorConfig, error) {
	var stored struct {
		Description string        `json:"description"`
		TextField   string        `json:"text_field"`
		Schema      events.Schema `json:"schema"`
	}
	if err := json.Unmarshal(settings, &stored); err != nil {
		return nil, err
	}
	return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Events{Events: &foundryv1.EventsConnectorConfig{
		Description: proto.String(stored.Description), TextField: proto.String(stored.TextField), Fields: eventFieldsToProto(stored.Schema.Fields()),
	}}}, nil
}

func eventFieldsToProto(fields []events.Field) []*foundryv1.EventSourceField {
	out := make([]*foundryv1.EventSourceField, len(fields))
	for i, f := range fields {
		out[i] = &foundryv1.EventSourceField{Name: f.Name, Type: f.Type.String()}
	}
	return out
}

// eventBatchToProto is a batch of a source whose table holds every batch up
// to cursor's seq.
func eventBatchToProto(b *foundrymodel.FoundryEventBatch, cursor int64) *foundryv1.FoundryEventBatch {
	return &foundryv1.FoundryEventBatch{
		Id: b.ID, Seq: b.Seq, Pieces: int32(b.Pieces), Bytes: b.Bytes,
		FirstAt: timestamppb.New(b.FirstAt), LastAt: timestamppb.New(b.LastAt), CreatedAt: timestamppb.New(b.CreatedAt),
		Synced: b.Seq <= cursor,
	}
}

func eventCapacityToProto(c events.Capacity) *foundryv1.EventSourceCapacity {
	return &foundryv1.EventSourceCapacity{Rows: c.Rows, MaxRows: c.MaxRows, Bytes: c.Bytes, MaxBytes: c.MaxBytes}
}

func eventTokenToProto(t *foundrymodel.FoundryEventSourceToken) *foundryv1.FoundryEventSourceToken {
	out := &foundryv1.FoundryEventSourceToken{
		Id: t.ID, ConnectorId: t.ConnectorID, Prefix: t.Prefix, CreatedByUserId: t.CreatedBy, CreatedAt: timestamppb.New(t.CreatedAt),
	}
	if t.RevokedAt != nil {
		out.RevokedAt = timestamppb.New(*t.RevokedAt)
	}
	if t.LastUsedAt != nil {
		out.LastUsedAt = timestamppb.New(*t.LastUsedAt)
	}
	return out
}
