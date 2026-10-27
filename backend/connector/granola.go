package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

var _ PassKind = (*granolaKind)(nil)

// granolaKeyRE takes personal and workspace keys alike; Granola documents
// only the grn_ prefix, so the rest is not held to a length.
var granolaKeyRE = regexp.MustCompile(`^grn_[A-Za-z0-9_-]+$`)

const (
	// granolaPageSize is the most any Granola list returns per page, and
	// granolaTranscriptPage the most transcript items a page of one does.
	granolaPageSize       = 30
	granolaTranscriptPage = 100
	maxGranolaPages       = 20_000
)

// granolaInterval spaces one read's requests. Granola allows a burst of 25
// per 5 seconds and 5 a second sustained, per user or workspace; 4 a second
// leaves room for the key's other clients. Tests set it to 0.
var granolaInterval = 250 * time.Millisecond

var granolaObjects = []saasTable{
	{"notes", "note_id"},
	// A note's attendees and folders come with the note; Discover keys them
	// by (note_id, email) and (note_id, folder_id).
	{"note_attendees", "note_id"},
	{"note_folders", "note_id"},
	{"folders", "folder_id"},
	{"spaces", "space_id"},
}

// granolaProbes is the list that proves an object readable.
var granolaProbes = map[string]string{
	"notes": "/v1/notes", "note_attendees": "/v1/notes", "note_folders": "/v1/notes",
	"folders": "/v1/folders", "spaces": "/v1/spaces",
}

var granolaForeignKeys = map[string][]ForeignKey{
	"note_attendees": {foreignKey("note_id", "notes", "note_id")},
	"note_folders":   {foreignKey("note_id", "notes", "note_id"), foreignKey("folder_id", "folders", "folder_id")},
	"folders":        {foreignKey("parent_folder_id", "folders", "folder_id"), foreignKey("space_id", "spaces", "space_id")},
}

var granolaChildKeys = map[string][]string{
	"note_attendees": {"note_id", "email"},
	"note_folders":   {"note_id", "folder_id"},
}

// granolaDropped are note fields kept out of the notes table: the lists have
// tables of their own, and the transcript is a document of its own;
// attachment URLs expire; a listed note is never deleted; and private notes
// are their owner's own jottings, returned only to the owner's key.
var granolaDropped = map[string]bool{
	"deleted_at": true, "attendees": true, "folder_membership": true, "space_membership": true,
	"attachments": true, "transcript": true, "private_notes_text": true, "private_notes_markdown": true,
}

type granolaKind struct {
	base string
	http *http.Client
}

func (k *granolaKind) Name() string        { return "granola" }
func (k *granolaKind) DisplayName() string { return "Granola" }

// GranolaSettings are a Granola connector's settings: whether it also
// keeps each note's transcript as a document. Off is stored as {}.
type GranolaSettings struct {
	Transcripts bool `json:"transcripts,omitempty"`
}

// The streams of the notes table: each note's summary, always, and its
// transcript when the settings turn transcripts on.
var (
	granolaNotesStream = Stream{Name: "notes", Table: "notes", Title: "Notes", Format: Markdown,
		Description: "Each Granola meeting note as Markdown: its title, date, attendees and note id, then the summary Granola wrote."}
	granolaTranscriptsStream = Stream{Name: "transcripts", Table: "notes", Title: "Transcripts", Format: PlainText,
		Description: "Each Granola meeting's transcript as plain text: its title, date, attendees and note id, then one " +
			"\"Speaker: words\" line per thing said, naming the speaker when Granola knows them, else Me (the note's owner) or Them."}
)

