package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/events"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/keytoken"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// eventStore keeps event sources' batches and send tokens in memory beside
// connectorStore's connectors, and commits a send by the rules
// store.SendFoundryEvents applies under the source's lock. The connector
// writes of other kinds refuse an event source, so a test fails when the
// service takes the generic path for one.
type eventStore struct {
	*connectorStore
	mu     sync.Mutex
	sends  []*foundrymodel.FoundryEventBatch
	tokens []*foundrymodel.FoundryEventSourceToken
	// committing, when set, runs as a commit begins, before it reads anything.
	committing func(foundrymodel.FoundryEventSend)
	// commitErr and sumErr, when set, are every commit's and every day
	// read's answer.
	commitErr, sumErr error
	// sums counts the reads of a workspace's day, and listLimits are the
	// limits batches were listed with.
	sums       int
	listLimits []int
	// updates are the edits UpdateEventSource was handed.
	updates []foundrymodel.EventSourceUpdate
}

func newEventStore(connectors *connectorStore) *eventStore {
	return &eventStore{connectorStore: connectors}
}

func (s *eventStore) state(workspaceID, connectorID string) (foundrymodel.FoundryEventSourceState, error) {
	c, ok := s.rows[connectorID]
	switch {
	case !ok || c.WorkspaceID != workspaceID:
		return foundrymodel.FoundryEventSourceState{}, foundrystore.ErrNotFound
	case c.Kind != foundrymodel.ConnectorKindEvents:
		return foundrymodel.FoundryEventSourceState{}, foundrystore.ErrNotEventSource
	}
	var settings struct {
		Schema events.Schema `json:"schema"`
	}
	if err := json.Unmarshal(c.Config, &settings); err != nil {
		return foundrymodel.FoundryEventSourceState{}, err
	}
	var cursor struct {
		Seq int64 `json:"seq"`
	}
	if len(c.ChangeCursor) > 0 {
		if err := json.Unmarshal(c.ChangeCursor, &cursor); err != nil {
			return foundrymodel.FoundryEventSourceState{}, err
		}
	}
	copied := *c
	state := foundrymodel.FoundryEventSourceState{Connector: &copied, Schema: settings.Schema, Cursor: cursor.Seq}
	for _, t := range s.tables {
		if t.ConnectorID == connectorID && t.TableName == "events" {
			var columns []struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(t.Columns, &columns); err != nil {
				return foundrymodel.FoundryEventSourceState{}, err
			}
			state.TableKey, state.TableRows = t.S3Key, t.RowCount
			for _, col := range columns {
				state.TableFields = append(state.TableFields, col.Name)
			}
		}
	}
	for _, b := range s.sends {
		if b.ConnectorID == connectorID && b.Seq > state.Cursor {
			state.PendingPieces += int64(b.Pieces)
			state.PendingBytes += b.Bytes
		}
	}
	return state, nil
}

func (s *eventStore) EventSendState(_ context.Context, workspaceID, connectorID string) (foundrymodel.FoundryEventSourceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state(workspaceID, connectorID)
}

