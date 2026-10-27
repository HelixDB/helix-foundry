package api

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
)

type decisionStore struct {
	foundrystore.Unimplemented
	ontology *foundrymodel.FoundryOntology
	load     *foundrymodel.FoundryLoad
	version  *foundrymodel.OntologyVersion
	rows     []*foundrymodel.FoundryReviewDecision
	saved    []*foundrymodel.FoundryReviewDecision
	saveErr  error
	deleted  string
}

func (s *decisionStore) GetFoundryOntology(_ context.Context, ws, id string) (*foundrymodel.FoundryOntology, error) {
	if s.ontology.WorkspaceID != ws || s.ontology.ID != id {
		return nil, foundrystore.ErrNotFound
	}
	return s.ontology, nil
}
func (s *decisionStore) GetFoundryLoad(_ context.Context, o, id string) (*foundrymodel.FoundryLoad, error) {
	if s.load == nil || o != s.ontology.ID || id != s.load.ID {
		return nil, foundrystore.ErrNotFound
	}
	return s.load, nil
}
func (s *decisionStore) GetOntologyVersion(_ context.Context, o, id string) (*foundrymodel.OntologyVersion, error) {
	if o != s.ontology.ID || id != s.version.ID {
		return nil, foundrystore.ErrNotFound
	}
	return s.version, nil
}
func (s *decisionStore) SettleFoundryReviewDecision(_ context.Context, ws string, d *foundrymodel.FoundryReviewDecision) error {
	if ws != s.ontology.WorkspaceID {
		return foundrystore.ErrNotFound
	}
	if s.saveErr != nil {
		return s.saveErr
	}
	d.DecidedAt = time.Now()
	s.saved = append(s.saved, d)
	return nil
}
func (s *decisionStore) ListFoundryReviewDecisions(_ context.Context, o, after string, docs []string, limit int) ([]*foundrymodel.FoundryReviewDecision, error) {
	if o != s.ontology.ID {
		panic("wrong ontology")
	}
	return s.rows, nil
}
func (s *decisionStore) DeleteFoundryReviewDecision(_ context.Context, o, id string) error {
	if o != s.ontology.ID {
		panic("wrong ontology")
	}
	if id == "missing" {
		return foundrystore.ErrNotFound
	}
	s.deleted = id
	return nil
}

// graphFunc is a ports.GraphReader of one function.
type graphFunc func(ctx context.Context, tenantID string, query []byte) (int, []byte, error)

func (f graphFunc) ReadQuery(ctx context.Context, tenantID string, query []byte) (int, []byte, error) {
	return f(ctx, tenantID, query)
}

// reviewer is user-a, who settles the reviews of workspace-a.
var reviewer = ports.Actor{Source: ports.SourceUser, UserID: "user-a"}

// reviewService has workspace-a's ontology o, loaded into tenant-a, whose
// graph answers review to the first read and finds customer-1 after.
func reviewService(t *testing.T, review string) (*Service, *decisionStore, *[]string) {
	t.Helper()
	queries := []string{}
	current := "load-1"
	def, _ := json.Marshal(ontologyagent.Definition{OntologyTOML: "[types.Customer]\nkey = [\"id\"]\nprops = { name = \"string\", amount = \"float\" }"})
	db := &decisionStore{ontology: &foundrymodel.FoundryOntology{ID: "o", WorkspaceID: "workspace-a", TenantID: "tenant-a", CurrentLoadID: &current}, load: &foundrymodel.FoundryLoad{ID: current, VersionID: "v", Status: foundrymodel.FoundryLoadReady}, version: &foundrymodel.OntologyVersion{ID: "v", Definition: def}}
	graph := graphFunc(func(ctx context.Context, tenantID string, query []byte) (int, []byte, error) {
		queries = append(queries, string(query))
		if tenantID != "tenant-a" {
			t.Errorf("wrong tenant: %s", tenantID)
		}
		if a, _ := ports.ActorFrom(ctx); a.UserID != reviewer.UserID {
			t.Errorf("read as %+v, not the reviewer", a)
		}
		response := review
		if len(queries) > 1 {
			response = `{"items":[{"entity_key":"customer-1"}]}`
		}
		return 200, []byte(response), nil
	})
	return &Service{deps: &runtimeDeps{DB: db, Graph: graph}}, db, &queries
}

