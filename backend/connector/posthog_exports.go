package connector

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/filesource"
)

// posthogExportMode reads PostHog batch exports from the customer's bucket
// instead of the query API, which PostHog forbids for bulk exports.
const posthogExportMode = "export"

// posthogModels is the export model each table is read from. The website asks
// for batch exports with the key prefix <prefix>{table}/{year}-{month}-{day}/,
// so PostHog writes <prefix>events/... and <prefix>persons/....
var posthogModels = map[string]string{
	"events":              "events",
	"daily_event_counts":  "events",
	"persons":             "persons",
	"person_distinct_ids": "persons",
}

// posthogVersions are the columns that order an export's rows of one key:
// a backfill can export an older version after a newer one.
var posthogVersions = map[string]string{
	"persons":             "person_version",
	"person_distinct_ids": "person_distinct_id_version",
}

// posthogMaxKeys bounds the ids an export read keeps in memory to pick each
// row's newest version or drop a repeated event.
const posthogMaxKeys = 5_000_000

func (k *posthogKind) Setup(workspaceID string) (string, string, bool) {
	if k.files == nil {
		return "", "", false
	}
	return k.files.Setup(workspaceID)
}

// prepareExports checks the export bucket. Helix reads it only through the
// customer's role, as the s3 kind does; the API key still reads groups.
func (k *posthogKind) prepareExports(b *S3Settings) error {
	switch {
	case b == nil:
		return invalid("enter the bucket your PostHog batch exports write to")
	case b.Keys != nil:
		return invalid("Helix reads PostHog exports through an IAM role; remove the access key")
	case b.Role == nil:
		return invalid(msgRoleARN)
	case k.files == nil:
		return invalid(msgNoRole)
	}
	b.Tables = nil
	return k.files.prepareSettings(b, "")
}

// discoverExports lists the export files of each model and reports the
// tables query mode reports, with the same keys.
func (k *posthogKind) discoverExports(ctx context.Context, src Source, s PostHogSettings, groupsReadable bool) (Discovery, error) {
	if k.files == nil || s.ExportBucket == nil {
		return Discovery{}, invalid(msgNoRole)
	}
	bucket := *s.ExportBucket
	deadline := k.files.timeouts.test
	if src.ForSync {
		deadline = k.files.timeouts.sync
	}
	listCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	sess, err := k.files.open(listCtx, bucket, src, true)
	if err != nil {
		return Discovery{}, err
	}
	files := map[string][]Object{}
	reasons := map[string]string{}
	for _, model := range []string{"events", "persons"} {
		sub := *sess
		sub.settings.Prefix += model + "/"
		listed, err := sub.list(listCtx)
		if err != nil {
			return Discovery{}, err
		}
		sess.region, sess.client = sub.region, sub.client
		var size int64
		var unreadable int
		files[model], unreadable = exportObjects(listed)
		for _, o := range files[model] {
			size += o.Size
		}
		switch {
		case len(files[model]) == 0 && unreadable > 0:
			reasons[model] = "PostHog's " + model + " export is in a format Helix can't read; export Parquet, or JSON Lines plain or gzipped"
		case len(files[model]) == 0:
			reasons[model] = fmt.Sprintf("no PostHog %s export under s3://%s/%s yet", model, bucket.Bucket, sub.settings.Prefix)
		case size > k.files.opts.MaxSourceBytes:
			reasons[model] = fmt.Sprintf("is %s, over the %s limit", FormatBytes(size), FormatBytes(k.files.opts.MaxSourceBytes))
		}
	}
	if len(files["events"]) == 0 && len(files["persons"]) == 0 {
		return Discovery{}, invalid("no PostHog exports under s3://%s/%s yet; export events and persons with the key prefix %s{table}/",
			bucket.Bucket, bucket.Prefix, bucket.Prefix)
	}
	d := saasDiscovery(posthogObjects, func(name string) bool {
		if model, ok := posthogModels[name]; ok {
			return reasons[model] == ""
		}
		return groupsReadable
	}, posthogForeignKeys)
	d.MaxBytes = k.files.opts.MaxConnectorBytes
	for i, u := range d.Unavailable {
		d.Unavailable[i].Reason = reasons[posthogModels[u.Name]]
	}
	shapePostHogTables(d.Tables)
	var stored int64
	for i := range d.Tables {
		d.Tables[i].Version = posthogVersions[d.Tables[i].Name]
		d.Tables[i].Files = files[posthogModels[d.Tables[i].Name]]
		for _, f := range d.Tables[i].Files {
			stored += f.Size
		}
	}
	if src.ForSync && stored > k.files.opts.MaxConnectorBytes {
		return Discovery{}, invalid("the PostHog exports hold %s, over the %s limit for one connection; lower event_days or expire old export files",
			FormatBytes(stored), FormatBytes(k.files.opts.MaxConnectorBytes))
	}
	bucket.Region = sess.region
	s.ExportBucket = &bucket
	d.Settings, err = json.Marshal(s)
	return d, err
}

