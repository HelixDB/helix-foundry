package local

import (
	"testing"

	"google.golang.org/grpc"

	directoryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/directory/v1"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// Every method of Foundry's API and the install's directory is either served
// or refused for a reason: a new one is decided, not forgotten.
func TestMethodsAreExhaustive(t *testing.T) {
	seen := map[string]bool{}
	for _, service := range []grpc.ServiceDesc{foundryv1.FoundryService_ServiceDesc, foundryv1.FoundryEventsService_ServiceDesc, directoryv1.DirectoryService_ServiceDesc} {
		for _, m := range service.Methods {
			method := "/" + service.ServiceName + "/" + m.MethodName
			seen[method] = true
			_, served := methods[method]
			why, refuse := refused[method]
			switch {
			case served == refuse:
				t.Errorf("%s: served %v, refused %v: it must be exactly one", method, served, refuse)
			case refuse && why == "":
				t.Errorf("%s is refused for no reason", method)
			}
		}
	}
	for method := range refused {
		if !seen[method] {
			t.Errorf("%s is refused, and no service has it", method)
		}
	}
}

// refused are the methods of Foundry's API a standalone install doesn't serve,
// with what it lacks.
var refused = map[string]string{
	foundry("GetFoundryPlan"):           noPlans,
	foundry("ListFoundryPlans"):         noPlans,
	foundry("CreateFoundryProCheckout"): noPlans,
	foundry("CancelFoundryPro"):         noPlans,
	foundry("ResumeFoundryPro"):         noPlans,
	foundry("GetFoundryUsage"):          noPlans,

	foundry("UpdateFoundryEnrichment"):   noLookups,
	foundry("StopFoundryLoadEnrichment"): noLookups,

	foundry("CreateFoundryEventSourceToken"):                                 noSenders,
	foundry("ListFoundryEventSourceTokens"):                                  noSenders,
	foundry("RevokeFoundryEventSourceToken"):                                 noSenders,
	foundry("ListFoundryEventBatches"):                                       noSenders,
	"/helix.foundry.v1.FoundryEventsService/SendFoundryEvents":               noSenders,
	"/helix.foundry.v1.FoundryEventsService/ValidateFoundryEventSourceToken": noSenders,

	foundry("ListFoundryAccess"):          noGrantees,
	foundry("SetFoundryAccessGrants"):     noGrantees,
	foundry("PreviewFoundryAccessChange"): noGrantees,
	foundry("GetMyFoundryAccess"):         noGrantees,
	foundry("SetFoundryMemberRestricted"): noGrantees,
	foundry("CreateFoundryTeam"):          noGrantees,
	foundry("UpdateFoundryTeam"):          noGrantees,
	foundry("DeleteFoundryTeam"):          noGrantees,

	foundry("CreateOntologyVersion"): notCalled,
	foundry("UpdateFoundryOntology"): notCalled,

	foundry("DeleteFoundryOntologyDocument"):    noOntologySets,
	foundry("ListFoundryOntologyDocumentSets"):  noOntologySets,
	foundry("DeleteFoundryOntologyDocumentSet"): noOntologySets,
}

const (
	noPlans        = "an install sells no plan"
	noLookups      = "an install has no lookup provider"
	noSenders      = "an API source's senders reach it through a door an install doesn't run"
	noGrantees     = "an install has one owner, so no one to grant sources to"
	notCalled      = "the local site doesn't call it"
	noOntologySets = "documents are the workspace's: the site calls the workspace's routes"
)