func (k *granolaKind) Prepare(src Source) (json.RawMessage, error) {
	if !granolaKeyRE.MatchString(strings.TrimSpace(src.Secret)) {
		return nil, invalid("enter a Granola API key (grn_…)")
	}
	var settings GranolaSettings
	decoder := json.NewDecoder(bytes.NewReader(orEmpty(src.Settings)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return nil, invalid("this connector takes only its key and whether to keep transcripts")
	}
	return json.Marshal(settings)
}

// Streams are notes, always, and transcripts when the settings keep them.
func (k *granolaKind) Streams(src Source) []Stream {
	var settings GranolaSettings
	_ = json.Unmarshal(orEmpty(src.Settings), &settings) // stored by Prepare
	if settings.Transcripts {
		return []Stream{granolaNotesStream, granolaTranscriptsStream}
	}
	return []Stream{granolaNotesStream}
}

// Discover proves the key with one-row lists of notes, folders and spaces;
// a list the key may not read makes its objects unavailable. The three
// requests are cheap enough for every sync to make them too.
func (k *granolaKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	c := k.client(src.Secret)
	readable, err := probeAll(ctx, uniqueValues(granolaProbes), func(ctx context.Context, path string) (bool, error) {
		status, _, err := c.get(ctx, path, url.Values{"page_size": {"1"}})
		switch {
		case err != nil:
			return false, err
		case status == http.StatusForbidden || status == http.StatusNotFound:
			return false, nil
		}
		return true, granolaStatus(status, path)
	})
	if err != nil {
		return Discovery{}, err
	}
	d := saasDiscovery(granolaObjects, func(name string) bool { return readable[granolaProbes[name]] }, granolaForeignKeys)
	if len(d.Tables) == 0 {
		return Discovery{}, refused(nil, "this key cannot read Granola notes, folders or spaces; check its access and test again")
	}
	for i, t := range d.Tables {
		if key, ok := granolaChildKeys[t.Name]; ok {
			d.Tables[i].PrimaryKey = key
		}
	}
	return d, nil
}

func (k *granolaKind) Hint(secret string) string { return keyHint(strings.TrimSpace(secret), 1) }

// Sync and SyncDocuments read one table through SyncPass. The syncer
// reads a PassKind's tables in one pass and calls neither.
func (k *granolaKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	return k.SyncDocuments(ctx, src, table, w, noDocuments{})
}

func (k *granolaKind) SyncDocuments(ctx context.Context, src Source, table Table, rows RowWriter, docs DocumentWriter) (Table, error) {
	_, err := k.SyncPass(ctx, src, []Table{table}, map[string]RowWriter{table.Name: rows}, docs)
	return table, err
}

// SyncPass reads folders and spaces from their lists, and every note table
// from one listing of the notes, newest first, with one Get Note per note:
// about 4.3 minutes per thousand notes at the paced rate. When the notes
// table is read, each note's documents are offered before it is got, so a
// transcript is asked for only when one is wanted.
func (k *granolaKind) SyncPass(ctx context.Context, src Source, tables []Table, rows map[string]RowWriter, docs DocumentWriter) ([]Table, error) {
	c := k.client(src.Secret)
	var noteTables []string
	for _, table := range tables {
		switch table.Name {
		case "folders", "spaces":
			w := rows[table.Name]
			if err := c.pages(ctx, "/v1/"+table.Name, table.Name, nil, func(row map[string]any) error {
				return w.Write(normalizeGranola(table.Name, row))
			}); err != nil {
				return tables, err
			}
		case "notes", "note_attendees", "note_folders":
			noteTables = append(noteTables, table.Name)
		default:
			return tables, invalid("Granola has no object %q", table.Name)
		}
	}
	if len(noteTables) == 0 {
		return tables, nil
	}
	summaries, err := c.list(ctx, nil)
	if err != nil {
		return tables, err
	}
	notes := granolaNotes{c: c, docs: docs}
	if slices.Contains(noteTables, "notes") {
		notes.streams = k.Streams(src)
	}
	found, missing := 0, 0
	err = notes.read(ctx, summaries, func(note map[string]any) error {
		found++
		for _, table := range noteTables {
			for _, row := range granolaRows(table, note) {
				if err := rows[table].Write(row); err != nil {
					return err
				}
			}
		}
		return nil
	}, func(_ string, deleted bool) error {
		if !deleted {
			missing++
		}
		return nil
	})
	// Get Note finding none of the notes the list showed is Granola failing,
	// not every note going between the two: an empty table would drop them all.
	if err == nil && found == 0 && missing > 0 {
		return tables, refused(nil, "Granola listed %d notes but returned none of them; try again", missing)
	}
	return tables, err
}

// granolaClient is one read's requests, paced together under Granola's rate limit.
type granolaClient struct {
	k    *granolaKind
	key  string
	pace *rate.Limiter
}

func (k *granolaKind) client(secret string) *granolaClient {
	return &granolaClient{k: k, key: strings.TrimSpace(secret), pace: rate.NewLimiter(rate.Every(granolaInterval), 1)}
}

// granolaSummary is a listed note: its id, whether the list shows it
// deleted, and the times that order a full read and version its documents.
type granolaSummary struct {
	id               string
	deleted          bool
	created, updated time.Time
}

// version is the version of both of the note's documents: a render
// revision, to bump whenever their text changes for the same note, then
// the note's update time.
func (s granolaSummary) version() string { return "1|" + s.updated.UTC().Format(time.RFC3339Nano) }

// list lists every note query matches, newest first by creation, each once:
// a note listed twice, as a list that changes while it is paged may, keeps
// its last listing. A live note without both times fails the read, since
// they order the read and version the note's documents. Only a key that
// sees every workspace note sees deleted ones.
func (c *granolaClient) list(ctx context.Context, query url.Values) ([]granolaSummary, error) {
	var summaries []granolaSummary
	at := map[string]int{}
	err := c.pages(ctx, "/v1/notes", "notes", query, func(item map[string]any) error {
		s := granolaSummary{deleted: item["deleted_at"] != nil}
		s.id, _ = item["id"].(string)
		if s.id == "" {
			return fmt.Errorf("granola /v1/notes: a listed note has no id")
		}
		if !s.deleted {
			created, _ := item["created_at"].(string)
			updated, _ := item["updated_at"].(string)
			var errCreated, errUpdated error
			s.created, errCreated = time.Parse(time.RFC3339, created)
			s.updated, errUpdated = time.Parse(time.RFC3339, updated)
			if errCreated != nil || errUpdated != nil {
				return fmt.Errorf("granola /v1/notes: note %s is listed without its created_at and updated_at times", s.id)
			}
		}
		if i, ok := at[s.id]; ok {
			summaries[i] = s
			return nil
		}
		at[s.id] = len(summaries)
		summaries = append(summaries, s)
		return nil
	})
	slices.SortStableFunc(summaries, func(a, b granolaSummary) int { return b.created.Compare(a.created) })
	return summaries, err
}

// granolaNotes gets listed notes in full and offers their documents to
// docs, for the streams that are on while the notes table is read.
type granolaNotes struct {
	c       *granolaClient
	docs    DocumentWriter
	streams []Stream
}

// read gets each listed note in turn, one Get Note each, with its
// transcript only when docs wants it; passes it to write; then Puts the
// documents docs wanted. A note the list shows deleted goes to gone with
// deleted set; one Get Note no longer finds goes there without it, and its
// wanted documents are not Put. Granola returns only processed notes, and
// a note can be deleted or unshared between the list and the get.
func (n granolaNotes) read(ctx context.Context, summaries []granolaSummary, write func(note map[string]any) error,
	gone func(id string, deleted bool) error) error {
	for _, s := range summaries {
		if s.deleted {
			if err := gone(s.id, true); err != nil {
				return err
			}
			continue
		}
		var wanted []Stream
		for _, stream := range n.streams {
			if n.docs.Want(stream.Name, s.id, s.version()) {
				wanted = append(wanted, stream)
			}
		}
		note, transcript, err := n.c.note(ctx, s.id, slices.Contains(wanted, granolaTranscriptsStream))
		if err != nil {
			return err
		}
		if note == nil {
			if err := gone(s.id, false); err != nil {
				return err
			}
			continue
		}
		if err := write(note); err != nil {
			return err
		}
		for _, stream := range wanted {
			doc, ok := granolaDocument(stream, s, note, transcript)
			if !ok {
				continue
			}
			if err := n.docs.Put(ctx, stream.Name, doc); err != nil {
				return err
			}
		}
	}
	return nil
}

// note gets a listed note in full, as Get Note returns it, and with
// transcript set its transcript's items: inline, or, when Granola answers
// that it is too large to return inline (413), from the note without it and
// then its transcript in pages. A nil note is one Get Note no longer finds.
// Rows and documents key the note by the id it was listed under.
func (c *granolaClient) note(ctx context.Context, id string, transcript bool) (map[string]any, []map[string]any, error) {
	path := "/v1/notes/" + url.PathEscape(id)
	var query url.Values
	if transcript {
		query = url.Values{"include": {"transcript"}}
	}
	status, body, err := c.get(ctx, path, query)
	switch {
	case err != nil:
		return nil, nil, err
	case status == http.StatusNotFound:
		return nil, nil, nil
	case transcript && status == http.StatusRequestEntityTooLarge:
		note, _, err := c.note(ctx, id, false)
		if err != nil || note == nil {
			return note, nil, err
		}
		var items []map[string]any
		err = c.pages(ctx, path+"/transcript", "transcript", url.Values{"page_size": {strconv.Itoa(granolaTranscriptPage)}},
			func(item map[string]any) error {
				items = append(items, item)
				return nil
			})
		return note, items, err
	}
	if err := granolaStatus(status, "/v1/notes/"+id); err != nil {
		return nil, nil, err
	}
	var note map[string]any
	if err := json.Unmarshal(body, &note); err != nil {
		return nil, nil, fmt.Errorf("decode granola note %s: %w", id, err)
	}
	// Keys come from the note's id, so an answer without it must not become rows.
	if got, _ := note["id"].(string); !strings.EqualFold(got, id) {
		return nil, nil, fmt.Errorf("granola note %s: Get Note answered with note %q", id, got)
	}
	note["id"] = id
	var items []map[string]any
	inline, _ := note["transcript"].([]any)
	for _, item := range inline {
		if m, ok := item.(map[string]any); ok {
			items = append(items, m)
		}
	}
	return note, items, nil
}

// pages lists a Granola collection a page at a time and passes each item of
// field to each. An incomplete page fails the attempt: ending early would
// publish a partial table.
func (c *granolaClient) pages(ctx context.Context, path, field string, query url.Values, each func(map[string]any) error) error {
	q := url.Values{"page_size": {strconv.Itoa(granolaPageSize)}}
	for key, values := range query {
		q[key] = values
	}
	for page := 0; ; page++ {
		if page == maxGranolaPages {
			return fmt.Errorf("granola %s: more than %d pages", path, maxGranolaPages)
		}
		status, body, err := c.get(ctx, path, q)
		if err != nil {
			return err
		}
		if err := granolaStatus(status, path); err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			return fmt.Errorf("decode granola %s page: %w", path, err)
		}
		var (
			items   []map[string]any
			hasMore *bool
			cursor  string
		)
		_ = json.Unmarshal(fields["cursor"], &cursor) // null on the last page; judged below when more follow
		switch {
		case json.Unmarshal(fields[field], &items) != nil || items == nil || json.Unmarshal(fields["hasMore"], &hasMore) != nil || hasMore == nil:
			return fmt.Errorf("granola %s: the response has no complete page of %s", path, field)
		case *hasMore && (cursor == "" || cursor == q.Get("cursor")):
			return fmt.Errorf("granola %s: a page says more follow but gives no new cursor", path)
		}
		for _, item := range items {
			if err := each(item); err != nil {
				return err
			}
		}
		if !*hasMore {
			return nil
		}
		q.Set("cursor", cursor)
	}
}