// exportObjects keeps an export's data files in key order, and counts those
// in a format Helix can't read. PostHog names them <interval>.<jsonl|parquet>
// with a codec suffix, and writes a manifest.json that holds no rows; a
// Parquet file's codec is inside it.
func exportObjects(listed []filesource.Listed) ([]Object, int) {
	var out []Object
	unreadable := 0
	for _, o := range listed {
		base := strings.ToLower(path.Base(o.Key))
		if o.Size == 0 || strings.HasSuffix(o.Key, "/") || o.Archived || strings.HasSuffix(base, "manifest.json") ||
			strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") {
			continue
		}
		format, ok := filesource.FormatOf(base)
		switch {
		case strings.HasSuffix(base, ".parquet") || strings.Contains(base, ".parquet."):
			format = filesource.FormatParquet
		case !ok || format != filesource.FormatJSON:
			unreadable++
			continue
		}
		out = append(out, Object{Key: o.Key, Size: o.Size, ETag: o.ETag, Format: format})
	}
	slices.SortFunc(out, func(a, b Object) int { return strings.Compare(a.Key, b.Key) })
	return out, unreadable
}

// syncExport writes one table's rows from the export files Discover listed.
func (k *posthogKind) syncExport(ctx context.Context, src Source, s PostHogSettings, table Table, w RowWriter) error {
	if _, ok := posthogModels[table.Name]; !ok {
		return invalid("PostHog has no object %q", table.Name)
	}
	exp, err := k.openExport(ctx, src, s, table.Files)
	if err != nil {
		return err
	}
	defer exp.close()
	from, to := posthogWindow(time.Now(), s.EventDays)
	switch table.Name {
	case "events":
		return exp.events(ctx, from, to, w.Write)
	case "daily_event_counts":
		return exp.dailyCounts(ctx, from, to, w.Write)
	case "persons":
		return exp.persons(ctx, func(row map[string]any, deleted bool) error {
			if deleted {
				return nil
			}
			return w.Write(row)
		})
	}
	return exp.distinctIDs(ctx, func(row map[string]any, deleted bool) error {
		if deleted {
			return nil
		}
		return w.Write(row)
	})
}

func (k *posthogKind) Capture(src Source, table string) Capture {
	var s PostHogSettings
	if json.Unmarshal(src.Settings, &s) != nil || s.Mode != posthogExportMode {
		return CaptureFull
	}
	switch table {
	// Groups come from the query API, which PostHog forbids for recurring
	// reads: followed with an always-empty file list, they are read only by
	// the daily reconcile.
	case "events", "persons", "person_distinct_ids", "groups":
		return CaptureCursor
	}
	// A day's count needs every event of that day, which may sit in any file.
	return CaptureFull
}

func (k *posthogKind) Start(_ context.Context, _ Source, tables []Table) (Start, error) {
	return fileStart(tables)
}

