package events

// Batch is one stored send, as its foundry_event_batches row has it.
type Batch struct {
	ID  string
	Seq int64
	// Key is its object's s3_key.
	Key string
	// Pieces is the row's events, and Bytes its object's uncompressed size.
	Pieces int
	Bytes  int64
}

// parsedFieldBytes is what a change read's parsed field map costs per field
// of a piece, beside the row itself.
const parsedFieldBytes = 64

// ReadBudget bounds one change read of a source (foundry.events
// max_batches_per_sync, max_pieces_per_sync and max_bytes_per_sync). A merge
// holds each piece's row and its parsed fields, so Bytes counts, per batch,
// its bytes plus each piece's null fill and parsedFieldBytes per field.
type ReadBudget struct {
	Batches int
	Pieces  int64
	Bytes   int64
}

// Cut is the longest run of batches, from the first, that one read under
// schema s takes within every bound. It always takes the first batch, so a
// batch larger than the budget alone still syncs: one send is at most
// max_send_bytes, which config keeps far below max_bytes_per_sync.
func (b ReadBudget) Cut(s Schema, batches []Batch) []Batch {
	perPiece := s.NullFill() + parsedFieldBytes*int64(s.Len())
	var pieces, bytes int64
	for i, batch := range batches {
		pieces += int64(batch.Pieces)
		bytes += batch.Bytes + int64(batch.Pieces)*perPiece
		if i > 0 && (i >= b.Batches || pieces > b.Pieces || bytes > b.Bytes) {
			return batches[:i]
		}
	}
	return batches
}

// Backlog is what a source holds beyond its cursor, as one read takes it.
type Backlog int

const (
	// BacklogNone is a source whose every batch is in its table.
	BacklogNone Backlog = iota
	// BacklogSome is a backlog one read takes whole: sync again after the
	// spacing.
	BacklogSome
	// BacklogMore is more than one read takes: sync again at once.
	BacklogMore
)

// Backlog says what of waiting, a source's batches above its cursor oldest
// first, one read under schema s takes. waiting must hold every batch that
// waits, or at least b.Batches+1 of them: a read of b.Batches cannot tell a
// backlog of exactly that many from a longer one.
func (b ReadBudget) Backlog(s Schema, waiting []Batch) Backlog {
	switch {
	case len(waiting) == 0:
		return BacklogNone
	case len(b.Cut(s, waiting)) < len(waiting):
		return BacklogMore
	}
	return BacklogSome
}
