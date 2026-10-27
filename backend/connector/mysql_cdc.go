package connector

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/go-sql-driver/mysql"
)

const (
	// mysqlMinRetention is the shortest binlog retention Test accepts: a
	// connector that falls further behind than the retention copies again.
	mysqlMinRetention = 24 * time.Hour
	// mysqlBinlogIdle bounds the wait for one event; heartbeats arrive well inside it.
	mysqlBinlogIdle = time.Minute
	// mysqlPendingKeys is how many changed keys of one table wait before they are fetched.
	mysqlPendingKeys = 5000
	mysqlFetchKeys   = 500
)

// mysqlBinlogCursor is a binlog-reading connector's change cursor.
type mysqlBinlogCursor struct {
	File   string `json:"file"`
	Offset uint32 `json:"offset"`
	// GTID is the executed GTID set, kept when gtid_mode is ON; reads then start
	// from it, so the place survives a failover to a server with the same history.
	GTID string         `json:"gtid,omitempty"`
	CDC  mysqlCDCStatus `json:"cdc"`
}

// mysqlCDCStatus is the cursor's cdc object, which the API shows as the connector's CDC status.
type mysqlCDCStatus struct {
	Position    string     `json:"position"`
	LastResetAt *time.Time `json:"last_reset_at,omitempty"`
	LastReset   string     `json:"last_reset,omitempty"`
}

type mysqlBinlogPosition struct {
	File   string
	Offset uint32
	// GTID is set only when gtid_mode is ON.
	GTID string
}

func (p mysqlBinlogPosition) String() string {
	return p.File + ":" + strconv.FormatUint(uint64(p.Offset), 10)
}

// same reports two positions naming one point. GTID sets decide when both
// have them, since file offsets differ between servers after a failover.
func (p mysqlBinlogPosition) same(q mysqlBinlogPosition) bool {
	if p.GTID != "" && q.GTID != "" {
		return p.GTID == q.GTID
	}
	return p.File == q.File && p.Offset == q.Offset
}

// compare orders the end of an event in file at pos against p. Files compare
// by their sequence number, which outgrows its zero padding after 999999.
func (p mysqlBinlogPosition) compare(file string, pos uint32) int {
	if file != p.File {
		base, seq, ok1 := mysqlBinlogFile(file)
		pBase, pSeq, ok2 := mysqlBinlogFile(p.File)
		if !ok1 || !ok2 || base != pBase {
			return strings.Compare(file, p.File)
		}
		if seq < pSeq {
			return -1
		}
		return 1
	}
	switch {
	case pos < p.Offset:
		return -1
	case pos > p.Offset:
		return 1
	}
	return 0
}

func mysqlBinlogFile(name string) (string, uint64, bool) {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return "", 0, false
	}
	seq, err := strconv.ParseUint(name[i+1:], 10, 64)
	return name[:i], seq, err == nil
}

var errMySQLBinlogOff = errors.New("connector: the MySQL binary log is off")

// mysqlBinlogStatus reads where the binlog ends now. MySQL 8.2 added SHOW
// BINARY LOG STATUS and 8.4 removed SHOW MASTER STATUS, so both are tried.
func mysqlBinlogStatus(ctx context.Context, db *sql.DB) (mysqlBinlogPosition, error) {
	status, err := mysqlStatusRow(ctx, db, "SHOW BINARY LOG STATUS")
	if mysqlErrorNumber(err) == 1064 {
		status, err = mysqlStatusRow(ctx, db, "SHOW MASTER STATUS")
	}
	if err != nil {
		return mysqlBinlogPosition{}, err
	}
	if status["File"] == "" {
		return mysqlBinlogPosition{}, errMySQLBinlogOff
	}
	offset, err := strconv.ParseUint(status["Position"], 10, 32)
	if err != nil {
		return mysqlBinlogPosition{}, fmt.Errorf("binlog position %q: %w", status["Position"], err)
	}
	pos := mysqlBinlogPosition{File: status["File"], Offset: uint32(offset)}
	var mode string
	if err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.gtid_mode").Scan(&mode); err != nil {
		return mysqlBinlogPosition{}, fmt.Errorf("read gtid_mode: %w", err)
	}
	if strings.EqualFold(mode, "ON") {
		pos.GTID = strings.Join(strings.Fields(status["Executed_Gtid_Set"]), "")
	}
	return pos, nil
}