func (s *eventStore) byToken(workspaceID, connectorID, token string) (*foundrymodel.FoundryEventBatch, error) {
	for _, b := range s.sends {
		if b.WorkspaceID == workspaceID && b.ConnectorID == connectorID && b.ClientToken != nil && *b.ClientToken == token {
			copied := *b
			return &copied, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *eventStore) EventBatchByToken(_ context.Context, workspaceID, connectorID, token string) (*foundrymodel.FoundryEventBatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byToken(workspaceID, connectorID, token)
}

func (s *eventStore) SendFoundryEvents(ctx context.Context, send foundrymodel.FoundryEventSend) (foundrymodel.FoundryEventsSent, error) {
	if s.committing != nil {
		s.committing(send)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commitErr != nil {
		return foundrymodel.FoundryEventsSent{}, s.commitErr
	}
	state, err := s.state(send.WorkspaceID, send.ConnectorID)
	if err != nil {
		return foundrymodel.FoundryEventsSent{}, err
	}
	if send.ClientToken != nil {
		if _, err := s.byToken(send.WorkspaceID, send.ConnectorID, *send.ClientToken); err == nil {
			return foundrymodel.FoundryEventsSent{}, foundrystore.ErrConflict
		}
	}
	if state.Connector.SchedulePaused != nil {
		return foundrymodel.FoundryEventsSent{}, &foundrystore.EventSourcePausedError{Reason: *state.Connector.SchedulePaused}
	}
	merged, err := state.Schema.Merge(send.Send, send.MaxFields)
	if err != nil {
		return foundrymodel.FoundryEventsSent{}, err
	}
	var tableBytes int64
	if state.TableKey != "" {
		if tableBytes, err = send.TableBytes(ctx, state.TableKey); err != nil {
			return foundrymodel.FoundryEventsSent{}, err
		}
	}
	capacity, err := state.Holding(tableBytes).Project(send.Send, merged.Schema, send.Caps)
	if err != nil {
		return foundrymodel.FoundryEventsSent{}, err
	}
	batch := foundrymodel.FoundryEventBatch{
		ID: send.ID, WorkspaceID: send.WorkspaceID, ConnectorID: send.ConnectorID, Seq: 1, ClientToken: send.ClientToken, S3Key: send.Key,
		Pieces: send.Send.Pieces(), Bytes: send.Send.Bytes(), FirstAt: send.Object.FirstAt, LastAt: send.Object.LastAt, CreatedAt: send.Object.ReceivedAt,
	}
	for _, b := range s.sends {
		if b.ConnectorID == send.ConnectorID {
			batch.Seq = max(batch.Seq, b.Seq+1)
		}
	}
	s.sends = append(s.sends, &batch)
	row := s.rows[send.ConnectorID]
	if merged.Changed() {
		var config map[string]json.RawMessage
		if err := json.Unmarshal(row.Config, &config); err != nil {
			return foundrymodel.FoundryEventsSent{}, err
		}
		schema, _ := json.Marshal(merged.Schema)
		config["schema"] = schema
		row.Config, _ = json.Marshal(config)
	}
	if row.NextSyncAt == nil {
		next := time.Now().Add(send.Spacing.For(state.TableRows))
		row.NextSyncAt = &next
	}
	return foundrymodel.FoundryEventsSent{Batch: batch, New: merged.New, Capacity: capacity}, nil
}

func (s *eventStore) ListFoundryEventBatches(_ context.Context, workspaceID, connectorID string, limit int) (foundrymodel.FoundryEventBatchList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listLimits = append(s.listLimits, limit)
	state, err := s.state(workspaceID, connectorID)
	if err != nil {
		return foundrymodel.FoundryEventBatchList{}, err
	}
	list := foundrymodel.FoundryEventBatchList{State: state}
	for i := len(s.sends) - 1; i >= 0 && len(list.Batches) < limit; i-- {
		if s.sends[i].ConnectorID == connectorID {
			list.Batches = append(list.Batches, s.sends[i])
		}
	}
	return list, nil
}

func (s *eventStore) SumFoundryEventBatches(_ context.Context, workspaceID string, since time.Time) (foundrymodel.FoundryEventTotals, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sums++
	if s.sumErr != nil {
		return foundrymodel.FoundryEventTotals{}, s.sumErr
	}
	var totals foundrymodel.FoundryEventTotals
	for _, b := range s.sends {
		if b.WorkspaceID == workspaceID && !b.CreatedAt.Before(since) {
			totals.Pieces += int64(b.Pieces)
			totals.Bytes += b.Bytes
		}
	}
	return totals, nil
}

// live is the token with all three ids while it authenticates: not revoked,
// of a live event source of its workspace.
func (s *eventStore) live(t *foundrymodel.FoundryEventSourceToken) bool {
	c, ok := s.rows[t.ConnectorID]
	return t.RevokedAt == nil && ok && c.WorkspaceID == t.WorkspaceID && c.Kind == foundrymodel.ConnectorKindEvents
}

func (s *eventStore) GetLiveFoundryEventSourceToken(_ context.Context, workspaceID, connectorID, id string) (*foundrymodel.FoundryEventSourceToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tokens {
		if t.ID == id && t.WorkspaceID == workspaceID && t.ConnectorID == connectorID && s.live(t) {
			copied := *t
			return &copied, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *eventStore) ValidateFoundryEventSourceToken(_ context.Context, hash string) (*foundrymodel.FoundryEventSourceToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tokens {
		if t.Hash == hash && s.live(t) {
			copied := *t
			return &copied, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *eventStore) CreateFoundryEventSourceToken(_ context.Context, t *foundrymodel.FoundryEventSourceToken, maxLive int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.state(t.WorkspaceID, t.ConnectorID); err != nil {
		return err
	}
	live := 0
	for _, existing := range s.tokens {
		if existing.ConnectorID == t.ConnectorID && existing.RevokedAt == nil {
			live++
		}
	}
	if live >= maxLive {
		return foundrystore.ErrLimitExceeded
	}
	// Each token is a second apart, so newest first is well defined.
	t.CreatedAt = time.Date(2026, 10, 24, 12, 0, len(s.tokens), 0, time.UTC)
	copied := *t
	s.tokens = append(s.tokens, &copied)
	return nil
}

func (s *eventStore) ListFoundryEventSourceTokens(_ context.Context, workspaceID, connectorID string, limit int) ([]*foundrymodel.FoundryEventSourceToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*foundrymodel.FoundryEventSourceToken
	for i := len(s.tokens) - 1; i >= 0 && len(out) < limit; i-- {
		if t := s.tokens[i]; t.WorkspaceID == workspaceID && t.ConnectorID == connectorID {
			copied := *t
			out = append(out, &copied)
		}
	}
	return out, nil
}

func (s *eventStore) RevokeFoundryEventSourceToken(_ context.Context, workspaceID, connectorID, id string) (*foundrymodel.FoundryEventSourceToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tokens {
		if t.ID == id && t.WorkspaceID == workspaceID && t.ConnectorID == connectorID {
			if t.RevokedAt == nil {
				at := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
				t.RevokedAt = &at
			}
			copied := *t
			return &copied, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

// nameTaken is the oldest live event source of the workspace but exceptID
// with this name.
func (s *eventStore) nameTaken(workspaceID, name, exceptID string) error {
	var taken []*foundrymodel.Connector
	for _, c := range s.rows {
		if c.WorkspaceID == workspaceID && c.Kind == foundrymodel.ConnectorKindEvents && c.Name == name && c.ID != exceptID {
			taken = append(taken, c)
		}
	}
	if len(taken) == 0 {
		return nil
	}
	oldest := slices.MinFunc(taken, func(a, b *foundrymodel.Connector) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return &foundrystore.EventSourceNameTakenError{ConnectorID: oldest.ID}
}

func (s *eventStore) CreateEventSource(ctx context.Context, c *foundrymodel.Connector, maxSources int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.nameTaken(c.WorkspaceID, c.Name, c.ID); err != nil {
		return err
	}
	n := 0
	for _, existing := range s.rows {
		if existing.WorkspaceID == c.WorkspaceID && existing.Kind == foundrymodel.ConnectorKindEvents {
			n++
		}
	}
	if n >= maxSources {
		return foundrystore.ErrLimitExceeded
	}
	return s.connectorStore.CreateConnector(ctx, c)
}

// UpdateEventSource edits as store.UpdateEventSource does, all or
// nothing: a new text field is locked as a string and queues a full sync of
// origin events, refused while a sync of the source is live.
func (s *eventStore) UpdateEventSource(ctx context.Context, workspaceID, id string, u foundrymodel.EventSourceUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, u)
	c, ok := s.rows[id]
	if !ok || c.WorkspaceID != workspaceID || c.Kind != foundrymodel.ConnectorKindEvents {
		return foundrystore.ErrNotFound
	}
	if u.Name != nil {
		if err := s.nameTaken(workspaceID, *u.Name, id); err != nil {
			return err
		}
	}
	var config map[string]json.RawMessage
	var stored struct {
		TextField string        `json:"text_field"`
		Schema    events.Schema `json:"schema"`
	}
	if err := json.Unmarshal(c.Config, &config); err != nil {
		return err
	}
	if err := json.Unmarshal(c.Config, &stored); err != nil {
		return err
	}
	if u.Description != nil {
		config["description"], _ = json.Marshal(*u.Description)
	}
	if text := u.Text; text != nil && text.Field != stored.TextField {
		if text.Field != "" {
			locked, err := stored.Schema.WithText(text.Field, text.MaxFields)
			if err != nil {
				return err
			}
			config["schema"], _ = json.Marshal(locked)
		}
		config["text_field"], _ = json.Marshal(text.Field)
		if err := s.connectorStore.EnqueueConnectorSync(ctx, &foundrymodel.ConnectorSync{ID: fmt.Sprint("sync-text-", len(s.syncs)), ConnectorID: id,
			WorkspaceID: workspaceID, RequestedByUserID: text.RequestedByUserID, Origin: foundrymodel.ConnectorSyncEvents, Mode: foundrymodel.ConnectorSyncFull}); err != nil {
			return err
		}
	}
	if u.Name != nil {
		c.Name = *u.Name
	}
	c.Config, _ = json.Marshal(config)
	return nil
}

var errGenericEventWrite = errors.New("an event source written through the generic connector path")

func (s *eventStore) CreateConnector(ctx context.Context, c *foundrymodel.Connector) error {
	if c.Kind == foundrymodel.ConnectorKindEvents {
		return errGenericEventWrite
	}
	return s.connectorStore.CreateConnector(ctx, c)
}

func (s *eventStore) UpdateConnector(ctx context.Context, workspaceID, id string, u foundrymodel.ConnectorUpdate) error {
	if c, ok := s.rows[id]; ok && c.Kind == foundrymodel.ConnectorKindEvents {
		return errGenericEventWrite
	}
	return s.connectorStore.UpdateConnector(ctx, workspaceID, id, u)
}

// DeleteConnector deletes a connector of any kind, as the provider does, and
// revokes the live tokens an event source has with it.
func (s *eventStore) DeleteConnector(ctx context.Context, workspaceID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.connectorStore.DeleteConnector(ctx, workspaceID, id); err != nil {
		return err
	}
	for _, t := range s.tokens {
		if t.ConnectorID == id && t.RevokedAt == nil {
			at := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
			t.RevokedAt = &at
		}
	}
	return nil
}

// eventBucket is the Foundry data bucket in memory. putErr and tailErr, when
// set, fail every write and every tail read.
type eventBucket struct {
	mu                 sync.Mutex
	objects            map[string][]byte
	putErr, tailErr    error
	puts, opens, tails int
	deleted            []string
	// entering, when set, runs as each write ("put") and read ("open")
	// begins, before it touches the bucket; it may block.
	entering func(op string)
}

func (b *eventBucket) Put(_ context.Context, key string, body io.ReadSeeker) error {
	if b.entering != nil {
		b.entering("put")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.puts++
	if b.putErr != nil {
		return b.putErr
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	b.objects[key] = data
	return nil
}

func (b *eventBucket) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if b.entering != nil {
		b.entering("open")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opens++
	data, ok := b.objects[key]
	if !ok {
		return nil, fmt.Errorf("get %s: %w", key, snapshot.ErrMissing)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *eventBucket) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, key)
	b.deleted = append(b.deleted, key)
	return nil
}

func (b *eventBucket) Tail(_ context.Context, key string, n int64) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tails++
	if b.tailErr != nil {
		return nil, b.tailErr
	}
	data, ok := b.objects[key]
	if !ok {
		return nil, fmt.Errorf("get %s: %w", key, snapshot.ErrMissing)
	}
	return data[max(0, int64(len(data))-n):], nil
}

// batchKeys are the bucket's batch objects, by key.
func (b *eventBucket) batchKeys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Sorted(maps.Keys(b.objects))
}

// eventClock is the time the send state reads, moved by hand.
type eventClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *eventClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *eventClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

type eventHarness struct {
	s      *Service
	store  *eventStore
	bucket *eventBucket
	clock  *eventClock
	// source is the event source "Orders" of ws-1.
	source string
}

// person is user-1, signed in to the site.
var person = ports.Actor{Source: ports.SourceUser, UserID: "user-1"}

func eventsConfig(description string) *foundryv1.ConnectorConfig {
	return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Events{Events: &foundryv1.EventsConnectorConfig{Description: proto.String(description)}}}
}

// newEventHarness is a Free workspace ws-1 with one event source, Orders, at
// noon UTC, with foundry.events at its defaults but for what tune sets.
func newEventHarness(t *testing.T, tune func(*config.Config)) *eventHarness {
	t.Helper()
	s, connectors, _ := connectorService(t, "http://unused", freePlan)
	cfg := config.Defaults()
	if tune != nil {
		tune(&cfg)
	}
	h := &eventHarness{
		s: s, store: newEventStore(connectors), bucket: &eventBucket{objects: map[string][]byte{}},
		clock: &eventClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)},
	}
	s.deps.DB, s.deps.Cfg = h.store, &cfg
	s.deps.EventSends = newEventSends(&cfg, h.bucket, h.clock.now)
	h.source = h.addSource(t, "Orders")
	return h
}

func (h *eventHarness) addSource(t *testing.T, name string) string {
	t.Helper()
	resp, err := h.s.CreateConnector(ports.WithActor(context.Background(), person), &foundryv1.CreateConnectorRequest{
		WorkspaceId: "ws-1", Name: name, Config: eventsConfig("Orders placed in the shop"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetConnector().GetId()
}

// send sends pieces, each "id=data", to the source as the person.
func (h *eventHarness) send(connectorID, clientToken string, pieces ...string) (*foundryv1.SendFoundryEventsResponse, error) {
	req := &foundryv1.SendFoundryEventsRequest{WorkspaceId: "ws-1", ConnectorId: connectorID, ClientToken: clientToken}
	for _, p := range pieces {
		id, data, _ := strings.Cut(p, "=")
		req.Pieces = append(req.Pieces, &foundryv1.FoundryEventPiece{Id: id, DataJson: data})
	}
	return h.s.SendFoundryEvents(ports.WithActor(context.Background(), person), req)
}

// synced gives the source an events table of rows rows whose snapshot
// unpacks to size bytes, with columns id, received_at, occurred_at and
// fields.
func (h *eventHarness) synced(connectorID string, rows int64, size int, fields ...string) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(bytes.Repeat([]byte{'x'}, size))
	_ = w.Close()
	key := "ws/ws-1/connectors/" + connectorID + "/events/snap-1.jsonl.gz"
	h.bucket.objects[key] = gz.Bytes()
	var columns []map[string]string
	for _, name := range append([]string{"id", "received_at", "occurred_at"}, fields...) {
		columns = append(columns, map[string]string{"name": name})
	}
	encoded, _ := json.Marshal(columns)
	h.store.tables = append(h.store.tables, &foundrymodel.ConnectorTable{
		ConnectorID: connectorID, WorkspaceID: "ws-1", TableName: "events", S3Key: key, RowCount: rows, SyncID: "sync-1",
		Columns: encoded, PrimaryKey: []byte(`["id"]`), ForeignKeys: []byte(`[]`), SyncedAt: time.Now(),
	})
}

// violations are the fields of err's BadRequest field violations; nil for none.
func violations(err error) []string {
	var fields []string
	for _, d := range status.Convert(err).Details() {
		if br, ok := d.(*errdetails.BadRequest); ok {
			for _, v := range br.GetFieldViolations() {
				fields = append(fields, v.GetField())
			}
		}
	}
	return fields
}

func retryDelay(err error) time.Duration {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.RetryInfo); ok {
			return info.GetRetryDelay().AsDuration()
		}
	}
	return -1
}

func TestSendFoundryEvents(t *testing.T) {
	ctx := context.Background()

	// A send is one batch: its pieces in one object named by the batch, and
	// one row numbered in its source. The source learns the send's fields,
	// and syncs a spacing from now.
	t.Run("stores a send as one batch and schedules its source's sync", func(t *testing.T) {
		h := newEventHarness(t, nil)
		storedBefore := testutil.ToFloat64(eventSendsTotal.WithLabelValues("stored", "other"))
		piecesBefore := testutil.ToFloat64(eventPiecesTotal)
		resp, err := h.send(h.source, "", `o-1={"amount": 12.5, "customer": "c_1"}`, `o-2={"amount":3}`)
		if err != nil {
			t.Fatal(err)
		}
		b := resp.GetBatch()
		key := events.BatchKey("ws-1", h.source, b.GetId())
		if b.GetSeq() != 1 || b.GetPieces() != 2 || b.GetSynced() || resp.GetReplayed() || resp.GetWarning() != "" ||
			!b.GetCreatedAt().AsTime().Equal(h.clock.now()) || !slices.Equal(h.bucket.batchKeys(), []string{key}) {
			t.Fatalf("response = %+v, objects %v", resp, h.bucket.batchKeys())
		}
		if got := resp.GetNewFields(); len(got) != 2 || got[0].GetName() != "amount" || got[0].GetType() != "number" ||
			got[1].GetName() != "customer" || got[1].GetType() != "string" {
			t.Fatalf("new fields = %v", got)
		}
		if c := resp.GetCapacity(); c.GetRows() != 2 || c.GetMaxRows() != 2_000_000 || c.GetBytes() <= b.GetBytes() || c.GetMaxBytes() != 2<<30 {
			t.Fatalf("capacity = %+v", c)
		}
		gz, err := gzip.NewReader(bytes.NewReader(h.bucket.objects[key]))
		if err != nil {
			t.Fatal(err)
		}
		lines, _ := io.ReadAll(gz)
		if want := `{"id":"o-1","received_at":"2026-10-24T12:00:00.000000Z","data":{"amount":12.5,"customer":"c_1"}}` + "\n" +
			`{"id":"o-2","received_at":"2026-10-24T12:00:00.000000Z","data":{"amount":3}}` + "\n"; string(lines) != want || int64(len(lines)) != b.GetBytes() {
			t.Fatalf("object = %q, want %q", lines, want)
		}
		row := h.store.rows[h.source]
		if !strings.Contains(string(row.Config), `"schema":{"amount":"number","customer":"string"}`) || row.NextSyncAt == nil {
			t.Fatalf("source = %s, next sync %v", row.Config, row.NextSyncAt)
		}
		if testutil.ToFloat64(eventSendsTotal.WithLabelValues("stored", "other"))-storedBefore != 1 || testutil.ToFloat64(eventPiecesTotal)-piecesBefore != 2 {
			t.Fatal("the send was not counted")
		}
	})
	// The front end checked the token when the request came in; it may have
	// been revoked since, or name another source, so every send reads it again.
	t.Run("checks a send token again on every send", func(t *testing.T) {
		h := newEventHarness(t, nil)
		other := h.addSource(t, "Refunds")
		h.store.tokens = []*foundrymodel.FoundryEventSourceToken{
			{ID: "tok-live", WorkspaceID: "ws-1", ConnectorID: h.source},
			{ID: "tok-revoked", WorkspaceID: "ws-1", ConnectorID: h.source, RevokedAt: &time.Time{}},
			{ID: "tok-other", WorkspaceID: "ws-1", ConnectorID: other},
		}
		for token, want := range map[string]codes.Code{"tok-live": codes.OK, "tok-revoked": codes.PermissionDenied, "tok-other": codes.PermissionDenied} {
			sender := ports.Actor{Source: ports.SourceEventSource, EventSourceTokenID: token, WorkspaceID: "ws-1"}
			_, err := h.s.SendFoundryEvents(ports.WithActor(ctx, sender), &foundryv1.SendFoundryEventsRequest{
				WorkspaceId: "ws-1", ConnectorId: h.source,
				Pieces: []*foundryv1.FoundryEventPiece{{Id: "o-1", DataJson: `{}`}},
			})
			if status.Code(err) != want {
				t.Fatalf("send with %s = %v, want %v", token, err, want)
			}
		}
		if h.bucket.puts != 1 || len(h.store.sends) != 1 {
			t.Fatalf("%d objects and %d batches written, want the live token's alone", h.bucket.puts, len(h.store.sends))
		}
	})
	// Each step runs before the next can refuse: who sends, then the
	// envelope, then a retry, then the limits, then the source's state.
	t.Run("checks a send in the spec's order", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) { c.Foundry.Events.SourceSendsBurst = 1 })
		h.store.tokens = []*foundrymodel.FoundryEventSourceToken{{ID: "tok-revoked", WorkspaceID: "ws-1", ConnectorID: h.source, RevokedAt: &time.Time{}}}
		revoked := ports.Actor{Source: ports.SourceEventSource, EventSourceTokenID: "tok-revoked", WorkspaceID: "ws-1"}
		_, err := h.s.SendFoundryEvents(ports.WithActor(ctx, revoked), &foundryv1.SendFoundryEventsRequest{
			WorkspaceId: "ws-1", ConnectorId: h.source,
			Pieces: []*foundryv1.FoundryEventPiece{{Id: "o 1", DataJson: `[]`}},
		})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("a revoked token with bad pieces = %v, want the token refused first", err)
		}
		if _, err := h.send(h.source, "", `o-1={}`); err != nil {
			t.Fatal(err)
		}
		paused := "batch 4 of this event source is missing from storage"
		h.store.rows[h.source].SchedulePaused = &paused
		if _, err := h.send(h.source, "", `o-2={}`); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("a paused source with the bucket empty = %v, want the limit first", err)
		}
	})
	// A sender is a person or a source's token: an agent registration or a
	// service credential, which reach Foundry as an MCP caller with no user,
	// has no send of its own.
	t.Run("refuses a sender that is neither a person nor a send token", func(t *testing.T) {
		h := newEventHarness(t, nil)
		_, err := h.s.SendFoundryEvents(ports.WithActor(ctx, ports.Actor{Source: ports.SourceMCP}), &foundryv1.SendFoundryEventsRequest{
			WorkspaceId: "ws-1", ConnectorId: h.source,
			Pieces: []*foundryv1.FoundryEventPiece{{Id: "o-1", DataJson: `{}`}},
		})
		if status.Code(err) != codes.PermissionDenied || h.bucket.puts != 0 {
			t.Fatalf("send = %v with %d objects written", err, h.bucket.puts)
		}
	})
	// The HTTP door has no person behind it: a send through it without a
	// send token is refused before anything is read, whatever else its
	// actor claims.
	t.Run("refuses a send over HTTP without a send token, reading nothing", func(t *testing.T) {
		h := newEventHarness(t, nil)
		h.s.deps.DB = foundrystore.Unimplemented{} // every read panics
		before := testutil.ToFloat64(eventSendsTotal.WithLabelValues("denied", "http"))
		for _, actor := range []ports.Actor{
			{Source: ports.SourceEventSource},
			{Source: ports.SourceEventSource, UserID: "user-1"},
		} {
			_, err := h.s.SendFoundryEvents(ports.WithActor(ctx, actor), &foundryv1.SendFoundryEventsRequest{
				WorkspaceId: "ws-1", ConnectorId: h.source, Pieces: []*foundryv1.FoundryEventPiece{{Id: "o-1", DataJson: `{}`}},
			})
			if status.Code(err) != codes.PermissionDenied || h.bucket.puts != 0 {
				t.Fatalf("send as %v = %v with %d objects written", actor, err, h.bucket.puts)
			}
		}
		if testutil.ToFloat64(eventSendsTotal.WithLabelValues("denied", "http"))-before != 2 {
			t.Fatal("the refusals were not counted as denied sends over HTTP")
		}
	})
	// Sends are counted and logged by the front end they came through, so
	// the HTTP door's traffic is told from the MCP's.
	t.Run("counts and logs the front end a send came through", func(t *testing.T) {
		h := newEventHarness(t, nil)
		h.store.tokens = []*foundrymodel.FoundryEventSourceToken{{ID: "tok-live", WorkspaceID: "ws-1", ConnectorID: h.source}}
		var logs bytes.Buffer
		logCtx := log.Into(ctx, log.NewWith(&logs, "json", "info"))
		for i, tc := range []struct {
			actor ports.Actor
			via   string
		}{
			{ports.Actor{Source: ports.SourceEventSource, EventSourceTokenID: "tok-live"}, "http"},
			{ports.Actor{Source: ports.SourceMCP, EventSourceTokenID: "tok-live"}, "mcp"},
			{ports.Actor{Source: ports.SourceMCP, UserID: "user-1"}, "mcp"},
			{person, "other"},
		} {
			before := testutil.ToFloat64(eventSendsTotal.WithLabelValues("stored", tc.via))
			logs.Reset()
			if _, err := h.s.SendFoundryEvents(ports.WithActor(logCtx, tc.actor), &foundryv1.SendFoundryEventsRequest{
				WorkspaceId: "ws-1", ConnectorId: h.source, Pieces: []*foundryv1.FoundryEventPiece{{Id: fmt.Sprint("o-", i), DataJson: `{}`}},
			}); err != nil {
				t.Fatalf("send as %v = %v", tc.actor, err)
			}
			if testutil.ToFloat64(eventSendsTotal.WithLabelValues("stored", tc.via))-before != 1 {
				t.Fatalf("send as %v was not counted via %s", tc.actor, tc.via)
			}
			var line struct{ Msg, Via string }
			if err := json.Unmarshal(logs.Bytes(), &line); err != nil || line.Msg != "foundry event send" || line.Via != tc.via {
				t.Fatalf("send as %v logged %s (%v), want via %s", tc.actor, logs.Bytes(), err, tc.via)
			}
		}
	})
	// A retry after a timeout must learn what happened, even when the first
	// send spent the last of the bucket: the stored send answers it, and
	// nothing new is stored.
	t.Run("answers a retry from the stored send before any limit", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) {
			c.Foundry.Events.SourceSendsPerSecond, c.Foundry.Events.SourceSendsBurst = 1, 1
		})
		first, err := h.send(h.source, "4d2c5f1e-9a7b-4c3d-8e6f-1a2b3c4d5e6f", `o-1={"amount":1}`)
		if err != nil {
			t.Fatal(err)
		}
		replayedBefore := testutil.ToFloat64(eventSendsTotal.WithLabelValues("replayed", "other"))
		retry, err := h.send(h.source, "4d2c5f1e-9a7b-4c3d-8e6f-1a2b3c4d5e6f", `o-1={ "amount" : 1 }`)
		if err != nil {
			t.Fatal(err)
		}
		if !retry.GetReplayed() || retry.GetBatch().GetId() != first.GetBatch().GetId() || retry.GetCapacity().GetRows() != 1 ||
			h.bucket.puts != 1 || len(h.store.sends) != 1 {
			t.Fatalf("retry = %+v; %d objects, %d batches", retry, h.bucket.puts, len(h.store.sends))
		}
		if testutil.ToFloat64(eventSendsTotal.WithLabelValues("replayed", "other"))-replayedBefore != 1 {
			t.Fatal("the replay was not counted")
		}
		if _, err := h.send(h.source, "", `o-2={}`); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("a new send with the bucket empty = %v, want ResourceExhausted", err)
		}
	})
	// Each retry reads the stored object and the source, so retries of one
	// send in a loop are bounded by a bucket of their own, as large as the
	// source's sends', which they never spend: a retry within it is
	// answered, and new sends go on.
	t.Run("bounds retries by a bucket of their own", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) {
			c.Foundry.Events.SourceSendsPerSecond, c.Foundry.Events.SourceSendsBurst = 1, 3
		})
		const token = "4d2c5f1e-9a7b-4c3d-8e6f-1a2b3c4d5e6f"
		if _, err := h.send(h.source, token, `o-1={"amount":1}`); err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			if retry, err := h.send(h.source, token, `o-1={"amount":1}`); err != nil || !retry.GetReplayed() {
				t.Fatalf("retry %d = %+v, %v", i+1, retry, err)
			}
		}
		opens := h.bucket.opens
		_, err := h.send(h.source, token, `o-1={"amount":1}`)
		if status.Code(err) != codes.ResourceExhausted || h.bucket.opens != opens ||
			status.Convert(err).Message() != "an API source answers at most 1 retries of its sends a second, 3 at once: send again in 1s" {
			t.Fatalf("a fourth retry at once = %v after %d more reads", err, h.bucket.opens-opens)
		}
		if _, err := h.send(h.source, "", `o-2={}`); err != nil {
			t.Fatalf("a new send after the retries = %v", err)
		}
		h.clock.advance(time.Second)
		if retry, err := h.send(h.source, token, `o-1={"amount":1}`); err != nil || !retry.GetReplayed() {
			t.Fatalf("a retry a second later = %+v, %v", retry, err)
		}
	})
	// A client token names one send: other pieces under it are refused, and a
	// send of another size is told apart without reading the stored object.
	t.Run("refuses a client token reused for other pieces", func(t *testing.T) {
		h := newEventHarness(t, nil)
		const token = "retry-token-0001"
		if _, err := h.send(h.source, token, `o-1={"amount":1}`); err != nil {
			t.Fatal(err)
		}
		_, err := h.send(h.source, token, `o-1={"amount":2}`)
		if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() !=
			`client_token "retry-token-0001" was already used for a different send; use a new client_token for new pieces` || h.bucket.opens != 1 {
			t.Fatalf("same size, other data = %v after %d reads", err, h.bucket.opens)
		}
		if _, err := h.send(h.source, token, `o-1={"amount":1}`, `o-2={}`); status.Code(err) != codes.InvalidArgument || h.bucket.opens != 1 ||
			len(status.Convert(err).Details()) != 0 {
			t.Fatalf("another piece count = %v after %d reads, want no read", err, h.bucket.opens)
		}
		if len(h.store.sends) != 1 {
			t.Fatalf("%d batches stored", len(h.store.sends))
		}
	})
	// A committed batch whose object is gone is lost data, never a replay:
	// the retry is told so rather than that its pieces are stored.
	t.Run("refuses a retry whose stored object is gone", func(t *testing.T) {
		h := newEventHarness(t, nil)
		first, err := h.send(h.source, "retry-token-0001", `o-1={}`)
		if err != nil {
			t.Fatal(err)
		}
		delete(h.bucket.objects, events.BatchKey("ws-1", h.source, first.GetBatch().GetId()))
		if _, err := h.send(h.source, "retry-token-0001", `o-1={}`); status.Code(err) != codes.Internal || len(h.store.sends) != 1 {
			t.Fatalf("retry = %v with %d batches", err, len(h.store.sends))
		}
	})
	// The token's column is ascii_bin, and a short one would collide: each
	// must be 16 to 64 printable ASCII characters other than space.
	for name, token := range map[string]string{
		"15 characters": strings.Repeat("a", 15), "65 characters": strings.Repeat("a", 65),
		"a space": "retry token 0001", "a control character": "retry-token-0001\x7f", "a non-ASCII letter": "retry-token-00é1",
	} {
		t.Run("refuses a client token of "+name, func(t *testing.T) {
			h := newEventHarness(t, nil)
			if _, err := h.send(h.source, token, `o-1={}`); status.Code(err) != codes.InvalidArgument || h.bucket.puts != 0 {
				t.Fatalf("send = %v", err)
			}
		})
	}
	t.Run("accepts client tokens of 16 and 64 characters", func(t *testing.T) {
		h := newEventHarness(t, nil)
		for i, token := range []string{strings.Repeat("!", 16), strings.Repeat("~", 64)} {
			if _, err := h.send(h.source, token, fmt.Sprintf(`o-%d={}`, i)); err != nil {
				t.Fatalf("send with %q = %v", token, err)
			}
		}
	})
	// The envelope's rules are the events package's; the backend answers
	// them as INVALID_ARGUMENT with the package's message, before anything
	// is spent or written.
	t.Run("refuses pieces the envelope rules refuse", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) { c.Foundry.Events.SourceSendsBurst = 1 })
		_, err := h.send(h.source, "", `o-1={"id":"o-1"}`)
		if status.Code(err) != codes.InvalidArgument ||
			status.Convert(err).Message() != `piece 1 (id "o-1"): data may not have a top-level id; send it as the piece's id` || h.bucket.puts != 0 {
			t.Fatalf("send = %v", err)
		}
		if _, err := h.send(h.source, ""); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("a send of no pieces = %v", err)
		}
		if _, err := h.send(h.source, "", `o-1={}`); err != nil {
			t.Fatalf("a valid send after the refusals = %v; a refused envelope must spend no rate", err)
		}
	})
	// A front end answers the refused piece's place on its own, so a
	// refusal of one piece carries it, from 0, as a BadRequest field
	// violation beside the message it has always had: from the envelope,
	// the pre-check, and the commit, which sees what other sends locked.
	t.Run("names the refused piece apart from its message", func(t *testing.T) {
		h := newEventHarness(t, nil)
		_, err := h.send(h.source, "", `o-1={}`, `o-2={"amount":1}`, `o-3={"id":"o-3"}`)
		want := &errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: "pieces[2]"}}}
		st := status.Convert(err)
		if st.Code() != codes.InvalidArgument || st.Message() != `piece 3 (id "o-3"): data may not have a top-level id; send it as the piece's id` ||
			len(st.Details()) != 1 || !proto.Equal(st.Details()[0].(proto.Message), want) {
			t.Fatalf("send = %v with details %v", err, st.Details())
		}
		h.store.rows[h.source].Config = []byte(`{"description":"","schema":{"amount":"number"}}`)
		if _, err := h.send(h.source, "", `o-1={"amount":1}`, `o-2={"amount":"1"}`); !slices.Equal(violations(err), []string{"pieces[1]"}) {
			t.Fatalf("a type conflict at the pre-check = %v, violations %q", err, violations(err))
		}
		h.store.rows[h.source].Config = []byte(`{"description":"","schema":{}}`)
		h.store.committing = func(foundrymodel.FoundryEventSend) {
			// Another send locked amount as a number while this one waited.
			h.store.rows[h.source].Config = []byte(`{"description":"","schema":{"amount":"number"}}`)
		}
		_, err = h.send(h.source, "", `o-1={"note":"x"}`, `o-2={"amount":"1"}`)
		if status.Code(err) != codes.InvalidArgument || !slices.Equal(violations(err), []string{"pieces[1]"}) || len(h.bucket.batchKeys()) != 0 {
			t.Fatalf("a type conflict at the commit = %v, violations %q, objects %v", err, violations(err), h.bucket.batchKeys())
		}
	})
	// A rule of the send as a whole is no one piece's, so its refusal names
	// none: a front end then answers it without a piece.
	for name, tc := range map[string]struct {
		tune   func(*config.Config)
		token  string
		pieces []string
	}{
		"no pieces":             {nil, "", nil},
		"too many pieces":       {func(c *config.Config) { c.Foundry.Events.MaxPiecesPerSend = 1 }, "", []string{`o-1={}`, `o-2={}`}},
		"too many bytes":        {func(c *config.Config) { c.Foundry.Events.MaxSendBytes = 3 }, "", []string{`o-1={}`, `o-2={}`}},
		"a client token's form": {nil, "short", []string{`o-1={}`}},
	} {
		t.Run("names no piece in a refusal of "+name, func(t *testing.T) {
			h := newEventHarness(t, tc.tune)
			_, err := h.send(h.source, tc.token, tc.pieces...)
			if st := status.Convert(err); st.Code() != codes.InvalidArgument || len(st.Details()) != 0 {
				t.Fatalf("send = %v with details %v", err, st.Details())
			}
		})
	}

	// Each bucket says how long until it holds a send again.
	t.Run("limits a source's sends, saying when to send again", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) {
			c.Foundry.Events.SourceSendsPerSecond, c.Foundry.Events.SourceSendsBurst = 4, 1
		})
		if _, err := h.send(h.source, "", `o-1={}`); err != nil {
			t.Fatal(err)
		}
		_, err := h.send(h.source, "", `o-2={}`)
		if status.Code(err) != codes.ResourceExhausted || retryDelay(err) != 250*time.Millisecond {
			t.Fatalf("second send = %v, retry in %v, want 250ms", err, retryDelay(err))
		}
		// Another source has its own bucket.
		if _, err := h.send(h.addSource(t, "Refunds"), "", `r-1={}`); err != nil {
			t.Fatalf("a send to another source = %v", err)
		}
		h.clock.advance(250 * time.Millisecond)
		if _, err := h.send(h.source, "", `o-2={}`); err != nil {
			t.Fatalf("send after the delay = %v", err)
		}
	})
	t.Run("limits a workspace's sends across its sources", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) {
			c.Foundry.Events.WorkspaceSendsPerSecond, c.Foundry.Events.WorkspaceSendsBurst = 2, 1
		})
		if _, err := h.send(h.source, "", `o-1={}`); err != nil {
			t.Fatal(err)
		}
		_, err := h.send(h.addSource(t, "Refunds"), "", `r-1={}`)
		if status.Code(err) != codes.ResourceExhausted || retryDelay(err) != 500*time.Millisecond {
			t.Fatalf("send to a second source = %v, retry in %v", err, retryDelay(err))
		}
		h.clock.advance(500 * time.Millisecond)
		if _, err := h.send(h.source, "", `o-2={}`); err != nil {
			t.Fatalf("send after the delay = %v", err)
		}
	})
	// The day's counters start from what the workspace already sent today,
	// so a restart does not hand out a second day's worth.
	t.Run("caps a workspace's pieces a day, counting its earlier sends", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) { c.Foundry.Events.WorkspacePiecesPerDay = 3 })
		h.store.sends = []*foundrymodel.FoundryEventBatch{
			{ID: "b-yesterday", WorkspaceID: "ws-1", ConnectorID: h.source, Seq: 1, Pieces: 50, CreatedAt: h.clock.now().Add(-13 * time.Hour)},
			{ID: "b-today", WorkspaceID: "ws-1", ConnectorID: h.source, Seq: 2, Pieces: 2, CreatedAt: h.clock.now().Add(-time.Hour)},
		}
		_, err := h.send(h.source, "", `o-1={}`, `o-2={}`)
		if status.Code(err) != codes.ResourceExhausted || retryDelay(err) != 12*time.Hour ||
			!strings.Contains(status.Convert(err).Message(), "resets at 00:00 UTC") {
			t.Fatalf("send past the day's pieces = %v, retry in %v", err, retryDelay(err))
		}
		if _, err := h.send(h.source, "", `o-1={}`); err != nil {
			t.Fatalf("send of the day's last piece = %v", err)
		}
		if _, err := h.send(h.source, "", `o-2={}`); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("send past the cap = %v", err)
		}
		h.clock.advance(12 * time.Hour)
		if _, err := h.send(h.source, "", `o-2={}`); err != nil {
			t.Fatalf("send the next day = %v", err)
		}
	})
	t.Run("caps a workspace's bytes a day", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) { c.Foundry.Events.WorkspaceBytesPerDay = 100 })
		h.store.sends = []*foundrymodel.FoundryEventBatch{{ID: "b-today", WorkspaceID: "ws-1", ConnectorID: h.source, Seq: 1, Pieces: 1, Bytes: 60,
			CreatedAt: h.clock.now().Add(-time.Hour)}}
		// One piece's line, {"id":"o-1","received_at":"…","data":{}}, is more than the 40 bytes left.
		_, err := h.send(h.source, "", `o-1={}`)
		if status.Code(err) != codes.ResourceExhausted || retryDelay(err) != 12*time.Hour {
			t.Fatalf("send past the day's bytes = %v", err)
		}
	})
	// Without the day's count the cap cannot be kept, so the send waits for
	// it, and spends no rate meanwhile.
	t.Run("stores nothing while the day's count cannot be read", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) { c.Foundry.Events.SourceSendsBurst = 1 })
		h.store.sumErr = errors.New("mysql is down")
		if _, err := h.send(h.source, "", `o-1={}`); status.Code(err) != codes.Unavailable || h.bucket.puts != 0 {
			t.Fatalf("send = %v with %d objects", err, h.bucket.puts)
		}
		h.store.sumErr = nil
		if _, err := h.send(h.source, "", `o-1={}`); err != nil {
			t.Fatalf("send once the count reads = %v", err)
		}
	})
	// Limits count what is stored: a send that stores nothing, refused or
	// failed, gives back its token and its share of the day.
	t.Run("gives back its limits when it stores nothing", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) {
			c.Foundry.Events.SourceSendsBurst, c.Foundry.Events.WorkspaceSendsBurst, c.Foundry.Events.WorkspacePiecesPerDay = 1, 1, 1
		})
		h.store.rows[h.source].Config = []byte(`{"description":"","schema":{"amount":"number"}}`)
		if _, err := h.send(h.source, "", `o-1={"amount":"one"}`); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("a type conflict = %v", err)
		}
		h.bucket.putErr = errors.New("s3 is down")
		if _, err := h.send(h.source, "", `o-1={"amount":1}`); status.Code(err) != codes.Unavailable {
			t.Fatalf("a failed write = %v", err)
		}
		h.bucket.putErr = nil
		// Time passes during a commit: the token given back is the one taken.
		h.store.commitErr = errors.New("commit tx: invalid connection")
		h.store.committing = func(foundrymodel.FoundryEventSend) { h.clock.advance(50 * time.Millisecond) }
		if _, err := h.send(h.source, "", `o-1={"amount":1}`); status.Code(err) != codes.Unavailable {
			t.Fatalf("a failed commit = %v", err)
		}
		h.store.commitErr, h.store.committing = nil, nil
		if _, err := h.send(h.source, "", `o-1={"amount":1}`); err != nil {
			t.Fatalf("a send after them = %v; the refused and failed sends kept their limits", err)
		}
	})

	// Everything that can refuse a send is checked before its object is
	// written, so a refused send leaves nothing behind. A refusal of one
	// piece names it in a field violation; one of the send as a whole, none.
	for _, tc := range []struct {
		name      string
		tune      func(*config.Config)
		setup     func(h *eventHarness) (workspaceID, connectorID string)
		piece     string
		code      codes.Code
		reason    string
		outcome   string
		inMessage string
		violation []string
	}{
		{"refuses another workspace's source", nil, func(h *eventHarness) (string, string) { return "ws-2", h.source },
			`o-1={}`, codes.NotFound, "", "not_found", "connector not found", nil},
		{"refuses a connector of another kind", nil, func(h *eventHarness) (string, string) {
			h.store.rows["conn-stripe"] = &foundrymodel.Connector{ID: "conn-stripe", WorkspaceID: "ws-1", Kind: "stripe", Config: []byte(`{}`)}
			return "ws-1", "conn-stripe"
		}, `o-1={}`, codes.FailedPrecondition, "FOUNDRY_NOT_EVENT_SOURCE", "not_event_source", "conn-stripe is not an API source", nil},
		{"refuses a paused source, saying why", nil, func(h *eventHarness) (string, string) {
			paused := "batch 4 of this event source is missing from storage"
			h.store.rows[h.source].SchedulePaused = &paused
			return "ws-1", h.source
		}, `o-1={}`, codes.FailedPrecondition, "FOUNDRY_EVENT_SOURCE_PAUSED", "paused", "batch 4 of this event source is missing", nil},
		{"refuses a field of another type", nil, func(h *eventHarness) (string, string) {
			h.store.rows[h.source].Config = []byte(`{"description":"","schema":{"amount":"number"}}`)
			return "ws-1", h.source
		}, `o-1={"amount":"12"}`, codes.InvalidArgument, "", "invalid", `"amount" is a string; this source holds "amount" as a number`, []string{"pieces[0]"}},
		{"refuses a field past the cap", func(c *config.Config) { c.Foundry.Events.MaxFields = 1 }, func(h *eventHarness) (string, string) {
			return "ws-1", h.source
		}, `o-1={"a":1,"b":2}`, codes.FailedPrecondition, "FOUNDRY_EVENT_SOURCE_FIELDS", "fields", "past the 1 a source may have", nil},
		{"refuses a send past the rows cap", func(c *config.Config) { c.Foundry.Sync.MaxRowsPerTable = 10 }, func(h *eventHarness) (string, string) {
			h.synced(h.source, 10, 100)
			return "ws-1", h.source
		}, `o-1={}`, codes.FailedPrecondition, "FOUNDRY_EVENT_SOURCE_FULL", "full", "one source holds at most 10", nil},
		{"refuses a send past the bytes cap", func(c *config.Config) { c.Foundry.Sync.MaxTableBytes = 1000 }, func(h *eventHarness) (string, string) {
			h.synced(h.source, 5, 990)
			return "ws-1", h.source
		}, `o-1={}`, codes.FailedPrecondition, "FOUNDRY_EVENT_SOURCE_FULL", "full", "one source holds at most 1,000", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newEventHarness(t, tc.tune)
			ws, cid := tc.setup(h)
			id, data, _ := strings.Cut(tc.piece, "=")
			before := testutil.ToFloat64(eventSendsTotal.WithLabelValues(tc.outcome, "other"))
			_, err := h.s.SendFoundryEvents(ports.WithActor(ctx, person), &foundryv1.SendFoundryEventsRequest{WorkspaceId: ws, ConnectorId: cid,
				Pieces: []*foundryv1.FoundryEventPiece{{Id: id, DataJson: data}}})
			if status.Code(err) != tc.code || foundryReason(err) != tc.reason || !strings.Contains(status.Convert(err).Message(), tc.inMessage) {
				t.Fatalf("send = %v (reason %q), want %v %q with %q", err, foundryReason(err), tc.code, tc.reason, tc.inMessage)
			}
			if got := violations(err); !slices.Equal(got, tc.violation) {
				t.Fatalf("field violations %q, want %q", got, tc.violation)
			}
			if h.bucket.puts != 0 || len(h.store.sends) != 0 {
				t.Fatalf("%d objects and %d batches written", h.bucket.puts, len(h.store.sends))
			}
			if testutil.ToFloat64(eventSendsTotal.WithLabelValues(tc.outcome, "other"))-before != 1 {
				t.Fatalf("the refusal was not counted as %s", tc.outcome)
			}
		})
	}

	// A write S3 refuses stores no row, and the sender is told to send again.
	t.Run("answers a failed write as unavailable", func(t *testing.T) {
		h := newEventHarness(t, nil)
		h.bucket.putErr = errors.New("SlowDown: please reduce your request rate")
		_, err := h.send(h.source, "retry-token-0001", `o-1={}`)
		if status.Code(err) != codes.Unavailable || !strings.Contains(status.Convert(err).Message(), "same client_token") || len(h.store.sends) != 0 {
			t.Fatalf("send = %v with %d batches", err, len(h.store.sends))
		}
	})
	// The lock may see what the pre-check did not: a refusal there has
	// written an object that no row will ever name, so it is deleted.
	t.Run("deletes its object when the commit refuses it", func(t *testing.T) {
		h := newEventHarness(t, nil)
		h.store.committing = func(foundrymodel.FoundryEventSend) {
			paused := "the sync stopped its worker 3 times; Helix paused it"
			h.store.rows[h.source].SchedulePaused = &paused
		}
		_, err := h.send(h.source, "", `o-1={}`)
		if foundryReason(err) != "FOUNDRY_EVENT_SOURCE_PAUSED" || len(h.bucket.batchKeys()) != 0 || len(h.bucket.deleted) != 1 || len(h.store.sends) != 0 {
			t.Fatalf("send = %v; objects %v, deleted %v", err, h.bucket.batchKeys(), h.bucket.deleted)
		}
	})
	// Two tries of one send can race past the replay check; the one that
	// commits second answers as the first, and leaves no object of its own.
	t.Run("answers as the retry that committed first", func(t *testing.T) {
		h := newEventHarness(t, nil)
		token := "retry-token-0001"
		won := events.BatchKey("ws-1", h.source, "batch-won")
		h.store.committing = func(send foundrymodel.FoundryEventSend) {
			// Another try of the send, on another replica, commits first with its own object.
			h.store.committing = nil
			h.bucket.objects[won] = h.bucket.objects[send.Key]
			h.store.sends = append(h.store.sends, &foundrymodel.FoundryEventBatch{ID: "batch-won", WorkspaceID: "ws-1", ConnectorID: h.source, Seq: 1,
				ClientToken: &token, S3Key: won, Pieces: send.Send.Pieces(), Bytes: send.Send.Bytes(), CreatedAt: send.Object.ReceivedAt})
		}
		resp, err := h.send(h.source, token, `o-1={}`)
		if err != nil {
			t.Fatal(err)
		}
		if !resp.GetReplayed() || resp.GetBatch().GetId() != "batch-won" || len(h.store.sends) != 1 || !slices.Equal(h.bucket.batchKeys(), []string{won}) {
			t.Fatalf("second try = %+v; objects %v", resp, h.bucket.batchKeys())
		}
	})
	// A commit that errs may still have landed: deleting its object then
	// could leave a committed batch with no pieces, which stalls the source
	// for good. The object stays, and the retry finds whichever happened.
	t.Run("keeps its object when its commit's outcome is unknown", func(t *testing.T) {
		h := newEventHarness(t, nil)
		h.store.commitErr = errors.New("commit tx: invalid connection")
		_, err := h.send(h.source, "retry-token-0001", `o-1={}`)
		if status.Code(err) != codes.Unavailable || len(h.bucket.batchKeys()) != 1 || len(h.bucket.deleted) != 0 {
			t.Fatalf("send = %v; objects %v, deleted %v", err, h.bucket.batchKeys(), h.bucket.deleted)
		}
	})
	// From 80% full every answer says how full the source is, and what
	// happens at 100%, and such sends are counted.
	t.Run("warns from 80% full", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) { c.Foundry.Sync.MaxRowsPerTable = 10 })
		h.synced(h.source, 6, 100)
		resp, err := h.send(h.source, "", `o-1={}`)
		if err != nil || resp.GetWarning() != "" || resp.GetCapacity().GetRows() != 7 {
			t.Fatalf("a send to 70%% = %+v, %v", resp, err)
		}
		before := testutil.ToFloat64(eventNearFullTotal)
		resp, err = h.send(h.source, "", `o-2={}`)
		if err != nil || resp.GetCapacity().GetRows() != 8 || resp.GetWarning() !=
			"This source is 80% full (8 of 10 pieces). Once it is full, sends are refused: send new pieces to a new source before then." {
			t.Fatalf("a send to 80%% = %+v, %v", resp, err)
		}
		if testutil.ToFloat64(eventNearFullTotal)-before != 1 {
			t.Fatal("the near-full send was not counted")
		}
	})
	// A snapshot's size comes from its gzip trailer, read once per key: keys
	// are never reused.
	t.Run("reads its table's size once per snapshot", func(t *testing.T) {
		h := newEventHarness(t, nil)
		h.synced(h.source, 3, 300)
		for i := range 3 {
			resp, err := h.send(h.source, "", fmt.Sprintf(`o-%d={}`, i))
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetCapacity().GetBytes() < 300 {
				t.Fatalf("capacity = %+v, want the table's 300 bytes counted", resp.GetCapacity())
			}
		}
		if h.bucket.tails != 1 {
			t.Fatalf("read the snapshot's size %d times, want once", h.bucket.tails)
		}
		// A sync published a newer snapshot, whose size S3 cannot read now.
		h.bucket.tailErr = errors.New("s3 is down")
		h.store.tables[0].S3Key = "ws/ws-1/connectors/" + h.source + "/events/snap-2.jsonl.gz"
		if _, err := h.send(h.source, "", `o-9={}`); status.Code(err) != codes.Unavailable || h.bucket.puts != 3 {
			t.Fatalf("send with a new snapshot S3 cannot size = %v", err)
		}
	})
	// A source's sends queue in memory, and at most send_concurrency send
	// transactions hold pool connections at once, deployment-wide.
	t.Run("commits one send of a source at a time, and send_concurrency in all", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) { c.Foundry.Events.SendConcurrency = 2 })
		sources := []string{h.source, h.addSource(t, "Refunds"), h.addSource(t, "Signups")}
		entered, release := make(chan struct{}, 8), make(chan struct{})
		var mu sync.Mutex
		running, most, bySource := 0, 0, map[string]int{}
		overlapped := false
		h.store.committing = func(send foundrymodel.FoundryEventSend) {
			mu.Lock()
			running, bySource[send.ConnectorID] = running+1, bySource[send.ConnectorID]+1
			most, overlapped = max(most, running), overlapped || bySource[send.ConnectorID] > 1
			mu.Unlock()
			entered <- struct{}{}
			<-release
			mu.Lock()
			running, bySource[send.ConnectorID] = running-1, bySource[send.ConnectorID]-1
			mu.Unlock()
		}
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for i, cid := range append(sources, h.source) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := h.send(cid, "", fmt.Sprintf("p-%d={}", i))
				errs <- err
			}()
		}
		for range 2 {
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("two commits never began")
			}
		}
		for range 4 {
			release <- struct{}{}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if most != 2 || overlapped || len(h.store.sends) != 4 {
			t.Fatalf("%d commits at most at once, overlapped %v, %d batches", most, overlapped, len(h.store.sends))
		}
	})
	t.Run("never commits two sends of one source together", func(t *testing.T) {
		h := newEventHarness(t, nil)
		entered, release := make(chan struct{}, 2), make(chan struct{}, 2)
		h.store.committing = func(foundrymodel.FoundryEventSend) {
			entered <- struct{}{}
			<-release
		}
		done := make(chan error, 2)
		for i := range 2 {
			go func() {
				_, err := h.send(h.source, "", fmt.Sprintf("p-%d={}", i))
				done <- err
			}()
		}
		<-entered
		select {
		case <-entered:
			t.Fatal("a second commit of the source began while the first ran")
		case <-time.After(100 * time.Millisecond):
		}
		release <- struct{}{}
		<-entered
		release <- struct{}{}
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		if seqs := []int64{h.store.sends[0].Seq, h.store.sends[1].Seq}; !slices.Equal(seqs, []int64{1, 2}) {
			t.Fatalf("seqs = %v", seqs)
		}
	})
	// A caller that gives up while its send waits to commit stores nothing,
	// and leaves no object behind.
	t.Run("stores nothing when its caller leaves while it waits", func(t *testing.T) {
		h := newEventHarness(t, nil)
		src, ws := h.s.deps.EventSends.enter("ws-1", h.source)
		src.commit <- struct{}{} // another send holds the source's commit lock
		waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_, err := h.s.SendFoundryEvents(ports.WithActor(waitCtx, person), &foundryv1.SendFoundryEventsRequest{WorkspaceId: "ws-1", ConnectorId: h.source,
			Pieces: []*foundryv1.FoundryEventPiece{{Id: "o-1", DataJson: `{}`}}})
		<-src.commit
		h.s.deps.EventSends.leave(src, ws)
		if status.Code(err) != codes.DeadlineExceeded || len(h.bucket.batchKeys()) != 0 || len(h.store.sends) != 0 {
			t.Fatalf("send = %v; objects %v", err, h.bucket.batchKeys())
		}
	})
	// State nobody has held for ten minutes goes; a send after that counts
	// the day afresh from the batches.
	t.Run("drops idle state, and counts the day again after", func(t *testing.T) {
		h := newEventHarness(t, nil)
		if _, err := h.send(h.source, "", `o-1={}`); err != nil {
			t.Fatal(err)
		}
		h.clock.advance(9 * time.Minute)
		if _, err := h.send(h.addSource(t, "Refunds"), "", `r-1={}`); err != nil {
			t.Fatal(err)
		}
		e := h.s.deps.EventSends
		if len(e.sources) != 2 || h.store.sums != 1 {
			t.Fatalf("%d sources held and %d day reads at 9 minutes", len(e.sources), h.store.sums)
		}
		h.clock.advance(10 * time.Minute)
		if _, err := h.send(h.source, "", `o-2={}`); err != nil {
			t.Fatal(err)
		}
		if len(e.sources) != 1 || h.store.sums != 2 {
			t.Fatalf("%d sources held and %d day reads after 19 minutes", len(e.sources), h.store.sums)
		}
	})
	t.Run("is unavailable without the Foundry bucket", func(t *testing.T) {
		h := newEventHarness(t, nil)
		h.s.deps.EventSends = nil
		if _, err := h.send(h.source, "", `o-1={}`); status.Code(err) != codes.Unavailable {
			t.Fatalf("send = %v", err)
		}
	})
	// occurred_at reaches the object in UTC to the microsecond, and a time
	// outside MySQL's DATETIME years is refused with the piece named.
	t.Run("stores occurred_at in UTC, within DATETIME's years", func(t *testing.T) {
		h := newEventHarness(t, nil)
		at := time.Date(2026, 10, 23, 20, 20, 0, 123456789, time.FixedZone("+02", 2*60*60))
		resp, err := h.s.SendFoundryEvents(ports.WithActor(ctx, person), &foundryv1.SendFoundryEventsRequest{WorkspaceId: "ws-1", ConnectorId: h.source,
			Pieces: []*foundryv1.FoundryEventPiece{{Id: "o-1", OccurredAt: timestamppb.New(at), DataJson: `{}`}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.GetBatch().GetFirstAt().AsTime(); !got.Equal(time.Date(2026, 10, 23, 18, 20, 0, 123456000, time.UTC)) {
			t.Fatalf("first_at = %v", got)
		}
		_, err = h.s.SendFoundryEvents(ports.WithActor(ctx, person), &foundryv1.SendFoundryEventsRequest{WorkspaceId: "ws-1", ConnectorId: h.source,
			Pieces: []*foundryv1.FoundryEventPiece{{Id: "o-2", OccurredAt: timestamppb.New(time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)), DataJson: `{}`}}})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), `piece 1 (id "o-2")`) {
			t.Fatalf("a piece of the year 999 = %v", err)
		}
	})
}

