package access

import foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"

// Rule is what one of Foundry's methods lets a grantee do. The zero Rule is
// Deny, so a method the table doesn't name refuses every grantee.
type Rule uint8

const (
	// Deny refuses a grantee.
	Deny Rule = iota
	// Workspace admits a grantee calling for its own workspace. The handler
	// answers only what the grantee covers.
	Workspace
	// Ontology admits a grantee calling for its own workspace on an ontology
	// whose current load it covers. The handler narrows what it answers
	// further, to that load.
	Ontology
	// Workflows admits a grantee as Ontology does while its Workflows
	// permission is on: neither it nor any of its teams has it off. The
	// handler narrows the runs it answers to those the grantee started,
	// unless it holds all of the ontology's current load.
	Workflows
)

// rules names every method of FoundryService and FoundryEventsService, so a
// method added without a rule fails the package's tests. A grantee only
// reads, asks and cancels its own questions, talks to Founder in its own
// threads, and starts and finishes its own workflow runs: it changes nothing
// else.
var rules = map[string]Rule{
	// Founder: a grantee talks to Founder in its own threads, from its
	// restriction on; Founder only reads and asks for it (the turn's Reach),
	// so a grantee's threads hold no cards to run, refresh or dismiss.
	foundryv1.FoundryService_SendFoundryAgentMessage_FullMethodName:    Workspace,
	foundryv1.FoundryService_ListFoundryAgentThreads_FullMethodName:    Workspace,
	foundryv1.FoundryService_GetFoundryAgentThread_FullMethodName:      Workspace,
	foundryv1.FoundryService_DeleteFoundryAgentThread_FullMethodName:   Workspace,
	foundryv1.FoundryService_ListFoundryAgentTurnEvents_FullMethodName: Workspace,
	foundryv1.FoundryService_CancelFoundryAgentTurn_FullMethodName:     Workspace,
	foundryv1.FoundryService_ExecuteFoundryAgentAction_FullMethodName:  Deny,
	foundryv1.FoundryService_RefreshFoundryAgentAction_FullMethodName:  Deny,
	foundryv1.FoundryService_DismissFoundryAgentAction_FullMethodName:  Deny,
	// Where a local install's Assistant runs: its owner's.
	foundryv1.FoundryService_GetFoundryAgentRuntime_FullMethodName:        Deny,
	foundryv1.FoundryService_UpdateFoundryAgentRuntime_FullMethodName:     Deny,
	foundryv1.FoundryService_CreateFoundryAgentHostPairing_FullMethodName: Deny,
	foundryv1.FoundryService_RevokeFoundryAgentHost_FullMethodName:        Deny,

	// Source grants, restricted members and teams: managed by owners and
	// admins; a grantee reads its own.
	foundryv1.FoundryService_ListFoundryAccess_FullMethodName:          Deny,
	foundryv1.FoundryService_SetFoundryAccessGrants_FullMethodName:     Deny,
	foundryv1.FoundryService_PreviewFoundryAccessChange_FullMethodName: Deny,
	foundryv1.FoundryService_GetMyFoundryAccess_FullMethodName:         Workspace,
	foundryv1.FoundryService_SetFoundryMemberRestricted_FullMethodName: Deny,
	foundryv1.FoundryService_CreateFoundryTeam_FullMethodName:          Deny,
	foundryv1.FoundryService_UpdateFoundryTeam_FullMethodName:          Deny,
	foundryv1.FoundryService_DeleteFoundryTeam_FullMethodName:          Deny,

	// Ontologies, versions, generations and loads.
	foundryv1.FoundryService_ListFoundryOntologies_FullMethodName:     Workspace,
	foundryv1.FoundryService_GetFoundryOntology_FullMethodName:        Ontology,
	foundryv1.FoundryService_CreateFoundryOntology_FullMethodName:     Deny,
	foundryv1.FoundryService_UpdateFoundryOntology_FullMethodName:     Deny,
	foundryv1.FoundryService_DeleteFoundryOntology_FullMethodName:     Deny,
	foundryv1.FoundryService_GetCurrentOntologyVersion_FullMethodName: Deny,
	foundryv1.FoundryService_GetOntologyVersion_FullMethodName:        Ontology,
	foundryv1.FoundryService_ListOntologyVersions_FullMethodName:      Deny,
	foundryv1.FoundryService_CreateOntologyVersion_FullMethodName:     Deny,
	foundryv1.FoundryService_SetCurrentOntologyVersion_FullMethodName: Deny,
	foundryv1.FoundryService_ReviseOntologyVersion_FullMethodName:     Deny,
	foundryv1.FoundryService_GenerateOntology_FullMethodName:          Deny,
	foundryv1.FoundryService_AnswerOntologyGeneration_FullMethodName:  Deny,
	foundryv1.FoundryService_CancelOntologyGeneration_FullMethodName:  Deny,
	foundryv1.FoundryService_GetOntologyGeneration_FullMethodName:     Deny,
	foundryv1.FoundryService_ListOntologyGenerations_FullMethodName:   Deny,
	foundryv1.FoundryService_CreateFoundryLoad_FullMethodName:         Deny,
	foundryv1.FoundryService_GetFoundryLoad_FullMethodName:            Ontology,
	foundryv1.FoundryService_ListFoundryLoads_FullMethodName:          Ontology,
	foundryv1.FoundryService_SetCurrentFoundryLoad_FullMethodName:     Deny,
	foundryv1.FoundryService_DeleteFoundryLoad_FullMethodName:         Deny,
	foundryv1.FoundryService_StopFoundryLoadEnrichment_FullMethodName: Deny,

	// Self-healing findings: drawn from the whole load, never narrowed to
	// what a grantee holds, and dismissing one changes them.
	foundryv1.FoundryService_ListFoundryHealFindings_FullMethodName:   Deny,
	foundryv1.FoundryService_DismissFoundryHealFinding_FullMethodName: Deny,

	// Workflows: a grantee with the Workflows permission lists an ontology's
	// workflows and records its own runs; it reads every run of an ontology
	// whose current load it holds all of, else only the runs it started.
	// The workflows Foundry makes for automations, which run with their
	// owners' access, it never sees, as it sees no automation.
	// Making, deleting and assessing them again stay with owners and admins.
	foundryv1.FoundryService_CreateFoundryWorkflow_FullMethodName:          Deny,
	foundryv1.FoundryService_ListFoundryWorkflows_FullMethodName:           Workflows,
	foundryv1.FoundryService_GetFoundryWorkflow_FullMethodName:             Workflows,
	foundryv1.FoundryService_DeleteFoundryWorkflow_FullMethodName:          Deny,
	foundryv1.FoundryService_StartFoundryWorkflowRun_FullMethodName:        Workflows,
	foundryv1.FoundryService_ListFoundryWorkflowRuns_FullMethodName:        Workflows,
	foundryv1.FoundryService_GetFoundryWorkflowRun_FullMethodName:          Workflows,
	foundryv1.FoundryService_FinishFoundryWorkflowRun_FullMethodName:       Workflows,
	foundryv1.FoundryService_ListFoundryWorkflowRunAccesses_FullMethodName: Workflows,
	foundryv1.FoundryService_GetFoundryWorkflowInsights_FullMethodName:     Workflows,
	foundryv1.FoundryService_AssessFoundryWorkflowRun_FullMethodName:       Deny,

	// Reading a load and asking it questions.
	foundryv1.FoundryService_ReadFoundryGraph_FullMethodName:          Ontology,
	foundryv1.FoundryService_AskFoundryQuestion_FullMethodName:        Ontology,
	foundryv1.FoundryService_GetFoundryQuestion_FullMethodName:        Ontology,
	foundryv1.FoundryService_ListFoundryQuestions_FullMethodName:      Ontology,
	foundryv1.FoundryService_ListFoundryQuestionEvents_FullMethodName: Ontology,
	foundryv1.FoundryService_CancelFoundryQuestion_FullMethodName:     Ontology,

	// Review decisions.
	foundryv1.FoundryService_ListFoundryReviewDecisions_FullMethodName:  Deny,
	foundryv1.FoundryService_ListFoundryReviews_FullMethodName:          Deny,
	foundryv1.FoundryService_SettleFoundryReview_FullMethodName:         Deny,
	foundryv1.FoundryService_DeleteFoundryReviewDecision_FullMethodName: Deny,
	foundryv1.FoundryService_SettleFoundryReviewLabel_FullMethodName:    Deny,
	foundryv1.FoundryService_ListFoundryReviewLabels_FullMethodName:     Deny,

	// Documents and their sets.
	foundryv1.FoundryService_ListFoundryDocuments_FullMethodName:                  Workspace,
	foundryv1.FoundryService_GetFoundryDocument_FullMethodName:                    Deny,
	foundryv1.FoundryService_GetFoundryDocumentContent_FullMethodName:             Workspace,
	foundryv1.FoundryService_GetFoundryDocumentBytes_FullMethodName:               Deny,
	foundryv1.FoundryService_CreateFoundryDocument_FullMethodName:                 Deny,
	foundryv1.FoundryService_CompleteFoundryDocumentUpload_FullMethodName:         Deny,
	foundryv1.FoundryService_UpdateFoundryDocument_FullMethodName:                 Deny,
	foundryv1.FoundryService_MoveFoundryDocument_FullMethodName:                   Deny,
	foundryv1.FoundryService_DeleteFoundryDocument_FullMethodName:                 Deny,
	foundryv1.FoundryService_ListFoundryDocumentSets_FullMethodName:               Deny,
	foundryv1.FoundryService_CreateFoundryDocumentSet_FullMethodName:              Deny,
	foundryv1.FoundryService_UpdateFoundryDocumentSet_FullMethodName:              Deny,
	foundryv1.FoundryService_DeleteFoundryDocumentSet_FullMethodName:              Deny,
	foundryv1.FoundryService_ListFoundryOntologyDocuments_FullMethodName:          Ontology,
	foundryv1.FoundryService_GetFoundryOntologyDocument_FullMethodName:            Ontology,
	foundryv1.FoundryService_CreateFoundryOntologyDocument_FullMethodName:         Deny,
	foundryv1.FoundryService_CompleteFoundryOntologyDocumentUpload_FullMethodName: Deny,
	foundryv1.FoundryService_MoveFoundryOntologyDocument_FullMethodName:           Deny,
	foundryv1.FoundryService_DeleteFoundryOntologyDocument_FullMethodName:         Deny,
	foundryv1.FoundryService_ListFoundryOntologyDocumentSets_FullMethodName:       Deny,
	foundryv1.FoundryService_CreateFoundryOntologyDocumentSet_FullMethodName:      Deny,
	foundryv1.FoundryService_UpdateFoundryOntologyDocumentSet_FullMethodName:      Deny,
	foundryv1.FoundryService_DeleteFoundryOntologyDocumentSet_FullMethodName:      Deny,
	foundryv1.FoundryService_GetFoundryDataLayout_FullMethodName:                  Deny,
	foundryv1.FoundryService_PutFoundryDataLayout_FullMethodName:                  Deny,

	// Connectors, their syncs and sign-ins, and event sources.
	foundryv1.FoundryService_ListConnectorKinds_FullMethodName:                    Deny,
	foundryv1.FoundryService_ListConnectors_FullMethodName:                        Deny,
	foundryv1.FoundryService_GetConnector_FullMethodName:                          Deny,
	foundryv1.FoundryService_TestConnector_FullMethodName:                         Deny,
	foundryv1.FoundryService_CreateConnector_FullMethodName:                       Deny,
	foundryv1.FoundryService_UpdateConnector_FullMethodName:                       Deny,
	foundryv1.FoundryService_DeleteConnector_FullMethodName:                       Deny,
	foundryv1.FoundryService_SyncConnector_FullMethodName:                         Deny,
	foundryv1.FoundryService_GetConnectorSync_FullMethodName:                      Deny,
	foundryv1.FoundryService_ListConnectorSyncs_FullMethodName:                    Deny,
	foundryv1.FoundryService_StartConnectorSignIn_FullMethodName:                  Deny,
	foundryv1.FoundryService_GetConnectorSignIn_FullMethodName:                    Deny,
	foundryv1.FoundryService_AuthorizeConnectorSignIn_FullMethodName:              Deny,
	foundryv1.FoundryService_FinishConnectorSignIn_FullMethodName:                 Deny,
	foundryv1.FoundryService_CreateFoundryEventSourceToken_FullMethodName:         Deny,
	foundryv1.FoundryService_RevokeFoundryEventSourceToken_FullMethodName:         Deny,
	foundryv1.FoundryService_ListFoundryEventSourceTokens_FullMethodName:          Deny,
	foundryv1.FoundryService_ListFoundryEventBatches_FullMethodName:               Deny,
	foundryv1.FoundryEventsService_SendFoundryEvents_FullMethodName:               Deny,
	foundryv1.FoundryEventsService_ValidateFoundryEventSourceToken_FullMethodName: Deny,

	// The landing page's reminders: the site's, through the door, of no
	// workspace.
	foundryv1.FoundryEventsService_CreateFoundryReminder_FullMethodName: Deny,
	foundryv1.FoundryEventsService_StopFoundryReminders_FullMethodName:  Deny,

	// Dashboards: a grantee sees the pinned ones, whose tiles it reads
	// through ReadFoundryGraph as its grants allow, and changes none. A
	// dashboard's public snapshot is the door's, for anyone with its link.
	foundryv1.FoundryService_ListFoundryDashboards_FullMethodName:             Workspace,
	foundryv1.FoundryService_GetFoundryDashboard_FullMethodName:               Workspace,
	foundryv1.FoundryService_CreateFoundryDashboard_FullMethodName:            Deny,
	foundryv1.FoundryService_UpdateFoundryDashboard_FullMethodName:            Deny,
	foundryv1.FoundryService_DeleteFoundryDashboard_FullMethodName:            Deny,
	foundryv1.FoundryService_PinFoundryDashboard_FullMethodName:               Deny,
	foundryv1.FoundryService_AddFoundryDashboardQuestionVisual_FullMethodName: Deny,
	foundryv1.FoundryService_PreviewFoundryDashboardTile_FullMethodName:       Deny,
	foundryv1.FoundryService_PublishFoundryDashboard_FullMethodName:           Deny,
	foundryv1.FoundryService_UnpublishFoundryDashboard_FullMethodName:         Deny,
	foundryv1.FoundryEventsService_GetSharedFoundryDashboard_FullMethodName:   Deny,
	foundryv1.FoundryService_GetFoundryDashboardSettings_FullMethodName:       Deny,
	foundryv1.FoundryService_UpdateFoundryDashboardSettings_FullMethodName:    Deny,
	// Automations are people's, run as their owners: a grantee has none.
	foundryv1.FoundryService_ListFoundryAutomations_FullMethodName:           Deny,
	foundryv1.FoundryService_GetFoundryAutomation_FullMethodName:             Deny,
	foundryv1.FoundryService_CreateFoundryAutomation_FullMethodName:          Deny,
	foundryv1.FoundryService_UpdateFoundryAutomation_FullMethodName:          Deny,
	foundryv1.FoundryService_DeleteFoundryAutomation_FullMethodName:          Deny,
	foundryv1.FoundryService_ApproveFoundryAutomation_FullMethodName:         Deny,
	foundryv1.FoundryService_PauseFoundryAutomation_FullMethodName:           Deny,
	foundryv1.FoundryService_ResumeFoundryAutomation_FullMethodName:          Deny,
	foundryv1.FoundryService_RunFoundryAutomation_FullMethodName:             Deny,
	foundryv1.FoundryService_ListFoundryAutomationRuns_FullMethodName:        Deny,
	foundryv1.FoundryService_GetFoundryAutomationRun_FullMethodName:          Deny,
	foundryv1.FoundryService_CancelFoundryAutomationRun_FullMethodName:       Deny,
	foundryv1.FoundryService_ListFoundryAutomationSuggestions_FullMethodName: Deny,

	// The workspace's settings, plan and spend.
	foundryv1.FoundryService_GetFoundryAI_FullMethodName:             Deny,
	foundryv1.FoundryService_UpdateFoundryAI_FullMethodName:          Deny,
	foundryv1.FoundryService_DetectFoundryAIDefault_FullMethodName:   Deny,
	foundryv1.FoundryService_ListFoundryOllamaModels_FullMethodName:  Deny,
	foundryv1.FoundryService_GetFoundryEnrichment_FullMethodName:     Deny,
	foundryv1.FoundryService_UpdateFoundryEnrichment_FullMethodName:  Deny,
	foundryv1.FoundryService_GetFoundryPlan_FullMethodName:           Deny,
	foundryv1.FoundryService_ListFoundryPlans_FullMethodName:         Deny,
	foundryv1.FoundryService_CreateFoundryProCheckout_FullMethodName: Deny,
	foundryv1.FoundryService_CancelFoundryPro_FullMethodName:         Deny,
	foundryv1.FoundryService_ResumeFoundryPro_FullMethodName:         Deny,
	foundryv1.FoundryService_GetFoundryUsage_FullMethodName:          Deny,
}

// RuleOf is what method, a full gRPC method name, lets a grantee do: Deny
// for a method the table doesn't name.
//
//	access.RuleOf(foundryv1.FoundryService_DeleteFoundryOntology_FullMethodName) // access.Deny
func RuleOf(method string) Rule { return rules[method] }

// GranteeAdmits reports whether method admits a grantee at all. A front end
// whose own permissions refuse a caller may send the call on as a grantee
// only to such a method.
func GranteeAdmits(method string) bool { return RuleOf(method) != Deny }
