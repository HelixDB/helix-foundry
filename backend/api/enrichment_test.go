package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// enrichmentStore is the Foundry store with People enrichment: the
// workspace's settings, the changes recorded, and each load's step.
type enrichmentStore struct {
	*foundryStore
	settings foundrymodel.FoundryEnrichmentSettings
	changes  []*foundrymodel.FoundryEnrichmentConsent
	steps    map[string]*foundrymodel.FoundryLoadEnrichment
	// stopped are the loads stopped, each with who asked.
	stopped []string
}

func (s *enrichmentStore) GetFoundryEnrichmentSettings(_ context.Context, _ string, since time.Time) (foundrymodel.FoundryEnrichmentSettings, error) {
	if since.Day() != 1 || since.Hour() != 0 || since.Location() != time.UTC {
		return foundrymodel.FoundryEnrichmentSettings{}, errors.New("the ledger counts from the month's start, UTC")
	}
	return s.settings, nil
}

func (s *enrichmentStore) SetFoundryEnrichmentConsent(_ context.Context, c *foundrymodel.FoundryEnrichmentConsent) error {
	s.changes = append(s.changes, c)
	s.settings.Consent = c
	return nil
}

func (s *enrichmentStore) ListFoundryLoadEnrichments(_ context.Context, loadIDs []string) (map[string]*foundrymodel.FoundryLoadEnrichment, error) {
	out := map[string]*foundrymodel.FoundryLoadEnrichment{}
	for _, id := range loadIDs {
		if step, ok := s.steps[id]; ok {
			out[id] = step
		}
	}
	return out, nil
}

func (s *enrichmentStore) StopFoundryLoadEnrichment(_ context.Context, _, _, loadID, userID string) error {
	if _, ok := s.steps[loadID]; !ok {
		return foundrystore.ErrNotFound
	}
	s.stopped = append(s.stopped, loadID+" by "+userID)
	return nil
}

// enrichmentService is the Foundry service with People enrichment's flag set,
// and the workspace on the plan with code plan.
func enrichmentService(t *testing.T, flag bool, plan string) (*Service, *enrichmentStore) {
	t.Helper()
	s, foundry, _ := foundryService(t)
	store := &enrichmentStore{foundryStore: foundry, steps: map[string]*foundrymodel.FoundryLoadEnrichment{}}
	s.deps.DB = store
	s.deps.Cfg.Foundry.Enrichment.Enabled = flag
	s.deps.Plans = &stubFoundryPlans{status: held(plan)}
	return s, store
}