// Changes reads only export files added after the cursor's. Events are
// upserted by uuid; a person or distinct id takes its newest row, and a newest
// row marked deleted deletes it. Any other change to the files reads the
// table in full.
func (k *posthogKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	prev, err := decodeFileCursor(cursor)
	if err != nil {
		return nil, err
	}
	var s PostHogSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return nil, invalid("PostHog settings are malformed")
	}
	next := fileCursor{Tables: make(map[string]fileList, len(tables))}
	added := map[string][]Object{}
	for _, t := range tables {
		next.Tables[t.Name] = listOf(t.Files)
		list, ok := prev.Tables[t.Name]
		if !ok {
			w.Reset(t.Name, "no file list yet")
			continue
		}
		files, reason := list.appended(t.Files)
		switch {
		case reason != "":
			w.Reset(t.Name, reason)
		case len(files) > 0:
			added[t.Name] = files
		}
	}
	if files := added["events"]; files != nil {
		exp, err := k.openExport(ctx, src, s, files)
		if err != nil {
			return nil, err
		}
		from, to := posthogWindow(time.Now(), s.EventDays)
		err = exp.events(ctx, from, to, func(row map[string]any) error { return w.Upsert("events", row) })
		exp.close()
		if err != nil {
			return nil, err
		}
	}
	persons, distinct := added["persons"], added["person_distinct_ids"]
	if persons != nil && distinct != nil && !slices.EqualFunc(persons, distinct, func(a, b Object) bool { return a.Key == b.Key }) {
		w.Reset("persons", "its files differ from person_distinct_ids'")
		w.Reset("person_distinct_ids", "its files differ from persons'")
		persons, distinct = nil, nil
	}
	files := persons
	if files == nil {
		files = distinct
	}
	if files != nil {
		exp, err := k.openExport(ctx, src, s, files)
		if err != nil {
			return nil, err
		}
		defer exp.close()
		if persons != nil {
			if err := exp.persons(ctx, func(row map[string]any, deleted bool) error {
				if deleted {
					return deleteVersion(w, "persons", map[string]any{"person_id": row["person_id"]}, row["person_version"])
				}
				return w.Upsert("persons", row)
			}); err != nil {
				return nil, err
			}
		}
		if distinct != nil {
			if err := exp.distinctIDs(ctx, func(row map[string]any, deleted bool) error {
				if deleted {
					return deleteVersion(w, "person_distinct_ids", map[string]any{"distinct_id": row["distinct_id"]}, row["person_distinct_id_version"])
				}
				return w.Upsert("person_distinct_ids", row)
			}); err != nil {
				return nil, err
			}
		}
	}
	return json.Marshal(next)
}

// posthogWindow is the span of event timestamps query mode reads: the last
// days UTC days, today included.
func posthogWindow(now time.Time, days int) (from, to time.Time) {
	today := now.UTC().Truncate(24 * time.Hour)
	return today.AddDate(0, 0, 1-days), today.AddDate(0, 0, 1)
}

// exportReader is one model's export files, downloaded and scanned.
type exportReader struct {
	schema *filesource.Schema
	files  []filesource.File
	dir    string
}

func (k *posthogKind) openExport(ctx context.Context, src Source, s PostHogSettings, objects []Object) (*exportReader, error) {
	if k.files == nil || s.ExportBucket == nil {
		return nil, invalid(msgNoRole)
	}
	// Discover proved the role's trust for this sync.
	sess, err := k.files.open(ctx, *s.ExportBucket, src, false)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(k.files.opts.TempDir, "foundry-posthog-*")
	if err != nil {
		return nil, err
	}
	r := &exportReader{dir: dir}
	if r.files, err = k.files.download(ctx, sess, Table{Files: objects}, dir); err != nil {
		r.close()
		return nil, err
	}
	lim := filesource.DefaultLimits
	lim.MaxDecompressedBytes = 2 * k.files.opts.MaxTableBytes
	if r.schema, err = filesource.Scan(ctx, r.files, lim); err != nil {
		r.close()
		return nil, readFailure(err, r.files)
	}
	return r, nil
}

func (r *exportReader) close() { _ = os.RemoveAll(r.dir) } // best-effort: the worker sweeps leftovers when it starts

// exportRecord is one row of an export model: the events model fills the
// event fields, the persons model the person ones.
type exportRecord struct {
	UUID              json.RawMessage `json:"uuid"`
	Event             json.RawMessage `json:"event"`
	DistinctID        json.RawMessage `json:"distinct_id"`
	PersonID          json.RawMessage `json:"person_id"`
	Properties        json.RawMessage `json:"properties"`
	Timestamp         json.RawMessage `json:"timestamp"`
	CreatedAt         json.RawMessage `json:"created_at"`
	PersonVersion     json.RawMessage `json:"person_version"`
	DistinctIDVersion json.RawMessage `json:"person_distinct_id_version"`
	IsDeleted         json.RawMessage `json:"is_deleted"`
}