// countedEventStore counts the reads a send makes after its actor's: of a
// retry's batch, of the workspace's day and of the source.
type countedEventStore struct {
	*eventStore
	reads atomic.Int64
}

func (s *countedEventStore) EventBatchByToken(ctx context.Context, workspaceID, connectorID, token string) (*foundrymodel.FoundryEventBatch, error) {
	s.reads.Add(1)
	return s.eventStore.EventBatchByToken(ctx, workspaceID, connectorID, token)
}

func (s *countedEventStore) SumFoundryEventBatches(ctx context.Context, workspaceID string, since time.Time) (foundrymodel.FoundryEventTotals, error) {
	s.reads.Add(1)
	return s.eventStore.SumFoundryEventBatches(ctx, workspaceID, since)
}

func (s *countedEventStore) EventSendState(ctx context.Context, workspaceID, connectorID string) (foundrymodel.FoundryEventSourceState, error) {
	s.reads.Add(1)
	return s.eventStore.EventSendState(ctx, workspaceID, connectorID)
}

// A send holds its pieces several times over while it is checked, encoded
// and stored, so at most max_sends_in_progress do that at once, deployment-wide:
// a send waits for a place before it checks, reads or writes anything past
// its actor, and gives the place up before its commit.
func TestFoundryEventSendsInProgress(t *testing.T) {
	ctx := context.Background()
	const token = "4d2c5f1e-9a7b-4c3d-8e6f-1a2b3c4d5e6f"
	twoPlaces := func(c *config.Config) { c.Foundry.Events.MaxSendsInProgress = 2 }
	// await takes n signals from c, failing after five seconds.
	await := func(t *testing.T, c <-chan string, n int, what string) {
		t.Helper()
		for i := range n {
			select {
			case <-c:
			case <-time.After(5 * time.Second):
				t.Fatalf("%d of %d sends %s", i, n, what)
			}
		}
	}

	t.Run("waits for a place before it checks, reads or writes anything", func(t *testing.T) {
		h := newEventHarness(t, twoPlaces)
		db := &countedEventStore{eventStore: h.store}
		h.s.deps.DB = db
		writing, release := make(chan string, 4), make(chan struct{})
		h.bucket.entering = func(op string) {
			writing <- op
			<-release
		}
		done := make(chan error, 3)
		for i := range 2 {
			go func() {
				_, err := h.send(h.source, "", fmt.Sprintf("p-%d={}", i))
				done <- err
			}()
		}
		await(t, writing, 2, "reached their writes")
		reads := db.reads.Load()
		// The third send's pieces break a rule, and the fourth would read its
		// retry, its day and its source, then write: neither checks, reads,
		// writes or answers while both places are held.
		invalid := make(chan error, 1)
		go func() {
			_, err := h.send(h.source, "", `p-3=[]`)
			invalid <- err
		}()
		go func() {
			_, err := h.send(h.source, token, `p-4={}`)
			done <- err
		}()
		select {
		case err := <-invalid:
			t.Fatalf("a send past the places was checked: %v", err)
		case err := <-done:
			t.Fatalf("a send past the places was answered: %v", err)
		case op := <-writing:
			t.Fatalf("a send past the places went on to its %s", op)
		case <-time.After(100 * time.Millisecond):
		}
		if got := db.reads.Load(); got != reads {
			t.Fatalf("sends past the places read %d times", got-reads)
		}
		close(release)
		if err := <-invalid; status.Code(err) != codes.InvalidArgument {
			t.Fatalf("the third send = %v, want its pieces refused once a place freed", err)
		}
		for range 3 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		if len(h.store.sends) != 3 || len(h.s.deps.EventSends.checking) != 0 {
			t.Fatalf("%d batches, %d places still held", len(h.store.sends), len(h.s.deps.EventSends.checking))
		}
	})

	// A caller that leaves while its send waits for a place is answered as
	// it left, and its send stored nothing, read nothing and wrote no object:
	// a retry waits as a new send does.
	for _, tc := range []struct {
		name  string
		retry bool
		leave func(context.Context) (context.Context, context.CancelFunc)
		want  codes.Code
	}{
		{"a send whose deadline passes", false, func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, 50*time.Millisecond)
		}, codes.DeadlineExceeded},
		{"a send whose caller cancels", false, func(ctx context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(ctx)
			time.AfterFunc(50*time.Millisecond, cancel)
			return ctx, cancel
		}, codes.Canceled},
		{"a retry whose deadline passes", true, func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, 50*time.Millisecond)
		}, codes.DeadlineExceeded},
	} {
		t.Run("gives up "+tc.name+" waiting for a place, storing nothing", func(t *testing.T) {
			h := newEventHarness(t, twoPlaces)
			if tc.retry {
				if _, err := h.send(h.source, token, `o-1={}`); err != nil {
					t.Fatal(err)
				}
			}
			e := h.s.deps.EventSends
			e.checking <- struct{}{} // two other sends hold both places
			e.checking <- struct{}{}
			db := &countedEventStore{eventStore: h.store}
			h.s.deps.DB = db
			puts, opens, objects, sends := h.bucket.puts, h.bucket.opens, len(h.bucket.batchKeys()), len(h.store.sends)
			waitCtx, cancel := tc.leave(ctx)
			defer cancel()
			_, err := h.s.SendFoundryEvents(ports.WithActor(waitCtx, person), &foundryv1.SendFoundryEventsRequest{WorkspaceId: "ws-1", ConnectorId: h.source,
				ClientToken: token, Pieces: []*foundryv1.FoundryEventPiece{{Id: "o-1", DataJson: `{}`}}})
			if status.Code(err) != tc.want || db.reads.Load() != 0 || h.bucket.puts != puts || h.bucket.opens != opens ||
				len(h.bucket.batchKeys()) != objects || len(h.store.sends) != sends {
				t.Fatalf("send = %v, want %v; %d reads, %d writes, %d object reads, %d batches", err, tc.want,
					db.reads.Load(), h.bucket.puts-puts, h.bucket.opens-opens, len(h.store.sends)-sends)
			}
			<-e.checking
			<-e.checking
			if resp, err := h.send(h.source, token, `o-1={}`); err != nil || resp.GetReplayed() != tc.retry || len(e.checking) != 0 {
				t.Fatalf("the send once the places freed = %+v, %v; %d places held", resp, err, len(e.checking))
			}
		})
	}

	// A retry reads its batch's object in its place, so retries in a loop
	// are bounded with every other send.
	t.Run("holds a place while it answers a retry", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) { c.Foundry.Events.MaxSendsInProgress = 1 })
		if _, err := h.send(h.source, token, `o-1={}`); err != nil {
			t.Fatal(err)
		}
		other := h.addSource(t, "Refunds")
		entered, release := make(chan string, 4), make(chan struct{})
		h.bucket.entering = func(op string) {
			entered <- op
			<-release
		}
		retried, sent := make(chan error, 1), make(chan error, 1)
		go func() {
			resp, err := h.send(h.source, token, `o-1={}`)
			if err == nil && !resp.GetReplayed() {
				err = errors.New("the retry stored a batch of its own")
			}
			retried <- err
		}()
		select {
		case op := <-entered:
			if op != "open" {
				t.Fatalf("the retry went on to its %s, want its batch's object read", op)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the retry never read its batch's object")
		}
		// Bounded, so a place the retry kept fails the test rather than hangs it.
		sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		go func() {
			_, err := h.s.SendFoundryEvents(ports.WithActor(sendCtx, person), &foundryv1.SendFoundryEventsRequest{WorkspaceId: "ws-1", ConnectorId: other,
				Pieces: []*foundryv1.FoundryEventPiece{{Id: "r-1", DataJson: `{}`}}})
			sent <- err
		}()
		select {
		case op := <-entered:
			t.Fatalf("a send went on to its %s while a retry held the only place", op)
		case err := <-sent:
			t.Fatalf("a send was answered while a retry held the only place: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		for _, done := range []chan error{retried, sent} {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		if len(h.store.sends) != 2 || len(h.s.deps.EventSends.checking) != 0 {
			t.Fatalf("%d batches, %d places still held", len(h.store.sends), len(h.s.deps.EventSends.checking))
		}
	})

	// Sends queued behind their source's commit lock hold no place, so a
	// busy source never stops another workspace's sends.
	t.Run("gives up its place before its commit", func(t *testing.T) {
		h := newEventHarness(t, twoPlaces)
		created, err := h.s.CreateConnector(ports.WithActor(ctx, person), &foundryv1.CreateConnectorRequest{
			WorkspaceId: "ws-2", Name: "Signups", Config: eventsConfig("Signups to the app"),
		})
		if err != nil {
			t.Fatal(err)
		}
		e := h.s.deps.EventSends
		src, ws := e.enter("ws-1", h.source)
		src.commit <- struct{}{} // another send of Orders holds its commit lock
		writing := make(chan string, 4)
		h.bucket.entering = func(op string) { writing <- op }
		done := make(chan error, 3)
		for i := range 3 {
			go func() {
				_, err := h.send(h.source, "", fmt.Sprintf("p-%d={}", i))
				done <- err
			}()
		}
		// Three sends wrote with two places: each gave its place up to wait for the lock.
		await(t, writing, 3, "wrote their objects")
		sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := h.s.SendFoundryEvents(ports.WithActor(sendCtx, person), &foundryv1.SendFoundryEventsRequest{WorkspaceId: "ws-2",
			ConnectorId: created.GetConnector().GetId(), Pieces: []*foundryv1.FoundryEventPiece{{Id: "s-1", DataJson: `{}`}}}); err != nil {
			t.Fatalf("a send of another workspace behind three sends at a held commit lock = %v", err)
		}
		<-src.commit
		e.leave(src, ws)
		for range 3 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		if len(h.store.sends) != 4 || len(e.checking) != 0 {
			t.Fatalf("%d batches, %d places still held", len(h.store.sends), len(e.checking))
		}
	})

	// Every way a send ends gives its place back: refused or failed at any
	// step, stored or replayed.
	for _, tc := range []struct {
		name string
		tune func(*config.Config)
		send func(h *eventHarness) error
		want codes.Code
	}{
		{"a revoked send token, before any place", nil, func(h *eventHarness) error {
			h.store.tokens = []*foundrymodel.FoundryEventSourceToken{{ID: "tok-revoked", WorkspaceID: "ws-1", ConnectorID: h.source, RevokedAt: &time.Time{}}}
			revoked := ports.Actor{Source: ports.SourceEventSource, EventSourceTokenID: "tok-revoked", WorkspaceID: "ws-1"}
			_, err := h.s.SendFoundryEvents(ports.WithActor(ctx, revoked), &foundryv1.SendFoundryEventsRequest{
				WorkspaceId: "ws-1", ConnectorId: h.source, Pieces: []*foundryv1.FoundryEventPiece{{Id: "o-1", DataJson: `{}`}}})
			return err
		}, codes.PermissionDenied},
		{"a malformed client token", nil, func(h *eventHarness) error {
			_, err := h.send(h.source, "short", `o-1={}`)
			return err
		}, codes.InvalidArgument},
		{"pieces the rules refuse", nil, func(h *eventHarness) error {
			_, err := h.send(h.source, "", `o-1=[]`)
			return err
		}, codes.InvalidArgument},
		{"a replayed retry", nil, func(h *eventHarness) error {
			if _, err := h.send(h.source, token, `o-1={}`); err != nil {
				return err
			}
			_, err := h.send(h.source, token, `o-1={}`)
			return err
		}, codes.OK},
		{"a client token reused for other pieces", nil, func(h *eventHarness) error {
			if _, err := h.send(h.source, token, `o-1={}`); err != nil {
				return err
			}
			_, err := h.send(h.source, token, `o-2={}`)
			return err
		}, codes.InvalidArgument},
		{"a retry past its bucket", func(c *config.Config) { c.Foundry.Events.SourceSendsBurst = 1 }, func(h *eventHarness) error {
			for range 2 {
				if _, err := h.send(h.source, token, `o-1={}`); err != nil {
					return err
				}
			}
			_, err := h.send(h.source, token, `o-1={}`)
			return err
		}, codes.ResourceExhausted},
		{"a retry whose object is gone", nil, func(h *eventHarness) error {
			if _, err := h.send(h.source, token, `o-1={}`); err != nil {
				return err
			}
			h.bucket.objects = map[string][]byte{}
			_, err := h.send(h.source, token, `o-1={}`)
			return err
		}, codes.Internal},
		{"a source's rate", func(c *config.Config) { c.Foundry.Events.SourceSendsBurst = 1 }, func(h *eventHarness) error {
			if _, err := h.send(h.source, "", `o-1={}`); err != nil {
				return err
			}
			_, err := h.send(h.source, "", `o-2={}`)
			return err
		}, codes.ResourceExhausted},
		{"a day it cannot count", nil, func(h *eventHarness) error {
			h.store.sumErr = errors.New("db is down")
			_, err := h.send(h.source, "", `o-1={}`)
			return err
		}, codes.Unavailable},
		{"another workspace's source", nil, func(h *eventHarness) error {
			_, err := h.s.SendFoundryEvents(ports.WithActor(ctx, person), &foundryv1.SendFoundryEventsRequest{WorkspaceId: "ws-2", ConnectorId: h.source,
				Pieces: []*foundryv1.FoundryEventPiece{{Id: "o-1", DataJson: `{}`}}})
			return err
		}, codes.NotFound},
		{"a paused source", nil, func(h *eventHarness) error {
			paused := "batch 4 of this event source is missing from storage"
			h.store.rows[h.source].SchedulePaused = &paused
			_, err := h.send(h.source, "", `o-1={}`)
			return err
		}, codes.FailedPrecondition},
		{"a field of another type", nil, func(h *eventHarness) error {
			h.store.rows[h.source].Config = []byte(`{"description":"","schema":{"amount":"number"}}`)
			_, err := h.send(h.source, "", `o-1={"amount":"12"}`)
			return err
		}, codes.InvalidArgument},
		{"a snapshot it cannot size", nil, func(h *eventHarness) error {
			h.synced(h.source, 3, 300)
			h.bucket.tailErr = errors.New("s3 is down")
			_, err := h.send(h.source, "", `o-1={}`)
			return err
		}, codes.Unavailable},
		{"a full source", func(c *config.Config) { c.Foundry.Sync.MaxRowsPerTable = 10 }, func(h *eventHarness) error {
			h.synced(h.source, 10, 100)
			_, err := h.send(h.source, "", `o-1={}`)
			return err
		}, codes.FailedPrecondition},
		{"a failed write", nil, func(h *eventHarness) error {
			h.bucket.putErr = errors.New("SlowDown: please reduce your request rate")
			_, err := h.send(h.source, token, `o-1={}`)
			return err
		}, codes.Unavailable},
		{"a commit that refuses it", nil, func(h *eventHarness) error {
			h.store.committing = func(foundrymodel.FoundryEventSend) {
				paused := "the sync stopped its worker 3 times; Helix paused it"
				h.store.rows[h.source].SchedulePaused = &paused
			}
			_, err := h.send(h.source, "", `o-1={}`)
			return err
		}, codes.FailedPrecondition},
		{"a commit of unknown outcome", nil, func(h *eventHarness) error {
			h.store.commitErr = errors.New("commit tx: invalid connection")
			_, err := h.send(h.source, token, `o-1={}`)
			return err
		}, codes.Unavailable},
		{"a retry that committed first", nil, func(h *eventHarness) error {
			h.store.committing = func(send foundrymodel.FoundryEventSend) {
				// Another try of the send commits first with its own object.
				h.store.committing = nil
				tok, won := token, events.BatchKey("ws-1", h.source, "batch-won")
				h.bucket.objects[won] = h.bucket.objects[send.Key]
				h.store.sends = append(h.store.sends, &foundrymodel.FoundryEventBatch{ID: "batch-won", WorkspaceID: "ws-1", ConnectorID: h.source, Seq: 1,
					ClientToken: &tok, S3Key: won, Pieces: send.Send.Pieces(), Bytes: send.Send.Bytes(), CreatedAt: send.Object.ReceivedAt})
			}
			resp, err := h.send(h.source, token, `o-1={}`)
			if err == nil && !resp.GetReplayed() {
				return errors.New("the send stored a batch of its own")
			}
			return err
		}, codes.OK},
		{"a caller that leaves at its source's commit lock", nil, func(h *eventHarness) error {
			src, _ := h.s.deps.EventSends.enter("ws-1", h.source)
			src.commit <- struct{}{} // another send of Orders holds its commit lock to the end
			waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
			_, err := h.s.SendFoundryEvents(ports.WithActor(waitCtx, person), &foundryv1.SendFoundryEventsRequest{WorkspaceId: "ws-1", ConnectorId: h.source,
				Pieces: []*foundryv1.FoundryEventPiece{{Id: "o-1", DataJson: `{}`}}})
			return err
		}, codes.DeadlineExceeded},
		{"a stored send", nil, func(h *eventHarness) error {
			_, err := h.send(h.source, token, `o-1={}`)
			return err
		}, codes.OK},
	} {
		t.Run("gives its place back after "+tc.name, func(t *testing.T) {
			h := newEventHarness(t, tc.tune)
			if err := tc.send(h); status.Code(err) != tc.want {
				t.Fatalf("send = %v, want %v", err, tc.want)
			}
			if held := len(h.s.deps.EventSends.checking); held != 0 {
				t.Fatalf("%d places still held", held)
			}
		})
	}
}