func mysqlStatusRow(ctx context.Context, db *sql.DB, query string) (map[string]string, error) {
	out := map[string]string{}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if rows.Next() {
		values := make([]sql.NullString, len(names))
		dest := make([]any, len(names))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		for i, name := range names {
			out[name] = values[i].String
		}
	}
	return out, rows.Err()
}

// mysqlServerID is the replica id the connector's binlog reads register.
// A derived id sits in the top half of the range, away from the small ids
// people give their own replicas; the settings can name another.
func mysqlServerID(s DatabaseSettings, connectorID string) uint32 {
	if s.CDC != nil && s.CDC.ServerID != 0 {
		return s.CDC.ServerID
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(connectorID)) // hash writes never fail
	return h.Sum32() | 1<<31
}

func mysqlErrorNumber(err error) int {
	var driverErr *mysql.MySQLError
	var replicaErr *gomysql.MyError
	switch {
	case errors.As(err, &driverErr):
		return int(driverErr.Number)
	case errors.As(err, &replicaErr):
		return int(replicaErr.Code)
	}
	return 0
}

func mysqlReplicationFailure(err error) error {
	// The replica connection follows a SQL login with the same password, so
	// its 1045 is the server refusing the replica registration, not the password.
	var replicaErr *gomysql.MyError
	switch {
	case errors.Is(err, errMySQLBinlogOff):
		return refused(err, "the binary log is off; turn on log_bin (on Amazon RDS, turn on automated backups), then test again")
	case mysqlErrorNumber(err) == gomysql.ER_SPECIFIC_ACCESS_DENIED_ERROR,
		errors.As(err, &replicaErr) && replicaErr.Code == gomysql.ER_ACCESS_DENIED_ERROR:
		return refused(err, "the user may not read the binary log; run GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* "+
			"TO the connector's user, then test again")
	}
	return connectFailure(err)
}

// startBinlog takes the binlog's position before the full read copies the
// tables, which needs no lock. A change made between the two is in both: the
// next change read fetches each such row by key as it is then, so the copy converges.
func (k *mysqlKind) startBinlog(ctx context.Context, s DatabaseSettings, src Source) (Start, error) {
	db := k.open(s, src.Secret)
	defer db.Close()
	pos, err := mysqlBinlogStatus(ctx, db)
	if err != nil {
		return Start{}, mysqlReplicationFailure(err)
	}
	cursor, err := json.Marshal(mysqlBinlogCursor{File: pos.File, Offset: pos.Offset, GTID: pos.GTID,
		CDC: mysqlCDCStatus{Position: pos.String()}})
	return Start{Cursor: cursor}, err
}

// binlogChanges reads the binlog from the cursor to where it ended at
// connect. Row events only say which keys changed: each changed key is then
// fetched with the full read's query and written as it is now, or deleted
// when it is gone. A replayed batch therefore lands on the same rows, and an
// upserted row's bytes equal a full read's.
//  1. Read the selected tables' columns, then the target position. In this
//     order a DDL after the column read is before the target, so it is in
//     the batch and resets its table.
//  2. Stream as a replica from the cursor, collecting changed keys and resets.
//  3. Fetch the remaining keys and return the target as the next cursor.
func (k *mysqlKind) binlogChanges(ctx context.Context, s DatabaseSettings, src Source, tables []Table, raw json.RawMessage,
	w ChangeWriter) (json.RawMessage, error) {
	var prev mysqlBinlogCursor
	if err := json.Unmarshal(raw, &prev); err != nil || prev.File == "" && prev.GTID == "" {
		return nil, ErrCursorExpired
	}
	db := k.open(s, src.Secret)
	defer db.Close()
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = t.Name
	}
	columns, err := mysqlColumnsOf(ctx, db, s.Database, names)
	if err != nil {
		return nil, connectFailure(err)
	}
	r := &mysqlBinlogRead{db: db, database: s.Database, w: w, tables: map[string]*mysqlFollowed{}, maps: map[uint64]*mysqlMapped{}}
	selected := map[string]bool{}
	for _, t := range tables {
		f := &mysqlFollowed{name: t.Name, columns: columns[t.Name], pending: map[string]mysqlChangedKey{}}
		r.tables[t.Name] = f
		selected[t.Name] = true
		if len(t.PrimaryKey) == 0 {
			r.reset(f, "no primary key", "")
			continue
		}
		for _, name := range t.PrimaryKey {
			for _, c := range f.columns {
				if c.Name == name && c.keyable() {
					f.key = append(f.key, c)
				}
			}
		}
		if len(f.key) != len(t.PrimaryKey) {
			r.reset(f, "key type", "")
		}
	}
	target, err := mysqlBinlogStatus(ctx, db)
	if err != nil {
		return nil, mysqlReplicationFailure(err)
	}
	from := mysqlBinlogPosition{File: prev.File, Offset: prev.Offset, GTID: prev.GTID}
	if target.GTID == "" {
		from.GTID = ""
	}
	if !from.same(target) && r.following() {
		err := k.readBinlog(ctx, s, src.Secret, mysqlServerID(s, src.ConnectorID), from, target,
			r.decoder(selected), func(e *replication.BinlogEvent) error { return r.event(ctx, e) })
		switch code := mysqlErrorNumber(err); {
		case errors.Is(err, ErrCursorExpired):
			return nil, err
		case code == gomysql.ER_MASTER_FATAL_ERROR_READING_BINLOG || code == gomysql.ER_MASTER_HAS_PURGED_REQUIRED_GTIDS:
			cdcResets.WithLabelValues("mysql", "purged").Inc()
			return nil, &PositionLostError{Reason: "every table was read in full: the binlog after the cursor was purged"}
		case err != nil:
			return nil, mysqlReplicationFailure(err)
		}
		for _, f := range r.tables {
			if err := r.flush(ctx, f); err != nil {
				return nil, err
			}
		}
	}
	next := mysqlBinlogCursor{File: target.File, Offset: target.Offset, GTID: target.GTID, CDC: prev.CDC}
	next.CDC.Position = target.String()
	if r.lastReset != "" {
		now := time.Now().UTC()
		next.CDC.LastResetAt, next.CDC.LastReset = &now, r.lastReset
	}
	return json.Marshal(next)
}

