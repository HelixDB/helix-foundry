package access_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// methods are the full names of every method Foundry serves.
func methods() []string {
	var out []string
	for _, desc := range []grpc.ServiceDesc{foundryv1.FoundryService_ServiceDesc, foundryv1.FoundryEventsService_ServiceDesc} {
		for _, m := range desc.Methods {
			out = append(out, "/"+desc.ServiceName+"/"+m.MethodName)
		}
		for _, s := range desc.Streams {
			out = append(out, "/"+desc.ServiceName+"/"+s.StreamName)
		}
	}
	return out
}

// Every method Foundry serves has a rule written for it, and the table names
// nothing else: a method added later is refused to grantees until someone
// decides, and says so here.
func TestRulesNameEveryMethod(t *testing.T) {
	served := methods()
	named := access.RuleMethods()
	for _, m := range served {
		if !slices.Contains(named, m) {
			t.Errorf("%s has no rule", m)
		}
	}
	for _, m := range named {
		if !slices.Contains(served, m) {
			t.Errorf("the rule for %s names no method Foundry serves", m)
		}
	}
}

// A method the table doesn't name, a misspelling or another service's,
// refuses every grantee.
func TestUnknownMethodsDeny(t *testing.T) {
	for _, m := range []string{"", "/helix.foundry.v1.FoundryService/Nope", "/helix.backend.v1.BackendService/GetWorkspace"} {
		if access.RuleOf(m) != access.Deny || access.GranteeAdmits(m) {
			t.Fatalf("%q admits a grantee", m)
		}
	}
}

// A grantee only ever reads, apart from asking and cancelling its own
// questions, talking to Founder in its own threads, linking its own Slack
// account, and starting and finishing its own workflow runs: no other method
// whose name says it writes admits one, the runs need the Workflows
// permission, and Founder's cards stay refused.
func TestGranteesNeverWrite(t *testing.T) {
	writes := []string{"Create", "Update", "Delete", "Set", "Settle", "Put", "Move", "Revise", "Generate", "Sync", "Test", "Complete",
		"Revoke", "Stop", "Resume", "Detect", "Send", "Validate", "Start", "Finish", "Assess", "Execute", "Refresh", "Dismiss"}
	runs := []string{foundryv1.FoundryService_StartFoundryWorkflowRun_FullMethodName, foundryv1.FoundryService_FinishFoundryWorkflowRun_FullMethodName}
	threads := []string{foundryv1.FoundryService_SendFoundryAgentMessage_FullMethodName, foundryv1.FoundryService_DeleteFoundryAgentThread_FullMethodName,
		// Its own Slack account.
		foundryv1.FoundryService_CreateFoundrySlackLink_FullMethodName, foundryv1.FoundryService_DeleteFoundrySlackLink_FullMethodName}
	for _, m := range methods() {
		name := m[strings.LastIndex(m, "/")+1:]
		for _, w := range writes {
			if strings.HasPrefix(name, w) && access.GranteeAdmits(m) && !slices.Contains(runs, m) && !slices.Contains(threads, m) {
				t.Errorf("%s admits a grantee", m)
			}
		}
	}
	for _, m := range threads {
		if access.RuleOf(m) != access.Workspace {
			t.Errorf("%s: rule %v, want Workspace", m, access.RuleOf(m))
		}
	}
	for _, m := range runs {
		if access.RuleOf(m) != access.Workflows {
			t.Errorf("%s: rule %v, want Workflows", m, access.RuleOf(m))
		}
	}
}

func TestPrincipalKindRoundTrip(t *testing.T) {
	for _, k := range []access.PrincipalKind{access.PrincipalUser, access.PrincipalServiceCredential, access.PrincipalTeam} {
		got, err := access.ParsePrincipalKind(k.String())
		if err != nil || got != k {
			t.Fatalf("%v: %v, %v", k, got, err)
		}
	}
	for _, s := range []string{"", "User", "agent"} {
		if _, err := access.ParsePrincipalKind(s); err == nil {
			t.Fatalf("%q parsed", s)
		}
	}
	if access.PrincipalKind(0).String() != "" {
		t.Fatal("the zero kind has a name")
	}
}