// get sends one Granola request, paced and retried by fetch. A rate limit
// that outlasts the retries is refused with when to try again, so the sync
// retries rather than failing for good. Granola documents no Retry-After;
// one in seconds is used when sent.
func (c *granolaClient) get(ctx context.Context, path string, query url.Values) (int, []byte, error) {
	target := c.k.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	status, header, body, err := fetchWithHeader(ctx, c.k.http, func() (*http.Request, error) {
		if err := c.pace.Wait(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err == nil {
			req.Header = http.Header{"Authorization": {"Bearer " + c.key}}
		}
		return req, err
	})
	if err != nil {
		return 0, nil, refused(err, "could not reach Granola; try again")
	}
	if status == http.StatusTooManyRequests {
		when := "in a few minutes"
		if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil && seconds > 0 {
			when = "after " + time.Now().Add(time.Duration(seconds)*time.Second).UTC().Format("15:04 UTC")
		}
		return 0, nil, refused(nil, "this key has used up its Granola rate limit; try again %s", when)
	}
	return status, body, nil
}

func granolaStatus(status int, path string) error {
	switch {
	case status == http.StatusUnauthorized:
		return refused(nil, "Granola rejected this key; check it and enter it again")
	case status == http.StatusForbidden || status == http.StatusNotFound:
		return refused(nil, "this key cannot read %s; check its access in Granola and test again", path)
	case status >= 300:
		return refused(nil, "Granola returned HTTP %d for %s", status, path)
	}
	return nil
}

// granolaRows is a note's rows of one note table, normalized.
func granolaRows(table string, note map[string]any) []map[string]any {
	switch table {
	case "notes":
		return []map[string]any{normalizeGranola(table, note)}
	case "note_attendees":
		return granolaAttendees(note)
	}
	var rows []map[string]any
	seen := map[string]bool{}
	folders, _ := note["folder_membership"].([]any)
	for _, f := range folders {
		folder, _ := f.(map[string]any)
		// Membership lists a note's folders and their ancestors; a folder is one row.
		if id, _ := folder["id"].(string); id != "" && !seen[id] {
			seen[id] = true
			rows = append(rows, map[string]any{"note_id": note["id"], "folder_id": id})
		}
	}
	return rows
}

// granolaAttendees is one row per person the note lists as an attendee or
// its calendar event invited, keyed by email in lower case: attended says
// the note lists them, invited that the event did. Only attendees carry a name.
func granolaAttendees(note map[string]any) []map[string]any {
	var rows []map[string]any
	byEmail := map[string]map[string]any{}
	add := func(person any, column string) {
		p, _ := person.(map[string]any)
		email, _ := p["email"].(string)
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" {
			return
		}
		row := byEmail[email]
		if row == nil {
			row = map[string]any{"note_id": note["id"], "email": email, "name": nil, "attended": false, "invited": false}
			byEmail[email] = row
			rows = append(rows, row)
		}
		row[column] = true
		if name, _ := p["name"].(string); name != "" && row["name"] == nil {
			row["name"] = name
		}
	}
	attendees, _ := note["attendees"].([]any)
	for _, a := range attendees {
		add(a, "attended")
	}
	event, _ := note["calendar_event"].(map[string]any)
	invitees, _ := event["invitees"].([]any)
	for _, i := range invitees {
		add(i, "invited")
	}
	return rows
}

