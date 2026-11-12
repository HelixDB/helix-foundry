package model

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/access"
)

// Workflows' caps, which the API and the store's checks hold.
const (
	// FoundryWorkflowsMax is how many workflows an ontology may have.
	FoundryWorkflowsMax = 100
	// FoundryWorkflowNameMax and FoundryWorkflowDescriptionMax are in
	// characters.
	FoundryWorkflowNameMax        = 100
	FoundryWorkflowDescriptionMax = 2000
	// FoundryWorkflowRunsKept is how many runs a workflow keeps: starting one
	// past it deletes the oldest.
	FoundryWorkflowRunsKept = 1000
	// FoundryWorkflowRunMaxAccesses is how many calls one run records.
	FoundryWorkflowRunMaxAccesses = 1000
	// FoundryWorkflowInputMaxBytes and FoundryWorkflowOutputMaxBytes bound a
	// run's request and answer.
	FoundryWorkflowInputMaxBytes  = 64 << 10
	FoundryWorkflowOutputMaxBytes = 256 << 10
	// FoundryWorkflowResultMaxBytes is the largest read result a run stores,
	// and FoundryWorkflowRunResultMaxBytes all its read results together.
	// A read past either is recorded without its result.
	FoundryWorkflowResultMaxBytes    = 256 << 10
	FoundryWorkflowRunResultMaxBytes = 4 << 20
)

// FoundryWorkflow is one row of foundry_workflows, with its runs counted.
type FoundryWorkflow struct {
	ID, WorkspaceID, OntologyID string
	Name, Description           string
	ClientToken                 string
	CreatedByUserID             string
	CreatedAt                   time.Time
	// Runs and FinishedRuns count the runs it keeps; LastRunAt is when its
	// newest started, nil before any.
	Runs, FinishedRuns int64
	LastRunAt          *time.Time
}

// FoundryWorkflowRunStatus is where a run stands.
type FoundryWorkflowRunStatus string

const (
	// FoundryWorkflowRunOpen records the calls made with its id.
	FoundryWorkflowRunOpen FoundryWorkflowRunStatus = "open"
	// FoundryWorkflowRunFinished has its answer and records nothing more.
	FoundryWorkflowRunFinished FoundryWorkflowRunStatus = "finished"
)

// FoundryWorkflowRunTally is what one principal's runs of a workflow come
// to: a FoundryWorkflow's counts and its verdicts, over those runs alone.
type FoundryWorkflowRunTally struct {
	Runs, FinishedRuns int64
	LastRunAt          *time.Time
	// Verdicts counts their done assessments by verdict.
	Verdicts map[string]int64
}

// FoundryWorkflowRun is one row of foundry_workflow_runs.
type FoundryWorkflowRun struct {
	ID, WorkflowID, WorkspaceID, OntologyID string
	ClientToken                             string
	// StartedBy is a user or an agent's service credential.
	StartedBy access.Principal
	StartedAt time.Time
	// Input is its request; nil until sent.
	Input *string
	// Finished is its end; nil while it is open.
	Finished *FoundryWorkflowRunEnd
	Counts   FoundryWorkflowRunCounts
}

// FoundryWorkflowRunEnd is how a run finished.
type FoundryWorkflowRunEnd struct {
	At     time.Time
	Output string
}

// Status is open until the run has its end.
//
//	(&FoundryWorkflowRun{}).Status() // FoundryWorkflowRunOpen
func (r *FoundryWorkflowRun) Status() FoundryWorkflowRunStatus {
	if r.Finished == nil {
		return FoundryWorkflowRunOpen
	}
	return FoundryWorkflowRunFinished
}

// FoundryWorkflowRunCounts sums a run's accesses as they are recorded.
type FoundryWorkflowRunCounts struct {
	Accesses, Reads, Questions, Sends, Uploads int32
	// Pieces is what its sends carried; ResultsOmitted its reads stored
	// without their result; ResultBytes the read results it stores.
	Pieces, ResultsOmitted int32
	ResultBytes            int64
}

// FoundryWorkflowAccessKind names what an access recorded, as the store
// keeps it.
type FoundryWorkflowAccessKind string

