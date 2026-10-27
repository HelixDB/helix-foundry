package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// postgresCDCStatus is the cursor's "cdc" object, which the connector API shows.
type postgresCDCStatus struct {
	Position      string     `json:"position"`
	RetainedBytes int64      `json:"retained_bytes"`
	LastResetAt   *time.Time `json:"last_reset_at,omitempty"`
	LastReset     string     `json:"last_reset,omitempty"`
}

func (c *postgresCDCStatus) reset(reason string) {
	now := time.Now().UTC()
	c.LastResetAt, c.LastReset = &now, reason
}

var postgresSlotRE = regexp.MustCompile(`[^a-z0-9_]`)

// postgresSlot is the connector's replication slot. Replication commands take
// the name unquoted, so it is reduced to the characters a slot name allows.
func postgresSlot(connectorID string) string {
	name := "helix_foundry_" + postgresSlotRE.ReplaceAllString(strings.ToLower(connectorID), "_")
	return name[:min(len(name), 63)]
}

// replicate opens a logical replication connection, dialed through the guard like every other.
func (k *postgresKind) replicate(ctx context.Context, s DatabaseSettings, password string) (*pgconn.PgConn, error) {
	cfg, err := k.config(s, password)
	if err != nil {
		return nil, err
	}
	rc := cfg.Config.Copy()
	rc.RuntimeParams["replication"] = "database"
	return pgconn.ConnectConfig(ctx, rc)
}

func closeReplication(repl *pgconn.PgConn) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = repl.Close(ctx) // best-effort: the server ends the walsender with the connection
}

// dropSlot drops slot when it exists. Only the drop needs REPLICATION, so a
// user that never had a slot is not refused.
func dropSlot(ctx context.Context, conn *pgx.Conn, slot string) error {
	_, err := conn.Exec(ctx, "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = $1", slot)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42704" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("drop replication slot %s: %w", slot, err)
	}
	return nil
}

// startLog sets up the slot the logged tables' changes are read from.
// A healthy slot is kept: the full read then sees a later state than its
// confirmed position, and replaying that gap re-fetches rows by key, which
// changes nothing. Otherwise a new slot exports the snapshot the full read imports.
func (k *postgresKind) startLog(ctx context.Context, conn *pgx.Conn, src Source, s postgresSettings, tables []Table, cur *postgresCursor) (Start, error) {
	if src.ConnectorID == "" {
		return Start{}, errors.New("a replication slot needs the connector's id")
	}
	cur.Slot, cur.CDC = postgresSlot(src.ConnectorID), &postgresCDCStatus{}
	var walStatus, confirmed *string
	var ours bool
	err := conn.QueryRow(ctx, `SELECT wal_status, confirmed_flush_lsn::text, plugin = 'pgoutput' AND database = current_database()
FROM pg_replication_slots WHERE slot_name = $1`, cur.Slot).Scan(&walStatus, &confirmed, &ours)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Start{}, fmt.Errorf("read replication slot %s: %w", cur.Slot, err)
	case ours && confirmed != nil && (walStatus == nil || *walStatus != "lost"):
		cur.LSN = *confirmed
	default:
		if walStatus != nil && *walStatus == "lost" {
			cur.CDC.reset("lost")
			cdcResets.WithLabelValues("postgres", "lost").Inc()
		}
		if err := dropSlot(ctx, conn, cur.Slot); err != nil {
			return Start{}, err
		}
	}
	// Read before the slot exists, a definition changed in between only costs an extra full read.
	if cur.Relations, err = relationSignatures(ctx, conn, s.Schema, tables); err != nil {
		return Start{}, err
	}
	if cur.LSN != "" {
		cur.CDC.Position = cur.Slot + " at " + cur.LSN
		return Start{}, nil
	}
	repl, err := k.replicate(ctx, s.DatabaseSettings, src.Secret)
	if err != nil {
		return Start{}, refused(err, "could not open a replication connection; check the user may replicate, then test again")
	}
	created, err := pglogrepl.CreateReplicationSlot(ctx, repl, cur.Slot, "pgoutput",
		pglogrepl.CreateReplicationSlotOptions{Mode: pglogrepl.LogicalReplication, SnapshotAction: "EXPORT_SNAPSHOT"})
	if err != nil {
		closeReplication(repl)
		return Start{}, fmt.Errorf("create replication slot %s: %w", cur.Slot, err)
	}
	cur.LSN = created.ConsistentPoint
	cur.CDC.Position = cur.Slot + " at " + cur.LSN
	s.Snapshot = created.SnapshotName
	settings, err := json.Marshal(s)
	if err != nil {
		closeReplication(repl)
		return Start{}, fmt.Errorf("encode postgres settings: %w", err)
	}
	read := src
	read.Settings = settings
	// The snapshot lives while this connection is open and idle.
	return Start{Source: &read, Close: func() { closeReplication(repl) }}, nil
}