func TestListFoundryEventBatches(t *testing.T) {
	ctx := context.Background()
	// The list shows which batches a sync merged, what waits, and how full
	// the source will be once the waiting batches are merged.
	t.Run("lists the newest batches with what waits and how full the source is", func(t *testing.T) {
		h := newEventHarness(t, nil)
		for i := range 3 {
			if _, err := h.send(h.source, "", fmt.Sprintf(`o-%d={}`, i), fmt.Sprintf(`p-%d={}`, i)); err != nil {
				t.Fatal(err)
			}
		}
		h.store.rows[h.source].ChangeCursor = []byte(`{"seq":1}`)
		h.synced(h.source, 2, 500)
		resp, err := h.s.ListFoundryEventBatches(ctx, &foundryv1.ListFoundryEventBatchesRequest{WorkspaceId: "ws-1", ConnectorId: h.source})
		if err != nil {
			t.Fatal(err)
		}
		var seqs []int64
		var synced []bool
		for _, b := range resp.GetBatches() {
			seqs, synced = append(seqs, b.GetSeq()), append(synced, b.GetSynced())
		}
		if !slices.Equal(seqs, []int64{3, 2, 1}) || !slices.Equal(synced, []bool{false, false, true}) || resp.GetPendingPieces() != 4 {
			t.Fatalf("batches %v synced %v, pending %d", seqs, synced, resp.GetPendingPieces())
		}
		if c := resp.GetCapacity(); c.GetRows() != 6 || c.GetBytes() < 500 || c.GetMaxRows() != 2_000_000 {
			t.Fatalf("capacity = %+v", c)
		}
	})
	t.Run("lists 20 by default and at most 100", func(t *testing.T) {
		h := newEventHarness(t, nil)
		for _, limit := range []int32{0, -1, 7, 500} {
			if _, err := h.s.ListFoundryEventBatches(ctx, &foundryv1.ListFoundryEventBatchesRequest{WorkspaceId: "ws-1", ConnectorId: h.source,
				Limit: limit}); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(h.store.listLimits, []int{20, 20, 7, 100}) {
			t.Fatalf("listed with limits %v", h.store.listLimits)
		}
	})
	// The registry checked only the workspace, so the source must be found in it.
	t.Run("does not find another workspace's source", func(t *testing.T) {
		h := newEventHarness(t, nil)
		_, err := h.s.ListFoundryEventBatches(ctx, &foundryv1.ListFoundryEventBatchesRequest{WorkspaceId: "ws-2", ConnectorId: h.source})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("list = %v", err)
		}
	})
	t.Run("refuses a connector of another kind", func(t *testing.T) {
		h := newEventHarness(t, nil)
		h.store.rows["conn-stripe"] = &foundrymodel.Connector{ID: "conn-stripe", WorkspaceID: "ws-1", Kind: "stripe", Config: []byte(`{}`)}
		_, err := h.s.ListFoundryEventBatches(ctx, &foundryv1.ListFoundryEventBatchesRequest{WorkspaceId: "ws-1", ConnectorId: "conn-stripe"})
		if foundryReason(err) != "FOUNDRY_NOT_EVENT_SOURCE" {
			t.Fatalf("list = %v", err)
		}
	})
}