const (
	FoundryWorkflowAccessRead     FoundryWorkflowAccessKind = "read"
	FoundryWorkflowAccessQuestion FoundryWorkflowAccessKind = "question"
	FoundryWorkflowAccessSend     FoundryWorkflowAccessKind = "send"
	FoundryWorkflowAccessUpload   FoundryWorkflowAccessKind = "upload"
)

// FoundryWorkflowAccess is one call a run recorded, by its place in the run.
type FoundryWorkflowAccess struct {
	Seq  int32
	At   time.Time
	Call FoundryWorkflowCall
}

// FoundryWorkflowCall is what an access recorded: one of
// *FoundryWorkflowRead, *FoundryWorkflowQuestion, *FoundryWorkflowSend and
// *FoundryWorkflowUpload.
type FoundryWorkflowCall interface {
	// Kind is the call's kind.
	Kind() FoundryWorkflowAccessKind
	// DedupeKey is what a retry of the call is recorded under once; empty
	// for a call that is its own each time (a read).
	DedupeKey() string
}

// FoundryWorkflowRead is a read of a load and what it returned.
type FoundryWorkflowRead struct {
	LoadID string          `json:"load_id"`
	Reads  json.RawMessage `json:"reads"`
	// Result is what it returned: given whole when recorded, and nil when
	// read back if it was left out (FoundryWorkflowResultMaxBytes,
	// FoundryWorkflowRunResultMaxBytes). The store keeps it apart.
	Result json.RawMessage `json:"-"`
}

// FoundryWorkflowQuestion is a question asked of a load.
type FoundryWorkflowQuestion struct {
	LoadID     string `json:"load_id"`
	QuestionID string `json:"question_id"`
}

// FoundryWorkflowSend is a send to an API source.
type FoundryWorkflowSend struct {
	ConnectorID string   `json:"connector_id"`
	Seq         int64    `json:"seq"`
	PieceIDs    []string `json:"piece_ids"`
}

// FoundryWorkflowUpload is a document whose upload completed.
type FoundryWorkflowUpload struct {
	DocumentID string `json:"document_id"`
	SetID      string `json:"set_id"`
}

func (*FoundryWorkflowRead) Kind() FoundryWorkflowAccessKind { return FoundryWorkflowAccessRead }
func (*FoundryWorkflowQuestion) Kind() FoundryWorkflowAccessKind {
	return FoundryWorkflowAccessQuestion
}
func (*FoundryWorkflowSend) Kind() FoundryWorkflowAccessKind   { return FoundryWorkflowAccessSend }
func (*FoundryWorkflowUpload) Kind() FoundryWorkflowAccessKind { return FoundryWorkflowAccessUpload }

func (*FoundryWorkflowRead) DedupeKey() string       { return "" }
func (q *FoundryWorkflowQuestion) DedupeKey() string { return "question:" + q.QuestionID }
func (s *FoundryWorkflowSend) DedupeKey() string {
	return fmt.Sprintf("send:%s:%d", s.ConnectorID, s.Seq)
}
func (u *FoundryWorkflowUpload) DedupeKey() string { return "upload:" + u.DocumentID }

// ParseFoundryWorkflowCall reads an access's detail as the store keeps it,
// by its kind; result is a read's stored result (nil when left out) and
// nothing else's.
//
//	call, err := ParseFoundryWorkflowCall("upload", []byte(`{"document_id":"d","set_id":"s"}`), nil)
//	// call.(*FoundryWorkflowUpload).DocumentID == "d", err == nil
func ParseFoundryWorkflowCall(kind string, detail, result []byte) (FoundryWorkflowCall, error) {
	var call FoundryWorkflowCall
	switch FoundryWorkflowAccessKind(kind) {
	case FoundryWorkflowAccessRead:
		call = &FoundryWorkflowRead{Result: result}
	case FoundryWorkflowAccessQuestion:
		call = &FoundryWorkflowQuestion{}
	case FoundryWorkflowAccessSend:
		call = &FoundryWorkflowSend{}
	case FoundryWorkflowAccessUpload:
		call = &FoundryWorkflowUpload{}
	default:
		return nil, fmt.Errorf("unknown workflow access kind %q", kind)
	}
	if err := json.Unmarshal(detail, call); err != nil {
		return nil, fmt.Errorf("workflow access %s: %w", kind, err)
	}
	return call, nil
}

// FoundryWorkflowAssessmentStatus is where a run's assessment stands.
type FoundryWorkflowAssessmentStatus string

