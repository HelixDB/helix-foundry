package boost

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// store records what a Booster writes and reads.
type store struct {
	acquireErr error
	releaseErr error
	acquired   []model.FoundryLoadBoost
	released   []model.FoundryLoadBoost
	// synced is the revision the cluster reports; syncReads counts reads.
	synced    uint64
	syncReads int
	sweeps    []int
	sweepErr  error
	swept     int
}

func (s *store) AcquireLoadBoost(_ context.Context, workerID string, b model.FoundryLoadBoost) (ports.BoostChange, error) {
	if workerID != "w1" {
		return ports.BoostChange{}, errors.New("wrong worker")
	}
	s.acquired = append(s.acquired, b)
	return ports.BoostChange{ClusterID: "c1", Revision: 3}, s.acquireErr
}

func (s *store) ReleaseLoadBoost(_ context.Context, b model.FoundryLoadBoost) error {
	s.released = append(s.released, b)
	return s.releaseErr
}

func (s *store) SyncedRevision(context.Context, string) (uint64, error) {
	s.syncReads++
	return s.synced, nil
}

func (s *store) SweepLoadBoosts(context.Context, int) (int, error) {
	s.swept++
	if s.sweepErr != nil {
		return 0, s.sweepErr
	}
	if len(s.sweeps) == 0 {
		return 0, nil
	}
	n := s.sweeps[0]
	s.sweeps = s.sweeps[1:]
	return n, nil
}

var question = Attempt{Holder: model.FoundryBoostQuestion, ID: "q1", Number: 2, TenantID: "t1", Timeout: 2 * time.Minute}

func booster(s *store, syncWait time.Duration) *Booster {
	return &Booster{Boosts: s, WorkerID: "w1", Limit: config.GraphLoadRateLimit{RequestsPerSecond: 50, Burst: 100},
		SyncWait: syncWait, SyncPoll: time.Millisecond}
}

func TestAcquire(t *testing.T) {
	// The boost names the holder's work and attempt and lapses Margin after
	// the attempt's own timeout; the release takes back exactly it.
	t.Run("writes the attempt's boost and takes it back", func(t *testing.T) {
		s := &store{synced: 3}
		release, err := booster(s, time.Minute).Acquire(context.Background(), question)
		if err != nil {
			t.Fatal(err)
		}
		want := model.FoundryLoadBoost{TenantID: "t1", Holder: model.FoundryBoostQuestion, JobID: "q1", Attempt: 2,
			RequestsPerSecond: 50, Burst: 100, TTL: 2*time.Minute + Margin}
		if len(s.acquired) != 1 || s.acquired[0] != want || s.syncReads != 1 {
			t.Fatalf("acquired %+v after %d sync reads", s.acquired, s.syncReads)
		}
		release()
		if len(s.released) != 1 || s.released[0] != want {
			t.Fatalf("released %+v", s.released)
		}
	})

	// A question doesn't wait for the gateway: the proxy's resends cover the lag.
	t.Run("doesn't wait for the sync without a SyncWait", func(t *testing.T) {
		s := &store{}
		if _, err := booster(s, 0).Acquire(context.Background(), question); err != nil || s.syncReads != 0 {
			t.Fatalf("err %v after %d sync reads", err, s.syncReads)
		}
	})

	// A sync that never comes costs the boost's confirmation, not the attempt.
	t.Run("gives up waiting after SyncWait", func(t *testing.T) {
		s := &store{synced: 2}
		if _, err := booster(s, 20*time.Millisecond).Acquire(context.Background(), question); err != nil || s.syncReads < 2 {
			t.Fatalf("err %v after %d sync reads", err, s.syncReads)
		}
	})

	// A staff override holds the database: nothing to take back.
	t.Run("runs at a limit held elsewhere", func(t *testing.T) {
		s := &store{acquireErr: foundrystore.ErrConflict}
		release, err := booster(s, time.Minute).Acquire(context.Background(), question)
		if err != nil {
			t.Fatal(err)
		}
		release()
		if len(s.released) != 0 || s.syncReads != 0 {
			t.Fatalf("released %+v after %d sync reads", s.released, s.syncReads)
		}
	})

	t.Run("reports an attempt that lost its work", func(t *testing.T) {
		s := &store{acquireErr: foundrystore.ErrNotFound}
		if _, err := booster(s, time.Minute).Acquire(context.Background(), question); !errors.Is(err, foundrystore.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})

	// A write that failed may have committed: the release takes back whatever landed.
	t.Run("runs unboosted when the write fails, and still releases", func(t *testing.T) {
		s := &store{acquireErr: errors.New("connection refused"), releaseErr: foundrystore.ErrNotFound}
		release, err := booster(s, time.Minute).Acquire(context.Background(), question)
		if err != nil {
			t.Fatal(err)
		}
		release()
		if len(s.released) != 1 || s.syncReads != 0 {
			t.Fatalf("released %+v after %d sync reads", s.released, s.syncReads)
		}
	})
}

func TestSweep(t *testing.T) {
	t.Run("sweeps until a batch comes back short", func(t *testing.T) {
		s := &store{sweeps: []int{SweepLimit, SweepLimit, 3}}
		Sweep(context.Background(), s)
		if s.swept != 3 {
			t.Fatalf("swept %d times, want 3", s.swept)
		}
	})
	t.Run("stops on an error", func(t *testing.T) {
		s := &store{sweepErr: errors.New("connection refused")}
		Sweep(context.Background(), s)
		if s.swept != 1 {
			t.Fatalf("swept %d times, want 1", s.swept)
		}
	})
}
