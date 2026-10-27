package local

import (
	directoryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/directory/v1"
)

// check is what the front end checks of a call's request before its handler
// runs, as a hosted front end's authorization does.
type check uint8

const (
	// checkHandler leaves every check to the handler: the directory's
	// methods, whose handlers read the rows they are of.
	checkHandler check = iota
	// checkWorkspace: request.workspace_id names a workspace of the owner.
	checkWorkspace
	// checkTenant: request.tenant_id is a live database of
	// request.workspace_id, as an ontology is created on.
	checkTenant
	// checkOntology: request.ontology_id is a live ontology of
	// request.workspace_id, for a call that reads or writes its database.
	checkOntology
)

// healthMethods are served to anyone: they say only whether the install is
// up.
var healthMethods = map[string]bool{
	"/grpc.health.v1.Health/Check": true,
	"/grpc.health.v1.Health/Watch": true,
}

// foundry is the full name of a FoundryService method.
func foundry(name string) string { return "/helix.foundry.v1.FoundryService/" + name }

// methods are the methods a standalone install serves, with what is checked
// of each: the directory, and what the Foundry site calls of Foundry's API.
// The rest need what an install has not, so they are refused before a
// handler runs (unsupported): plans and checkout, People enrichment's
// lookups, and event sources' send tokens and batches, whose senders reach a
// source through a door an install does not run (FoundryEventsService
// included). It fails closed: a new method is refused until it is listed.
var methods = map[string]check{
	directoryv1.DirectoryService_Whoami_FullMethodName:                   checkHandler,
	directoryv1.DirectoryService_ListWorkspaces_FullMethodName:           checkHandler,
	directoryv1.DirectoryService_CreateWorkspace_FullMethodName:          checkHandler,
	directoryv1.DirectoryService_GetWorkspace_FullMethodName:             checkHandler,
	directoryv1.DirectoryService_ListProjects_FullMethodName:             checkWorkspace,
	directoryv1.DirectoryService_CreateProject_FullMethodName:            checkWorkspace,
	directoryv1.DirectoryService_ListTenants_FullMethodName:              checkHandler,
	directoryv1.DirectoryService_CreateTenant_FullMethodName:             checkHandler,
	directoryv1.DirectoryService_ExecuteDatabaseReadQuery_FullMethodName: checkHandler,

	foundry("ListConnectorKinds"): checkWorkspace,
	foundry("TestConnector"):      checkWorkspace,
	foundry("ListConnectors"):     checkWorkspace,
	foundry("CreateConnector"):    checkWorkspace,
	foundry("GetConnector"):       checkWorkspace,
	foundry("UpdateConnector"):    checkWorkspace,
	foundry("DeleteConnector"):    checkWorkspace,
	foundry("SyncConnector"):      checkWorkspace,
	foundry("ListConnectorSyncs"): checkWorkspace,
	foundry("GetConnectorSync"):   checkWorkspace,

	foundry("ListFoundryOntologies"):     checkWorkspace,
	foundry("CreateFoundryOntology"):     checkTenant,
	foundry("GetFoundryOntology"):        checkWorkspace,
	foundry("DeleteFoundryOntology"):     checkOntology,
	foundry("ListOntologyVersions"):      checkWorkspace,
	foundry("GetOntologyVersion"):        checkWorkspace,
	foundry("SetCurrentOntologyVersion"): checkWorkspace,
	foundry("ReviseOntologyVersion"):     checkWorkspace,
	foundry("GenerateOntology"):          checkWorkspace,
	foundry("ListOntologyGenerations"):   checkWorkspace,
	foundry("GetOntologyGeneration"):     checkWorkspace,

	foundry("ListFoundryDocuments"):          checkWorkspace,
	foundry("CreateFoundryDocument"):         checkWorkspace,
	foundry("CompleteFoundryDocumentUpload"): checkWorkspace,
	foundry("GetFoundryDocument"):            checkWorkspace,
	foundry("DeleteFoundryDocument"):         checkWorkspace,
	foundry("MoveFoundryDocument"):           checkWorkspace,
	foundry("ListFoundryDocumentSets"):       checkWorkspace,
	foundry("CreateFoundryDocumentSet"):      checkWorkspace,
	foundry("UpdateFoundryDocumentSet"):      checkWorkspace,
	foundry("DeleteFoundryDocumentSet"):      checkWorkspace,
	foundry("UpdateFoundryDocument"):         checkWorkspace,
	foundry("GetFoundryDocumentContent"):     checkWorkspace,
	foundry("GetFoundryDocumentBytes"):       checkWorkspace,
	foundry("ListFoundryOntologyDocuments"):  checkWorkspace,
	foundry("GetFoundryOntologyDocument"):    checkWorkspace,
	foundry("GetFoundryDataLayout"):          checkWorkspace,
	foundry("PutFoundryDataLayout"):          checkWorkspace,

	// The ontology pages read the setting: on an install it is unavailable
	// (no lookup provider, no plan sold), and turning it on stays refused.
	foundry("GetFoundryEnrichment"): checkWorkspace,
	// An install brings its own model provider, and may have none until the
	// owner chooses one.
	foundry("GetFoundryAI"):    checkWorkspace,
	foundry("UpdateFoundryAI"): checkWorkspace,

	foundry("CreateFoundryLoad"):     checkOntology,
	foundry("ListFoundryLoads"):      checkWorkspace,
	foundry("GetFoundryLoad"):        checkWorkspace,
	foundry("SetCurrentFoundryLoad"): checkOntology,
	foundry("ReadFoundryGraph"):      checkOntology,
	foundry("DeleteFoundryLoad"):     checkOntology,

	foundry("AskFoundryQuestion"):        checkOntology,
	foundry("ListFoundryQuestions"):      checkOntology,
	foundry("GetFoundryQuestion"):        checkOntology,
	foundry("ListFoundryQuestionEvents"): checkOntology,
	foundry("CancelFoundryQuestion"):     checkOntology,

	foundry("SettleFoundryReview"):         checkWorkspace,
	foundry("ListFoundryReviewDecisions"):  checkOntology,
	foundry("DeleteFoundryReviewDecision"): checkWorkspace,
}
