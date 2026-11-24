package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// researchLimits are 3 calls an hour and a $1 month, a call's worst case
// $0.10, the month from a day ago.
func researchLimits() model.FoundryCompanyResearchLimits {
	return model.FoundryCompanyResearchLimits{PerUserPerHour: 3, Since: time.Now().Add(-24 * time.Hour), WorstUSDMicros: 100_000, MonthlyMicros: 1_000_000}
}

func spend(usdMicros int64) model.FoundryCompanyResearchSpend {
	return model.FoundryCompanyResearchSpend{USDMicros: usdMicros}
}

func researchRow(n int, ws, user string, kind model.FoundryCompanyResearchKind) *model.FoundryCompanyResearch {
	return &model.FoundryCompanyResearch{ID: id26(fmt.Sprintf("CR-%03d-", n)), WorkspaceID: ws, UserID: user, Kind: kind, Domain: "acme.com"}
}

// A research starts with no result and its worst case reserved, is finished
// with what it spent and its research, and only then reads; a goal never
// reads, nor another workspace's research.
func TestFoundryCompanyResearchStartFinishGet(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws, other := id26("CW-one"), id26("CW-other")
	createWorkspace(t, ws)
	createWorkspace(t, other)

	research := researchRow(1, ws, "user-ada", model.FoundryCompanyResearchOfSite)
	require.NoError(t, db.StartFoundryCompanyResearch(ctx, research, researchLimits()))
	assert.WithinDuration(t, time.Now(), research.CreatedAt, time.Minute, "the row's time is read back")
	assert.EqualValues(t, 100_000, research.USDMicros, "the row holds the worst case")
	_, err := db.GetFoundryCompanyResearch(ctx, ws, research.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "a research that hasn't ended doesn't read")

	result := json.RawMessage(`{"company_name":"Acme","questions":[{"id":"q1"}]}`)
	require.NoError(t, db.FinishFoundryCompanyResearch(ctx, ws, research.ID, result, spend(1234)))
	got, err := db.GetFoundryCompanyResearch(ctx, ws, research.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryCompanyResearchOfSite, got.Kind)
	assert.Equal(t, "user-ada", got.UserID)
	assert.Equal(t, "acme.com", got.Domain)
	assert.EqualValues(t, 1234, got.USDMicros)
	assert.JSONEq(t, string(result), string(got.Result))
	require.NoError(t, db.FinishFoundryCompanyResearch(ctx, ws, research.ID, result, spend(1234)), "the same finish again changes nothing")

	_, err = db.GetFoundryCompanyResearch(ctx, other, research.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "another workspace's")
	require.ErrorIs(t, db.FinishFoundryCompanyResearch(ctx, other, research.ID, nil, spend(1)), store.ErrNotFound)

	goal := researchRow(2, ws, "user-ada", model.FoundryCompanyResearchGoal)
	require.NoError(t, db.StartFoundryCompanyResearch(ctx, goal, researchLimits()))
	require.NoError(t, db.FinishFoundryCompanyResearch(ctx, ws, goal.ID, nil, spend(99)))
	_, err = db.GetFoundryCompanyResearch(ctx, ws, goal.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "a goal holds no research")
	require.Error(t, db.FinishFoundryCompanyResearch(ctx, ws, goal.ID, result, spend(99)), "a goal can't hold a result")

	failed := researchRow(3, ws, "user-ada", model.FoundryCompanyResearchOfSite)
	require.NoError(t, db.StartFoundryCompanyResearch(ctx, failed, researchLimits()))
	require.NoError(t, db.FinishFoundryCompanyResearch(ctx, ws, failed.ID, nil, spend(50)))
	_, err = db.GetFoundryCompanyResearch(ctx, ws, failed.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "a research that failed doesn't read")

	require.Error(t, db.FinishFoundryCompanyResearch(ctx, ws, failed.ID, nil, spend(-1)))
	require.Error(t, db.StartFoundryCompanyResearch(ctx, &model.FoundryCompanyResearch{ID: id26("CR-bad"), WorkspaceID: ws, UserID: "u",
		Kind: "audit", Domain: "acme.com"}, researchLimits()))
	require.Error(t, db.StartFoundryCompanyResearch(ctx, researchRow(4, ws, "", model.FoundryCompanyResearchGoal), researchLimits()))
	bad := researchLimits()
	bad.PerUserPerHour = 0
	require.Error(t, db.StartFoundryCompanyResearch(ctx, researchRow(5, ws, "u", model.FoundryCompanyResearchGoal), bad))
}

// Concurrent calls of one person count each other: of 6 at once, 3 pass
// the hour; an hour later the person may again, and another person's
// calls never counted.
func TestFoundryCompanyResearchHourlyCap(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws := id26("CW-hour")
	createWorkspace(t, ws)

	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		kind := model.FoundryCompanyResearchOfSite
		if i%2 == 1 {
			kind = model.FoundryCompanyResearchGoal
		}
		wg.Go(func() {
			errs[i] = db.StartFoundryCompanyResearch(ctx, researchRow(i, ws, "user-ada", kind), researchLimits())
		})
	}
	wg.Wait()
	passed, limited := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			passed++
		case assert.ErrorIs(t, err, store.ErrLimitExceeded):
			limited++
		}
	}
	assert.Equal(t, 3, passed, "both kinds count toward the hour")
	assert.Equal(t, 3, limited)

	require.NoError(t, db.StartFoundryCompanyResearch(ctx, researchRow(10, ws, "user-grace", model.FoundryCompanyResearchOfSite), researchLimits()))

	_, err := testDB.Exec(`UPDATE foundry_company_research SET created_at = NOW(6) - INTERVAL 61 MINUTE WHERE user_id='user-ada'`)
	require.NoError(t, err)
	require.NoError(t, db.StartFoundryCompanyResearch(ctx, researchRow(11, ws, "user-ada", model.FoundryCompanyResearchOfSite), researchLimits()))
}

