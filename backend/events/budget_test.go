package events

import "testing"

func TestReadBudgetCut(t *testing.T) {
	batches := func(pieces ...int) []Batch {
		out := make([]Batch, len(pieces))
		for i, n := range pieces {
			out[i] = Batch{Seq: int64(i + 1), Pieces: n, Bytes: int64(100 * n)}
		}
		return out
	}

	// A backlog within every bound is read in one sync.
	t.Run("takes every batch within the budget", func(t *testing.T) {
		got := ReadBudget{Batches: 3, Pieces: 6, Bytes: 600}.Cut(Schema{}, batches(1, 2, 3))
		if len(got) != 3 {
			t.Fatalf("took %d", len(got))
		}
	})

	// Each bound cuts the read on its own, so one sync's memory stays
	// bounded however the backlog is shaped; the rest waits for the next.
	t.Run("stops at the batch count", func(t *testing.T) {
		got := ReadBudget{Batches: 2, Pieces: 1000, Bytes: 1 << 30}.Cut(Schema{}, batches(1, 1, 1))
		if len(got) != 2 || got[1].Seq != 2 {
			t.Fatalf("took %v", got)
		}
	})
	t.Run("stops at the piece count", func(t *testing.T) {
		got := ReadBudget{Batches: 100, Pieces: 5, Bytes: 1 << 30}.Cut(Schema{}, batches(2, 3, 1))
		if len(got) != 2 {
			t.Fatalf("took %v", got)
		}
	})

	// The byte estimate counts what a merge holds per piece beyond the
	// batch's bytes: the null fill of every field and a parsed map entry
	// per field.
	t.Run("stops at the estimated bytes, padding and parsed fields included", func(t *testing.T) {
		s := schemaOf(map[string]Type{"a": TypeNumber, "bb": TypeString})
		// ,"a":null and ,"bb":null are 9 and 10 bytes; two fields parse to 128.
		perPiece := int64(9 + 10 + 2*parsedFieldBytes)
		two := 100*1 + perPiece*1 + 100*2 + perPiece*2
		if got := (ReadBudget{Batches: 100, Pieces: 1000, Bytes: two}).Cut(s, batches(1, 2, 1)); len(got) != 2 {
			t.Fatalf("at the estimate took %d", len(got))
		}
		if got := (ReadBudget{Batches: 100, Pieces: 1000, Bytes: two - 1}).Cut(s, batches(1, 2, 1)); len(got) != 1 {
			t.Fatalf("a byte under took %d", len(got))
		}
	})

	// A batch over the budget alone must still sync, or the source would
	// stall behind it for good.
	t.Run("always takes the first batch", func(t *testing.T) {
		got := ReadBudget{Batches: 1, Pieces: 1, Bytes: 1}.Cut(Schema{}, batches(500, 1))
		if len(got) != 1 || got[0].Seq != 1 {
			t.Fatalf("took %v", got)
		}
	})

	// Nothing waiting is nothing to read.
	t.Run("takes nothing from nothing", func(t *testing.T) {
		if got := (ReadBudget{Batches: 1, Pieces: 1, Bytes: 1}).Cut(Schema{}, nil); len(got) != 0 {
			t.Fatalf("took %v", got)
		}
	})
}

func TestReadBudgetBacklog(t *testing.T) {
	waiting := func(pieces ...int) []Batch {
		out := make([]Batch, len(pieces))
		for i, n := range pieces {
			out[i] = Batch{Seq: int64(i + 1), Pieces: n, Bytes: int64(100 * n)}
		}
		return out
	}
	budget := ReadBudget{Batches: 2, Pieces: 4, Bytes: 1 << 20}

	// The scheduler syncs again after the spacing only while a read can take
	// what waits, and at once when it cannot, so a backlog never waits a
	// spacing per budget's worth.
	for _, c := range []struct {
		name    string
		waiting []Batch
		want    Backlog
	}{
		{"nothing waits", nil, BacklogNone},
		{"one read takes it", waiting(1, 3), BacklogSome},
		{"more batches than one read takes", waiting(1, 1, 1), BacklogMore},
		{"more pieces than one read takes", waiting(1, 4), BacklogMore},
		{"one batch over the budget alone", waiting(9), BacklogSome},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := budget.Backlog(Schema{}, c.waiting); got != c.want {
				t.Fatalf("Backlog = %d, want %d", got, c.want)
			}
		})
	}
}