func TestFoundryEventSourceTokens(t *testing.T) {
	ctx := context.Background()
	create := func(h *eventHarness, actor ports.Actor, workspaceID, connectorID string) (*foundryv1.CreateFoundryEventSourceTokenResponse, error) {
		return h.s.CreateFoundryEventSourceToken(ports.WithActor(ctx, actor), &foundryv1.CreateFoundryEventSourceTokenRequest{WorkspaceId: workspaceID,
			ConnectorId: connectorID})
	}
	// The secret is shown this once: only its SHA-256 is stored, with its
	// first 12 characters to tell tokens apart.
	t.Run("makes a token shown once and stored as its hash", func(t *testing.T) {
		h := newEventHarness(t, nil)
		resp, err := create(h, person, "ws-1", h.source)
		if err != nil {
			t.Fatal(err)
		}
		secret := resp.GetSecret()
		sum := sha256.Sum256([]byte(secret))
		stored := h.store.tokens[0]
		if keytoken.ValidateEventSource(secret) != nil || stored.Hash != hex.EncodeToString(sum[:]) || stored.Prefix != secret[:12] ||
			stored.CreatedBy != "user-1" || strings.Contains(fmt.Sprintf("%+v", *stored), secret) {
			t.Fatalf("secret %q stored as %+v", secret, stored)
		}
		if tok := resp.GetToken(); tok.GetId() != stored.ID || tok.GetPrefix() != secret[:12] || tok.GetConnectorId() != h.source ||
			tok.GetCreatedByUserId() != "user-1" || tok.GetRevokedAt() != nil {
			t.Fatalf("token = %+v", tok)
		}
	})
	t.Run("needs a person to make one", func(t *testing.T) {
		h := newEventHarness(t, nil)
		// A service credential reaches Foundry through the MCP with no user.
		if _, err := create(h, ports.Actor{Source: ports.SourceMCP}, "ws-1", h.source); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("create = %v", err)
		}
	})
	t.Run("caps a source's live tokens", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) { c.Foundry.Events.MaxTokensPerSource = 2 })
		var first string
		for i := range 2 {
			resp, err := create(h, person, "ws-1", h.source)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				first = resp.GetToken().GetId()
			}
		}
		if _, err := create(h, person, "ws-1", h.source); status.Code(err) != codes.ResourceExhausted ||
			!strings.Contains(status.Convert(err).Message(), "revoke one first") {
			t.Fatalf("a third token = %v", err)
		}
		if _, err := h.s.RevokeFoundryEventSourceToken(ports.WithActor(ctx, person), &foundryv1.RevokeFoundryEventSourceTokenRequest{WorkspaceId: "ws-1",
			ConnectorId: h.source, Id: first}); err != nil {
			t.Fatal(err)
		}
		if _, err := create(h, person, "ws-1", h.source); err != nil {
			t.Fatalf("a token after a revoke = %v", err)
		}
	})
	// Revoking is final and idempotent: the token keeps its first revoked_at,
	// stays listed, and no longer authenticates.
	t.Run("revokes a token, again without error, and lists it", func(t *testing.T) {
		h := newEventHarness(t, nil)
		older, _ := create(h, person, "ws-1", h.source)
		newer, _ := create(h, person, "ws-1", h.source)
		revoke := &foundryv1.RevokeFoundryEventSourceTokenRequest{WorkspaceId: "ws-1", ConnectorId: h.source, Id: older.GetToken().GetId()}
		first, err := h.s.RevokeFoundryEventSourceToken(ports.WithActor(ctx, person), revoke)
		if err != nil || first.GetToken().GetRevokedAt() == nil {
			t.Fatalf("revoke = %+v, %v", first, err)
		}
		again, err := h.s.RevokeFoundryEventSourceToken(ports.WithActor(ctx, person), revoke)
		if err != nil || !again.GetToken().GetRevokedAt().AsTime().Equal(first.GetToken().GetRevokedAt().AsTime()) {
			t.Fatalf("revoke again = %+v, %v", again, err)
		}
		list, err := h.s.ListFoundryEventSourceTokens(ctx, &foundryv1.ListFoundryEventSourceTokensRequest{WorkspaceId: "ws-1", ConnectorId: h.source})
		if err != nil {
			t.Fatal(err)
		}
		if got := list.GetTokens(); len(got) != 2 || got[0].GetId() != newer.GetToken().GetId() || got[1].GetRevokedAt() == nil {
			t.Fatalf("tokens = %v", got)
		}
		valid, err := h.s.ValidateFoundryEventSourceToken(ctx, &foundryv1.ValidateFoundryEventSourceTokenRequest{Hash: hashToken(older.GetSecret())})
		if err != nil || valid.GetValid() {
			t.Fatalf("validate a revoked token = %+v, %v", valid, err)
		}
	})
	// The registry checked only the workspace in the request, so the source,
	// and for a revoke the token, must be found in it.
	t.Run("does not find another workspace's source or another source's token", func(t *testing.T) {
		h := newEventHarness(t, nil)
		made, err := create(h, person, "ws-1", h.source)
		if err != nil {
			t.Fatal(err)
		}
		other := h.addSource(t, "Refunds")
		if _, err := create(h, person, "ws-2", h.source); status.Code(err) != codes.NotFound {
			t.Fatalf("create in ws-2 = %v", err)
		}
		if _, err := h.s.ListFoundryEventSourceTokens(ctx, &foundryv1.ListFoundryEventSourceTokensRequest{WorkspaceId: "ws-2",
			ConnectorId: h.source}); status.Code(err) != codes.NotFound {
			t.Fatalf("list in ws-2 = %v", err)
		}
		for name, req := range map[string]*foundryv1.RevokeFoundryEventSourceTokenRequest{
			"another workspace": {WorkspaceId: "ws-2", ConnectorId: h.source, Id: made.GetToken().GetId()},
			"another source":    {WorkspaceId: "ws-1", ConnectorId: other, Id: made.GetToken().GetId()},
		} {
			if _, err := h.s.RevokeFoundryEventSourceToken(ports.WithActor(ctx, person), req); status.Code(err) != codes.NotFound {
				t.Fatalf("revoke through %s = %v", name, err)
			}
		}
		if h.store.tokens[0].RevokedAt != nil {
			t.Fatal("the token was revoked")
		}
	})
	t.Run("refuses a connector of another kind", func(t *testing.T) {
		h := newEventHarness(t, nil)
		h.store.rows["conn-stripe"] = &foundrymodel.Connector{ID: "conn-stripe", WorkspaceID: "ws-1", Kind: "stripe", Config: []byte(`{}`)}
		if _, err := create(h, person, "ws-1", "conn-stripe"); foundryReason(err) != "FOUNDRY_NOT_EVENT_SOURCE" {
			t.Fatalf("create = %v", err)
		}
	})
	// A front end validates a bearer by its hash; anything but a live token
	// is simply not valid, and a malformed hash is the front end's mistake.
	t.Run("validates a live token by its hash", func(t *testing.T) {
		h := newEventHarness(t, nil)
		made, err := create(h, person, "ws-1", h.source)
		if err != nil {
			t.Fatal(err)
		}
		valid, err := h.s.ValidateFoundryEventSourceToken(ctx, &foundryv1.ValidateFoundryEventSourceTokenRequest{Hash: hashToken(made.GetSecret())})
		if err != nil || !valid.GetValid() || valid.GetTokenId() != made.GetToken().GetId() || valid.GetWorkspaceId() != "ws-1" ||
			valid.GetConnectorId() != h.source {
			t.Fatalf("validate = %+v, %v", valid, err)
		}
		unknown, err := h.s.ValidateFoundryEventSourceToken(ctx, &foundryv1.ValidateFoundryEventSourceTokenRequest{Hash: strings.Repeat("0", 64)})
		if err != nil || unknown.GetValid() {
			t.Fatalf("validate an unknown hash = %+v, %v", unknown, err)
		}
		for _, hash := range []string{"", strings.Repeat("A", 64), strings.Repeat("0", 63)} {
			if _, err := h.s.ValidateFoundryEventSourceToken(ctx, &foundryv1.ValidateFoundryEventSourceTokenRequest{Hash: hash}); status.Code(err) !=
				codes.InvalidArgument {
				t.Fatalf("validate %q = %v", hash, err)
			}
		}
	})
}

