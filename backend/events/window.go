package events

import (
	"container/heap"
	"fmt"
	"slices"
	"time"
)

// Kept is a piece a Window keeps: when it arrived, its id, and Loc, where
// the read that offered it can find its line again (a batch and line, or a
// snapshot line). It holds no row, so a window costs the same whatever its
// pieces hold, and the read re-reads the pieces it keeps.
type Kept[L any] struct {
	ReceivedAt time.Time
	ID         string
	Loc        L
}

// Newer reports whether a comes before b among a source's newest pieces:
// it arrived later, or at the same instant with the lower id by byte order.
func Newer[L any](a, b Kept[L]) bool {
	if !a.ReceivedAt.Equal(b.ReceivedAt) {
		return a.ReceivedAt.After(b.ReceivedAt)
	}
	return a.ID < b.ID
}

// Window keeps the newest n pieces a read offers it, by Newer, in memory for
// n pieces however many it is offered, and O(log n) time per piece. A read
// offers each id once, at its newest line: a full read each id's first
// sighting reading newest first, a change read each id's last line.
type Window[L any] struct {
	n int
	// oldest is a heap of the pieces kept, the oldest at its root.
	oldest oldestFirst[L]
}

// Newest is an empty window of the newest n pieces. n must be at least 1.
func Newest[L any](n int) *Window[L] {
	if n < 1 {
		panic(fmt.Sprintf("events: a window keeps at least 1 piece, not %d", n))
	}
	return &Window[L]{n: n, oldest: make(oldestFirst[L], 0, n)}
}

// Push offers a piece: the window keeps it while it is among the newest n it
// has been offered. Once the window is full, an offer older than all it
// keeps allocates nothing.
func (w *Window[L]) Push(receivedAt time.Time, id string, loc L) {
	p := Kept[L]{ReceivedAt: receivedAt, ID: id, Loc: loc}
	switch {
	case len(w.oldest) < w.n:
		heap.Push(&w.oldest, p)
	case Newer(p, w.oldest[0]):
		w.oldest[0] = p
		heap.Fix(&w.oldest, 0)
	}
}

// Pieces are the pieces the window keeps, newest first: at most n. The
// window is left as it was.
func (w *Window[L]) Pieces() []Kept[L] {
	// A copy of the heap pops oldest first, so it fills from the end.
	h := slices.Clone(w.oldest)
	pieces := make([]Kept[L], len(h))
	for i := len(pieces) - 1; i >= 0; i-- {
		pieces[i] = heap.Pop(&h).(Kept[L])
	}
	return pieces
}

// oldestFirst is a heap.Interface whose root is its oldest piece.
type oldestFirst[L any] []Kept[L]

func (h oldestFirst[L]) Len() int           { return len(h) }
func (h oldestFirst[L]) Less(i, j int) bool { return Newer(h[j], h[i]) }
func (h oldestFirst[L]) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *oldestFirst[L]) Push(x any)        { *h = append(*h, x.(Kept[L])) }

func (h *oldestFirst[L]) Pop() any {
	old := *h
	p := old[len(old)-1]
	*h = old[:len(old)-1]
	return p
}