func TestFoundryEnrichmentSettings(t *testing.T) {
	ctx := ports.WithActor(context.Background(), owner)

	// The page says whether lookups can run, from Helix's flag and the plan,
	// with the owner's setting and the month's credits.
	t.Run("shows availability, the setting and the month", func(t *testing.T) {
		for name, tc := range map[string]struct {
			flag bool
			plan string
			want foundryv1.FoundryEnrichmentAvailability
		}{
			"flag off": {false, "pro", foundryv1.FoundryEnrichmentAvailability_FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED},
			"Free":     {true, "free", foundryv1.FoundryEnrichmentAvailability_FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED},
			"Pro":      {true, "pro", foundryv1.FoundryEnrichmentAvailability_FOUNDRY_ENRICHMENT_AVAILABILITY_AVAILABLE},
			"Max":      {true, "max", foundryv1.FoundryEnrichmentAvailability_FOUNDRY_ENRICHMENT_AVAILABILITY_AVAILABLE},
		} {
			t.Run(name, func(t *testing.T) {
				s, store := enrichmentService(t, tc.flag, tc.plan)
				allowed := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
				store.settings = foundrymodel.FoundryEnrichmentSettings{
					Consent: &foundrymodel.FoundryEnrichmentConsent{Enabled: true, MonthlyCreditCap: 50_000},
					Enabled: &foundrymodel.FoundryEnrichmentConsent{UserID: "user-ada", CreatedAt: allowed},
					Ledger:  foundrymodel.FoundryEnrichmentLedger{WorkspaceCharged: 8_460, WorkspaceReserved: 12_240}, PurgesPending: 2,
				}
				out, err := s.GetFoundryEnrichment(ctx, &foundryv1.GetFoundryEnrichmentRequest{WorkspaceId: "ws-1"})
				if err != nil {
					t.Fatal(err)
				}
				e := out.GetEnrichment()
				now := time.Now().UTC()
				next := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
				if e.GetAvailability() != tc.want || !e.GetEnabled() || e.GetMonthlyCreditCap() != 50_000 || e.GetCreditsUsed() != 8_460 ||
					e.GetCreditsReserved() != 12_240 || !e.GetCapResetsAt().AsTime().Equal(next) || e.GetEnabledBy() != "user-ada" ||
					!e.GetEnabledAt().AsTime().Equal(allowed) || e.GetPurgesPending() != 2 {
					t.Fatalf("enrichment = %+v", e)
				}
			})
		}
	})

	// A workspace that never set it shows it off, with no owner.
	t.Run("shows a setting never made as off", func(t *testing.T) {
		s, _ := enrichmentService(t, true, "pro")
		out, err := s.GetFoundryEnrichment(ctx, &foundryv1.GetFoundryEnrichmentRequest{WorkspaceId: "ws-1"})
		if err != nil || out.GetEnrichment().GetEnabled() || out.GetEnrichment().GetEnabledBy() != "" || out.GetEnrichment().GetEnabledAt() != nil {
			t.Fatalf("enrichment = (%+v, %v)", out, err)
		}
	})

	// An owner's change is a new history row, with who made it and the
	// notice they were shown.
	t.Run("records an owner's change", func(t *testing.T) {
		s, store := enrichmentService(t, true, "pro")
		out, err := s.UpdateFoundryEnrichment(ctx, &foundryv1.UpdateFoundryEnrichmentRequest{
			WorkspaceId: "ws-1", Enabled: true, MonthlyCreditCap: 50_000, NoticeVersion: "2026-10",
		})
		if err != nil {
			t.Fatal(err)
		}
		c := store.changes[0]
		if len(store.changes) != 1 || c.ID == "" || c.WorkspaceID != "ws-1" || !c.Enabled || c.MonthlyCreditCap != 50_000 ||
			c.NoticeVersion != "2026-10" || c.UserID != "user-owner" || !out.GetEnrichment().GetEnabled() {
			t.Fatalf("change = %+v, response %+v", c, out)
		}
	})

	// New lookups need Helix's flag and Foundry Pro; withdrawing consent
	// needs neither, so an owner can always stop lookups and purge.
	t.Run("turns it on only with the flag and Pro, and off always", func(t *testing.T) {
		on := &foundryv1.UpdateFoundryEnrichmentRequest{WorkspaceId: "ws-1", Enabled: true, MonthlyCreditCap: 1, NoticeVersion: "n"}
		s, store := enrichmentService(t, false, "pro")
		if _, err := s.UpdateFoundryEnrichment(ctx, on); status.Code(err) != codes.FailedPrecondition || len(store.changes) != 0 {
			t.Fatalf("flag off: %v, %d changes", err, len(store.changes))
		}
		s, store = enrichmentService(t, true, "free")
		_, err := s.UpdateFoundryEnrichment(ctx, on)
		if reason := proReason(err); status.Code(err) != codes.FailedPrecondition || reason != "FOUNDRY_PRO_REQUIRED" || len(store.changes) != 0 {
			t.Fatalf("Free: %v (reason %q), %d changes", err, reason, len(store.changes))
		}
		for _, plan := range []string{"free", "pro"} {
			s, store = enrichmentService(t, false, plan)
			if _, err := s.UpdateFoundryEnrichment(ctx, &foundryv1.UpdateFoundryEnrichmentRequest{
				WorkspaceId: "ws-1", Enabled: false, NoticeVersion: "n",
			}); err != nil || len(store.changes) != 1 || store.changes[0].Enabled {
				t.Fatalf("off on %s with the flag off: %v, %+v", plan, err, store.changes)
			}
		}
	})

	// Only availability needs the billing system. While it can't answer, the page
	// still loads with the setting and the month (availability unspecified),
	// and turning lookups off, which needs no Pro, is saved and reported
	// saved rather than failing after it took effect.
	t.Run("shows the setting and turns it off while the plan can't be read", func(t *testing.T) {
		s, store := enrichmentService(t, true, "pro")
		s.deps.Plans = &stubFoundryPlans{err: errors.New("billing is down")}
		store.settings = foundrymodel.FoundryEnrichmentSettings{
			Consent: &foundrymodel.FoundryEnrichmentConsent{Enabled: true, MonthlyCreditCap: 50_000},
			Ledger:  foundrymodel.FoundryEnrichmentLedger{WorkspaceCharged: 8_460},
		}
		out, err := s.GetFoundryEnrichment(ctx, &foundryv1.GetFoundryEnrichmentRequest{WorkspaceId: "ws-1"})
		if e := out.GetEnrichment(); err != nil || !e.GetEnabled() || e.GetCreditsUsed() != 8_460 ||
			e.GetAvailability() != foundryv1.FoundryEnrichmentAvailability_FOUNDRY_ENRICHMENT_AVAILABILITY_UNSPECIFIED {
			t.Fatalf("read: (%+v, %v)", e, err)
		}
		out, err = s.UpdateFoundryEnrichment(ctx, &foundryv1.UpdateFoundryEnrichmentRequest{
			WorkspaceId: "ws-1", Enabled: false, MonthlyCreditCap: 50_000, NoticeVersion: "n",
		})
		if err != nil || len(store.changes) != 1 || store.changes[0].Enabled || out.GetEnrichment().GetEnabled() {
			t.Fatalf("off: (%+v, %v), %+v", out, err, store.changes)
		}
		if _, err := s.UpdateFoundryEnrichment(ctx, &foundryv1.UpdateFoundryEnrichmentRequest{
			WorkspaceId: "ws-1", Enabled: true, MonthlyCreditCap: 50_000, NoticeVersion: "n",
		}); status.Code(err) != codes.Unavailable || len(store.changes) != 1 {
			t.Fatalf("on: %v, %d changes", err, len(store.changes))
		}
	})

	// Operators may lower the most an owner may set below a cap already
	// stored, which the website sends back when it turns lookups off:
	// withdrawing consent is never refused for it.
	t.Run("turns it off with a cap above a lowered maximum", func(t *testing.T) {
		s, store := enrichmentService(t, true, "pro")
		s.deps.Cfg.Foundry.Enrichment.MonthlyCreditsMax = 50_000
		if _, err := s.UpdateFoundryEnrichment(ctx, &foundryv1.UpdateFoundryEnrichmentRequest{
			WorkspaceId: "ws-1", Enabled: false, MonthlyCreditCap: 80_000, NoticeVersion: "n",
		}); err != nil || len(store.changes) != 1 || store.changes[0].Enabled {
			t.Fatalf("off: %v, %+v", err, store.changes)
		}
		if _, err := s.UpdateFoundryEnrichment(ctx, &foundryv1.UpdateFoundryEnrichmentRequest{
			WorkspaceId: "ws-1", Enabled: true, MonthlyCreditCap: 80_000, NoticeVersion: "n",
		}); status.Code(err) != codes.InvalidArgument || len(store.changes) != 1 {
			t.Fatalf("on above the maximum: %v, %d changes", err, len(store.changes))
		}
	})

	// A change must say which notice was shown, stay within the cap Helix
	// allows, and come from a signed-in owner.
	t.Run("refuses an incomplete change", func(t *testing.T) {
		for name, tc := range map[string]struct {
			actor ports.Actor
			req   *foundryv1.UpdateFoundryEnrichmentRequest
			code  codes.Code
		}{
			"no notice":    {owner, &foundryv1.UpdateFoundryEnrichmentRequest{WorkspaceId: "ws-1", MonthlyCreditCap: 1}, codes.InvalidArgument},
			"long notice":  {owner, &foundryv1.UpdateFoundryEnrichmentRequest{WorkspaceId: "ws-1", NoticeVersion: "123456789012345678901234567890123"}, codes.InvalidArgument},
			"cap too high": {owner, &foundryv1.UpdateFoundryEnrichmentRequest{WorkspaceId: "ws-1", Enabled: true, MonthlyCreditCap: 100_001, NoticeVersion: "n"}, codes.InvalidArgument},
			"negative cap": {owner, &foundryv1.UpdateFoundryEnrichmentRequest{WorkspaceId: "ws-1", MonthlyCreditCap: -1, NoticeVersion: "n"}, codes.InvalidArgument},
			"no workspace": {owner, &foundryv1.UpdateFoundryEnrichmentRequest{NoticeVersion: "n"}, codes.InvalidArgument},
			"no owner":     {ports.Actor{}, &foundryv1.UpdateFoundryEnrichmentRequest{WorkspaceId: "ws-1", NoticeVersion: "n"}, codes.PermissionDenied},
		} {
			t.Run(name, func(t *testing.T) {
				s, store := enrichmentService(t, true, "pro")
				if _, err := s.UpdateFoundryEnrichment(ports.WithActor(ctx, tc.actor), tc.req); status.Code(err) != tc.code || len(store.changes) != 0 {
					t.Fatalf("%v, %d changes", err, len(store.changes))
				}
			})
		}
	})
}