func resolveChoice(key string) *foundryv1.FoundryReviewChoice {
	return &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Resolve{Resolve: &foundryv1.FoundryResolveChoice{Target: &foundryv1.FoundryResolveChoice_EntityKey{EntityKey: key}}}}
}
func noneChoice() *foundryv1.FoundryReviewChoice {
	return &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Resolve{Resolve: &foundryv1.FoundryResolveChoice{Target: &foundryv1.FoundryResolveChoice_NotInData{NotInData: true}}}}
}
func propChoice(v *foundryv1.FoundryPropValue) *foundryv1.FoundryReviewChoice {
	return &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Prop{Prop: v}}
}
func propText(s string) *foundryv1.FoundryPropValue {
	return &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Text{Text: s}}
}
func propInteger(n int64) *foundryv1.FoundryPropValue {
	return &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Integer{Integer: n}}
}
func propNumber(f float64) *foundryv1.FoundryPropValue {
	return &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Number{Number: f}}
}
func propBoolean(b bool) *foundryv1.FoundryPropValue {
	return &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Boolean{Boolean: b}}
}
func propNull() *foundryv1.FoundryPropValue {
	return &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Null{}}
}
func settleRequest(choice *foundryv1.FoundryReviewChoice) *foundryv1.SettleFoundryReviewRequest {
	return &foundryv1.SettleFoundryReviewRequest{WorkspaceId: "workspace-a", OntologyId: "o", ReviewId: "review-1", Decision: choice}
}

func TestSettleFoundryReview(t *testing.T) {
	for _, reason := range []string{"unresolved", "ambiguous", "collision", "identifier_conflict", "adjudicated"} {
		for _, choice := range []*foundryv1.FoundryReviewChoice{resolveChoice("customer-1"), noneChoice()} {
			t.Run(reason+choice.String(), func(t *testing.T) {
				review := `{"items":[{"reason":"` + reason + `","type":"Customer","doc_id":"doc-a","norm_name":"variant","entity_key":"previous-link","confidence":0.98}]}`
				s, db, queries := reviewService(t, review)
				out, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(choice))
				if err != nil {
					t.Fatal(err)
				}
				if len(db.saved) != 1 || out.Decision.DocId == nil || *out.Decision.DocId != "doc-a" || out.Decision.GetType() != "Customer" || out.Decision.GetDecidedByUserId() != "user-a" {
					t.Fatalf("%+v saved %v", out, db.saved)
				}
				if (out.Decision.EntityKey == nil) != (choice.GetResolve().GetNotInData()) {
					t.Fatalf("bad choice: %+v", out.Decision)
				}
				if !choice.GetResolve().GetNotInData() && out.Decision.GetEntityKey() != choice.GetResolve().GetEntityKey() {
					t.Fatalf("kept the previous adjudicated target: %+v", out.Decision)
				}
				for _, q := range *queries {
					if !strings.Contains(q, `"request_type":"read"`) || !strings.Contains(q, `"foundry_load"`) || !strings.Contains(q, `"load-1"`) {
						t.Fatalf("unscoped read %s", q)
					}
				}
			})
		}
	}
	for _, value := range []*foundryv1.FoundryPropValue{propText("Chosen"), propNull()} {
		s, db, _ := reviewService(t, `{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"name"}]}`)
		out, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(propChoice(value)))
		if err != nil {
			t.Fatal(err)
		}
		if len(db.saved) != 1 || out.Decision.Kind != "prop" || out.Decision.GetProp() != "name" || out.Decision.Value == nil {
			t.Fatalf("%+v", out)
		}
	}
}

func TestSettleFoundryReviewValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		review string
		choice *foundryv1.FoundryReviewChoice
		code   codes.Code
	}{
		"wrong resolve kind":       {`{"items":[{"reason":"unresolved","type":"Customer","doc_id":"a","norm_name":"variant"}]}`, propChoice(propText("name")), codes.InvalidArgument},
		"wrong prop kind":          {`{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"name"}]}`, noneChoice(), codes.InvalidArgument},
		"unsupported reason":       {`{"items":[{"reason":"low_confidence","type":"Customer"}]}`, noneChoice(), codes.InvalidArgument},
		"missing review":           {`{"items":[]}`, noneChoice(), codes.NotFound},
		"missing type":             {`{"items":[{"reason":"unresolved"}]}`, noneChoice(), codes.FailedPrecondition},
		"missing name":             {`{"items":[{"reason":"unresolved","type":"Customer","doc_id":"a"}]}`, noneChoice(), codes.FailedPrecondition},
		"empty target":             {`{"items":[{"reason":"unresolved","type":"Customer","doc_id":"a","norm_name":"a"}]}`, resolveChoice(""), codes.InvalidArgument},
		"wrong value type":         {`{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"name"}]}`, propChoice(propNumber(3)), codes.InvalidArgument},
		"removed property":         {`{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"gone"}]}`, propChoice(propNull()), codes.FailedPrecondition},
		"other type":               {`{"items":[{"reason":"unresolved","type":"Other"}]}`, noneChoice(), codes.FailedPrecondition},
		"malformed graph response": {`{`, noneChoice(), codes.Internal},
	} {
		t.Run(name, func(t *testing.T) {
			s, db, _ := reviewService(t, tc.review)
			_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(tc.choice))
			if status.Code(err) != tc.code || len(db.saved) != 0 {
				t.Fatalf("err %v, writes %d", err, len(db.saved))
			}
		})
	}
	for name, change := range map[string]func(*Service, *decisionStore, *foundryv1.SettleFoundryReviewRequest){
		"another workspace": func(_ *Service, _ *decisionStore, r *foundryv1.SettleFoundryReviewRequest) { r.WorkspaceId = "other" },
		"no current load": func(_ *Service, d *decisionStore, _ *foundryv1.SettleFoundryReviewRequest) {
			d.ontology.CurrentLoadID = nil
		},
		"load deleted": func(_ *Service, d *decisionStore, _ *foundryv1.SettleFoundryReviewRequest) { d.load = nil },
		"load writing": func(_ *Service, d *decisionStore, _ *foundryv1.SettleFoundryReviewRequest) {
			d.load.Status = foundrymodel.FoundryLoadWriting
		},
		"missing choice": func(_ *Service, _ *decisionStore, r *foundryv1.SettleFoundryReviewRequest) { r.Decision = nil },
	} {
		t.Run(name, func(t *testing.T) {
			s, db, _ := reviewService(t, `{"items":[]}`)
			r := settleRequest(noneChoice())
			change(s, db, r)
			_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), r)
			if err == nil || len(db.saved) != 0 {
				t.Fatalf("err %v", err)
			}
		})
	}
	t.Run("missing actor", func(t *testing.T) {
		s, db, _ := reviewService(t, `{"items":[]}`)
		_, err := s.SettleFoundryReview(context.Background(), settleRequest(noneChoice()))
		if status.Code(err) != codes.Unauthenticated || len(db.saved) != 0 {
			t.Fatalf("err %v", err)
		}
	})
}

// A prop decision's value is checked as the engine parses it, and stored as
// the engine keeps it: one the engine refuses would fail every later load.
func TestSettleFoundryReviewPropValues(t *testing.T) {
	for _, tc := range []struct {
		prop  string
		value *foundryv1.FoundryPropValue
		saved string // the value stored; empty when refused
	}{
		{"name", propText("  Chosen "), `"Chosen"`},
		{"name", propText(""), ``},
		{"name", propText("   "), ``},
		{"name", propNull(), `null`},
		{"signed", propText("2026-03"), `"2026-03"`},
		{"signed", propText("2026"), `"2026"`},
		{"signed", propText("2026-02-30"), ``},
		{"at", propText("2026-03-14T09:30:00+01:00"), `"2026-03-14T08:30:00Z"`},
		{"at", propText("9999-12-31T23:30:00-01:00"), ``},
		{"count", propNumber(3), `3`},
		{"count", propNumber(3.5), ``},
		{"count", propText("3"), ``},
		// An integer keeps every digit: past 2^53 a double would round it,
		// and int64's extremes are in range.
		{"count", propInteger(9007199254740993), `9007199254740993`},
		{"count", propInteger(math.MaxInt64), `9223372036854775807`},
		{"count", propInteger(math.MinInt64), `-9223372036854775808`},
		{"count", &foundryv1.FoundryPropValue{}, ``},
		// A float keeps what the engine's f64 does.
		{"amount", propInteger(9007199254740993), `9007199254740992`},
		{"amount", propNumber(2.5), `2.5`},
		{"amount", propNumber(math.Inf(1)), ``},
		{"amount", propNumber(math.NaN()), ``},
		{"active", propBoolean(true), `true`},
		{"active", propInteger(1), ``},
	} {
		t.Run(tc.prop+" "+tc.value.String(), func(t *testing.T) {
			s, db, _ := reviewService(t, `{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"`+tc.prop+`"}]}`)
			db.version.Definition, _ = json.Marshal(ontologyagent.Definition{OntologyTOML: "[types.Customer]\nkey = [\"id\"]\n" +
				`props = { id = "string", name = "string", signed = "date", at = "datetime", count = "integer", amount = "float", active = "boolean" }`})
			_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(propChoice(tc.value)))
			if tc.saved == "" {
				if status.Code(err) != codes.InvalidArgument || len(db.saved) != 0 {
					t.Fatalf("err %v, saved %v", err, db.saved)
				}
				return
			}
			if err != nil || len(db.saved) != 1 || string(db.saved[0].Value) != tc.saved {
				t.Fatalf("err %v, saved %v", err, db.saved)
			}
		})
	}
}

