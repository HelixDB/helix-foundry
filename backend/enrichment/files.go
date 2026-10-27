// Package enrichment is People enrichment's core: it plans a load's
// lookups from the people the engine's Clues read, looks them up on
// context.dev (or a fake), checks each match against the person it is for,
// charges a ledger, and publishes the profiles the load applies.
//
// A load's files, in foundry.sync.data_bucket under
// ws/<workspace>/enrichment/<ontology>/loads/<load>/ (Layout), are gzipped
// JSON lines:
//
//	plan.jsonl.gz          every entity and what the load does with it (Entry)
//	carried.jsonl.gz       the accepted profiles carried from earlier loads, slim
//	calls/<b:06d>.jsonl.gz batch b's raw calls (Call), kept for the retention
//	results/<b:06d>.jsonl.gz batch b's outcomes, until publish
//	results.jsonl.gz       everyone called or carried (Result), with full rows
//	profiles.jsonl.gz      the profiles the load applies, slim
//
// Slim profiles hold only what the version's item maps, plus profile_id and
// the clues the engine binds them by.
package enrichment

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"iter"
	"os"
	"path/filepath"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Profile is one row of the built-in foundry_enrichment catalog. The engine
// holds the same catalog; only the fields in Mappable may be mapped.
type Profile struct {
	// ProfileID is li:<slug> for a profile with a LinkedIn URL, else h: and
	// 16 hex digits of a digest of its name and employers.
	ProfileID string `json:"profile_id"`
	// The clues and the name the engine binds the row by: the entity's own
	// values, as Clues read them.
	ClueEmail       string `json:"clue_email,omitempty"`
	ClueLinkedInURL string `json:"clue_linkedin_url,omitempty"`
	ClueName        string `json:"clue_name"`

	FullName          string       `json:"full_name,omitempty"`
	FirstName         string       `json:"first_name,omitempty"`
	LastName          string       `json:"last_name,omitempty"`
	CurrentTitle      string       `json:"current_title,omitempty"`
	CurrentRoleStatus string       `json:"current_role_status,omitempty"`
	CurrentOrgName    string       `json:"current_org_name,omitempty"`
	CurrentOrgDomain  string       `json:"current_org_domain,omitempty"`
	LinkedInURL       string       `json:"linkedin_url,omitempty"`
	Location          string       `json:"location,omitempty"`
	LocationCity      string       `json:"location_city,omitempty"`
	LocationRegion    string       `json:"location_region,omitempty"`
	LocationCountry   string       `json:"location_country,omitempty"`
	Experience        []Experience `json:"experience,omitempty"`

	// Metadata, never mappable.
	MatchedOn         string `json:"matched_on,omitempty"`
	Score             int    `json:"score"`
	LastUpdated       string `json:"last_updated,omitempty"`
	CheckedAt         string `json:"checked_at,omitempty"`
	FetchedAt         string `json:"fetched_at,omitempty"`
	LoadID            string `json:"load_id,omitempty"`
	CallID            string `json:"call_id,omitempty"`
	ProviderRequestID string `json:"provider_request_id,omitempty"`
}

// Experience is one employer of a profile: the newest per domain, at most
// MaxExperience, numbered from 1.
type Experience struct {
	ID           int          `json:"id"`
	Title        string       `json:"title,omitempty"`
	Organization Organization `json:"organization"`
	Location     string       `json:"location,omitempty"`
	StartDate    string       `json:"start_date,omitempty"`
	EndDate      string       `json:"end_date,omitempty"`
	IsCurrent    bool         `json:"is_current"`
}

// Organization is an employer; Domain is lowercase, without www.
type Organization struct {
	Name   string `json:"name,omitempty"`
	Domain string `json:"domain,omitempty"`
}

// MaxExperience is how many employers a profile keeps.
const MaxExperience = 20

// Mappable are the catalog fields a version's item may map through props.
var Mappable = map[string]bool{
	"full_name": true, "first_name": true, "last_name": true, "current_title": true, "current_role_status": true,
	"current_org_name": true, "current_org_domain": true, "linkedin_url": true, "location": true, "location_city": true,
	"location_region": true, "location_country": true,
}

// domains are the registrable domains of the profile's employers, current
// role included.
func (p *Profile) domains() []string {
	var out []string
	for _, d := range append([]string{p.CurrentOrgDomain}, experienceDomains(p.Experience)...) {
		if registrable, ok := RegistrableDomain(d); ok {
			out = append(out, registrable)
		}
	}
	return out
}

func experienceDomains(experience []Experience) []string {
	out := make([]string, 0, len(experience))
	for _, e := range experience {
		out = append(out, e.Organization.Domain)
	}
	return out
}