// proReason is the ErrorInfo reason a billing refusal carries.
func proReason(err error) string {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}
	return ""
}

func TestFoundryLoadEnrichment(t *testing.T) {
	ctx := context.Background()
	published := time.Date(2026, 10, 20, 14, 30, 0, 0, time.UTC)
	capped := foundrymodel.FoundryEnrichmentStopCap
	step := &foundrymodel.FoundryLoadEnrichment{
		LoadID: "load-1", Status: foundrymodel.FoundryEnrichmentPartial, StoppedBy: &capped, PublishedAt: &published,
		Counts: foundrymodel.FoundryEnrichmentCounts{People: 980, NoName: 13, NoClue: 43, Shared: 2, Skipped: 56, Carried: 272, ToLookUp: 612,
			LookedUp: 400, Found: 172, Held: 38, NotFound: 188, Uncertain: 2, Profiles: 412, Credits: 4_240, Applied: 409, Unmatched: 3},
	}
	withLoad := func(t *testing.T) (*Service, *enrichmentStore) {
		t.Helper()
		s, store := enrichmentService(t, true, "pro")
		store.loads = append(store.loads, &foundrymodel.FoundryLoad{ID: "load-1", OntologyID: "ont-1", VersionID: "v-1", Status: foundrymodel.FoundryLoadWriting},
			&foundrymodel.FoundryLoad{ID: "load-2", OntologyID: "ont-1", VersionID: "v-1", Status: foundrymodel.FoundryLoadReady})
		store.steps["load-1"] = step
		return s, store
	}

	// A load shows its enrichment step: the status, why a
	// partial one stopped, every count and when it published; a load whose
	// version enriches no one shows none.
	t.Run("shows a load's step", func(t *testing.T) {
		s, _ := withLoad(t)
		list, err := s.ListFoundryLoads(ctx, &foundryv1.ListFoundryLoadsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || len(list.GetLoads()) != 2 {
			t.Fatalf("list = (%+v, %v)", list, err)
		}
		byID := map[string]*foundryv1.FoundryLoad{}
		for _, l := range list.GetLoads() {
			byID[l.GetId()] = l
		}
		e := byID["load-1"].GetEnrichment()
		raw, _ := json.Marshal(map[string]any{
			"status": e.GetStatus(), "stoppedBy": e.GetStoppedBy(), "people": e.GetPeople(), "noName": e.GetNoName(), "noClue": e.GetNoClue(),
			"shared": e.GetShared(), "skipped": e.GetSkipped(), "carried": e.GetCarried(), "toLookUp": e.GetToLookUp(), "lookedUp": e.GetLookedUp(),
			"found": e.GetFound(), "held": e.GetHeld(), "notFound": e.GetNotFound(), "uncertain": e.GetUncertain(), "credits": e.GetCredits(),
			"applied": e.GetApplied(), "unmatched": e.GetUnmatched(),
		})
		want := `{"applied":409,"carried":272,"credits":4240,"found":172,"held":38,"lookedUp":400,"noClue":43,"noName":13,"notFound":188,` +
			`"people":980,"shared":2,"skipped":56,"status":"partial","stoppedBy":"cap","toLookUp":612,"uncertain":2,"unmatched":3}`
		if string(raw) != want || !e.GetPublishedAt().AsTime().Equal(published) {
			t.Fatalf("enrichment = %s, published %v", raw, e.GetPublishedAt())
		}
		if byID["load-2"].GetEnrichment() != nil {
			t.Fatalf("a load without a step shows %+v", byID["load-2"].GetEnrichment())
		}
		got, err := s.GetFoundryLoad(ctx, &foundryv1.GetFoundryLoadRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "load-1"})
		if err != nil || got.GetLoad().GetEnrichment().GetStatus() != "partial" || got.GetLoad().GetEnrichment().GetStoppedBy() != "cap" {
			t.Fatalf("get = (%+v, %v)", got, err)
		}
	})

	// Stop names who asked and returns the load; a load that looks no one
	// up has nothing to stop.
	t.Run("stops a load's lookups", func(t *testing.T) {
		s, store := withLoad(t)
		out, err := s.StopFoundryLoadEnrichment(ports.WithActor(ctx, ports.Actor{Source: ports.SourceUser, UserID: "user-ada"}), &foundryv1.StopFoundryLoadEnrichmentRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "load-1",
		})
		if err != nil || out.GetLoad().GetId() != "load-1" || len(store.stopped) != 1 || store.stopped[0] != "load-1 by user-ada" {
			t.Fatalf("stop = (%+v, %v), stopped %v", out, err, store.stopped)
		}
		u := ports.Actor{Source: ports.SourceUser, UserID: "u"}
		for name, tc := range map[string]struct {
			actor ports.Actor
			req   *foundryv1.StopFoundryLoadEnrichmentRequest
			code  codes.Code
		}{
			"no step":   {u, &foundryv1.StopFoundryLoadEnrichmentRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "load-2"}, codes.NotFound},
			"no load":   {u, &foundryv1.StopFoundryLoadEnrichmentRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}, codes.InvalidArgument},
			"no user":   {ports.Actor{}, &foundryv1.StopFoundryLoadEnrichmentRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "load-1"}, codes.PermissionDenied},
			"another's": {u, &foundryv1.StopFoundryLoadEnrichmentRequest{WorkspaceId: "ws-2", OntologyId: "ont-1", Id: "load-1"}, codes.NotFound},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := s.StopFoundryLoadEnrichment(ports.WithActor(ctx, tc.actor), tc.req); status.Code(err) != tc.code {
					t.Fatalf("err = %v, want %s", err, tc.code)
				}
			})
		}
	})
}

