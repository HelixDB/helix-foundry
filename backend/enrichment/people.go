package enrichment

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"iter"
	"math"
	"os"
	"slices"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// PersonBytes bounds the memory People hold per person: where their line
// starts (8 bytes), their state (1), and an index entry for the email (16)
// and the slug (16) a lookup would send, plus the planner's offset of a
// carried not_found (8). Slices grow by a quarter past it at most.
const PersonBytes = 49

// People are a load's people as the engine's Clues sends them, in entity
// order, kept for Planner.Plan. Each person's clues go to one JSON line of
// an unlinked temporary file in Files.Dir (gone however the worker ends);
// memory holds only fixed-size words per person (PersonBytes), whatever
// their clues hold, and max bounds how many people there are. Plan finds
// people by their clues' hashes and reads their lines back by offset: it
// compares clues by their value, never only by their hash.
type People struct {
	// Shared is how many lost a clue value another entity holds too, as
	// Clues counted them: set once Clues ends.
	Shared int64

	file *os.File
	w    *bufio.Writer
	max  int
	// end is where the next line goes; lines is where the people's lines
	// end, once planned: the not_found results Plan carries follow them.
	end, lines int64
	// at[i] is where person i's line starts; state[i] what the filters left
	// them and what Plan made of them.
	at    []int64
	state []uint8
	// emails and slugs are the people who send an email or a slug, by the
	// value's hash; Plan sorts them by hash, then person.
	emails, slugs []keyed
	hash          func(string) uint64
	counts        PlanCounts
	settled       bool
	buf           []byte
}

// keyed is a person who sends a value, by its hash.
type keyed struct {
	hash   uint64
	person uint32
}

// A person's state bits.
const (
	// sendsEmail: the filters left them an email.
	sendsEmail uint8 = 1 << iota
	// sharedSlug: another person sends the same slug, so neither does.
	sharedSlug
	// carriedFinal: Plan has emitted them carrying a result.
	carriedFinal
)

// People starts a load's people, at most max of them: Add past max is an
// error, which fails the step (the load goes on with the newest profiles).
// max is at most math.MaxUint32.
func (f *Files) People(max int) (*People, error) {
	if max < 1 || int64(max) > math.MaxUint32 {
		return nil, fmt.Errorf("plan at most %d people: it must be 1 to %d", max, math.MaxUint32)
	}
	file, err := f.temp("foundry-enrichment-people-*.jsonl")
	if err != nil {
		return nil, fmt.Errorf("create people file: %w", err)
	}
	seed := maphash.MakeSeed()
	return &People{
		file: file, w: bufio.NewWriterSize(file, 1<<16), max: max,
		hash: func(s string) uint64 { return maphash.String(seed, s) },
	}, nil
}

// Add takes the next person Clues sent. It runs the filters that need no
// one else (Planner.Plan's 1 to 4), and indexes the clues a lookup would
// send. Its errors name the person by position only: an entity's key may be
// their email, and errors end up in the logs, which hold no personal data.
func (p *People) Add(c Clue) error {
	if p.settled {
		return errors.New("a person added after the plan")
	}
	if len(p.at) == p.max {
		return fmt.Errorf("more than %d people to plan (foundry.enrichment.max_people, which bounds the step's memory)", p.max)
	}
	line, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode person %d: %w", len(p.at), err)
	}
	if _, err := p.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write person %d: %w", len(p.at), err)
	}
	person, e := uint32(len(p.at)), filter(c)
	var state uint8
	switch {
	case e.Action == ActionSkip:
		p.counts.NoName++
	case e.Sent == Sent{}:
		p.counts.NoClue++
	}
	if e.Sent.Email != "" {
		state |= sendsEmail
		p.emails = append(p.emails, keyed{hash: p.hash(e.Sent.Email), person: person})
	}
	if e.Sent.Slug != "" {
		p.slugs = append(p.slugs, keyed{hash: p.hash(e.Sent.Slug), person: person})
	}
	p.at = append(p.at, p.end)
	p.state = append(p.state, state)
	p.end += int64(len(line)) + 1
	p.counts.People++
	return nil
}

// Close closes the file, which goes with it (it is unlinked); a nil People
// is none.
func (p *People) Close() {
	if p != nil {
		_ = p.file.Close() // best-effort: unlinked, the file is gone either way
	}
}

// Counts are the step's counts once planned: every entity Clues sent is
// skipped (no full name, or no clue left), carried or looked up, and
// Profiles is the accepted rows carried.
func (p *People) Counts(planned PlanCounts, carriedProfiles int64) model.FoundryEnrichmentCounts {
	return model.FoundryEnrichmentCounts{
		People: planned.People, NoName: planned.NoName, NoClue: planned.NoClue, Shared: p.Shared + planned.Shared,
		Skipped: planned.NoName + planned.NoClue, Carried: planned.Carried, ToLookUp: planned.Call, Profiles: carriedProfiles,
	}
}