func (k *mysqlKind) replicaConfig(s DatabaseSettings, password string, serverID uint32,
	decode func(*replication.RowsEvent, []byte) error) replication.BinlogSyncerConfig {
	return replication.BinlogSyncerConfig{
		ServerID: serverID, Flavor: gomysql.MySQLFlavor, Localhost: "helix-foundry",
		Host: s.Host, Port: uint16(s.Port), User: s.User, Password: password,
		TLSConfig: s.tlsConfig(), Dialer: k.guard.DialContext,
		HeartbeatPeriod: 10 * time.Second, ReadTimeout: mysqlBinlogIdle, DisableRetrySync: true,
		Logger: slog.New(slog.DiscardHandler), RowsEventDecodeFunc: decode,
	}
}

// readBinlog streams the binlog from `from` and calls each with every event
// up to `to`. A dump never ends on its own (go-mysql's non-blocking flag only
// logs the end), so the read compares the positions of events and heartbeats
// alike with `to`.
func (k *mysqlKind) readBinlog(ctx context.Context, s DatabaseSettings, password string, serverID uint32,
	from, to mysqlBinlogPosition, decode func(*replication.RowsEvent, []byte) error, each func(*replication.BinlogEvent) error) error {
	syncer := replication.NewBinlogSyncer(k.replicaConfig(s, password, serverID, decode))
	defer syncer.Close()
	var streamer *replication.BinlogStreamer
	if from.GTID != "" {
		set, err := gomysql.ParseMysqlGTIDSet(from.GTID)
		if err != nil {
			return fmt.Errorf("%w: gtid set %q: %w", ErrCursorExpired, from.GTID, err)
		}
		if streamer, err = syncer.StartSyncGTID(set); err != nil {
			return err
		}
	} else {
		var err error
		if streamer, err = syncer.StartSync(gomysql.Position{Name: from.File, Pos: from.Offset}); err != nil {
			return err
		}
	}
	file := from.File
	for {
		readCtx, cancel := context.WithTimeout(ctx, mysqlBinlogIdle)
		e, err := streamer.GetEvent(readCtx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("the binlog sent nothing for %s before reaching %s", mysqlBinlogIdle, to)
		}
		if err != nil {
			return err
		}
		switch ev := e.Event.(type) {
		case *replication.RotateEvent:
			file = string(ev.NextLogName)
			continue
		case *replication.HeartbeatEvent:
			// GTID auto-positioning also sends heartbeats while it skips the
			// transactions the cursor holds; only one at or past the target
			// says the source has sent everything up to it.
			if ev.Filename != "" {
				file = ev.Filename
			}
			pos := e.Header.LogPos
			if ev.Offset > 0 {
				pos = uint32(ev.Offset)
			}
			if to.compare(file, pos) >= 0 {
				return nil
			}
			continue
		}
		// Events the server makes up for the stream, such as its first format description, have no position.
		if e.Header.LogPos == 0 {
			continue
		}
		at := to.compare(file, e.Header.LogPos)
		if at > 0 {
			return nil
		}
		if err := each(e); err != nil {
			return err
		}
		if at == 0 {
			return nil
		}
	}
}