// each calls fn with every record, files in key order.
func (r *exportReader) each(ctx context.Context, fn func(rec *exportRecord) error) error {
	for row, err := range r.schema.Rows(ctx) {
		if err != nil {
			return readFailure(err, r.files)
		}
		var rec exportRecord
		if err := json.Unmarshal(row, &rec); err != nil {
			return fmt.Errorf("decode posthog export row: %w", err)
		}
		if err := fn(&rec); err != nil {
			return err
		}
	}
	return nil
}

// events writes each event timestamped inside [from, to) once. PostHog may
// export an event again under its uuid, with another created_at; the last
// copy wins, as it does when a change read upserts it.
func (r *exportReader) events(ctx context.Context, from, to time.Time, fn func(map[string]any) error) error {
	inWindow := func(rec *exportRecord) (key, version json.RawMessage, ok bool) {
		_, at, timed := exportTime(rec.Timestamp)
		return rec.UUID, nil, timed && !at.Before(from) && at.Before(to)
	}
	return r.newest(ctx, "events in the window; lower event_days", inWindow, func(rec *exportRecord) error {
		timestamp, _, _ := exportTime(rec.Timestamp)
		created, _, _ := exportTime(rec.CreatedAt)
		p := exportProperties(rec.Properties)
		return fn(map[string]any{"event_id": exportValue(rec.UUID), "event": exportValue(rec.Event), "timestamp": timestamp, "created_at": created,
			"distinct_id": exportValue(rec.DistinctID), "person_id": exportValue(rec.PersonID),
			"session_id": p["$session_id"], "current_url": p["$current_url"], "properties": p})
	})
}

// dailyCounts counts the window's events per UTC day, event and person,
// each event once.
func (r *exportReader) dailyCounts(ctx context.Context, from, to time.Time, fn func(map[string]any) error) error {
	type key struct{ day, event, person string }
	type count struct {
		event, person any
		n             int64
	}
	counts := map[key]*count{}
	text := func(v any) string {
		b, _ := json.Marshal(v) // decoded JSON values always encode
		return string(b)
	}
	err := r.events(ctx, from, to, func(e map[string]any) error {
		// The timestamp is RFC 3339 UTC, so its date is the UTC day.
		k := key{day: e["timestamp"].(string)[:len(time.DateOnly)], event: text(e["event"]), person: text(e["person_id"])}
		c := counts[k]
		if c == nil {
			c = &count{event: e["event"], person: e["person_id"]}
			counts[k] = c
		}
		c.n++
		return nil
	})
	if err != nil {
		return err
	}
	keys := make([]key, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b key) int {
		return cmp.Or(strings.Compare(a.day, b.day), strings.Compare(a.event, b.event), strings.Compare(a.person, b.person))
	})
	for _, k := range keys {
		c := counts[k]
		if err := fn(map[string]any{"day": k.day, "event": c.event, "person_id": c.person, "events": c.n}); err != nil {
			return err
		}
	}
	return nil
}

// persons writes each person's row with the newest person_version.
func (r *exportReader) persons(ctx context.Context, fn func(row map[string]any, deleted bool) error) error {
	byPerson := func(rec *exportRecord) (key, version json.RawMessage, ok bool) {
		return rec.PersonID, rec.PersonVersion, true
	}
	return r.newest(ctx, "persons", byPerson,
		func(rec *exportRecord) error {
			created, _, _ := exportTime(rec.CreatedAt)
			p := exportProperties(rec.Properties)
			return fn(map[string]any{"person_id": exportValue(rec.PersonID), "created_at": created, "email": p["email"], "name": p["name"],
				"properties": p, "person_version": exportValue(rec.PersonVersion)}, exportBool(rec.IsDeleted))
		})
}