// settle ends the adding: it sorts the indexes and finds the slugs two
// people send (filter 5), reading back only people whose slugs' hashes
// collide, to compare the slugs themselves.
func (p *People) settle() error {
	if p.settled {
		return fmt.Errorf("people planned twice")
	}
	p.settled = true
	if err := p.w.Flush(); err != nil {
		return fmt.Errorf("write people: %w", err)
	}
	p.lines = p.end
	byHash := func(a, b keyed) int { return cmp.Or(cmp.Compare(a.hash, b.hash), cmp.Compare(a.person, b.person)) }
	slices.SortFunc(p.emails, byHash)
	slices.SortFunc(p.slugs, byHash)
	for run := range runs(p.slugs) {
		if len(run) == 1 {
			continue
		}
		slugs := make([]string, len(run))
		sent := map[string]int{}
		for j, k := range run {
			e, err := p.read(k.person)
			if err != nil {
				return err
			}
			slugs[j] = e.Sent.Slug
			sent[slugs[j]]++
		}
		for j, k := range run {
			if sent[slugs[j]] == 1 {
				continue
			}
			p.state[k.person] |= sharedSlug
			p.counts.Shared++
			if p.state[k.person]&sendsEmail == 0 {
				p.counts.NoClue++
			}
		}
	}
	return nil
}

// forget lets go of the people's memory once planned: what follows the
// plan (publishing the carried profiles) holds its own.
func (p *People) forget() {
	p.at, p.state, p.emails, p.slugs, p.buf = nil, nil, nil, nil, nil
}

// runs yields keys' runs of one hash, keys sorted by hash.
func runs(keys []keyed) iter.Seq[[]keyed] {
	return func(yield func([]keyed) bool) {
		for start := 0; start < len(keys); {
			end := start + 1
			for end < len(keys) && keys[end].hash == keys[start].hash {
				end++
			}
			if !yield(keys[start:end]) {
				return
			}
			start = end
		}
	}
}

// match hands each, once, every person not yet carried who sends sent's
// email or its slug, read back as planned.
func (p *People) match(sent Sent, each func(person uint32, e Entry) error) error {
	if sent.Email != "" {
		for _, k := range p.lookup(p.emails, sent.Email) {
			if p.state[k.person]&carriedFinal != 0 {
				continue
			}
			e, err := p.read(k.person)
			switch {
			case err != nil:
				return err
			case e.Sent.Email != sent.Email: // another email of the same hash
				continue
			}
			if err := each(k.person, e); err != nil {
				return err
			}
		}
	}
	if sent.Slug == "" {
		return nil
	}
	for _, k := range p.lookup(p.slugs, sent.Slug) {
		if p.state[k.person]&(carriedFinal|sharedSlug) != 0 {
			continue
		}
		e, err := p.read(k.person)
		switch {
		case err != nil:
			return err
		case e.Sent.Slug != sent.Slug: // another slug of the same hash
			continue
		case sent.Email != "" && e.Sent.Email == sent.Email: // handed on by its email
			continue
		}
		if err := each(k.person, e); err != nil {
			return err
		}
	}
	return nil
}

// lookup is the run of keys, sorted, with value's hash.
func (p *People) lookup(keys []keyed, value string) []keyed {
	h := p.hash(value)
	start, _ := slices.BinarySearchFunc(keys, h, func(k keyed, h uint64) int { return cmp.Compare(k.hash, h) })
	end := start
	for end < len(keys) && keys[end].hash == h {
		end++
	}
	return keys[start:end]
}

// read reads person back from their line, as planned.
func (p *People) read(person uint32) (Entry, error) {
	start, end := p.at[person], p.lines
	if int(person)+1 < len(p.at) {
		end = p.at[person+1]
	}
	p.buf = slices.Grow(p.buf[:0], int(end-start))[:end-start]
	if _, err := p.file.ReadAt(p.buf, start); err != nil {
		return Entry{}, fmt.Errorf("read person %d: %w", person, err)
	}
	var c Clue
	if err := json.Unmarshal(p.buf, &c); err != nil {
		return Entry{}, fmt.Errorf("read person %d: %w", person, err)
	}
	return p.entry(person, c), nil
}

// each hands each every person in entity order, as planned.
func (p *People) each(each func(person uint32, e Entry) error) error {
	dec := json.NewDecoder(bufio.NewReaderSize(io.NewSectionReader(p.file, 0, p.lines), 1<<16))
	for person := range uint32(len(p.at)) {
		var c Clue
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("read person %d: %w", person, err)
		}
		if err := each(person, p.entry(person, c)); err != nil {
			return err
		}
	}
	return nil
}

// entry is c, person's clues, through every filter.
func (p *People) entry(person uint32, c Clue) Entry {
	e := filter(c)
	if e.Action == ActionSkip {
		return e
	}
	if p.state[person]&sharedSlug != 0 {
		e.Sent.Slug, e.Reason = "", ReasonSharedSlug
	}
	if e.Sent == (Sent{}) {
		e.Action, e.Reason = ActionSkip, cmp.Or(e.Reason, ReasonNoClue)
	}
	return e
}

// keep writes a not_found result Plan may carry after the people's lines,
// and returns where it starts.
func (p *People) keep(r Result) (int64, error) {
	line, err := json.Marshal(r)
	if err != nil {
		return 0, fmt.Errorf("encode a carried result: %w", err)
	}
	if _, err := p.w.Write(append(line, '\n')); err != nil {
		return 0, fmt.Errorf("write a carried result: %w", err)
	}
	at := p.end
	p.end += int64(len(line)) + 1
	return at, nil
}

// kept reads back the result keep wrote at at; flush first.
func (p *People) kept(at int64) (Result, error) {
	var r Result
	if err := json.NewDecoder(io.NewSectionReader(p.file, at, p.end-at)).Decode(&r); err != nil {
		return Result{}, fmt.Errorf("read a carried result: %w", err)
	}
	return r, nil
}