// checkBinlog refuses a server whose binlog a change read can't follow,
// naming the setting to change.
//  1. The binlog is on, row-based and logs whole rows.
//  2. It is kept for a day: RDS and Aurora report that through a procedure,
//     other servers through binlog_expire_logs_seconds.
//  3. The connector's replica id is not the server's own.
//  4. The user may read the binlog's position (REPLICATION CLIENT) and
//     stream it (REPLICATION SLAVE); both are tried rather than read from
//     SHOW GRANTS, which leaves out privileges granted through roles.
func (k *mysqlKind) checkBinlog(ctx context.Context, db *sql.DB, s DatabaseSettings, src Source) error {
	vars := map[string]string{}
	if err := sqlRows(ctx, db, `SHOW GLOBAL VARIABLES WHERE Variable_name IN ('log_bin', 'binlog_format', 'binlog_row_image',
'binlog_expire_logs_seconds', 'binlog_expire_logs_auto_purge', 'expire_logs_days', 'basedir', 'aurora_version', 'server_id')`, nil,
		func(r *sql.Rows) error {
			var name, value string
			if err := r.Scan(&name, &value); err != nil {
				return err
			}
			vars[name] = value
			return nil
		}); err != nil {
		return connectFailure(err)
	}
	switch {
	case !strings.EqualFold(vars["log_bin"], "ON") && vars["log_bin"] != "1":
		return mysqlReplicationFailure(errMySQLBinlogOff)
	case !strings.EqualFold(vars["binlog_format"], "ROW"):
		return refused(nil, "binlog_format is %s; set it to ROW, then test again", vars["binlog_format"])
	case !strings.EqualFold(vars["binlog_row_image"], "FULL"):
		return refused(nil, "binlog_row_image is %s; set it to FULL, then test again", vars["binlog_row_image"])
	}
	if err := mysqlCheckRetention(ctx, db, vars); err != nil {
		return err
	}
	serverID := mysqlServerID(s, src.ConnectorID)
	if strconv.FormatUint(uint64(serverID), 10) == vars["server_id"] {
		return invalid("replica server id %d is the database's own; choose another", serverID)
	}
	pos, err := mysqlBinlogStatus(ctx, db)
	if err != nil {
		return mysqlReplicationFailure(err)
	}
	if err := k.probeReplica(ctx, s, src.Secret, serverID, pos); err != nil {
		return mysqlReplicationFailure(err)
	}
	return nil
}

func mysqlCheckRetention(ctx context.Context, db *sql.DB, vars map[string]string) error {
	if strings.HasPrefix(vars["basedir"], "/rdsdbbin/") || vars["aurora_version"] != "" {
		hours, err := mysqlRDSRetentionHours(ctx, db)
		if err != nil {
			return refused(err, "could not read the binlog retention; let the user run CALL mysql.rds_show_configuration, then test again")
		}
		if time.Duration(hours)*time.Hour < mysqlMinRetention {
			return refused(nil, "the binlog is kept %d hours; run CALL mysql.rds_set_configuration('binlog retention hours', 24), "+
				"then test again", hours)
		}
		return nil
	}
	if strings.EqualFold(vars["binlog_expire_logs_auto_purge"], "OFF") {
		return nil
	}
	seconds, _ := strconv.ParseInt(vars["binlog_expire_logs_seconds"], 10, 64) // absent is 0, refused below
	if seconds == 0 {
		days, _ := strconv.ParseInt(vars["expire_logs_days"], 10, 64) // absent before 8.4 only
		seconds = days * 86400
	}
	// Azure reads 0 as deleting each binary log at once, and nothing a client
	// can query tells Azure apart from a server that keeps logs forever.
	if seconds == 0 {
		return refused(nil, "binlog_expire_logs_seconds is 0, which some hosts read as keeping no binlog; "+
			"set it to at least %d, then test again", int64(mysqlMinRetention/time.Second))
	}
	if time.Duration(seconds)*time.Second < mysqlMinRetention {
		return refused(nil, "the binlog is kept %d seconds; set binlog_expire_logs_seconds to at least %d, then test again",
			seconds, int64(mysqlMinRetention/time.Second))
	}
	return nil
}