// distinctIDs writes each distinct id's person by the newest
// person_distinct_id_version: a merge moves it to another person.
func (r *exportReader) distinctIDs(ctx context.Context, fn func(row map[string]any, deleted bool) error) error {
	byDistinctID := func(rec *exportRecord) (key, version json.RawMessage, ok bool) {
		return rec.DistinctID, rec.DistinctIDVersion, true
	}
	return r.newest(ctx, "distinct ids", byDistinctID,
		func(rec *exportRecord) error {
			return fn(map[string]any{"distinct_id": exportValue(rec.DistinctID), "person_id": exportValue(rec.PersonID),
				"person_distinct_id_version": exportValue(rec.DistinctIDVersion)}, exportBool(rec.IsDeleted))
		})
}

// newest reads the records twice: first for the newest version of each key
// among the records keyOf keeps, a later record winning a tie, then to hand
// fn exactly those records. Two reads keep one small entry per key in memory
// rather than a row; what names the keys when there are too many.
func (r *exportReader) newest(ctx context.Context, what string, keyOf func(*exportRecord) (key, version json.RawMessage, ok bool),
	fn func(*exportRecord) error) error {
	type pick struct{ version, ord int64 }
	picks := map[[16]byte]pick{}
	var ord int64
	err := r.each(ctx, func(rec *exportRecord) error {
		ord++
		raw, version, kept := keyOf(rec)
		key, ok := exportKey(raw)
		if !kept || !ok {
			return nil
		}
		v := exportInt(version)
		h := idHash(key)
		p, seen := picks[h]
		switch {
		case !seen && len(picks) >= posthogMaxKeys:
			return invalid("the PostHog export holds more than %d %s", posthogMaxKeys, what)
		case !seen || v >= p.version:
			picks[h] = pick{version: v, ord: ord}
		}
		return nil
	})
	if err != nil {
		return err
	}
	ord = 0
	err = r.each(ctx, func(rec *exportRecord) error {
		ord++
		raw, _, kept := keyOf(rec)
		if key, ok := exportKey(raw); !kept || !ok || picks[idHash(key)].ord != ord {
			return nil
		}
		return fn(rec)
	})
	if err == nil {
		countRead(r.schema, r.files)
	}
	return err
}

func idHash(id string) [16]byte {
	sum := sha256.Sum256([]byte(id))
	return [16]byte(sum[:16])
}

// exportValue decodes one exported value; numbers keep their text so a row
// encodes the same whichever read wrote it.
func exportValue(raw json.RawMessage) any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	return v
}

// exportKey is a value that identifies a row, as text; ok is false for null.
func exportKey(raw json.RawMessage) (string, bool) {
	switch v := exportValue(raw).(type) {
	case string:
		return v, v != ""
	case json.Number:
		return v.String(), true
	}
	return "", false
}

// exportProperties decodes a properties column: a JSON string in Parquet,
// an object in JSON Lines.
func exportProperties(raw json.RawMessage) map[string]any {
	if text, ok := exportValue(raw).(string); ok {
		raw = json.RawMessage(text)
	}
	if p, ok := exportValue(raw).(map[string]any); ok {
		return p
	}
	return map[string]any{}
}

// exportTime reads a timestamp, RFC 3339 from Parquet or ISO 8601 from JSON
// Lines, and writes it as RFC 3339 UTC so both formats give the same row.
func exportTime(raw json.RawMessage) (any, time.Time, bool) {
	text, ok := exportValue(raw).(string)
	if !ok {
		return nil, time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999"} {
		if at, err := time.Parse(layout, text); err == nil {
			at = at.UTC()
			return at.Format(time.RFC3339Nano), at, true
		}
	}
	return text, time.Time{}, false
}

func exportInt(raw json.RawMessage) int64 {
	switch v := exportValue(raw).(type) {
	case json.Number:
		n, _ := v.Int64() // a version past int64 counts as 0, and loses every tie
		return n
	case string:
		n, _ := json.Number(v).Int64() // as above
		return n
	}
	return 0
}

// exportBool reads is_deleted, a boolean in Parquet that JSON Lines may
// write as 0 or 1.
func exportBool(raw json.RawMessage) bool {
	switch v := exportValue(raw).(type) {
	case bool:
		return v
	case json.Number:
		return v.String() != "0"
	case string:
		return v == "true" || v == "1"
	}
	return false
}