// A call is refused once the month's spend and its worst case would pass
// the budget; spend before the month doesn't count, and nor does another
// workspace's.
func TestFoundryCompanyResearchBudget(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws, other := id26("CW-budget"), id26("CW-budget-other")
	createWorkspace(t, ws)
	createWorkspace(t, other)
	limits := researchLimits()
	limits.PerUserPerHour = 100

	first := researchRow(1, ws, "user-ada", model.FoundryCompanyResearchOfSite)
	require.NoError(t, db.StartFoundryCompanyResearch(ctx, first, limits))
	require.NoError(t, db.FinishFoundryCompanyResearch(ctx, ws, first.ID, nil, spend(900_000)))
	second := researchRow(2, ws, "user-ada", model.FoundryCompanyResearchGoal)
	require.NoError(t, db.StartFoundryCompanyResearch(ctx, second, limits),
		"$0.90 spent and a $0.10 worst case fit the $1")
	require.ErrorIs(t, db.StartFoundryCompanyResearch(ctx, researchRow(3, ws, "user-grace", model.FoundryCompanyResearchGoal), func() model.FoundryCompanyResearchLimits {
		l := limits
		l.WorstUSDMicros = 100_001
		return l
	}()), store.ErrBudgetSpent)
	require.NoError(t, db.StartFoundryCompanyResearch(ctx, researchRow(4, other, "user-ada", model.FoundryCompanyResearchGoal), func() model.FoundryCompanyResearchLimits {
		l := limits
		l.WorstUSDMicros = 1_000_000
		return l
	}()), "another workspace's spend")

	require.NoError(t, db.FinishFoundryCompanyResearch(ctx, ws, second.ID, nil, spend(0)))
	_, err := testDB.Exec(`UPDATE foundry_company_research SET created_at = NOW(6) - INTERVAL 2 DAY WHERE id=?`, first.ID)
	require.NoError(t, err)
	limits.WorstUSDMicros = 1_000_000
	require.NoError(t, db.StartFoundryCompanyResearch(ctx, researchRow(5, ws, "user-grace", model.FoundryCompanyResearchGoal), limits),
		"last month's spend is the last month's")

	deleteWorkspace(t, ws)
	require.ErrorIs(t, db.StartFoundryCompanyResearch(ctx, researchRow(6, ws, "user-grace", model.FoundryCompanyResearchGoal), researchLimits()),
		store.ErrNotFound)
}

// A started call holds its worst case against the budget until it ends:
// concurrent calls can't together pass the month; a finished call is
// charged what it spent, and one cut short the larger of that and its
// reservation.
func TestFoundryCompanyResearchReservation(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws := id26("CW-reserve")
	createWorkspace(t, ws)
	limits := researchLimits()
	limits.PerUserPerHour = 100
	limits.WorstUSDMicros = 300_000

	var wg sync.WaitGroup
	rows := make([]*model.FoundryCompanyResearch, 6)
	errs := make([]error, len(rows))
	for i := range rows {
		rows[i] = researchRow(i, ws, "user-ada", model.FoundryCompanyResearchOfSite)
		wg.Go(func() { errs[i] = db.StartFoundryCompanyResearch(ctx, rows[i], limits) })
	}
	wg.Wait()
	var started []*model.FoundryCompanyResearch
	for i, err := range errs {
		if err == nil {
			started = append(started, rows[i])
			continue
		}
		assert.ErrorIs(t, err, store.ErrBudgetSpent)
	}
	require.Len(t, started, 3, "three $0.30 reservations fit the $1, a fourth doesn't")

	charged := func(id string) (micros int64) {
		require.NoError(t, testDB.QueryRow(`SELECT usd_micros FROM foundry_company_research WHERE id=?`, id).Scan(&micros))
		return micros
	}
	require.NoError(t, db.FinishFoundryCompanyResearch(ctx, ws, started[0].ID, nil, spend(10_000)))
	assert.EqualValues(t, 10_000, charged(started[0].ID), "a finished call is charged what it spent")
	require.NoError(t, db.FinishFoundryCompanyResearch(ctx, ws, started[1].ID, nil,
		model.FoundryCompanyResearchSpend{USDMicros: 5_000, CutShort: true}))
	assert.EqualValues(t, 300_000, charged(started[1].ID), "a call cut short keeps its reservation")
	require.NoError(t, db.FinishFoundryCompanyResearch(ctx, ws, started[2].ID, nil,
		model.FoundryCompanyResearchSpend{USDMicros: 400_000, CutShort: true}))
	assert.EqualValues(t, 400_000, charged(started[2].ID), "or what it reported, if more")

	limits.WorstUSDMicros = 290_000
	require.NoError(t, db.StartFoundryCompanyResearch(ctx, researchRow(10, ws, "user-ada", model.FoundryCompanyResearchGoal), limits),
		"$0.71 charged and a $0.29 worst case fit the $1")
	limits.WorstUSDMicros = 1
	require.ErrorIs(t, db.StartFoundryCompanyResearch(ctx, researchRow(11, ws, "user-ada", model.FoundryCompanyResearchGoal), limits),
		store.ErrBudgetSpent, "the running call's reservation counts")
}
