package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// usageStore answers the month's three sums for ws-1 and none for any other
// workspace, recording what each was asked; failing names the sum that
// fails.
type usageStore struct {
	foundrystore.Unimplemented
	loads     int64
	sources   int64
	questions foundrymodel.FoundryQuestionSpend
	failing   string
	asked     []string
}

func (s *usageStore) ask(sum, ws string, since time.Time) error {
	s.asked = append(s.asked, sum+" "+ws+" "+since.Format(time.RFC3339Nano))
	if s.failing == sum {
		return errors.New("database unavailable")
	}
	return nil
}

func (s *usageStore) SumFoundryGraphJobSpend(_ context.Context, ws string, since time.Time) (int64, error) {
	if ws != "ws-1" {
		return 0, s.ask("loads", ws, since)
	}
	return s.loads, s.ask("loads", ws, since)
}

func (s *usageStore) SumFoundryDocumentSetSpend(_ context.Context, ws, kind string, month time.Time) (int64, error) {
	if ws != "ws-1" || kind != foundrymodel.ConnectorKindEvents {
		return 0, s.ask("sources:"+kind, ws, month)
	}
	return s.sources, s.ask("sources:"+kind, ws, month)
}

func (s *usageStore) SumFoundryQuestionSpend(_ context.Context, ws string, since time.Time) (foundrymodel.FoundryQuestionSpend, error) {
	if ws != "ws-1" {
		return foundrymodel.FoundryQuestionSpend{}, s.ask("questions", ws, since)
	}
	return s.questions, s.ask("questions", ws, since)
}

// usageService has prod's caps: $50 of loads, $20 of API sources' work no
// person started, $15 of questions.
func usageService(store *usageStore) *Service {
	cfg := config.Defaults()
	cfg.Foundry.Graph.MaxJobUSD, cfg.Foundry.Graph.MonthlyUSD = 10, 50
	cfg.Foundry.Events.MaxSystemUSDPerWorkspaceMonth = 20
	cfg.Foundry.Ask.MonthlyUSD = 15
	return &Service{deps: &runtimeDeps{DB: store, Cfg: &cfg}}
}

func TestGetFoundryUsage(t *testing.T) {
	ctx := context.Background()
	monthOf := func(now time.Time) time.Time {
		now = now.UTC()
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	entry := func(task foundryv1.FoundryUsageTask, spent, reserved, capMicros int64) string {
		return fmt.Sprintf("%s spent=%d reserved=%d cap=%d", task, spent, reserved, capMicros)
	}
	entries := func(u *foundryv1.FoundryUsage) []string {
		var out []string
		for _, e := range u.GetTasks() {
			if e.CapUsdMicros == nil {
				t.Fatalf("%s has no cap", e.GetTask())
			}
			out = append(out, entry(e.GetTask(), e.GetSpentUsdMicros(), e.GetReservedUsdMicros(), e.GetCapUsdMicros()))
		}
		return out
	}
	equal := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %q, want %q", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %q, want %q", got, want)
			}
		}
	}

	// Every task's month and cap, in the enum's order; the total counts
	// API sources once (inside loads) and leaves questions' reservations
	// out. Every sum reads the same UTC month, from its first instant.
	t.Run("reads each task's month with its cap", func(t *testing.T) {
		store := &usageStore{loads: 12_400_000, sources: 1_200_000, questions: foundrymodel.FoundryQuestionSpend{SpentMicros: 4_800_000, ReservedMicros: 500_000}}
		before := monthOf(time.Now())
		out, err := usageService(store).GetFoundryUsage(ctx, &foundryv1.GetFoundryUsageRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		u := out.GetUsage()
		start := u.GetMonthStart().AsTime()
		if !start.Equal(before) && !start.Equal(monthOf(time.Now())) {
			t.Fatalf("month_start = %v", start)
		}
		if !u.GetResetsAt().AsTime().Equal(start.AddDate(0, 1, 0)) || u.GetTotalUsdMicros() != 17_200_000 {
			t.Fatalf("usage = %+v", u)
		}
		equal(t, entries(u), []string{
			entry(foundryv1.FoundryUsageTask_FOUNDRY_USAGE_TASK_LOADS, 12_400_000, 0, 50_000_000),
			entry(foundryv1.FoundryUsageTask_FOUNDRY_USAGE_TASK_API_SOURCES, 1_200_000, 0, 20_000_000),
			entry(foundryv1.FoundryUsageTask_FOUNDRY_USAGE_TASK_QUESTIONS, 4_800_000, 500_000, 15_000_000),
		})
		since := start.Format(time.RFC3339Nano)
		equal(t, store.asked, []string{"loads ws-1 " + since, "sources:events ws-1 " + since, "questions ws-1 " + since})
	})

	// A month with no spend reads as zero everywhere, caps still set.
	t.Run("reads an empty month", func(t *testing.T) {
		out, err := usageService(&usageStore{}).GetFoundryUsage(ctx, &foundryv1.GetFoundryUsageRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		if out.GetUsage().GetTotalUsdMicros() != 0 {
			t.Fatalf("total = %d", out.GetUsage().GetTotalUsdMicros())
		}
		equal(t, entries(out.GetUsage()), []string{
			entry(foundryv1.FoundryUsageTask_FOUNDRY_USAGE_TASK_LOADS, 0, 0, 50_000_000),
			entry(foundryv1.FoundryUsageTask_FOUNDRY_USAGE_TASK_API_SOURCES, 0, 0, 20_000_000),
			entry(foundryv1.FoundryUsageTask_FOUNDRY_USAGE_TASK_QUESTIONS, 0, 0, 15_000_000),
		})
	})

	// Each sum is asked of the request's workspace only.
	t.Run("reads only the asked workspace", func(t *testing.T) {
		store := &usageStore{loads: 12_400_000, sources: 1_200_000, questions: foundrymodel.FoundryQuestionSpend{SpentMicros: 4_800_000}}
		out, err := usageService(store).GetFoundryUsage(ctx, &foundryv1.GetFoundryUsageRequest{WorkspaceId: "ws-2"})
		if err != nil || out.GetUsage().GetTotalUsdMicros() != 0 {
			t.Fatalf("usage = (%+v, %v)", out, err)
		}
		since := out.GetUsage().GetMonthStart().AsTime().Format(time.RFC3339Nano)
		equal(t, store.asked, []string{"loads ws-2 " + since, "sources:events ws-2 " + since, "questions ws-2 " + since})
	})

	t.Run("needs a workspace", func(t *testing.T) {
		store := &usageStore{}
		_, err := usageService(store).GetFoundryUsage(ctx, &foundryv1.GetFoundryUsageRequest{})
		if status.Code(err) != codes.InvalidArgument || len(store.asked) != 0 {
			t.Fatalf("err = %v, asked %q", err, store.asked)
		}
	})

	// A sum that fails fails the read, saying which; nothing is guessed.
	t.Run("fails with the sum that failed", func(t *testing.T) {
		for _, sum := range []string{"loads", "sources:events", "questions"} {
			_, err := usageService(&usageStore{failing: sum}).GetFoundryUsage(ctx, &foundryv1.GetFoundryUsageRequest{WorkspaceId: "ws-1"})
			if status.Code(err) != codes.Internal {
				t.Fatalf("%s: err = %v", sum, err)
			}
		}
	})
}