func TestEventSourceConnectorRPCs(t *testing.T) {
	ctx := context.Background()
	// An event source has no credential and nothing to dial. It syncs when
	// pieces arrive, so its creation schedules nothing.
	t.Run("creates a source with nothing scheduled and no credential", func(t *testing.T) {
		h := newEventHarness(t, nil)
		resp, err := h.s.CreateConnector(ports.WithActor(ctx, person), &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Signups",
			Config: eventsConfig("  A person who signed up  ")})
		if err != nil {
			t.Fatal(err)
		}
		c := resp.GetConnector()
		row := h.store.rows[c.GetId()]
		secret, err := h.s.deps.ConnectorKeys.Open(row.SecretCiphertext, row.SecretKeyID, connector.SecretContext(row.ID))
		if c.GetKind() != "events" || c.GetSecretHint() != "" || c.GetConfig().GetEvents().GetDescription() != "A person who signed up" ||
			row.NextSyncAt != nil || err != nil || len(secret) != 0 {
			t.Fatalf("connector = %+v, row %+v, secret %q (%v)", c, row, secret, err)
		}
		if tables := resp.GetDiscovery().GetTables(); len(tables) != 1 || tables[0].GetName() != "events" || len(tables[0].GetColumns()) != 3 {
			t.Fatalf("discovery = %+v", resp.GetDiscovery())
		}
	})
	// A name tells sources apart for people and agents alike, so a taken
	// one is refused, naming the source that has it: an agent's retry finds
	// that source rather than making a second.
	t.Run("refuses a name another live source has, naming it", func(t *testing.T) {
		h := newEventHarness(t, nil)
		_, err := h.s.CreateConnector(ports.WithActor(ctx, person), &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Orders", Config: eventsConfig("")})
		if status.Code(err) != codes.AlreadyExists || !strings.Contains(status.Convert(err).Message(), h.source) {
			t.Fatalf("create = %v", err)
		}
	})
	t.Run("caps the sources a workspace has", func(t *testing.T) {
		h := newEventHarness(t, func(c *config.Config) { c.Foundry.Events.MaxSourcesPerWorkspace = 1 })
		_, err := h.s.CreateConnector(ports.WithActor(ctx, person), &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Refunds", Config: eventsConfig("")})
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("create = %v", err)
		}
	})
	t.Run("refuses a description past 1,000 characters", func(t *testing.T) {
		h := newEventHarness(t, nil)
		_, err := h.s.CreateConnector(ports.WithActor(ctx, person), &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Refunds",
			Config: eventsConfig(strings.Repeat("é", 1001))})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("create = %v", err)
		}
	})
	// The schema, cursor and next sync are the sends' and syncs', written
	// under the source's lock: an edit that rewrote them would drop a field a
	// concurrent send learned, or the sync it scheduled.
	t.Run("edits the description and leaves what sends and syncs wrote", func(t *testing.T) {
		h := newEventHarness(t, nil)
		row := h.store.rows[h.source]
		next := time.Date(2026, 10, 24, 12, 1, 0, 0, time.UTC)
		row.Config, row.ChangeCursor, row.NextSyncAt = []byte(`{"description":"Orders","schema":{"amount":"number"}}`), []byte(`{"seq":4}`), &next
		sealed := slices.Clone(row.SecretCiphertext)
		resp, err := h.s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: h.source, Config: eventsConfig("Orders and refunds")})
		if err != nil {
			t.Fatal(err)
		}
		var stored map[string]any
		_ = json.Unmarshal(row.Config, &stored)
		if stored["description"] != "Orders and refunds" || fmt.Sprint(stored["schema"]) != "map[amount:number]" || string(row.ChangeCursor) != `{"seq":4}` ||
			!row.NextSyncAt.Equal(next) || !bytes.Equal(row.SecretCiphertext, sealed) {
			t.Fatalf("row = %s, cursor %s, next %v", row.Config, row.ChangeCursor, row.NextSyncAt)
		}
		if f := resp.GetConnector().GetConfig().GetEvents().GetFields(); len(f) != 1 || f[0].GetName() != "amount" || f[0].GetType() != "number" {
			t.Fatalf("fields = %v", f)
		}
	})
	t.Run("renames a source, refusing a name another has", func(t *testing.T) {
		h := newEventHarness(t, nil)
		other := h.addSource(t, "Refunds")
		taken := "Orders"
		_, err := h.s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: other, Name: &taken})
		if status.Code(err) != codes.AlreadyExists || !strings.Contains(status.Convert(err).Message(), h.source) {
			t.Fatalf("rename to a taken name = %v", err)
		}
		free := "Returns"
		if resp, err := h.s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: other, Name: &free}); err != nil ||
			resp.GetConnector().GetName() != "Returns" {
			t.Fatalf("rename = %+v, %v", resp, err)
		}
	})
	t.Run("refuses a sync interval, and an update of nothing", func(t *testing.T) {
		h := newEventHarness(t, nil)
		every := int64(3600)
		if _, err := h.s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: h.source, SyncEverySeconds: &every}); status.Code(err) !=
			codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "syncs when pieces arrive") {
			t.Fatalf("an interval = %v", err)
		}
		if _, err := h.s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: h.source}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("an update of nothing = %v", err)
		}
		if _, err := h.s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: h.source,
			Config: stripeConfig("sk_test_abc")}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("another kind's config = %v", err)
		}
	})
	// Helix holds an event source's rows, so its table is followed on every
	// plan, Free's [full] included, and it has no interval.
	t.Run("shows its table followed on every plan, and no interval", func(t *testing.T) {
		h := newEventHarness(t, nil)
		h.synced(h.source, 3, 100)
		resp, err := h.s.GetConnector(ctx, &foundryv1.GetConnectorRequest{WorkspaceId: "ws-1", Id: h.source})
		if err != nil {
			t.Fatal(err)
		}
		c := resp.GetConnector()
		if len(c.GetTables()) != 1 || c.GetTables()[0].GetCapture() != "events" || c.GetSchedule().GetEverySeconds() != 0 ||
			c.GetSchedule().GetEffectiveEverySeconds() != 0 {
			t.Fatalf("connector = %+v", c)
		}
	})
	// A sync of an empty source would publish an empty table; once it has
	// pieces, a sync asked of it (a resume) is of the events class.
	t.Run("refuses a sync while it holds no batch, and queues one of the events class", func(t *testing.T) {
		h := newEventHarness(t, nil)
		_, err := h.s.SyncConnector(ctx, &foundryv1.SyncConnectorRequest{WorkspaceId: "ws-1", Id: h.source})
		if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "an API source syncs when pieces arrive; send some first" {
			t.Fatalf("sync = %v", err)
		}
		if _, err := h.send(h.source, "", `o-1={}`); err != nil {
			t.Fatal(err)
		}
		resp, err := h.s.SyncConnector(ctx, &foundryv1.SyncConnectorRequest{WorkspaceId: "ws-1", Id: h.source})
		if err != nil || resp.GetSync().GetOrigin() != "events" {
			t.Fatalf("sync = %+v, %v", resp, err)
		}
	})
	// An event source is deleted as every connector is, through the generic
	// delete, which retires its table, owes the refreshes of the ontologies
	// reading it and revokes its tokens in one transaction: no token
	// outlives its source.
	t.Run("is deleted through the generic delete, which revokes its tokens", func(t *testing.T) {
		h := newEventHarness(t, nil)
		made, err := h.s.CreateFoundryEventSourceToken(ports.WithActor(ctx, person), &foundryv1.CreateFoundryEventSourceTokenRequest{WorkspaceId: "ws-1",
			ConnectorId: h.source})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.s.DeleteConnector(ctx, &foundryv1.DeleteConnectorRequest{WorkspaceId: "ws-1", Id: h.source}); err != nil {
			t.Fatal(err)
		}
		valid, err := h.s.ValidateFoundryEventSourceToken(ctx, &foundryv1.ValidateFoundryEventSourceTokenRequest{Hash: hashToken(made.GetSecret())})
		if _, ok := h.store.rows[h.source]; ok || err != nil || valid.GetValid() || h.store.tokens[0].RevokedAt == nil {
			t.Fatalf("after delete: row kept %v, validate %+v (%v)", ok, valid, err)
		}
		if _, err := h.send(h.source, "", `o-1={}`); status.Code(err) != codes.NotFound {
			t.Fatalf("a send to the deleted source = %v", err)
		}
	})
}

