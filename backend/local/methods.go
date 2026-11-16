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
// of each: the directory, and what the Foundry site and the agents' tools
// (package integration) call of Foundry's API.
// The rest need what an install has not, so they are refused before a
// handler runs (unsupported); a test holds every method of every service to
// being listed here or refused for a reason. It fails closed: a new method is
// refused until it is listed.
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

	foundry("StartConnectorSignIn"):     checkWorkspace,
	foundry("GetConnectorSignIn"):       checkWorkspace,
	foundry("AuthorizeConnectorSignIn"): checkWorkspace,
	foundry("FinishConnectorSignIn"):    checkWorkspace,

	foundry("ListFoundryOntologies"):                 checkWorkspace,
	foundry("CreateFoundryOntology"):                 checkTenant,
	foundry("GetFoundryOntology"):                    checkWorkspace,
	foundry("DeleteFoundryOntology"):                 checkOntology,
	foundry("ListOntologyVersions"):                  checkWorkspace,
	foundry("GetCurrentOntologyVersion"):             checkWorkspace,
	foundry("CreateFoundryOntologyDocument"):         checkOntology,
	foundry("CompleteFoundryOntologyDocumentUpload"): checkOntology,
	foundry("CreateFoundryOntologyDocumentSet"):      checkOntology,
	foundry("UpdateFoundryOntologyDocumentSet"):      checkOntology,
	foundry("MoveFoundryOntologyDocument"):           checkOntology,
	foundry("GetOntologyVersion"):                    checkWorkspace,
	foundry("SetCurrentOntologyVersion"):             checkWorkspace,
	foundry("ReviseOntologyVersion"):                 checkWorkspace,
	foundry("GenerateOntology"):                      checkWorkspace,
	foundry("ListOntologyGenerations"):               checkWorkspace,
	foundry("GetOntologyGeneration"):                 checkWorkspace,
	foundry("AnswerOntologyGeneration"):              checkWorkspace,
	foundry("CancelOntologyGeneration"):              checkWorkspace,

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
	// It finds its default on this machine, and reads its Ollama servers'
	// models.
	foundry("DetectFoundryAIDefault"):  checkWorkspace,
	foundry("ListFoundryOllamaModels"): checkWorkspace,

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
	foundry("ListFoundryReviews"):          checkOntology,
	foundry("DeleteFoundryReviewDecision"): checkWorkspace,
	foundry("SettleFoundryReviewLabel"):    checkWorkspace,
	foundry("ListFoundryReviewLabels"):     checkOntology,

	foundry("ListFoundryHealFindings"):   checkOntology,
	foundry("DismissFoundryHealFinding"): checkWorkspace,

	// The Assistant: each handler reads the owner's own thread, turn or action.
	foundry("SendFoundryAgentMessage"):    checkWorkspace,
	foundry("ListFoundryAgentThreads"):    checkWorkspace,
	foundry("GetFoundryAgentThread"):      checkWorkspace,
	foundry("DeleteFoundryAgentThread"):   checkWorkspace,
	foundry("ListFoundryAgentTurnEvents"): checkWorkspace,
	foundry("CancelFoundryAgentTurn"):     checkWorkspace,
	foundry("ExecuteFoundryAgentAction"):  checkWorkspace,
	foundry("RefreshFoundryAgentAction"):  checkWorkspace,
	foundry("DismissFoundryAgentAction"):  checkWorkspace,
	// Where the Assistant runs, and the helpers that run it on the owner's
	// subscription (agent_host.go).
	foundry("GetFoundryAgentRuntime"):        checkWorkspace,
	foundry("UpdateFoundryAgentRuntime"):     checkWorkspace,
	foundry("CreateFoundryAgentHostPairing"): checkWorkspace,
	foundry("RevokeFoundryAgentHost"):        checkWorkspace,
	// The caller's source grants, which every Founder attempt reads for its
	// Reach (agent.reachOf): the owner is never a grantee, so it answers so.
	foundry("GetMyFoundryAccess"): checkWorkspace,

	// Dashboards: each handler reads the workspace's own. An install
	// publishes no public link (no door serves one).
	foundry("ListFoundryDashboards"):             checkWorkspace,
	foundry("GetFoundryDashboard"):               checkWorkspace,
	foundry("CreateFoundryDashboard"):            checkWorkspace,
	foundry("UpdateFoundryDashboard"):            checkWorkspace,
	foundry("DeleteFoundryDashboard"):            checkWorkspace,
	foundry("PinFoundryDashboard"):               checkWorkspace,
	foundry("AddFoundryDashboardQuestionVisual"): checkWorkspace,
	foundry("PreviewFoundryDashboardTile"):       checkWorkspace,
	// Automations: each handler reads the workspace's own, and runs and
	// changes only the owner's.
	foundry("ListFoundryAutomations"):     checkWorkspace,
	foundry("GetFoundryAutomation"):       checkWorkspace,
	foundry("CreateFoundryAutomation"):    checkWorkspace,
	foundry("UpdateFoundryAutomation"):    checkWorkspace,
	foundry("DeleteFoundryAutomation"):    checkWorkspace,
	foundry("ApproveFoundryAutomation"):   checkWorkspace,
	foundry("PauseFoundryAutomation"):     checkWorkspace,
	foundry("ResumeFoundryAutomation"):    checkWorkspace,
	foundry("RunFoundryAutomation"):       checkWorkspace,
	foundry("ListFoundryAutomationRuns"):  checkWorkspace,
	foundry("GetFoundryAutomationRun"):    checkWorkspace,
	foundry("CancelFoundryAutomationRun"): checkWorkspace,

	foundry("CreateFoundryWorkflow"):          checkOntology,
	foundry("ListFoundryWorkflows"):           checkOntology,
	foundry("GetFoundryWorkflow"):             checkOntology,
	foundry("DeleteFoundryWorkflow"):          checkOntology,
	foundry("StartFoundryWorkflowRun"):        checkOntology,
	foundry("ListFoundryWorkflowRuns"):        checkOntology,
	foundry("GetFoundryWorkflowRun"):          checkOntology,
	foundry("FinishFoundryWorkflowRun"):       checkOntology,
	foundry("ListFoundryWorkflowRunAccesses"): checkOntology,
	foundry("GetFoundryWorkflowInsights"):     checkOntology,
	foundry("AssessFoundryWorkflowRun"):       checkOntology,
}
