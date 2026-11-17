package model

import "time"

// FoundryLoadUnread is what a load's documents came to when the model failed
// some of them: how many the model read, how many it failed and why, and
// where Foundry stands in reading those again on its own.
//
// A document the model failed was never read: the engine commits nothing of
// it, so the load holds none of its names or facts, and any later job into
// the load sends it again (it waits, waitingFoundryDocumentWhere). It is
// never counted as read.
type FoundryLoadUnread struct {
	// Read counts the documents a model read into the load (extracted), or
	// found repeating one it holds (duplicate).
	Read int
	// Failed counts the documents the model failed, by the engine's model
	// failure class; "" is one recorded before Foundry kept the class.
	Failed map[string]int
	// Unrecorded counts the load's documents no job has recorded yet. While
	// the model fails them they are unread as well: a run the model stopped
	// (a workspace's own provider refusing its key or quota) leaves them so.
	Unrecorded int
	// Live means a job writes the load now: its counts are still moving.
	Live bool
	// Rereads is how many re-reads Foundry queued on its own
	// (FoundryRereadToken) since the newest job anything else queued on the
	// load, and LastEnd is when the load's newest job ended; nil when none
	// has.
	Rereads int
	LastEnd *time.Time
}

// FoundryRereadToken prefixes the client token of a re-read Foundry queues
// on its own, an extract of the current load's documents the model failed
// (store.QueueFoundryReread), so the load's jobs say how many have run.
const FoundryRereadToken = "reread:"

const (
	// FoundryRereadFirst is how long after the job that left documents unread
	// the first re-read starts; each later one waits twice as long as the
	// last, up to FoundryRereadMost.
	FoundryRereadFirst = 15 * time.Minute
	// FoundryRereadMost is the longest wait between re-reads.
	FoundryRereadMost = 6 * time.Hour
	// FoundryRereadLimit is how many re-reads in a row Foundry queues on its
	// own: about a week, the last 23 six hours apart. Past it, the next job
	// into the load (an upload, a refresh) or a new load reads them.
	FoundryRereadLimit = 28
)

// Failures counts the documents the model failed.
func (u FoundryLoadUnread) Failures() int {
	n := 0
	for _, c := range u.Failed {
		n += c
	}
	return n
}

// Documents is how many of the load's documents went unread because the
// model failed: those it failed, and, once no job writes the load, those no
// job recorded. 0 when the model failed none.
//
//	u := model.FoundryLoadUnread{Failed: map[string]int{"quota": 1}, Unrecorded: 8}
//	u.Documents() // 9
func (u FoundryLoadUnread) Documents() int {
	n := u.Failures()
	if n == 0 {
		return 0
	}
	if !u.Live {
		n += u.Unrecorded
	}
	return n
}

// RereadAt is when Foundry may next read the documents again on its own:
// FoundryRereadFirst after the load's newest job ended, doubling with each
// re-read in a row up to FoundryRereadMost. False while a job writes the
// load, before any job ended, and past FoundryRereadLimit re-reads in a row.
// Whether the failures pass on their own (a quota, a busy provider) is the
// caller's to judge.
func (u FoundryLoadUnread) RereadAt() (time.Time, bool) {
	if u.Live || u.LastEnd == nil || u.Rereads >= FoundryRereadLimit {
		return time.Time{}, false
	}
	wait := FoundryRereadMost
	// 15 min << 5 is 8 h, past the cap: shift no further, so no overflow.
	if u.Rereads < 5 {
		wait = min(FoundryRereadFirst<<u.Rereads, FoundryRereadMost)
	}
	return u.LastEnd.Add(wait), true
}