// Slim is what the load applies of a profile: profile_id, the clues and the
// name, the fields item maps, and experience only when it maps work history.
// A field outside Mappable is an error: the engine refuses such an item.
func Slim(p *Profile, item *ontologyagent.Enrichment) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	keep := []string{"profile_id", "clue_email", "clue_linkedin_url", "clue_name"}
	for _, field := range item.Fields() {
		if !Mappable[field] {
			return nil, fmt.Errorf("enrichments.%s maps %q, which is not a profile field", item.Name, field)
		}
		keep = append(keep, field)
	}
	if item.WorkHistory != nil {
		keep = append(keep, "experience")
	}
	slim := make(map[string]json.RawMessage, len(keep))
	for _, k := range keep {
		if v, ok := all[k]; ok {
			slim[k] = v
		}
	}
	return slim, nil
}

// Layout is where one load's files live.
type Layout string

// LoadLayout is load's layout.
func LoadLayout(workspaceID, ontologyID, loadID string) Layout {
	return Layout(fmt.Sprintf("ws/%s/enrichment/%s/loads/%s/", workspaceID, ontologyID, loadID))
}

func (l Layout) Plan() string     { return string(l) + "plan.jsonl.gz" }
func (l Layout) Carried() string  { return string(l) + "carried.jsonl.gz" }
func (l Layout) Results() string  { return string(l) + "results.jsonl.gz" }
func (l Layout) Profiles() string { return string(l) + "profiles.jsonl.gz" }

// Calls is batch b's raw calls; BatchResults its outcomes.
func (l Layout) Calls(b int) string        { return fmt.Sprintf("%scalls/%06d.jsonl.gz", l, b) }
func (l Layout) BatchResults(b int) string { return fmt.Sprintf("%sresults/%06d.jsonl.gz", l, b) }

// Files reads and writes a step's files.
type Files struct {
	Store snapshot.Store
	// Dir holds the temporary files a write fills before its upload, and a
	// step's copy of its plan: each unlinked as it is created, so it lives
	// only while open, and a worker that dies (an OOM kill, a panic) leaves
	// no one's clues or profiles behind on the pod.
	Dir string
}

// Lines collects JSON lines in a gzipped temporary file, so a file larger
// than memory still uploads as one object, and digests what it uploads.
type Lines struct {
	file *os.File
	gz   *gzip.Writer
	buf  *bufio.Writer
	sum  hash.Hash
}

// Create starts a file of JSON lines.
func (f *Files) Create() (*Lines, error) {
	file, err := f.temp("foundry-enrichment-*.jsonl.gz")
	if err != nil {
		return nil, fmt.Errorf("create enrichment file: %w", err)
	}
	sum := sha256.New()
	buf := bufio.NewWriterSize(io.MultiWriter(file, sum), 1<<16)
	return &Lines{file: file, buf: buf, gz: gzip.NewWriter(buf), sum: sum}, nil
}

// temp creates a temporary file in f.Dir, unlinked: it is gone once
// closed, or once the process ends, however it ends.
func (f *Files) temp(pattern string) (*os.File, error) {
	file, err := os.CreateTemp(f.Dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(file.Name()); err != nil {
		_ = file.Close() // best-effort: the error says why
		return nil, err
	}
	return file, nil
}

// Write appends v as one line.
func (w *Lines) Write(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode enrichment line: %w", err)
	}
	_, err = w.gz.Write(append(line, '\n'))
	return err
}

// Upload finishes the file, stores it under key and closes the temporary
// file; the file is the object's bytes, whose SHA-256 it returns.
func (w *Lines) Upload(ctx context.Context, store snapshot.Store, key string) (model.FoundryEnrichmentApplied, error) {
	defer w.Close()
	if err := w.gz.Close(); err != nil {
		return model.FoundryEnrichmentApplied{}, fmt.Errorf("finish %s: %w", key, err)
	}
	if err := w.buf.Flush(); err != nil {
		return model.FoundryEnrichmentApplied{}, fmt.Errorf("flush %s: %w", key, err)
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return model.FoundryEnrichmentApplied{}, fmt.Errorf("rewind %s: %w", key, err)
	}
	if err := store.Put(ctx, key, w.file); err != nil {
		return model.FoundryEnrichmentApplied{}, err
	}
	return model.FoundryEnrichmentApplied{Key: key, SHA256: hex.EncodeToString(w.sum.Sum(nil))}, nil
}

// Close closes the temporary file, which goes with it; Upload closes it
// too.
func (w *Lines) Close() {
	_ = w.file.Close() // best-effort: unlinked, the file is gone either way
}

// Put writes every value of values as one file under key.
func Put[T any](ctx context.Context, f *Files, key string, values []T) (model.FoundryEnrichmentApplied, error) {
	w, err := f.Create()
	if err != nil {
		return model.FoundryEnrichmentApplied{}, err
	}
	for _, v := range values {
		if err := w.Write(v); err != nil {
			w.Close()
			return model.FoundryEnrichmentApplied{}, err
		}
	}
	return w.Upload(ctx, f.Store, key)
}

// Read yields the lines of the file under key one at a time, decoded as T;
// snapshot.ErrMissing when there is no such file. It stops at the first
// error, which it yields.
func Read[T any](ctx context.Context, store snapshot.Store, key string) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		body, err := store.Open(ctx, key)
		if err != nil {
			var zero T
			yield(zero, err)
			return
		}
		defer func() { _ = body.Close() }() // best-effort: only read
		decode[T](body, key)(yield)
	}
}