// mysqlRDSRetentionHours reads RDS's own binlog retention; NULL, its
// default, keeps no binlog at all.
func mysqlRDSRetentionHours(ctx context.Context, db *sql.DB) (int64, error) {
	var hours int64
	err := sqlRows(ctx, db, "CALL mysql.rds_show_configuration", nil, func(r *sql.Rows) error {
		var name string
		var value, description sql.NullString
		if err := r.Scan(&name, &value, &description); err != nil {
			return err
		}
		if name == "binlog retention hours" && value.Valid {
			var err error
			hours, err = strconv.ParseInt(value.String, 10, 64)
			return err
		}
		return nil
	})
	return hours, err
}

// probeReplica connects as a replica at pos and reads the first event,
// which the server sends only to a user that may replicate.
func (k *mysqlKind) probeReplica(ctx context.Context, s DatabaseSettings, password string, serverID uint32, pos mysqlBinlogPosition) error {
	syncer := replication.NewBinlogSyncer(k.replicaConfig(s, password, serverID, nil))
	defer syncer.Close()
	streamer, err := syncer.StartSync(gomysql.Position{Name: pos.File, Pos: pos.Offset})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = streamer.GetEvent(ctx)
	return err
}

// mysqlBinlogRead is one change read's state: the followed tables, the
// binlog's table ids, and the keys each table changed.
type mysqlBinlogRead struct {
	db        *sql.DB
	database  string
	w         ChangeWriter
	tables    map[string]*mysqlFollowed
	maps      map[uint64]*mysqlMapped
	lastReset string
}

type mysqlFollowed struct {
	name    string
	columns []mysqlColumn
	key     []mysqlColumn
	pending map[string]mysqlChangedKey
	reset   bool
}

// mysqlChangedKey is one changed row's key as query arguments and as a full read's JSON values.
type mysqlChangedKey struct {
	args []any
	key  map[string]any
}

// mysqlMapped is a binlog table id bound to a followed table, with the
// ordinal of each key column in its row images.
type mysqlMapped struct {
	table *mysqlFollowed
	keyAt []int
}

func (r *mysqlBinlogRead) following() bool {
	for _, f := range r.tables {
		if !f.reset {
			return true
		}
	}
	return false
}

// reset reads the table in full this sync. A status names the change the
// customer made, for the connector's CDC status; empty for a table that is
// never followed.
func (r *mysqlBinlogRead) reset(f *mysqlFollowed, reason, status string) {
	if f.reset {
		return
	}
	f.reset, f.pending = true, nil
	r.w.Reset(f.name, reason)
	if status != "" {
		r.lastReset = fmt.Sprintf("table %s was read in full: %s", f.name, status)
		cdcResets.WithLabelValues("mysql", reason).Inc()
	}
}

// decoder leaves the rows of tables the read doesn't follow undecoded: the
// binlog carries every database on the server. It runs on go-mysql's
// goroutine, so it reads only selected, which never changes.
func (r *mysqlBinlogRead) decoder(selected map[string]bool) func(*replication.RowsEvent, []byte) error {
	database := r.database
	return func(e *replication.RowsEvent, data []byte) error {
		pos, err := e.DecodeHeader(data)
		if err != nil {
			return err
		}
		if string(e.Table.Schema) != database || !selected[string(e.Table.Table)] {
			return nil
		}
		return e.DecodeData(pos, data)
	}
}

func (r *mysqlBinlogRead) event(ctx context.Context, e *replication.BinlogEvent) error {
	switch ev := e.Event.(type) {
	case *replication.TableMapEvent:
		r.tableMap(ev)
	case *replication.RowsEvent:
		return r.rows(ctx, ev)
	case *replication.QueryEvent:
		r.statement(string(ev.Schema), string(ev.Query))
	case *replication.TransactionPayloadEvent:
		for _, inner := range ev.Events {
			if err := r.event(ctx, inner); err != nil {
				return err
			}
		}
	}
	return nil
}