const (
	// FoundryWorkflowAssessmentQueued waits for a worker, from NextAttemptAt.
	FoundryWorkflowAssessmentQueued FoundryWorkflowAssessmentStatus = "queued"
	// FoundryWorkflowAssessmentRunning is leased to a worker.
	FoundryWorkflowAssessmentRunning FoundryWorkflowAssessmentStatus = "running"
	// FoundryWorkflowAssessmentDone holds the engine's assessment.
	FoundryWorkflowAssessmentDone FoundryWorkflowAssessmentStatus = "done"
	// FoundryWorkflowAssessmentFailed ended without one, saying why.
	FoundryWorkflowAssessmentFailed FoundryWorkflowAssessmentStatus = "failed"
	// FoundryWorkflowAssessmentSkipped wasn't run: the workspace's month of
	// assessments was spent.
	FoundryWorkflowAssessmentSkipped FoundryWorkflowAssessmentStatus = "skipped"
)

// Live is whether the assessment may still change without being queued
// again.
//
//	FoundryWorkflowAssessmentRunning.Live() // true
func (s FoundryWorkflowAssessmentStatus) Live() bool {
	return s == FoundryWorkflowAssessmentQueued || s == FoundryWorkflowAssessmentRunning
}

// FoundryWorkflowVerdicts are the engine's verdicts (assess_result.json
// Verdict), in the order the site lists them.
var FoundryWorkflowVerdicts = []string{"essential", "helpful", "marginal", "unused", "misleading"}

// FoundryWorkflowAssessment is one row of foundry_workflow_run_assessments.
type FoundryWorkflowAssessment struct {
	RunID, WorkspaceID, OntologyID, WorkflowID string
	Status                                     FoundryWorkflowAssessmentStatus
	Attempts                                   int
	// Verdict and Assessment are the engine's, once done; LeftOut the calls
	// whose results didn't fit the model.
	Verdict    string
	Assessment json.RawMessage
	LeftOut    []int32
	// Failure is the engine's ModelFailure of a failed one; Error why it
	// failed or was skipped, for a person.
	Failure, Error string
	USDMicros      int64
	QueuedAt       time.Time
	FinishedAt     *time.Time
}

// FoundryWorkflowAssessmentOutcome is how an attempt of an assessment
// ended: Done with the engine's assessment, Failed for good, Retry at
// RetryAt, or Skipped, the month's budget spent. USDMicros is what the
// attempt spent, whatever it came to.
type FoundryWorkflowAssessmentOutcome struct {
	Status     FoundryWorkflowAssessmentStatus
	Verdict    string
	Assessment json.RawMessage
	LeftOut    []int32
	Failure    string
	Error      string
	USDMicros  int64
	// RetryAt is when a queued (retried) one is tried again.
	RetryAt time.Time
	// Uncounted gives the attempt back: it couldn't run for a reason of
	// Helix's (an ontology agent not serving Assess yet), so it doesn't
	// count towards the attempts a failure gets.
	Uncounted bool
}

// Check holds the outcome to the shapes the table takes.
//
//	FoundryWorkflowAssessmentOutcome{Status: FoundryWorkflowAssessmentRunning}.Check() // an error
func (o FoundryWorkflowAssessmentOutcome) Check() error {
	switch {
	case o.USDMicros < 0:
		return fmt.Errorf("an assessment spends no less than nothing")
	case o.Uncounted && o.Status != FoundryWorkflowAssessmentQueued:
		return fmt.Errorf("only an assessment queued again gives its attempt back")
	case o.Status == FoundryWorkflowAssessmentDone && (len(o.Assessment) == 0 || !slices.Contains(FoundryWorkflowVerdicts, o.Verdict)):
		return fmt.Errorf("a done assessment has an assessment and a verdict, not %q", o.Verdict)
	case o.Status == FoundryWorkflowAssessmentFailed || o.Status == FoundryWorkflowAssessmentSkipped || o.Status == FoundryWorkflowAssessmentQueued:
		if o.Error == "" {
			return fmt.Errorf("a %s assessment says why", o.Status)
		}
	case o.Status != FoundryWorkflowAssessmentDone:
		return fmt.Errorf("an attempt ends done, failed, skipped or queued again, not %s", o.Status)
	}
	return nil
}