// A stored value is answered with every digit an integer has: listed as a
// double, one past 2^53 would come back as another number.
func TestFoundryReviewDecisionValueReadBack(t *testing.T) {
	for stored, want := range map[string]*foundryv1.FoundryPropValue{
		`9007199254740993`:     propInteger(9007199254740993),
		`9223372036854775807`:  propInteger(math.MaxInt64),
		`-9223372036854775808`: propInteger(math.MinInt64),
		`3`:                    propInteger(3),
		`2.5`:                  propNumber(2.5),
		`1e+20`:                propNumber(1e20),
		`"Chosen"`:             propText("Chosen"),
		`true`:                 propBoolean(true),
		`null`:                 propNull(),
	} {
		d := &foundrymodel.FoundryReviewDecision{ID: "d", Kind: foundrymodel.FoundryReviewProp, Type: "Customer", EntityKey: ptr("customer-1"), Prop: ptr("count"), Value: json.RawMessage(stored)}
		if got := foundryReviewDecisionProto(d).GetValue(); !proto.Equal(got, want) {
			t.Errorf("%s read back as %v, want %v", stored, got, want)
		}
	}
	resolve := &foundrymodel.FoundryReviewDecision{ID: "d", Kind: foundrymodel.FoundryReviewResolve, Type: "Customer", DocID: ptr("doc"), NormName: ptr("name")}
	if got := foundryReviewDecisionProto(resolve).GetValue(); got != nil {
		t.Errorf("a resolve has no value, got %v", got)
	}
}

func TestListDeleteFoundryReviewDecisions(t *testing.T) {
	s, db, _ := reviewService(t, `{}`)
	for i := 0; i < 101; i++ {
		db.rows = append(db.rows, &foundrymodel.FoundryReviewDecision{ID: "d", Kind: foundrymodel.FoundryReviewResolve, Type: "Customer"})
	}
	out, err := s.ListFoundryReviewDecisions(context.Background(), &foundryv1.ListFoundryReviewDecisionsRequest{WorkspaceId: "workspace-a", OntologyId: "o"})
	if err != nil || len(out.GetDecisions()) != 100 || out.GetNextPageToken() != "d" {
		t.Fatalf("%v %v", out, err)
	}
	for _, ws := range []string{"workspace-a", "other"} {
		_, err := s.DeleteFoundryReviewDecision(context.Background(), &foundryv1.DeleteFoundryReviewDecisionRequest{WorkspaceId: ws, OntologyId: "o", Id: "d"})
		if ws == "workspace-a" && (err != nil || db.deleted != "d") {
			t.Fatal(err)
		}
		if ws == "other" && status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
		_, err = s.ListFoundryReviewDecisions(context.Background(), &foundryv1.ListFoundryReviewDecisionsRequest{WorkspaceId: ws, OntologyId: "o"})
		if ws == "other" && status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
	}
	_, err = s.DeleteFoundryReviewDecision(context.Background(), &foundryv1.DeleteFoundryReviewDecisionRequest{WorkspaceId: "workspace-a", OntologyId: "o", Id: "missing"})
	if status.Code(err) != codes.NotFound {
		t.Fatal(err)
	}
}

func TestSettleFoundryReviewReadFailures(t *testing.T) {
	review := `{"items":[{"reason":"unresolved","type":"Customer","doc_id":"doc-a","norm_name":"variant"}]}`
	for _, code := range []int{429, 500} {
		s, db, _ := reviewService(t, review)
		s.deps.Graph = graphFunc(func(context.Context, string, []byte) (int, []byte, error) { return code, []byte(`{}`), nil })
		_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(noneChoice()))
		if status.Code(err) != codes.Unavailable || len(db.saved) != 0 {
			t.Fatalf("%v", err)
		}
	}
	s, db, _ := reviewService(t, review)
	calls := 0
	s.deps.Graph = graphFunc(func(context.Context, string, []byte) (int, []byte, error) {
		calls++
		body := review
		if calls > 1 {
			body = `{"items":[]}`
		}
		return 200, []byte(body), nil
	})
	_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(resolveChoice("other-load-entity")))
	if status.Code(err) != codes.InvalidArgument || len(db.saved) != 0 {
		t.Fatalf("%v", err)
	}
	for _, saveErr := range []error{foundrystore.ErrConflict, foundrystore.ErrNotFound, io.ErrUnexpectedEOF} {
		s, db, _ := reviewService(t, review)
		db.saveErr = saveErr
		_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(noneChoice()))
		want := codes.Internal
		if saveErr == foundrystore.ErrConflict {
			want = codes.FailedPrecondition
		}
		if saveErr == foundrystore.ErrNotFound {
			want = codes.NotFound
		}
		if status.Code(err) != want || len(db.saved) != 0 {
			t.Fatalf("%v", err)
		}
	}
}