// tableMap binds a table id to a followed table. Without binlog_row_metadata
// = FULL, rows name columns only by ordinal, so the table's column count must
// match information_schema's or the ordinals can't be trusted.
func (r *mysqlBinlogRead) tableMap(e *replication.TableMapEvent) {
	f := r.tables[string(e.Table)]
	if string(e.Schema) != r.database || f == nil || f.reset {
		r.maps[e.TableID] = nil
		return
	}
	names := e.ColumnNameString()
	if len(names) == 0 {
		if int(e.ColumnCount) != len(f.columns) {
			r.reset(f, "columns", "its columns changed")
			return
		}
		for _, c := range f.columns {
			names = append(names, c.Name)
		}
	}
	m := &mysqlMapped{table: f}
	for _, k := range f.key {
		at := -1
		for i, name := range names {
			if name == k.Name {
				at = i
			}
		}
		if at < 0 {
			r.reset(f, "columns", "its key columns changed")
			return
		}
		m.keyAt = append(m.keyAt, at)
	}
	r.maps[e.TableID] = m
}

// rows records the key of every row image, before and after: a key that
// changed is a delete of the old row and an insert of the new.
func (r *mysqlBinlogRead) rows(ctx context.Context, e *replication.RowsEvent) error {
	m := r.maps[e.TableID]
	if m == nil || m.table.reset {
		return nil
	}
	f := m.table
images:
	for _, image := range e.Rows {
		changed := mysqlChangedKey{args: make([]any, len(f.key)), key: map[string]any{}}
		values := make([]any, len(f.key))
		for i, at := range m.keyAt {
			// A row image without its key value is the after image of a
			// partial image; its before image carries the key.
			if at >= len(image) || image[at] == nil {
				continue images
			}
			arg, value, ok := f.key[i].keyValue(image[at])
			if !ok {
				r.reset(f, "key type", "")
				return nil
			}
			changed.args[i], changed.key[f.key[i].Name], values[i] = arg, value, value
		}
		id, err := json.Marshal(values)
		if err != nil {
			return fmt.Errorf("encode key of %s: %w", f.name, err)
		}
		f.pending[string(id)] = changed
	}
	if len(f.pending) >= mysqlPendingKeys {
		return r.flush(ctx, f)
	}
	return nil
}

// statement reads a table in full when a statement names it. In ROW format
// DDL is the only statement that touches a table's rows, and a loose match
// costs no more than one full read.
func (r *mysqlBinlogRead) statement(schema, query string) {
	first, _, _ := strings.Cut(strings.TrimSpace(query), " ")
	switch strings.ToUpper(first) {
	case "BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT", "XA":
		return
	}
	text := strings.ToLower(query)
	if schema != r.database && !mysqlNames(text, strings.ToLower(r.database)) {
		return
	}
	for _, f := range r.tables {
		if !f.reset && mysqlNames(text, strings.ToLower(f.name)) {
			r.reset(f, "schema", "a statement changed it")
		}
	}
}

// mysqlNames reports text naming the identifier name: name not inside a longer identifier.
func mysqlNames(text, name string) bool {
	identifier := func(b byte) bool {
		return b == '_' || b == '$' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
	}
	for i := 0; ; {
		at := strings.Index(text[i:], name)
		if at < 0 {
			return false
		}
		start, end := i+at, i+at+len(name)
		if (start == 0 || !identifier(text[start-1])) && (end == len(text) || !identifier(text[end])) {
			return true
		}
		i = start + 1
	}
}

