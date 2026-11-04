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

// A grantee only ever reads: no method whose name says it writes admits one.
func TestGranteesNeverWrite(t *testing.T) {
	writes := []string{"Create", "Update", "Delete", "Set", "Settle", "Put", "Move", "Revise", "Generate", "Sync", "Test", "Complete",
		"Revoke", "Stop", "Resume", "Detect", "Send", "Validate"}
	for _, m := range methods() {
		name := m[strings.LastIndex(m, "/")+1:]
		for _, w := range writes {
			if strings.HasPrefix(name, w) && access.GranteeAdmits(m) {
				t.Errorf("%s admits a grantee", m)
			}
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
