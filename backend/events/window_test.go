package events

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

var windowEpoch = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)

// kept is a piece of the window tests, at seconds past windowEpoch.
func kept(seconds int, id string) Kept[int] {
	return Kept[int]{ReceivedAt: windowEpoch.Add(time.Duration(seconds) * time.Second), ID: id}
}

func TestNewer(t *testing.T) {
	// The window and the set's documents keep the same newest pieces only if
	// they order them the same way: by arrival, newest first, and on a tie
	// by id, lowest first, so the order is total and never depends on the
	// order a read meets them in.
	for _, c := range []struct {
		name string
		a, b Kept[int]
		want bool
	}{
		{"a later arrival", kept(2, "b"), kept(1, "a"), true},
		{"an earlier arrival", kept(1, "a"), kept(2, "b"), false},
		{"a tie, the lower id", kept(1, "a"), kept(1, "b"), true},
		{"a tie, the higher id", kept(1, "b"), kept(1, "a"), false},
		{"a tie by byte order", kept(1, "B"), kept(1, "a"), true},
		{"the same piece", kept(1, "a"), kept(1, "a"), false},
		{"a microsecond later", Kept[int]{ReceivedAt: windowEpoch.Add(time.Microsecond), ID: "z"}, kept(0, "a"), true},
		{"the same instant in another zone", Kept[int]{ReceivedAt: windowEpoch.In(time.FixedZone("+02", 2*60*60)), ID: "b"}, kept(0, "a"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Newer(c.a, c.b); got != c.want {
				t.Fatalf("Newer(%v, %v) = %v", c.a, c.b, got)
			}
		})
	}
}