// A call that never passed the access check is the host's.
func TestCallerFrom(t *testing.T) {
	if _, ok := access.CallerFrom(context.Background()).(access.Host); !ok {
		t.Fatal("no caller is not the host")
	}
	if _, ok := access.GranteeFrom(context.Background()); ok {
		t.Fatal("no caller is a grantee")
	}
	g := access.Grantee{WorkspaceID: "ws", Principal: access.Principal{Kind: access.PrincipalUser, ID: "u"}}
	got, ok := access.GranteeFrom(access.WithCaller(context.Background(), g))
	if !ok || got.Principal != g.Principal {
		t.Fatalf("got %+v, %v", got, ok)
	}
	if _, ok := access.GranteeFrom(access.WithCaller(context.Background(), access.Host{})); ok {
		t.Fatal("the host is a grantee")
	}
}

// A reader is no grantee, whose questions are its own as a grantee's are;
// the host's are everyone's.
func TestReaderFrom(t *testing.T) {
	bot := access.Principal{Kind: access.PrincipalServiceCredential, ID: "cred"}
	ctx := access.WithCaller(context.Background(), access.Reader{WorkspaceID: "ws", Principal: bot})
	if _, ok := access.GranteeFrom(ctx); ok {
		t.Fatal("a reader is a grantee")
	}
	if p, ok := access.PrincipalFrom(ctx); !ok || p != bot {
		t.Fatalf("a reader's principal: %+v, %v", p, ok)
	}
	ada := access.Principal{Kind: access.PrincipalUser, ID: "ada"}
	if p, ok := access.PrincipalFrom(access.WithCaller(context.Background(), access.Grantee{WorkspaceID: "ws", Principal: ada})); !ok || p != ada {
		t.Fatalf("a grantee's principal: %+v, %v", p, ok)
	}
	if _, ok := access.PrincipalFrom(context.Background()); ok {
		t.Fatal("the host's caller has a principal")
	}
}

// A reader is admitted exactly where a grantee is, but to workflows: it
// reads, asks and talks to Founder, and so never writes what a grantee
// can't.
func TestReadersAdmitted(t *testing.T) {
	for _, m := range methods() {
		rule := access.RuleOf(m)
		if want := rule == access.Workspace || rule == access.Ontology; access.ReaderAdmits(m) != want {
			t.Errorf("%s: a reader admitted %v, its rule %v", m, !want, rule)
		}
		if access.ReaderAdmits(m) && !access.GranteeAdmits(m) {
			t.Errorf("%s admits a reader, not a grantee", m)
		}
	}
	for _, m := range []string{"", foundryv1.FoundryService_StartFoundryWorkflowRun_FullMethodName, foundryv1.FoundryService_CreateFoundryLoad_FullMethodName,
		foundryv1.FoundryService_ExecuteFoundryAgentAction_FullMethodName, foundryv1.FoundryService_UpdateFoundrySlack_FullMethodName} {
		if access.ReaderAdmits(m) {
			t.Errorf("%q admits a reader", m)
		}
	}
}

func ExampleReaderAdmits() {
	fmt.Println(access.ReaderAdmits(foundryv1.FoundryService_AskFoundryQuestion_FullMethodName),
		access.ReaderAdmits(foundryv1.FoundryService_SetFoundryAccessGrants_FullMethodName))
	// Output: true false
}

func ExampleParsePrincipalKind() {
	k, err := access.ParsePrincipalKind("team")
	fmt.Println(k == access.PrincipalTeam, err)
	// Output: true <nil>
}

func ExampleRuleOf() {
	fmt.Println(access.RuleOf(foundryv1.FoundryService_DeleteFoundryOntology_FullMethodName) == access.Deny)
	// Output: true
}

func ExampleGrantee_Principals() {
	g := access.Grantee{WorkspaceID: "ws", Principal: access.Principal{Kind: access.PrincipalUser, ID: "ada"}, Teams: []string{"finance"}}
	for _, p := range g.Principals() {
		fmt.Println(p.Kind, p.ID)
	}
	// Output:
	// user ada
	// team finance
}