// normalizeGranola renames id to the table's key and drops the object type;
// a note flattens its owner and calendar event into columns and drops what
// granolaDropped names.
func normalizeGranola(table string, row map[string]any) map[string]any {
	var key string
	for _, o := range granolaObjects {
		if o.name == table {
			key = o.key
		}
	}
	out := make(map[string]any, len(row))
	for field, value := range row {
		switch {
		case field == "object", table == "notes" && (granolaDropped[field] || field == "owner" || field == "calendar_event"):
		case field == "id":
			out[key] = value
		default:
			out[field] = value
		}
	}
	switch table {
	case "notes":
		owner, _ := row["owner"].(map[string]any)
		event, _ := row["calendar_event"].(map[string]any)
		out["owner_name"], out["owner_email"] = owner["name"], owner["email"]
		out["calendar_event_id"], out["calendar_event_title"], out["organiser_email"] = event["calendar_event_id"], event["event_title"], event["organiser"]
		out["scheduled_start_time"], out["scheduled_end_time"] = event["scheduled_start_time"], event["scheduled_end_time"]
	case "folders":
		// Granola may leave space_id out; every row has both columns.
		out["parent_folder_id"], out["space_id"] = row["parent_folder_id"], row["space_id"]
	}
	return out
}

// granolaDocument is a note's document of stream: both open with the note's
// title, date, attendees and id, then the notes stream has the summary
// (summary_markdown, else summary_text) and the transcripts stream one
// "Speaker: text" line per transcript item. ok is false for a transcript
// with nothing said, which is not Put.
func granolaDocument(stream Stream, s granolaSummary, note map[string]any, transcript []map[string]any) (_ Document, ok bool) {
	title, _ := note["title"].(string)
	title = strings.Join(strings.Fields(title), " ")
	heading := title
	if heading == "" {
		heading = "Untitled note"
	}
	var people []string
	for _, row := range granolaAttendees(note) {
		if row["attended"] != true {
			continue
		}
		person := row["email"].(string)
		if name, _ := row["name"].(string); name != "" {
			person = strings.Join(strings.Fields(name), " ") + " <" + person + ">"
		}
		people = append(people, person)
	}
	var text strings.Builder
	fmt.Fprintf(&text, "# %s\n\nDate: %s\n", heading, s.created.UTC().Format("2006-01-02 15:04 UTC"))
	if len(people) > 0 {
		fmt.Fprintf(&text, "Attendees: %s\n", strings.Join(people, ", "))
	}
	fmt.Fprintf(&text, "Note id: %s\n", s.id)
	switch stream.Name {
	case granolaNotesStream.Name:
		summary, _ := note["summary_markdown"].(string)
		if strings.TrimSpace(summary) == "" {
			summary, _ = note["summary_text"].(string)
		}
		if summary = strings.TrimSpace(summary); summary != "" {
			text.WriteString("\n" + summary + "\n")
		}
	case granolaTranscriptsStream.Name:
		said := 0
		for _, item := range transcript {
			line, _ := item["text"].(string)
			if line = strings.Join(strings.Fields(line), " "); line == "" {
				continue
			}
			if said == 0 {
				text.WriteString("\n")
			}
			said++
			text.WriteString(granolaSpeaker(item) + ": " + line + "\n")
		}
		if said == 0 {
			return Document{}, false
		}
	}
	webURL, _ := note["web_url"].(string)
	return Document{ExternalID: s.id, Version: s.version(), At: s.created, Title: title, URL: webURL, Text: []byte(text.String())}, true
}

// granolaSpeaker names who said a transcript item: the speaker's name when
// Granola identified them, else their diarization label (such as
// "Speaker A"), else Me for the note's owner or Them for anyone else, by
// the item's attribution or, without one, its audio source: the microphone
// is the owner's, the speaker output the others'.
func granolaSpeaker(item map[string]any) string {
	speaker, _ := item["speaker"].(map[string]any)
	for _, field := range []string{"name", "diarization_label"} {
		if label, _ := speaker[field].(string); strings.TrimSpace(label) != "" {
			return strings.Join(strings.Fields(label), " ")
		}
	}
	switch speaker["attribution"] {
	case "me":
		return "Me"
	case "them":
		return "Them"
	}
	if speaker["source"] == "microphone" {
		return "Me"
	}
	return "Them"
}