func TestNewest(t *testing.T) {
	ids := func(pieces []Kept[int]) string {
		var out []string
		for _, p := range pieces {
			out = append(out, p.ID)
		}
		return strings.Join(out, " ")
	}

	// The set holds the newest n pieces: the window keeps exactly those,
	// whatever order the read offers them in, and hands them over newest
	// first, so the writer's first-n rule keeps the same ones.
	t.Run("keeps exactly the newest n, newest first", func(t *testing.T) {
		w := Newest[int](3)
		for _, p := range []Kept[int]{kept(5, "e"), kept(1, "a"), kept(9, "i"), kept(3, "c"), kept(7, "g"), kept(2, "b")} {
			w.Push(p.ReceivedAt, p.ID, 0)
		}
		if got := ids(w.Pieces()); got != "i g e" {
			t.Fatalf("kept %s", got)
		}
	})

	// Pieces sent in one batch share their received_at: the lower ids win
	// the last places, so two reads of the same pieces keep the same ones.
	t.Run("breaks ties on arrival by the lower id", func(t *testing.T) {
		w := Newest[int](3)
		for _, id := range []string{"d", "b", "e", "a", "c"} {
			w.Push(windowEpoch, id, 0)
		}
		w.Push(windowEpoch.Add(-time.Second), "0", 0)
		if got := ids(w.Pieces()); got != "a b c" {
			t.Fatalf("kept %s", got)
		}
	})

	t.Run("keeps every piece of a read of fewer than n", func(t *testing.T) {
		w := Newest[int](500)
		if got := w.Pieces(); len(got) != 0 {
			t.Fatalf("an empty window kept %v", got)
		}
		w.Push(windowEpoch, "a", 0)
		w.Push(windowEpoch.Add(time.Second), "b", 0)
		if got := ids(w.Pieces()); got != "b a" {
			t.Fatalf("kept %s", got)
		}
	})

	t.Run("keeps one piece", func(t *testing.T) {
		w := Newest[int](1)
		for i := range 10 {
			w.Push(windowEpoch.Add(time.Duration(i%4)*time.Second), fmt.Sprint(i), i)
		}
		if got := w.Pieces(); len(got) != 1 || got[0].ID != "3" || got[0].Loc != 3 {
			t.Fatalf("kept %v", got)
		}
	})

	// A read re-reads the pieces it keeps, so each must come back with the
	// place it was offered at.
	t.Run("keeps each piece's location", func(t *testing.T) {
		type line struct {
			batch int64
			line  int
		}
		w := Newest[line](2)
		w.Push(windowEpoch, "a", line{7, 0})
		w.Push(windowEpoch.Add(time.Second), "b", line{7, 1})
		w.Push(windowEpoch.Add(2*time.Second), "c", line{9, 4})
		got := w.Pieces()
		if len(got) != 2 || got[0] != (Kept[line]{windowEpoch.Add(2 * time.Second), "c", line{9, 4}}) || got[1].Loc != (line{7, 1}) {
			t.Fatalf("kept %v", got)
		}
		names := Newest[string](1)
		names.Push(windowEpoch, "a", "snapshot line 12")
		if got := names.Pieces(); got[0].Loc != "snapshot line 12" {
			t.Fatalf("kept %v", got)
		}
	})

	// Pieces hands over a copy: the window keeps what it kept, and a read
	// that asks twice gets the same pieces.
	t.Run("is left as it was by Pieces", func(t *testing.T) {
		w := Newest[int](2)
		w.Push(windowEpoch, "a", 0)
		w.Push(windowEpoch.Add(time.Second), "b", 0)
		first := w.Pieces()
		first[0].ID = "changed"
		if got := ids(w.Pieces()); got != "b a" {
			t.Fatalf("kept %s", got)
		}
		w.Push(windowEpoch.Add(2*time.Second), "c", 0)
		if got := ids(w.Pieces()); got != "c b" {
			t.Fatalf("kept %s", got)
		}
	})

	// The window is checked against sorting every piece offered, over reads
	// of many sizes with many ties: it must never drop a piece of the newest
	// n or keep one outside them.
	t.Run("agrees with sorting every piece", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(1, 2))
		for range 300 {
			n, offered := 1+rng.IntN(20), rng.IntN(60)
			all := make([]Kept[int], offered)
			for i := range all {
				all[i] = Kept[int]{ReceivedAt: windowEpoch.Add(time.Duration(rng.IntN(10)) * time.Second), ID: fmt.Sprintf("p%03d", i), Loc: i}
			}
			w := Newest[int](n)
			for _, p := range all {
				w.Push(p.ReceivedAt, p.ID, p.Loc)
			}
			slices.SortFunc(all, func(a, b Kept[int]) int {
				switch {
				case Newer(a, b):
					return -1
				case Newer(b, a):
					return 1
				}
				return 0
			})
			if want := all[:min(n, offered)]; !slices.Equal(w.Pieces(), want) {
				t.Fatalf("n %d: kept %v, want %v", n, w.Pieces(), want)
			}
		}
	})

	// A full read offers every row of a table of up to 2M pieces: once the
	// window is full, an offer costs no allocation, so the read's memory is
	// the window's n pieces whatever the table holds.
	t.Run("allocates nothing once full", func(t *testing.T) {
		w := Newest[int](3)
		for i := range 3 {
			w.Push(windowEpoch.Add(time.Duration(i)*time.Hour), fmt.Sprint("p", i), i)
		}
		older, newer := windowEpoch.Add(-time.Hour), windowEpoch.Add(time.Hour)
		if a := testing.AllocsPerRun(100, func() { w.Push(older, "q", 0) }); a != 0 {
			t.Errorf("an older offer allocates %v times", a)
		}
		if a := testing.AllocsPerRun(100, func() { newer = newer.Add(time.Second); w.Push(newer, "q", 0) }); a != 0 {
			t.Errorf("a newer offer allocates %v times", a)
		}
	})

	// A window of nothing is a caller's bug (config keeps max_extracted at
	// least 1): it fails at once rather than keeping nothing.
	for _, n := range []int{0, -1} {
		t.Run(fmt.Sprint("refuses a window of ", n), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			Newest[int](n)
		})
	}
}