// decode yields the gzipped JSON lines of body one at a time, decoded as T,
// stopping at the first error, which it yields; key names the file in it.
func decode[T any](body io.Reader, key string) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		gz, err := gzip.NewReader(body)
		if err != nil {
			yield(zero, fmt.Errorf("read %s: %w", key, err))
			return
		}
		dec := json.NewDecoder(gz)
		for {
			var v T
			err := dec.Decode(&v)
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(zero, fmt.Errorf("read %s: %w", key, err))
				return
			}
			if !yield(v, nil) {
				return
			}
		}
	}
}

// ReadAll reads the whole file under key.
func ReadAll[T any](ctx context.Context, store snapshot.Store, key string) ([]T, error) {
	var out []T
	for v, err := range Read[T](ctx, store, key) {
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// publication is what publish wrote: the profiles a load applies, slim, and
// the results, with what they hold.
type publication struct {
	profiles model.FoundryEnrichmentApplied
	results  model.FoundryEnrichmentApplied
	counts   Published
}

// applied is the file a load applies of the publication: none for no rows.
func (p publication) applied() model.FoundryEnrichmentApplied {
	if p.counts.Profiles == 0 {
		return model.FoundryEnrichmentNone
	}
	return p.profiles
}

// publish joins the plan under planKey with fresh (see Publish), writing the
// accepted rows, slim, under profilesKey and, unless resultsKey is empty,
// every result under resultsKey. The graph worker publishes a plan's carried
// profiles this way, the enrichment worker the step's.
func (f *Files) publish(ctx context.Context, item *ontologyagent.Enrichment, planKey string, fresh func() iter.Seq2[Fresh, error],
	profilesKey, resultsKey string) (publication, error) {
	profiles, err := f.Create()
	if err != nil {
		return publication{}, err
	}
	defer profiles.Close()
	var results *Lines
	if resultsKey != "" {
		if results, err = f.Create(); err != nil {
			return publication{}, err
		}
		defer results.Close()
	}
	plan := func() iter.Seq2[Entry, error] { return Read[Entry](ctx, f.Store, planKey) }
	pub, err := Publish(plan, fresh, func(r Result) error {
		if results != nil {
			if err := results.Write(r); err != nil {
				return err
			}
		}
		if r.Outcome != Accepted {
			return nil
		}
		if r.Row == nil {
			// Not named: a profile id may hold a LinkedIn slug, and errors are logged.
			return errors.New("an accepted result has no profile row")
		}
		slim, err := Slim(r.Row, item)
		if err != nil {
			return err
		}
		return profiles.Write(slim)
	})
	if err != nil {
		return publication{}, fmt.Errorf("publish: %w", err)
	}
	out := publication{counts: pub}
	if results != nil {
		if out.results, err = results.Upload(ctx, f.Store, resultsKey); err != nil {
			return publication{}, err
		}
	}
	if out.profiles, err = profiles.Upload(ctx, f.Store, profilesKey); err != nil {
		return publication{}, err
	}
	return out, nil
}

// Stage writes the file a load applies, decompressed, to path, as the
// engine reads it: the zero-byte file for none. The object's bytes must
// still have the digest they had when the load chose them; a retry of the
// load then stages the very same profiles.
func Stage(ctx context.Context, store snapshot.Store, applied model.FoundryEnrichmentApplied, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	if applied.Key == "" {
		return out.Close()
	}
	err = stage(ctx, store, applied, out)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path) // best-effort: the next attempt writes it again
	}
	return err
}

func stage(ctx context.Context, store snapshot.Store, applied model.FoundryEnrichmentApplied, out io.Writer) error {
	body, err := store.Open(ctx, applied.Key)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	sum := sha256.New()
	digested := io.TeeReader(body, sum)
	gz, err := gzip.NewReader(digested)
	if err != nil {
		return fmt.Errorf("read %s: %w", applied.Key, err)
	}
	if _, err := io.Copy(out, gz); err != nil {
		return fmt.Errorf("read %s: %w", applied.Key, err)
	}
	if _, err := io.Copy(io.Discard, digested); err != nil { // bytes past the gzip stream, if any, are digested too
		return fmt.Errorf("read %s: %w", applied.Key, err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != applied.SHA256 {
		return fmt.Errorf("%s changed since the load chose it: SHA-256 %s, not %s", applied.Key, got, applied.SHA256)
	}
	return nil
}

// Digest is the SHA-256 of the object under key, for a file the load
// applies that another writer uploaded.
func Digest(ctx context.Context, store snapshot.Store, key string) (model.FoundryEnrichmentApplied, error) {
	body, err := store.Open(ctx, key)
	if err != nil {
		return model.FoundryEnrichmentApplied{}, err
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	sum := sha256.New()
	if _, err := io.Copy(sum, body); err != nil {
		return model.FoundryEnrichmentApplied{}, fmt.Errorf("read %s: %w", key, err)
	}
	return model.FoundryEnrichmentApplied{Key: key, SHA256: hex.EncodeToString(sum.Sum(nil))}, nil
}