// eventsText is an event source's config carrying exactly the fields given:
// an absent one is left as it is on update.
func eventsText(description, textField *string) *foundryv1.ConnectorConfig {
	return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Events{Events: &foundryv1.EventsConnectorConfig{
		Description: description, TextField: textField,
	}}}
}

// A source's text field is the body of each piece's document in its own set.
// It is locked as a string when chosen, at create or later, refused as a send
// would be, and a change re-renders the newest pieces by one full sync.
func TestEventSourceTextField(t *testing.T) {
	ctx := context.Background()

	t.Run("creates a source with its text field locked as a string, syncing nothing", func(t *testing.T) {
		h := newEventHarness(t, nil)
		resp, err := h.s.CreateConnector(ports.WithActor(ctx, person), &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Replies",
			Config: eventsText(proto.String("A reply to a ticket"), proto.String("reply"))})
		if err != nil {
			t.Fatal(err)
		}
		row := h.store.rows[resp.GetConnector().GetId()]
		if string(row.Config) != `{"description":"A reply to a ticket","text_field":"reply","schema":{"reply":"string"}}` || row.NextSyncAt != nil ||
			len(h.store.syncs) != 0 {
			t.Fatalf("row = %s, next %v, syncs %d", row.Config, row.NextSyncAt, len(h.store.syncs))
		}
		got := resp.GetConnector().GetConfig().GetEvents()
		if got.GetTextField() != "reply" || len(got.GetFields()) != 1 || got.GetFields()[0].GetName() != "reply" || got.GetFields()[0].GetType() != "string" {
			t.Fatalf("config = %v", got)
		}
	})
	t.Run("creates a source with no text field when it carries none or an empty one", func(t *testing.T) {
		h := newEventHarness(t, nil)
		for name, field := range map[string]*string{"Plain": nil, "Cleared": proto.String("")} {
			resp, err := h.s.CreateConnector(ports.WithActor(ctx, person), &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: name,
				Config: eventsText(nil, field)})
			if err != nil {
				t.Fatal(err)
			}
			got := resp.GetConnector().GetConfig().GetEvents()
			if got.GetTextField() != "" || len(got.GetFields()) != 0 || strings.Contains(string(h.store.rows[resp.GetConnector().GetId()].Config), "schema") {
				t.Fatalf("%s: config = %v", name, got)
			}
		}
	})
	for _, tc := range []struct {
		name, field string
		tune        func(*config.Config)
		code        codes.Code
		reason      string
	}{
		{"refuses a reserved name at create", "id", nil, codes.InvalidArgument, ""},
		{"refuses a field past the field cap at create", "reply", func(c *config.Config) { c.Foundry.Events.MaxFields = 0 }, codes.FailedPrecondition,
			"FOUNDRY_EVENT_SOURCE_FIELDS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newEventHarness(t, tc.tune)
			_, err := h.s.CreateConnector(ports.WithActor(ctx, person), &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Replies",
				Config: eventsText(nil, &tc.field)})
			if status.Code(err) != tc.code || foundryReason(err) != tc.reason || len(h.store.rows) != 1 {
				t.Fatalf("create = %v (reason %q), %d sources", err, foundryReason(err), len(h.store.rows))
			}
		})
	}

	// Orders holds a learned schema, a cursor and a scheduled sync, which only
	// its sends and syncs write, and maybe a text field of its own.
	edited := func(t *testing.T, tune func(*config.Config), stored string) *eventHarness {
		t.Helper()
		h := newEventHarness(t, tune)
		row := h.store.rows[h.source]
		next := time.Date(2026, 10, 24, 12, 1, 0, 0, time.UTC)
		row.Config, row.ChangeCursor, row.NextSyncAt = []byte(stored), []byte(`{"seq":4}`), &next
		return h
	}
	const plain = `{"description":"Orders","schema":{"amount":"number","note":"null"}}`
	const withText = `{"description":"Orders","schema":{"amount":"number","reply":"string"},"text_field":"reply"}`
	for _, tc := range []struct {
		name, stored       string
		description, field *string
		storedAfter        string
		// queued is whether one full sync of origin events is queued for
		// the person; handed whether the store was handed the edit.
		queued, handed bool
	}{
		// Review finding 1: a description edit must never reach the text
		// field, the set or the sync queue.
		{"edits the description alone", withText, proto.String("Orders and refunds"), nil,
			`{"description":"Orders and refunds","schema":{"amount":"number","reply":"string"},"text_field":"reply"}`, false, true},
		{"locks a new text field and queues one full sync, keeping the description", plain, nil, proto.String("reply"),
			`{"description":"Orders","schema":{"amount":"number","note":"null","reply":"string"},"text_field":"reply"}`, true, true},
		{"locks a field only ever null", plain, nil, proto.String("note"),
			`{"description":"Orders","schema":{"amount":"number","note":"string"},"text_field":"note"}`, true, true},
		{"changes the description and the text field together", withText, proto.String("Replies"), proto.String("note"),
			`{"description":"Replies","schema":{"amount":"number","note":"string","reply":"string"},"text_field":"note"}`, true, true},
		{"clears the text field and queues the sync that empties the set", withText, nil, proto.String(""),
			`{"description":"Orders","schema":{"amount":"number","reply":"string"},"text_field":""}`, true, true},
		{"writes nothing for the text field it already has", withText, nil, proto.String("reply"), withText, false, false},
		{"writes nothing for clearing a field it never had", plain, nil, proto.String(""), plain, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := edited(t, nil, tc.stored)
			row := h.store.rows[h.source]
			next, sealed := *row.NextSyncAt, slices.Clone(row.SecretCiphertext)
			resp, err := h.s.UpdateConnector(ports.WithActor(ctx, person), &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: h.source,
				Config: eventsText(tc.description, tc.field)})
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			_ = json.Unmarshal(row.Config, &got)
			_ = json.Unmarshal([]byte(tc.storedAfter), &want)
			if fmt.Sprint(got) != fmt.Sprint(want) || string(row.ChangeCursor) != `{"seq":4}` || !row.NextSyncAt.Equal(next) ||
				!bytes.Equal(row.SecretCiphertext, sealed) {
				t.Fatalf("row = %s, cursor %s, next %v; want %s", row.Config, row.ChangeCursor, row.NextSyncAt, tc.storedAfter)
			}
			if handed := len(h.store.updates) == 1; handed != tc.handed {
				t.Fatalf("handed the store %d edits, want one: %v", len(h.store.updates), tc.handed)
			}
			if tc.handed && (h.store.updates[0].Text != nil) != tc.queued {
				t.Fatalf("handed %+v", h.store.updates[0])
			}
			switch {
			case !tc.queued && len(h.store.syncs) != 0:
				t.Fatalf("queued %d syncs, want none", len(h.store.syncs))
			case tc.queued:
				text := h.store.updates[0].Text
				if len(h.store.syncs) != 1 || h.store.syncs[0].Origin != foundrymodel.ConnectorSyncEvents || h.store.syncs[0].Mode != foundrymodel.ConnectorSyncFull ||
					*h.store.syncs[0].RequestedByUserID != "user-1" || text.MaxFields != h.s.deps.Cfg.Foundry.Events.MaxFields {
					t.Fatalf("syncs = %+v, handed %+v", h.store.syncs, text)
				}
			}
			var stored struct {
				TextField string `json:"text_field"`
			}
			_ = json.Unmarshal(row.Config, &stored)
			if resp.GetConnector().GetConfig().GetEvents().GetTextField() != stored.TextField {
				t.Fatalf("answered text field %q, stored %q", resp.GetConnector().GetConfig().GetEvents().GetTextField(), stored.TextField)
			}
		})
	}
	// A text field must hold strings, within the field cap, as a send's
	// would; a sync already live read the old field. Each is refused with
	// nothing written, the description of the same edit included.
	for _, tc := range []struct {
		name   string
		field  string
		tune   func(*config.Config)
		live   bool
		code   codes.Code
		reason string
		inMsg  string
	}{
		{"refuses a field held as another type", "amount", nil, false, codes.InvalidArgument, "", `this source holds "amount" as a number`},
		{"refuses a field past the field cap", "reply", func(c *config.Config) { c.Foundry.Events.MaxFields = 2 }, false, codes.FailedPrecondition,
			"FOUNDRY_EVENT_SOURCE_FIELDS", "a source may have 2"},
		{"refuses a reserved name", "received_at", nil, false, codes.InvalidArgument, "", "received_at is a column of every piece"},
		{"refuses a change while a sync is live", "reply", nil, true, codes.FailedPrecondition, "",
			"a sync of this API source is running; change its text field in a minute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := edited(t, tc.tune, plain)
			if tc.live {
				h.store.syncs = append(h.store.syncs, &foundrymodel.ConnectorSync{ID: "sync-live", ConnectorID: h.source, WorkspaceID: "ws-1",
					Status: foundrymodel.ConnectorSyncRunning, Origin: foundrymodel.ConnectorSyncEvents})
			}
			syncs := len(h.store.syncs)
			_, err := h.s.UpdateConnector(ports.WithActor(ctx, person), &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: h.source,
				Config: eventsText(proto.String("never stored"), &tc.field)})
			if status.Code(err) != tc.code || foundryReason(err) != tc.reason || !strings.Contains(status.Convert(err).Message(), tc.inMsg) {
				t.Fatalf("update = %v (reason %q), want %v %q with %q", err, foundryReason(err), tc.code, tc.reason, tc.inMsg)
			}
			if row := h.store.rows[h.source]; string(row.Config) != plain || len(h.store.syncs) != syncs {
				t.Fatalf("row = %s, %d syncs; want it as it was", row.Config, len(h.store.syncs))
			}
		})
	}
	t.Run("refuses an edit that carries nothing", func(t *testing.T) {
		h := edited(t, nil, plain)
		_, err := h.s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: h.source, Config: eventsText(nil, nil)})
		if status.Code(err) != codes.InvalidArgument || len(h.store.updates) != 0 {
			t.Fatalf("update = %v, handed %d edits", err, len(h.store.updates))
		}
	})
}