// flush fetches the table's changed keys with the full read's query: a row
// found is written as it is now, and a key not found is deleted.
func (r *mysqlBinlogRead) flush(ctx context.Context, f *mysqlFollowed) error {
	if f.reset || len(f.pending) == 0 {
		return nil
	}
	names := make([]string, len(f.columns))
	for i, c := range f.columns {
		names[i] = c.Name
	}
	keyNames := make([]string, len(f.key))
	for i, c := range f.key {
		keyNames[i] = mysqlIdentifier(c.Name)
	}
	var chunk []string
	fetch := func() error {
		args := make([]any, 0, len(chunk)*len(f.key))
		tuples := make([]string, len(chunk))
		for i, id := range chunk {
			args = append(args, f.pending[id].args...)
			tuples[i] = "?" + strings.Repeat(",?", len(f.key)-1)
			if len(f.key) > 1 {
				tuples[i] = "(" + tuples[i] + ")"
			}
		}
		where := " WHERE " + keyNames[0]
		if len(f.key) > 1 {
			where = " WHERE (" + strings.Join(keyNames, ",") + ")"
		}
		found := map[string]bool{}
		err := sqlRows(ctx, r.db, mysqlRowSelect(f.name, names)+where+" IN ("+strings.Join(tuples, ",")+")", args, func(rows *sql.Rows) error {
			var row []byte
			if err := rows.Scan(&row); err != nil {
				return err
			}
			id, err := mysqlRowKey(row, f.key)
			if err != nil {
				return err
			}
			found[id] = true
			return r.w.UpsertJSON(f.name, row)
		})
		if err != nil {
			return fmt.Errorf("fetch changed rows of %s: %w", f.name, err)
		}
		for _, id := range chunk {
			if !found[id] {
				if err := r.w.Delete(f.name, f.pending[id].key); err != nil {
					return err
				}
			}
		}
		chunk = chunk[:0]
		return nil
	}
	for id := range f.pending {
		chunk = append(chunk, id)
		if len(chunk) == mysqlFetchKeys {
			if err := fetch(); err != nil {
				return err
			}
		}
	}
	if len(chunk) > 0 {
		if err := fetch(); err != nil {
			return err
		}
	}
	f.pending = map[string]mysqlChangedKey{}
	return nil
}

// mysqlRowKey is a fetched row's key values encoded as rows records them.
func mysqlRowKey(row []byte, key []mysqlColumn) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(row))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return "", fmt.Errorf("decode row: %w", err)
	}
	values := make([]any, len(key))
	for i, c := range key {
		values[i] = fields[c.Name]
	}
	id, err := json.Marshal(values)
	return string(id), err
}

// keyable reports a key column whose binlog values the reader can turn into
// a full read's JSON; a table keyed on any other type is read in full.
func (c mysqlColumn) keyable() bool {
	switch c.DataType {
	case "tinyint", "smallint", "mediumint", "int", "bigint", "decimal",
		"binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		return true
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext":
		// Other character sets reach the binlog as their own bytes, which JSON would hold converted.
		switch c.Charset {
		case "utf8mb4", "utf8mb3", "utf8", "ascii":
			return true
		}
	}
	return false
}

// mysqlBinaryTypes are the type codes JSON_OBJECT writes into a binary value.
var mysqlBinaryTypes = map[string]string{
	"binary": "254", "varbinary": "15", "tinyblob": "252", "blob": "252", "mediumblob": "252", "longblob": "252",
}

// keyValue turns a key column's binlog value into the argument that finds its
// row and the value JSON_OBJECT gives it.
func (c mysqlColumn) keyValue(v any) (arg, value any, ok bool) {
	switch c.DataType {
	case "tinyint", "smallint", "mediumint", "int", "bigint":
		n, ok := mysqlInteger(v, c)
		return n, n, ok
	case "decimal":
		s, ok := v.(string)
		return s, json.Number(s), ok
	}
	var b []byte
	switch s := v.(type) {
	case string:
		b = []byte(s)
	case []byte:
		b = s
	default:
		return nil, nil, false
	}
	code, binary := mysqlBinaryTypes[c.DataType]
	if !binary {
		return string(b), string(b), true
	}
	// The binlog drops a BINARY value's trailing zero bytes, which the column keeps.
	if c.DataType == "binary" && int64(len(b)) < c.Octets {
		b = append(b, make([]byte, c.Octets-int64(len(b)))...)
	}
	return b, "base64:type" + code + ":" + base64.StdEncoding.EncodeToString(b), true
}

// mysqlInteger reads a binlog integer as int64 or uint64. Without the
// signedness metadata go-mysql reads an unsigned column as signed, so a
// negative value is reinterpreted at the column's width.
func mysqlInteger(v any, c mysqlColumn) (any, bool) {
	var n int64
	switch i := v.(type) {
	case int8:
		n = int64(i)
	case int16:
		n = int64(i)
	case int32:
		n = int64(i)
	case int64:
		n = i
	case uint8:
		return uint64(i), true
	case uint16:
		return uint64(i), true
	case uint32:
		return uint64(i), true
	case uint64:
		return i, true
	default:
		return nil, false
	}
	if !c.Unsigned || n >= 0 {
		return n, true
	}
	bits := map[string]uint{"tinyint": 8, "smallint": 16, "mediumint": 24, "int": 32, "bigint": 64}[c.DataType]
	u := uint64(n)
	if bits < 64 {
		u &= 1<<bits - 1
	}
	return u, true
}