// People enrichment's item is Helix's to offer, so no edit
// puts or removes one while the flag is off; with it on, the edit is queued
// whatever the workspace's consent (a load without it applies nothing).
func TestReviseOntologyVersionEnrichment(t *testing.T) {
	for name, flag := range map[string]bool{"refuses with the flag off": false, "queues with the flag on": true} {
		t.Run(name, func(t *testing.T) {
			s, db := generationService(t, 20)
			s.deps.Cfg.Foundry.Enrichment.Enabled = flag
			id := "base"
			raw, _ := json.Marshal(ontologyagent.Definition{Status: ontologyagent.StatusDone, Draft: json.RawMessage(`{"draft":{"items":{}}}`)})
			db.versions = append(db.versions, &foundrymodel.OntologyVersion{ID: "parent", OntologyID: "ont-1", Format: foundrymodel.OntologyFormatAgentBundle, GenerationID: &id, Definition: raw})
			db.generations = append(db.generations, &foundrymodel.OntologyGeneration{ID: id, OntologyID: "ont-1", Status: foundrymodel.OntologyGenerationSucceeded})
			for _, edit := range []*foundryv1.OntologyEdit{put("enrichment.people", "[enrichments.people]"), remove("enrichment.people")} {
				before := len(db.generations)
				_, err := s.ReviseOntologyVersion(context.Background(), &foundryv1.ReviseOntologyVersionRequest{
					WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: "parent", Edits: []*foundryv1.OntologyEdit{put("type.Person", "[types.Person]"), edit},
				})
				switch {
				case !flag && (status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "People enrichment isn't available" || len(db.generations) != before):
					t.Fatalf("flag off: %v, %d queued", err, len(db.generations)-before)
				case flag && (err != nil || len(db.generations) != before+1):
					t.Fatalf("flag on: %v", err)
				}
				db.generations = db.generations[:before] // one revision queued at a time
			}
		})
	}
}