// relationSignatures are tables' definitions as pgoutput's Relation message
// lists them: columns in order, without dropped or generated ones.
func relationSignatures(ctx context.Context, conn *pgx.Conn, schema string, tables []Table) (map[string]string, error) {
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = t.Name
	}
	out := map[string]string{}
	err := pgRows(ctx, conn, `SELECT c.relname, string_agg(a.attname || ' ' || a.atttypid::text, ',' ORDER BY a.attnum)
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = ANY($2) AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''
GROUP BY c.relname`, []any{schema, names}, func(r pgx.Rows) error {
		var name, signature string
		if err := r.Scan(&name, &signature); err != nil {
			return err
		}
		out[name] = signature
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read table definitions: %w", err)
	}
	return out, nil
}

func relationSignature(m *pglogrepl.RelationMessage) string {
	parts := make([]string, len(m.Columns))
	for i, c := range m.Columns {
		parts[i] = c.Name + " " + strconv.FormatUint(uint64(c.DataType), 10)
	}
	return strings.Join(parts, ",")
}

// readLog reads the slot's changes since cur.LSN up to where the server's WAL
// was when the read began, then fetches each changed key's current row: the
// stream says which keys changed, and a fetched row has a full read's bytes
// whatever the stream left out, such as unchanged TOASTed values.
//  1. A missing or lost slot expires the cursor.
//  2. Stream from cur.LSN, first confirming cur.LSN, which the syncer
//     committed; nothing later is confirmed until the next read.
//  3. Stop at the first commit, or keepalive between transactions, at or past the target.
//  4. Fetch the changed keys in one snapshot; a key with no row is a delete.
func (k *postgresKind) readLog(ctx context.Context, conn *pgx.Conn, src Source, s postgresSettings, tables []Table,
	cur *postgresCursor, w ChangeWriter) error {
	if cur.CDC == nil {
		cur.CDC = &postgresCDCStatus{}
	}
	var walStatus *string
	err := conn.QueryRow(ctx, `SELECT wal_status, COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn), 0)::bigint
FROM pg_replication_slots WHERE slot_name = $1 AND plugin = 'pgoutput' AND database = current_database()`, cur.Slot).
		Scan(&walStatus, &cur.CDC.RetainedBytes)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		cdcResets.WithLabelValues("postgres", "missing").Inc()
		return &PositionLostError{Reason: "missing"}
	case err != nil:
		return fmt.Errorf("read replication slot %s: %w", cur.Slot, err)
	case walStatus != nil && *walStatus == "lost":
		// Counted by the full read's Start, which finds the slot lost and recreates it.
		return fmt.Errorf("%w: replication slot %s lost WAL it needed", ErrCursorExpired, cur.Slot)
	}
	cdcRetainedBytes.WithLabelValues(src.ConnectorID).Set(float64(cur.CDC.RetainedBytes))
	start, err := pglogrepl.ParseLSN(cur.LSN)
	if err != nil {
		return fmt.Errorf("%w: unreadable position %q", ErrCursorExpired, cur.LSN)
	}
	log := newPostgresLog(s.Schema, tables, cur.Relations, w)
	repl, err := k.replicate(ctx, s.DatabaseSettings, src.Secret)
	if err != nil {
		return refused(err, "could not open a replication connection; check the user may still replicate")
	}
	defer closeReplication(repl)
	end, err := log.read(ctx, repl, cur.Slot, s.CDC.Publication, start)
	if err != nil {
		return err
	}
	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := endStream(stopCtx, repl); err != nil {
		return fmt.Errorf("end the replication stream: %w", err)
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return connectFailure(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // a no-op after Commit
	if err := log.fetch(ctx, conn); err != nil {
		return err
	}
	var unknown []Table
	for _, t := range tables {
		if _, ok := log.signatures[t.Name]; !ok {
			unknown = append(unknown, t)
		}
	}
	if len(unknown) > 0 {
		found, err := relationSignatures(ctx, conn, s.Schema, unknown)
		if err != nil {
			return err
		}
		for name, signature := range found {
			log.signatures[name] = signature
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("finish reading changed rows: %w", err)
	}
	cur.LSN, cur.Relations = end.String(), log.signatures
	cur.CDC.Position = cur.Slot + " at " + cur.LSN
	if log.lastReset != "" {
		cur.CDC.reset(log.lastReset)
	}
	return nil
}

// postgresStatusEvery is how long a read waits on a quiet stream before
// asking the server where it is.
var postgresStatusEvery = time.Second

// postgresMaxKeys bounds the keys a read fetches per table; past it the
// table is read in full, which is cheaper than that many lookups.
const postgresMaxKeys = 100_000

// postgresFetchBatch is how many changed keys one fetch query reads.
const postgresFetchBatch = 1000

// postgresLog is one read's pass over the stream: the keys that changed per
// followed table, and the tables to read in full instead.
type postgresLog struct {
	schema string
	w      ChangeWriter
	tables map[string]*postgresChanged
	// relations are the stream's relations by id; signatures the followed
	// tables' definitions as of the newest Relation message.
	relations  map[uint32]*postgresRelation
	signatures map[string]string
	lastReset  string
}

type postgresChanged struct {
	table Table
	reset bool
	keys  [][]string
	seen  map[string]bool
}

type postgresRelation struct {
	changed *postgresChanged
	// key are the primary key's positions in the relation's tuples.
	key []int
}

// newPostgresLog follows tables. One without a known definition was not
// read from the log before, so it is read in full once.
func newPostgresLog(schema string, tables []Table, signatures map[string]string, w ChangeWriter) *postgresLog {
	l := &postgresLog{schema: schema, w: w, tables: map[string]*postgresChanged{},
		relations: map[uint32]*postgresRelation{}, signatures: map[string]string{}}
	for _, t := range tables {
		c := &postgresChanged{table: t, seen: map[string]bool{}}
		l.tables[t.Name] = c
		if signature, ok := signatures[t.Name]; ok {
			l.signatures[t.Name] = signature
		} else {
			l.reset(c, "new")
		}
	}
	return l
}

// reset reads the table in full this sync. Every reason but a table's first
// read from the log is recorded as the connector's last reset.
func (l *postgresLog) reset(c *postgresChanged, reason string) {
	if !c.reset {
		c.reset, c.keys, c.seen = true, nil, nil
		l.w.Reset(c.table.Name, reason)
		if reason != "new" {
			cdcResets.WithLabelValues("postgres", reason).Inc()
		}
	}
	if reason != "new" {
		l.lastReset = c.table.Name + ": " + reason
	}
}

func (l *postgresLog) relation(m *pglogrepl.RelationMessage) {
	rel := &postgresRelation{}
	l.relations[m.RelationID] = rel
	c := l.tables[m.RelationName]
	if m.Namespace != l.schema || c == nil {
		return
	}
	rel.changed = c
	signature := relationSignature(m)
	if prev, ok := l.signatures[m.RelationName]; ok && prev != signature {
		l.reset(c, "schema")
	}
	l.signatures[m.RelationName] = signature
	for _, column := range c.table.PrimaryKey {
		i := slices.IndexFunc(m.Columns, func(rc *pglogrepl.RelationMessageColumn) bool { return rc.Name == column })
		if i < 0 {
			l.reset(c, "schema")
			return
		}
		rel.key = append(rel.key, i)
	}
}

// keyOf reads the primary key's text values from a tuple; false when the tuple
// lacks one, as a replica identity without the key or an unchanged TOASTed key does.
func (r *postgresRelation) keyOf(tuple *pglogrepl.TupleData) ([]string, bool) {
	if tuple == nil || len(r.key) == 0 {
		return nil, false
	}
	values := make([]string, len(r.key))
	for i, pos := range r.key {
		if pos >= len(tuple.Columns) || tuple.Columns[pos].DataType != pglogrepl.TupleDataTypeText {
			return nil, false
		}
		values[i] = string(tuple.Columns[pos].Data)
	}
	return values, true
}

// change records the keys an insert (old nil), update or delete (tuple nil) touched.
func (l *postgresLog) change(id uint32, old, tuple *pglogrepl.TupleData) {
	rel := l.relations[id]
	if rel == nil || rel.changed == nil || rel.changed.reset {
		return
	}
	c := rel.changed
	oldKey, oldOK := rel.keyOf(old)
	newKey, newOK := rel.keyOf(tuple)
	if old != nil && !oldOK || tuple != nil && !newOK && !oldOK {
		l.reset(c, "key")
		return
	}
	for _, key := range [][]string{oldKey, newKey} {
		joined := strings.Join(key, "\x00") // text values never hold NUL
		if key == nil || c.seen[joined] {
			continue
		}
		c.seen[joined] = true
		c.keys = append(c.keys, key)
	}
	if len(c.keys) > postgresMaxKeys {
		l.reset(c, "volume")
	}
}

func (l *postgresLog) truncate(m *pglogrepl.TruncateMessage) {
	for _, id := range m.RelationIDs {
		if rel := l.relations[id]; rel != nil && rel.changed != nil {
			l.reset(rel.changed, "truncate")
		}
	}
}

// read streams the slot from start and returns the position the next read starts at.
func (l *postgresLog) read(ctx context.Context, repl *pgconn.PgConn, slot, publication string, start pglogrepl.LSN) (pglogrepl.LSN, error) {
	system, err := pglogrepl.IdentifySystem(ctx, repl)
	if err != nil {
		return 0, fmt.Errorf("identify the server: %w", err)
	}
	target := system.XLogPos
	err = pglogrepl.StartReplication(ctx, repl, slot, start, pglogrepl.StartReplicationOptions{
		Mode: pglogrepl.LogicalReplication,
		PluginArgs: []string{
			"proto_version '1'",
			"publication_names '" + strings.ReplaceAll(pgx.Identifier{publication}.Sanitize(), "'", "''") + "'",
		},
	})
	if err != nil {
		return 0, fmt.Errorf("stream replication slot %s from %s: %w", slot, start, err)
	}
	// reply asks the server where it is, which a quiet stream otherwise never says.
	confirm := func(reply bool) error {
		return pglogrepl.SendStandbyStatusUpdate(ctx, repl, pglogrepl.StandbyStatusUpdate{WALWritePosition: start, ReplyRequested: reply})
	}
	if err := confirm(true); err != nil {
		return 0, fmt.Errorf("confirm %s: %w", start, err)
	}
	pos, inTxn := start, false
	for inTxn || pos < target {
		wait, cancel := context.WithTimeout(ctx, postgresStatusEvery)
		msg, err := repl.ReceiveMessage(wait)
		cancel()
		if pgconn.Timeout(err) && ctx.Err() == nil {
			if err := confirm(true); err != nil {
				return 0, fmt.Errorf("confirm %s: %w", start, err)
			}
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read replication slot %s: %w", slot, err)
		}
		var data []byte
		switch msg := msg.(type) {
		case *pgproto3.ErrorResponse:
			return 0, fmt.Errorf("read replication slot %s: %w", slot, pgconn.ErrorResponseToPgError(msg))
		case *pgproto3.CopyData:
			data = msg.Data
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			keepalive, err := pglogrepl.ParsePrimaryKeepaliveMessage(data[1:])
			if err != nil {
				return 0, fmt.Errorf("read replication slot %s: %w", slot, err)
			}
			// Between transactions, the server has sent every commit before its WAL end.
			if !inTxn && keepalive.ServerWALEnd > pos {
				pos = keepalive.ServerWALEnd
			}
			if keepalive.ReplyRequested {
				if err := confirm(false); err != nil {
					return 0, fmt.Errorf("confirm %s: %w", start, err)
				}
			}
		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(data[1:])
			if err != nil {
				return 0, fmt.Errorf("read replication slot %s: %w", slot, err)
			}
			msg, err := pglogrepl.Parse(xld.WALData)
			if err != nil {
				return 0, fmt.Errorf("decode replication slot %s at %s: %w", slot, xld.WALStart, err)
			}
			switch m := msg.(type) {
			case *pglogrepl.BeginMessage:
				inTxn = true
			case *pglogrepl.CommitMessage:
				inTxn = false
				pos = max(pos, m.TransactionEndLSN)
			case *pglogrepl.RelationMessage:
				l.relation(m)
			case *pglogrepl.InsertMessage:
				l.change(m.RelationID, nil, m.Tuple)
			case *pglogrepl.UpdateMessage:
				l.change(m.RelationID, m.OldTuple, m.NewTuple)
			case *pglogrepl.DeleteMessage:
				l.change(m.RelationID, m.OldTuple, nil)
			case *pglogrepl.TruncateMessage:
				l.truncate(m)
			}
		}
	}
	return pos, nil
}

// endStream leaves streaming cleanly: the server frees the slot before it
// answers, so the next read can take it at once.
func endStream(ctx context.Context, repl *pgconn.PgConn) error {
	repl.Frontend().Send(&pgproto3.CopyDone{})
	if err := repl.Frontend().Flush(); err != nil {
		return err
	}
	for {
		msg, err := repl.ReceiveMessage(ctx)
		if err != nil {
			return err
		}
		switch msg := msg.(type) {
		case *pgproto3.ErrorResponse:
			return pgconn.ErrorResponseToPgError(msg)
		case *pgproto3.ReadyForQuery:
			return nil
		}
	}
}

// fetch writes each changed key's current row, or a delete when it has none.
// It runs inside the caller's transaction on conn.
func (l *postgresLog) fetch(ctx context.Context, conn *pgx.Conn) error {
	names := make([]string, 0, len(l.tables))
	for name := range l.tables {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		c := l.tables[name]
		if c.reset || len(c.keys) == 0 {
			continue
		}
		query, err := fetchQuery(ctx, conn, l.schema, c.table)
		if err != nil {
			return err
		}
		if query == "" {
			l.reset(c, "schema")
			continue
		}
		for i := 0; i < len(c.keys); i += postgresFetchBatch {
			if err := l.fetchBatch(ctx, conn, query, name, c.table.PrimaryKey, c.keys[i:min(i+postgresFetchBatch, len(c.keys))]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *postgresLog) fetchBatch(ctx context.Context, conn *pgx.Conn, query, table string, key []string, keys [][]string) error {
	args := make([]any, len(key))
	for j := range key {
		column := make([]string, len(keys))
		for i, k := range keys {
			column[i] = k[j]
		}
		args[j] = column
	}
	return pgRows(ctx, conn, query, args, func(r pgx.Rows) error {
		var keyJSON, row []byte
		if err := r.Scan(&keyJSON, &row); err != nil {
			return fmt.Errorf("read changed rows of %s.%s: %w", l.schema, table, err)
		}
		if row != nil {
			return l.w.UpsertJSON(table, row)
		}
		dec := json.NewDecoder(strings.NewReader(string(keyJSON)))
		dec.UseNumber()
		var values map[string]any
		if err := dec.Decode(&values); err != nil {
			return fmt.Errorf("decode key of %s.%s: %w", l.schema, table, err)
		}
		return l.w.Delete(table, values)
	})
}

// fetchQuery reads rows by primary key from text arrays, one per key column.
// The key is cast to the column's own type, so the lookup uses the key's
// index and the key's JSON matches the row's. Empty when a key column is gone.
func fetchQuery(ctx context.Context, conn *pgx.Conn, schema string, t Table) (string, error) {
	types := map[string]string{}
	err := pgRows(ctx, conn, `SELECT attname, format_type(atttypid, NULL) FROM pg_attribute
WHERE attrelid = $1::regclass AND attnum > 0 AND NOT attisdropped`, []any{pgx.Identifier{schema, t.Name}.Sanitize()},
		func(r pgx.Rows) error {
			var name, typ string
			if err := r.Scan(&name, &typ); err != nil {
				return err
			}
			types[name] = typ
			return nil
		})
	if err != nil {
		return "", fmt.Errorf("read the key types of %s.%s: %w", schema, t.Name, err)
	}
	var arrays, aliases, casts, joins []string
	for i, column := range t.PrimaryKey {
		typ, ok := types[column]
		if !ok {
			return "", nil
		}
		ident := pgx.Identifier{column}.Sanitize()
		arrays = append(arrays, fmt.Sprintf("$%d::text[]", i+1))
		aliases = append(aliases, fmt.Sprintf("k%d", i))
		casts = append(casts, fmt.Sprintf("CAST(u.k%d AS %s) AS %s", i, typ, ident))
		joins = append(joins, "t."+ident+" = k."+ident)
	}
	return fmt.Sprintf(`SELECT row_to_json(k)::text, %s FROM unnest(%s) AS u(%s)
CROSS JOIN LATERAL (SELECT %s) k
LEFT JOIN %s t ON %s`, pgRowJSON, strings.Join(arrays, ", "), strings.Join(aliases, ", "), strings.Join(casts, ", "),
		pgx.Identifier{schema, t.Name}.Sanitize(), strings.Join(joins, " AND ")), nil
}

// Release drops the connector's slot. A cursor without one still names the
// connector, whose slot a failed first sync may have left.
func (k *postgresKind) Release(ctx context.Context, src Source, cursor json.RawMessage) error {
	var cur postgresCursor
	_ = json.Unmarshal(cursor, &cur) // an unreadable cursor falls back to the connector's slot
	slot := cur.Slot
	if slot == "" && src.ConnectorID != "" {
		slot = postgresSlot(src.ConnectorID)
	}
	if slot == "" {
		return nil
	}
	s, err := decodePostgresSettings(src.Settings)
	if err != nil {
		return err
	}
	conn, err := k.connect(ctx, s.DatabaseSettings, src.Secret)
	if err != nil {
		return connectFailure(err)
	}
	defer conn.Close(context.Background())
	if err := dropSlot(ctx, conn, slot); err != nil {
		return err
	}
	cdcRetainedBytes.DeleteLabelValues(src.ConnectorID)
	return nil
}

// postgresLogSetup is what Test reads off a database before following its log.
type postgresLogSetup struct {
	version             int
	walLevel            string
	replicationErr      error
	maxSlots, usedSlots int
	publication         bool
	publishesChanges    bool
	// noIdentity are published tables whose updates and deletes the database refuses.
	noIdentity []string
	// published are the schema's tables in the publication whose every row it
	// carries; filtered are those it carries through a row filter (PostgreSQL 15+).
	published, filtered []string
	// idleSlotTimeout drops a slot no connection has read for that long (PostgreSQL 18); 0 when off.
	idleSlotTimeout time.Duration
}

// minIdleSlotTimeout is the shortest idle_replication_slot_timeout a connector
// accepts: its slot sits idle between syncs, which can be a day apart.
const minIdleSlotTimeout = 24 * time.Hour

// checkLog runs Test's CDC checks and returns the keyed tables in scope that
// the log can't follow, because the publication leaves them out or filters
// their rows, and a warning naming the filtered ones.
func (k *postgresKind) checkLog(ctx context.Context, conn *pgx.Conn, src Source, s DatabaseSettings, keyed []string) ([]string, []string, error) {
	var p postgresLogSetup
	pub := s.CDC.Publication
	var idle int64
	var idleUnit string
	err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::int, current_setting('wal_level'),
current_setting('max_replication_slots')::int, (SELECT count(*) FROM pg_replication_slots WHERE slot_name <> $1),
COALESCE((SELECT setting::bigint FROM pg_settings WHERE name = 'idle_replication_slot_timeout'), 0),
COALESCE((SELECT unit FROM pg_settings WHERE name = 'idle_replication_slot_timeout'), 's')`,
		postgresSlot(src.ConnectorID)).Scan(&p.version, &p.walLevel, &p.maxSlots, &p.usedSlots, &idle, &idleUnit)
	if err != nil {
		return nil, nil, connectFailure(err)
	}
	p.idleSlotTimeout = pgSettingDuration(idle, idleUnit)
	if p.version < 130000 {
		unpublished, err := p.problem(s, keyed)
		return unpublished, nil, err
	}
	repl, err := k.replicate(ctx, s, src.Secret)
	if err == nil {
		_, err = pglogrepl.IdentifySystem(ctx, repl)
		closeReplication(repl)
	}
	p.replicationErr = err
	err = conn.QueryRow(ctx, "SELECT pubinsert AND pubupdate AND pubdelete AND pubtruncate FROM pg_publication WHERE pubname = $1", pub).Scan(&p.publishesChanges)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		unpublished, err := p.problem(s, keyed)
		return unpublished, nil, err
	case err != nil:
		return nil, nil, connectFailure(err)
	}
	p.publication = true
	if err := pgRows(ctx, conn, `SELECT p.schemaname || '.' || p.tablename
FROM pg_publication_tables p
JOIN pg_namespace n ON n.nspname = p.schemaname
JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = p.tablename
WHERE p.pubname = $1 AND (c.relreplident = 'n'
  OR c.relreplident = 'd' AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary)
  OR c.relreplident = 'i' AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisreplident))
ORDER BY 1`, []any{pub}, func(r pgx.Rows) error {
		var name string
		err := r.Scan(&name)
		p.noIdentity = append(p.noIdentity, name)
		return err
	}); err != nil {
		return nil, nil, connectFailure(err)
	}
	// A row filter hides changes to rows outside it, which a full read still
	// copies, so the log can't follow such a table.
	filter := "false"
	if p.version >= 150000 {
		filter = "rowfilter IS NOT NULL"
	}
	if err := pgRows(ctx, conn, "SELECT tablename, "+filter+" FROM pg_publication_tables WHERE pubname = $1 AND schemaname = $2",
		[]any{pub, s.Schema}, func(r pgx.Rows) error {
			var name string
			var filtered bool
			err := r.Scan(&name, &filtered)
			if filtered {
				p.filtered = append(p.filtered, name)
			} else {
				p.published = append(p.published, name)
			}
			return err
		}); err != nil {
		return nil, nil, connectFailure(err)
	}
	unpublished, err := p.problem(s, keyed)
	var warnings []string
	if len(p.filtered) > 0 && err == nil {
		warnings = append(warnings, fmt.Sprintf("publication %s filters the rows of %s, so their changes are read by cursor column or in full; "+
			"publish them without a WHERE clause for change data capture", pub, strings.Join(p.filtered, ", ")))
	}
	return unpublished, warnings, err
}

// problem is the first thing stopping CDC, worded with its fix; without one
// it returns the keyed tables the publication leaves out.
func (p postgresLogSetup) problem(s DatabaseSettings, keyed []string) ([]string, error) {
	pub := s.CDC.Publication
	pubIdent, user := pgx.Identifier{pub}.Sanitize(), pgx.Identifier{s.User}.Sanitize()
	qualified := make([]string, len(keyed))
	for i, t := range keyed {
		qualified[i] = pgx.Identifier{s.Schema, t}.Sanitize()
	}
	tableList := strings.Join(qualified, ", ")
	switch {
	case p.version < 130000:
		return nil, invalid("CDC needs PostgreSQL 13 or later; this server runs PostgreSQL %d", p.version/10000)
	case p.walLevel != "logical":
		return nil, invalid("logical decoding is off (wal_level is %s): set wal_level = logical (rds.logical_replication = 1 on Amazon RDS and Aurora, "+
			"cloudsql.logical_decoding = on on Cloud SQL), restart the database, then test again", p.walLevel)
	case p.replicationErr != nil:
		return nil, refused(p.replicationErr, "user %s may not read the replication log: run ALTER ROLE %s WITH REPLICATION "+
			"(GRANT rds_replication TO %s on Amazon RDS and Aurora), then test again", s.User, user, user)
	case !p.publication:
		return nil, invalid("publication %s does not exist: as the tables' owner, run CREATE PUBLICATION %s FOR TABLE %s, then test again",
			pub, pubIdent, tableList)
	case !p.publishesChanges:
		return nil, invalid("publication %s does not publish inserts, updates, deletes and truncates: run ALTER PUBLICATION %s SET "+
			"(publish = 'insert, update, delete, truncate'), then test again", pub, pubIdent)
	case len(p.noIdentity) > 0:
		return nil, invalid("publication %s holds tables without a replica identity, so your database refuses updates and deletes to them: %s; "+
			"give each a primary key, run ALTER TABLE … REPLICA IDENTITY FULL, or drop it from the publication, then test again",
			pub, strings.Join(p.noIdentity, ", "))
	}
	var unpublished []string
	for _, t := range keyed {
		if !slices.Contains(p.published, t) {
			unpublished = append(unpublished, t)
		}
	}
	switch {
	case len(keyed) > 0 && len(unpublished) == len(keyed):
		return nil, invalid("publication %s holds none of the chosen tables: run ALTER PUBLICATION %s ADD TABLE %s, then test again",
			pub, pubIdent, tableList)
	case p.usedSlots >= p.maxSlots:
		return nil, invalid("every replication slot is in use (max_replication_slots is %d): raise it or drop a slot no longer used, then test again",
			p.maxSlots)
	case p.idleSlotTimeout > 0 && p.idleSlotTimeout < minIdleSlotTimeout:
		return nil, invalid("idle_replication_slot_timeout is %s, so the database would drop Helix's slot between syncs: "+
			"set it to 0 or at least %s, then test again", p.idleSlotTimeout, minIdleSlotTimeout)
	}
	return unpublished, nil
}

// pgSettingDuration reads a time setting from pg_settings' value and unit.
func pgSettingDuration(value int64, unit string) time.Duration {
	switch unit {
	case "ms":
		return time.Duration(value) * time.Millisecond
	case "min":
		return time.Duration(value) * time.Minute
	case "h":
		return time.Duration(value) * time.Hour
	case "d":
		return time.Duration(value) * 24 * time.Hour
	}
	return time.Duration(value) * time.Second
}
