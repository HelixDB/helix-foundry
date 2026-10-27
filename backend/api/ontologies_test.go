package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/document"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// foundryStore adds tenants, projects, documents and loads to the ontology
// store. It keeps the provider's rules for which loads may be made current
// or deleted; the MySQL tests hold the rest.
type foundryStore struct {
	*ontologyStore
	// databases are the host's, by tenant id; projects the live projects.
	databases map[string]ports.Database
	projects  map[string]bool
	// draining are deleted ontologies still holding their database.
	draining map[string]bool
	// sets are the live document sets, oldest first.
	sets      []*foundrymodel.FoundryDocumentSet
	documents []*foundrymodel.FoundryDocument
	// members are each load's documents.
	members map[string][]string
	// reads is what completing or moving a document answers; fans what they were handed.
	reads []foundrymodel.FoundryDocumentRead
	fans  []foundrymodel.FoundryDocumentFanOut
	// layouts are the Data layouts, by workspace.
	layouts map[string]*foundrymodel.FoundryDataLayout
	// extractions are by load, then document.
	extractions map[string]map[string]*foundrymodel.FoundryDocumentExtraction
	loads       []*foundrymodel.FoundryLoad
	jobs        []*foundrymodel.FoundryGraphJob
	// deletesQueued are the delete job ids the store was handed.
	deletesQueued []string
	spent         int64
	// completions are the extract jobs SetCurrentFoundryLoad was handed.
	completions []*foundrymodel.FoundryGraphJob
	// resumeErr fails ResumeFoundryOntologyDrain, as a database outage does.
	resumeErr error
	// listErr fails every list, as a database outage does.
	listErr error
}

// listPage is how the fake lists answer a page token and an outage.
func (s *foundryStore) listPage(pageToken string) error {
	if pageToken == "bad" {
		return fmt.Errorf("%w: bad", foundrystore.ErrInvalidPageToken)
	}
	return s.listErr
}

func (s *foundryStore) GetFoundryOntologyByClientToken(_ context.Context, workspaceID, token string) (*foundrymodel.FoundryOntology, error) {
	for _, o := range s.ontologies {
		if o.WorkspaceID == workspaceID && o.ClientToken == token {
			return o, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *foundryStore) GetFoundryOntologyByTenant(_ context.Context, tenantID string) (*foundrymodel.FoundryOntology, error) {
	for _, o := range s.ontologies {
		if o.TenantID == tenantID && (o.DeletedAt == nil || s.draining[o.ID]) {
			copied := *o
			return &copied, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *foundryStore) CreateFoundryOntology(ctx context.Context, o *foundrymodel.FoundryOntology) error {
	if _, err := s.GetFoundryOntologyByTenant(ctx, o.TenantID); err == nil {
		return foundrystore.ErrConflict
	}
	for _, existing := range s.ontologies {
		if existing.WorkspaceID == o.WorkspaceID && existing.ClientToken == o.ClientToken {
			return foundrystore.ErrConflict
		}
	}
	copied := *o
	copied.CreatedAt = time.Now()
	s.ontologies[o.ID] = &copied
	return nil
}

// set finds a live set of the workspace.
func (s *foundryStore) set(workspaceID, id string) (*foundrymodel.FoundryDocumentSet, int) {
	for i, set := range s.sets {
		if set.ID == id && set.WorkspaceID == workspaceID {
			return set, i
		}
	}
	return nil, -1
}

// named reports a live set of the workspace no connector owns with this
// name, ignoring case, other than except.
func (s *foundryStore) named(workspaceID, name, except string) bool {
	for _, set := range s.sets {
		if set.WorkspaceID == workspaceID && set.ID != except && set.Owner == nil && strings.EqualFold(set.Name, name) {
			return true
		}
	}
	return false
}

func (s *foundryStore) CreateFoundryDocumentSet(ctx context.Context, set *foundrymodel.FoundryDocumentSet, max int) error {
	if set.WorkspaceID != "ws-1" && set.WorkspaceID != "ws-2" {
		return foundrystore.ErrNotFound
	}
	sets, _ := s.ListFoundryDocumentSets(ctx, set.WorkspaceID)
	sets = slices.DeleteFunc(sets, func(set *foundrymodel.FoundryDocumentSet) bool { return set.Owner != nil })
	switch {
	case len(sets) >= max:
		return foundrystore.ErrLimitExceeded
	case s.named(set.WorkspaceID, set.Name, ""):
		return foundrystore.ErrConflict
	}
	copied := *set
	copied.CreatedAt = time.Now()
	s.sets = append(s.sets, &copied)
	return nil
}

func (s *foundryStore) GetFoundryDocumentSet(_ context.Context, workspaceID, id string) (*foundrymodel.FoundryDocumentSet, error) {
	set, _ := s.set(workspaceID, id)
	if set == nil {
		return nil, foundrystore.ErrNotFound
	}
	copied := *set
	copied.Documents = s.setDocuments(set.ID)
	return &copied, nil
}

func (s *foundryStore) ListFoundryDocumentSets(ctx context.Context, workspaceID string) ([]*foundrymodel.FoundryDocumentSet, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []*foundrymodel.FoundryDocumentSet
	for _, set := range s.sets {
		if set.WorkspaceID == workspaceID {
			copied, _ := s.GetFoundryDocumentSet(ctx, workspaceID, set.ID)
			out = append(out, copied)
		}
	}
	return out, nil
}

// setDocuments counts a set's documents that are not deleted.
func (s *foundryStore) setDocuments(setID string) int {
	n := 0
	for _, d := range s.documents {
		if d.SetID == setID && d.Status != foundrymodel.FoundryDocumentDeleted {
			n++
		}
	}
	return n
}

func (s *foundryStore) UpdateFoundryDocumentSet(_ context.Context, workspaceID, id string, update foundrymodel.FoundryDocumentSetUpdate) error {
	set, _ := s.set(workspaceID, id)
	switch {
	case set == nil:
		return foundrystore.ErrNotFound
	case set.Owner != nil:
		return foundrystore.ErrConnectorOwned
	case update.Name != nil && s.named(workspaceID, *update.Name, id):
		return foundrystore.ErrConflict
	}
	if update.Name != nil {
		set.Name = *update.Name
	}
	if update.Description != nil {
		set.Description = *update.Description
	}
	return nil
}

func (s *foundryStore) DeleteFoundryDocumentSet(_ context.Context, workspaceID, id string) error {
	set, i := s.set(workspaceID, id)
	switch {
	case set == nil:
		return foundrystore.ErrNotFound
	case set.Owner != nil:
		return foundrystore.ErrConnectorOwned
	case s.setDocuments(id) > 0:
		return foundrystore.ErrNotEmpty
	}
	s.sets = append(s.sets[:i], s.sets[i+1:]...)
	return nil
}

func (s *foundryStore) MoveFoundryDocument(ctx context.Context, workspaceID, id, setID string, fan foundrymodel.FoundryDocumentFanOut) (
	*foundrymodel.FoundryDocument, []foundrymodel.FoundryDocumentRead, error) {
	switch set, _ := s.set(workspaceID, setID); {
	case set == nil:
		return nil, nil, foundrystore.ErrNotFound
	case set.Owner != nil:
		return nil, nil, foundrystore.ErrConnectorOwned
	}
	for _, d := range s.documents {
		if d.ID == id && d.WorkspaceID == workspaceID && d.Status != foundrymodel.FoundryDocumentDeleted {
			if d.Source != nil {
				return nil, nil, foundrystore.ErrConnectorOwned
			}
			d.SetID = setID
			s.fans = append(s.fans, fan)
			moved, err := s.GetFoundryDocument(ctx, workspaceID, id)
			return moved, s.reads, err
		}
	}
	return nil, nil, foundrystore.ErrNotFound
}

func (s *foundryStore) ListFoundryOntologies(_ context.Context, workspaceID, pageToken string, _ int) ([]*foundrymodel.FoundryOntology, string, error) {
	if err := s.listPage(pageToken); err != nil {
		return nil, "", err
	}
	var out []*foundrymodel.FoundryOntology
	for _, o := range s.ontologies {
		if o.WorkspaceID == workspaceID && o.DeletedAt == nil {
			out = append(out, o)
		}
	}
	return out, "", nil
}

// DeleteFoundryOntology cancels the live job and holds the database while
// the ontology's loads are deleted.
func (s *foundryStore) DeleteFoundryOntology(_ context.Context, workspaceID, id, deleteJobID string) error {
	o, ok := s.ontologies[id]
	if !ok || o.WorkspaceID != workspaceID || o.DeletedAt != nil {
		return foundrystore.ErrNotFound
	}
	for _, j := range s.jobs {
		if j.OntologyID == id && j.Status.Live() {
			j.Status = foundrymodel.FoundryGraphJobCancelled
		}
	}
	for _, l := range s.loads {
		if l.OntologyID == id && l.Status != foundrymodel.FoundryLoadDeleted {
			l.Status, s.draining[id] = foundrymodel.FoundryLoadDeleting, true
		}
	}
	if s.draining[id] {
		s.deletesQueued = append(s.deletesQueued, deleteJobID)
	}
	now := time.Now()
	o.DeletedAt, o.CurrentLoadID = &now, nil
	return nil
}

// ResumeFoundryOntologyDrain queues again the deletes of a deleted
// ontology's loads that failed for good, unless a job is live.
func (s *foundryStore) ResumeFoundryOntologyDrain(ctx context.Context, workspaceID, id, deleteJobID string) error {
	o, ok := s.ontologies[id]
	switch {
	case s.resumeErr != nil:
		return s.resumeErr
	case !ok || o.WorkspaceID != workspaceID || o.DeletedAt == nil:
		return foundrystore.ErrNotFound
	}
	if _, err := s.GetActiveFoundryGraphJob(ctx, id); err == nil {
		return nil
	}
	retried := false
	for _, l := range s.loads {
		if l.OntologyID == id && l.Status == foundrymodel.FoundryLoadDeleteFailed {
			l.Status, retried = foundrymodel.FoundryLoadDeleting, true
		}
	}
	if retried {
		s.deletesQueued = append(s.deletesQueued, deleteJobID)
	}
	return nil
}

func (s *foundryStore) GetLatestFoundryGraphJob(_ context.Context, ontologyID string, kind foundrymodel.FoundryGraphJobKind) (*foundrymodel.FoundryGraphJob, error) {
	for i := len(s.jobs) - 1; i >= 0; i-- {
		if s.jobs[i].OntologyID == ontologyID && s.jobs[i].Kind == kind {
			return s.jobs[i], nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *foundryStore) GetActiveFoundryGraphJob(_ context.Context, ontologyID string) (*foundrymodel.FoundryGraphJob, error) {
	for _, j := range s.jobs {
		if j.OntologyID == ontologyID && j.Status.Live() {
			return j, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *foundryStore) CountFoundryDocuments(_ context.Context, workspaceID string, pendingSince time.Time) (int, error) {
	n := 0
	for _, d := range s.documents {
		if d.WorkspaceID == workspaceID && d.Source == nil && (d.Status == foundrymodel.FoundryDocumentUploaded ||
			d.Status == foundrymodel.FoundryDocumentPendingUpload && !d.CreatedAt.Before(pendingSince)) {
			n++
		}
	}
	return n, nil
}

func (s *foundryStore) CountFoundryDocumentsInSets(_ context.Context, workspaceID string, setIDs []string) (int, error) {
	n := 0
	for _, d := range s.documents {
		if d.WorkspaceID == workspaceID && d.Status == foundrymodel.FoundryDocumentUploaded && slices.Contains(setIDs, d.SetID) {
			n++
		}
	}
	return n, nil
}

// CreateFoundryDocument keeps the provider's rules, its lock aside: the
// client token, then the bytes, find the workspace's document; an abandoned
// upload asked for again, and a new one, need a place under the cap.
func (s *foundryStore) CreateFoundryDocument(ctx context.Context, d *foundrymodel.FoundryDocument, limit foundrymodel.FoundryDocumentCap) (
	*foundrymodel.FoundryDocument, bool, error) {
	found, err := s.findDocument(func(e *foundrymodel.FoundryDocument) bool {
		return e.WorkspaceID == d.WorkspaceID && e.ClientToken != nil && d.ClientToken != nil && *e.ClientToken == *d.ClientToken
	})
	if err != nil {
		found, err = s.findDocument(func(e *foundrymodel.FoundryDocument) bool {
			return e.WorkspaceID == d.WorkspaceID && e.SHA256 == d.SHA256 && e.Status != foundrymodel.FoundryDocumentDeleted && e.Source == nil
		})
	}
	if err == nil {
		if n, _ := s.CountFoundryDocuments(ctx, d.WorkspaceID, limit.PendingSince); found.Status == foundrymodel.FoundryDocumentPendingUpload &&
			found.CreatedAt.Before(limit.PendingSince) && n >= limit.Max {
			return nil, false, foundrystore.ErrLimitExceeded
		}
		return found, true, nil
	}
	switch set, _ := s.set(d.WorkspaceID, d.SetID); {
	case set == nil:
		return nil, false, foundrystore.ErrNotFound
	case set.Owner != nil:
		return nil, false, foundrystore.ErrConnectorOwned
	}
	if n, _ := s.CountFoundryDocuments(ctx, d.WorkspaceID, limit.PendingSince); n >= limit.Max {
		return nil, false, foundrystore.ErrLimitExceeded
	}
	copied := *d
	copied.Status, copied.CreatedAt = foundrymodel.FoundryDocumentPendingUpload, time.Now()
	s.documents = append(s.documents, &copied)
	stored := copied
	return &stored, false, nil
}

func (s *foundryStore) findDocument(match func(*foundrymodel.FoundryDocument) bool) (*foundrymodel.FoundryDocument, error) {
	for _, d := range s.documents {
		if match(d) {
			copied := *d
			return &copied, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *foundryStore) GetFoundryDocument(_ context.Context, workspaceID, id string) (*foundrymodel.FoundryDocument, error) {
	return s.findDocument(func(d *foundrymodel.FoundryDocument) bool { return d.ID == id && d.WorkspaceID == workspaceID })
}

func (s *foundryStore) ListFoundryDocuments(_ context.Context, workspaceID, setID, pageToken string, _ int) ([]*foundrymodel.FoundryDocument, string, error) {
	if err := s.listPage(pageToken); err != nil {
		return nil, "", err
	}
	var out []*foundrymodel.FoundryDocument
	for _, d := range s.documents {
		if d.WorkspaceID == workspaceID && d.Status != foundrymodel.FoundryDocumentDeleted && (setID == "" && d.Source == nil || d.SetID == setID) {
			out = append(out, d)
		}
	}
	return out, "", nil
}

// ListFoundryDocumentsByID keeps the provider's rule: either origin, deleted
// or not; an id with no document is absent.
func (s *foundryStore) ListFoundryDocumentsByID(_ context.Context, workspaceID string, ids []string) (map[string]*foundrymodel.FoundryDocument, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := map[string]*foundrymodel.FoundryDocument{}
	for _, d := range s.documents {
		if d.WorkspaceID == workspaceID && slices.Contains(ids, d.ID) {
			out[d.ID] = d
		}
	}
	return out, nil
}

// ListFoundryOntologyDocuments keeps the provider's rule: the load's
// members and the scope's uploaded documents; filters are the MySQL tests'.
func (s *foundryStore) ListFoundryOntologyDocuments(_ context.Context, workspaceID, loadID string, scope []string, _ foundrymodel.FoundryDocumentFilter,
	pageToken string, _ int) ([]*foundrymodel.FoundryDocument, string, error) {
	if err := s.listPage(pageToken); err != nil {
		return nil, "", err
	}
	var out []*foundrymodel.FoundryDocument
	for _, d := range s.documents {
		read := slices.Contains(s.members[loadID], d.ID) || d.Status == foundrymodel.FoundryDocumentUploaded && slices.Contains(scope, d.SetID)
		if d.WorkspaceID == workspaceID && d.Status != foundrymodel.FoundryDocumentDeleted && read {
			out = append(out, d)
		}
	}
	return out, "", nil
}

func (s *foundryStore) FoundryLoadHoldsDocument(_ context.Context, loadID, documentID string) (bool, error) {
	return slices.Contains(s.members[loadID], documentID), nil
}

func (s *foundryStore) RenameFoundryDocument(_ context.Context, workspaceID, id, name string) error {
	for _, d := range s.documents {
		if d.ID == id && d.WorkspaceID == workspaceID && d.Status != foundrymodel.FoundryDocumentDeleted {
			if d.Source != nil {
				return foundrystore.ErrConnectorOwned
			}
			d.Name = name
			return nil
		}
	}
	return foundrystore.ErrNotFound
}

func (s *foundryStore) CompleteFoundryDocumentUpload(ctx context.Context, workspaceID, id string, fan foundrymodel.FoundryDocumentFanOut) (
	*foundrymodel.FoundryDocument, []foundrymodel.FoundryDocumentRead, error) {
	for _, d := range s.documents {
		if d.ID == id && d.WorkspaceID == workspaceID && d.Status == foundrymodel.FoundryDocumentPendingUpload {
			now := time.Now()
			d.Status, d.UploadedAt = foundrymodel.FoundryDocumentUploaded, &now
			s.fans = append(s.fans, fan)
			completed, err := s.GetFoundryDocument(ctx, workspaceID, id)
			return completed, s.reads, err
		}
	}
	return nil, nil, foundrystore.ErrNotFound
}

func (s *foundryStore) DeleteFoundryDocument(_ context.Context, workspaceID, id string) error {
	for _, d := range s.documents {
		if d.ID == id && d.WorkspaceID == workspaceID && d.Status != foundrymodel.FoundryDocumentDeleted {
			d.Status = foundrymodel.FoundryDocumentDeleted
			return nil
		}
	}
	return foundrystore.ErrNotFound
}

func (s *foundryStore) GetFoundryDataLayout(_ context.Context, workspaceID string) (*foundrymodel.FoundryDataLayout, error) {
	if l, ok := s.layouts[workspaceID]; ok {
		copied := *l
		return &copied, nil
	}
	return &foundrymodel.FoundryDataLayout{WorkspaceID: workspaceID, Layout: json.RawMessage(`{}`)}, nil
}

func (s *foundryStore) PutFoundryDataLayout(_ context.Context, l *foundrymodel.FoundryDataLayout, expected int64) error {
	var revision int64
	if stored, ok := s.layouts[l.WorkspaceID]; ok {
		revision = stored.Revision
	}
	if revision != expected {
		return foundrystore.ErrConflict
	}
	l.Revision = expected + 1
	copied := *l
	s.layouts[l.WorkspaceID] = &copied
	return nil
}

func (s *foundryStore) GetFoundryDocumentExtraction(_ context.Context, loadID, documentID string) (*foundrymodel.FoundryDocumentExtraction, error) {
	if e, ok := s.extractions[loadID][documentID]; ok {
		return e, nil
	}
	return nil, foundrystore.ErrNotFound
}

func (s *foundryStore) ListFoundryDocumentExtractions(_ context.Context, loadID string, ids []string) (map[string]*foundrymodel.FoundryDocumentExtraction, error) {
	out := map[string]*foundrymodel.FoundryDocumentExtraction{}
	for _, id := range ids {
		if e, ok := s.extractions[loadID][id]; ok {
			out[id] = e
		}
	}
	return out, nil
}

// extracted records what extracting a document into a load made of it.
func (s *foundryStore) extracted(loadID string, e *foundrymodel.FoundryDocumentExtraction) {
	if s.extractions[loadID] == nil {
		s.extractions[loadID] = map[string]*foundrymodel.FoundryDocumentExtraction{}
	}
	s.extractions[loadID][e.DocumentID] = e
}

func (s *foundryStore) GetFoundryLoad(_ context.Context, ontologyID, id string) (*foundrymodel.FoundryLoad, error) {
	for _, l := range s.loads {
		if l.ID == id && l.OntologyID == ontologyID {
			copied := *l
			return &copied, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *foundryStore) ListFoundryLoads(_ context.Context, ontologyID string) ([]*foundrymodel.FoundryLoad, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []*foundrymodel.FoundryLoad
	for i := len(s.loads) - 1; i >= 0; i-- {
		if l := s.loads[i]; l.OntologyID == ontologyID && l.Status != foundrymodel.FoundryLoadDeleted {
			out = append(out, l)
		}
	}
	return out, nil
}

// ListFoundryLoadEnrichments finds no step: these loads' versions enrich no one.
func (s *foundryStore) ListFoundryLoadEnrichments(context.Context, []string) (map[string]*foundrymodel.FoundryLoadEnrichment, error) {
	return map[string]*foundrymodel.FoundryLoadEnrichment{}, nil
}

// ListFoundryLoadJobs lists each load's newest jobs, newest first, and
// counts and adds up all of them.
func (s *foundryStore) ListFoundryLoadJobs(_ context.Context, loadIDs []string, newest int) (map[string]foundrymodel.FoundryLoadJobs, error) {
	out := map[string]foundrymodel.FoundryLoadJobs{}
	for i := len(s.jobs) - 1; i >= 0; i-- {
		for _, id := range loadIDs {
			if job := s.jobs[i]; job.LoadID == id {
				jobs := out[id]
				if len(jobs.Newest) < newest {
					jobs.Newest = append(jobs.Newest, job)
				}
				jobs.Count, jobs.USDMicros = jobs.Count+1, jobs.USDMicros+job.Spend.USDMicros
				out[id] = jobs
			}
		}
	}
	return out, nil
}

// CreateFoundryLoad queues a load job and its load; a live delete gives way.
func (s *foundryStore) CreateFoundryLoad(ctx context.Context, job *foundrymodel.FoundryGraphJob) error {
	live, err := s.GetActiveFoundryGraphJob(ctx, job.OntologyID)
	switch {
	case err == nil && live.Kind != foundrymodel.FoundryGraphJobDelete:
		return foundrystore.ErrConflict
	case err == nil:
		live.Status = foundrymodel.FoundryGraphJobCancelled
	}
	for _, j := range s.jobs {
		if j.OntologyID == job.OntologyID && j.ClientToken != nil && job.ClientToken != nil && *j.ClientToken == *job.ClientToken {
			return foundrystore.ErrConflict
		}
	}
	version := 0
	for _, v := range s.versions {
		if v.ID == job.VersionID {
			version = v.Version
		}
	}
	s.loads = append(s.loads, &foundrymodel.FoundryLoad{
		ID: job.LoadID, WorkspaceID: job.WorkspaceID, OntologyID: job.OntologyID, VersionID: job.VersionID, Version: version,
		Status: foundrymodel.FoundryLoadWriting, CreatedAt: time.Now(),
	})
	copied := *job
	copied.Status, copied.CreatedAt = foundrymodel.FoundryGraphJobQueued, time.Now()
	s.jobs = append(s.jobs, &copied)
	return nil
}

func (s *foundryStore) load(id string) *foundrymodel.FoundryLoad {
	for _, l := range s.loads {
		if l.ID == id {
			return l
		}
	}
	return nil
}

func (s *foundryStore) SetCurrentFoundryLoad(_ context.Context, _, ontologyID, loadID string, extract *foundrymodel.FoundryGraphJob,
	_ foundrymodel.FoundryGraphBudget) (bool, error) {
	l := s.load(loadID)
	switch {
	case l == nil || l.OntologyID != ontologyID || l.Status == foundrymodel.FoundryLoadDeleted:
		return false, foundrystore.ErrNotFound
	case !l.Status.Servable():
		return false, foundrystore.FoundryLoadNotServable
	}
	s.ontologies[ontologyID].CurrentLoadID = &loadID
	s.completions = append(s.completions, extract)
	return false, nil
}

func (s *foundryStore) DeleteFoundryLoad(ctx context.Context, _, ontologyID, loadID, deleteJobID string) error {
	l := s.load(loadID)
	o := s.ontologies[ontologyID]
	live, liveErr := s.GetActiveFoundryGraphJob(ctx, ontologyID)
	switch {
	case l == nil || l.OntologyID != ontologyID || l.Status == foundrymodel.FoundryLoadDeleted:
		return foundrystore.ErrNotFound
	case o.CurrentLoadID != nil && *o.CurrentLoadID == loadID:
		return foundrystore.FoundryLoadIsCurrent
	case !l.Status.Deletable() || liveErr == nil && live.LoadID == loadID:
		return foundrystore.FoundryLoadBusy
	}
	l.Status = foundrymodel.FoundryLoadDeleting
	s.deletesQueued = append(s.deletesQueued, deleteJobID)
	return nil
}

func (s *foundryStore) GetFoundryGraphJobByClientToken(_ context.Context, ontologyID, token string) (*foundrymodel.FoundryGraphJob, error) {
	for _, j := range s.jobs {
		if j.OntologyID == ontologyID && j.ClientToken != nil && *j.ClientToken == token {
			return j, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *foundryStore) GetFoundryGraphJob(_ context.Context, ontologyID, id string) (*foundrymodel.FoundryGraphJob, error) {
	for _, j := range s.jobs {
		if j.ID == id && j.OntologyID == ontologyID {
			return j, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *foundryStore) SumFoundryGraphJobSpend(context.Context, string, time.Time) (int64, error) {
	return s.spent, nil
}

// documentBucket is the documents store: what was presigned, verified and deleted.
type documentBucket struct {
	presigned []string
	verifyErr error
	deleted   []string
	// prefixes are the prefixes removed; prefixErr fails each removal.
	prefixes  []string
	prefixErr error
	// content is what Open reads, by key; openErr fails it.
	content map[string]string
	openErr error
}

func (b *documentBucket) PresignPut(_ context.Context, key string, d document.Declared, ttl time.Duration) (document.Upload, error) {
	b.presigned = append(b.presigned, key)
	return document.Upload{
		URL: "https://bucket.example/" + key, Method: "PUT", ExpiresAt: time.Now().Add(ttl),
		Headers: map[string]string{"Content-Type": string(d.MediaType), "X-Amz-Checksum-Sha256": "sum"},
	}, nil
}

func (b *documentBucket) Verify(context.Context, string, document.Declared) error { return b.verifyErr }

func (b *documentBucket) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if b.openErr != nil {
		return nil, b.openErr
	}
	return io.NopCloser(strings.NewReader(b.content[key])), nil
}

func (b *documentBucket) Delete(_ context.Context, key string) error {
	b.deleted = append(b.deleted, key)
	return nil
}

func (b *documentBucket) DeletePrefix(_ context.Context, prefix string) error {
	if b.prefixErr != nil {
		return b.prefixErr
	}
	b.prefixes = append(b.prefixes, prefix)
	return nil
}

// foundryService has ws-1's ontology ont-1 on tenant-1, the workspace's
// set set-general, a free tenant-9 of ws-1, tenant-x of ws-2, and a
// documents bucket.
func foundryService(t *testing.T) (*Service, *foundryStore, *documentBucket) {
	t.Helper()
	s, ontologies := ontologyService(t)
	store := &foundryStore{
		ontologyStore: ontologies,
		databases: map[string]ports.Database{
			"tenant-1": {TenantID: "tenant-1", WorkspaceID: "ws-1", ProjectID: "proj-1", Name: "billing-db"},
			"tenant-9": {TenantID: "tenant-9", WorkspaceID: "ws-1", ProjectID: "proj-1", Name: "spare-db"},
			"tenant-x": {TenantID: "tenant-x", WorkspaceID: "ws-2", ProjectID: "proj-2", Name: "theirs"},
		},
		projects:    map[string]bool{"proj-1": true, "proj-2": true},
		draining:    map[string]bool{},
		sets:        []*foundrymodel.FoundryDocumentSet{{ID: "set-general", WorkspaceID: "ws-1", Name: foundrymodel.FoundryGeneralSetName}},
		extractions: map[string]map[string]*foundrymodel.FoundryDocumentExtraction{},
		members:     map[string][]string{},
		layouts:     map[string]*foundrymodel.FoundryDataLayout{},
	}
	bucket := &documentBucket{}
	cfg := config.Defaults()
	s.deps.DB, s.deps.Databases, s.deps.Cfg, s.deps.Documents = store, foundryDatabases{store: store}, &cfg, bucket
	return s, store, bucket
}

// foundryDatabases is the host's databases as foundryStore holds them.
type foundryDatabases struct{ store *foundryStore }

func (d foundryDatabases) Get(_ context.Context, tenantID string) (ports.Database, error) {
	database, ok := d.store.databases[tenantID]
	if !ok {
		return ports.Database{}, foundrystore.ErrNotFound
	}
	return database, nil
}

// Live refuses a database that is gone, deleted, or of a deleted project.
func (d foundryDatabases) Live(ctx context.Context, tenantID string) (ports.Database, error) {
	database, err := d.Get(ctx, tenantID)
	switch {
	case err != nil:
		return ports.Database{}, status.Error(codes.NotFound, "tenant not found")
	case database.Deleted || database.Retired:
		return ports.Database{}, status.Errorf(codes.FailedPrecondition, "tenant %s is deleted", tenantID)
	case !d.store.projects[database.ProjectID]:
		return ports.Database{}, status.Error(codes.FailedPrecondition, "the database's project is deleted")
	}
	return database, nil
}

func (foundryDatabases) Endpoint(context.Context, string, string) (string, error) {
	return "", status.Error(codes.Unavailable, "no database is served here")
}

// lateTokenStore misses a client token on its first lookup, as a create that
// raced a delete of the token's ontology does.
type lateTokenStore struct {
	*foundryStore
	looked bool
}

func (s *lateTokenStore) GetFoundryOntologyByClientToken(ctx context.Context, workspaceID, token string) (*foundrymodel.FoundryOntology, error) {
	if !s.looked {
		s.looked = true
		return nil, foundrystore.ErrNotFound
	}
	return s.foundryStore.GetFoundryOntologyByClientToken(ctx, workspaceID, token)
}

func TestFoundryOntologyLifecycle(t *testing.T) {
	ctx := context.Background()
	create := func(s *Service, tenant, token string) (*foundryv1.FoundryOntologyResponse, error) {
		return s.CreateFoundryOntology(ports.WithActor(ctx, person), &foundryv1.CreateFoundryOntologyRequest{
			WorkspaceId: "ws-1", Name: " Support ", TenantId: tenant, ClientToken: token,
		})
	}

	// The ontology names its database, and a retry of the same click returns it.
	t.Run("binds a new ontology to a database of the workspace", func(t *testing.T) {
		s, store, _ := foundryService(t)
		resp, err := create(s, "tenant-9", "click-1")
		if err != nil {
			t.Fatal(err)
		}
		o := resp.GetOntology()
		if o.GetName() != "Support" || o.GetTenantId() != "tenant-9" || o.GetProjectId() != "proj-1" ||
			o.GetDatabaseName() != "spare-db" || o.GetCounts().GetDocuments() != 0 || o.GetLastLoad() != nil || o.CurrentVersionId != nil {
			t.Fatalf("ontology = %+v", o)
		}
		if got := store.ontologies[o.GetId()]; got.CreatedByUserID == nil || *got.CreatedByUserID != "user-1" {
			t.Fatalf("stored = %+v", got)
		}
		again, err := create(s, "tenant-9", "click-1")
		if err != nil || again.GetOntology().GetId() != o.GetId() {
			t.Fatalf("retry = (%+v, %v)", again, err)
		}
		if _, err := create(s, "tenant-1", "click-1"); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("token reused for another database: %v", err)
		}
	})

	// A token names one create for good: replaying a deleted ontology's would
	// hand back an ontology every later call says is gone.
	t.Run("refuses the token of a deleted ontology", func(t *testing.T) {
		s, _, _ := foundryService(t)
		resp, err := create(s, "tenant-9", "click-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteFoundryOntology(ctx, &foundryv1.DeleteFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: resp.GetOntology().GetId()}); err != nil {
			t.Fatal(err)
		}
		if _, err := create(s, "tenant-9", "click-1"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("the deleted ontology's token: %v", err)
		}
		if again, err := create(s, "tenant-9", "click-2"); err != nil || again.GetOntology().GetId() == resp.GetOntology().GetId() {
			t.Fatalf("a new token = (%+v, %v), want a new ontology on the freed database", again, err)
		}
	})

	// The ontology can be deleted between the token lookup and the insert
	// that then conflicts on the token; the answer must be the same.
	t.Run("refuses the token of an ontology deleted mid-create", func(t *testing.T) {
		s, store, _ := foundryService(t)
		now := time.Now()
		store.ontologies["ont-gone"] = &foundrymodel.FoundryOntology{
			ID: "ont-gone", WorkspaceID: "ws-1", TenantID: "tenant-9", ClientToken: "click-1", DeletedAt: &now,
		}
		s.deps.DB = &lateTokenStore{foundryStore: store}
		if _, err := create(s, "tenant-9", "click-1"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("error = %v", err)
		}
	})

	// A database has one Foundry writer, and only the workspace's own live databases can be chosen.
	t.Run("refuses a database it can't bind", func(t *testing.T) {
		s, store, _ := foundryService(t)
		store.databases["tenant-gone"] = ports.Database{TenantID: "tenant-gone", WorkspaceID: "ws-1", ProjectID: "proj-1", Deleted: true}
		store.databases["tenant-orphan"] = ports.Database{TenantID: "tenant-orphan", WorkspaceID: "ws-1", ProjectID: "proj-gone"}
		// Another workspace's database is not found, though its project is deleted.
		store.databases["tenant-y"] = ports.Database{TenantID: "tenant-y", WorkspaceID: "ws-2", ProjectID: "proj-gone"}
		for tenant, code := range map[string]codes.Code{
			"tenant-1":      codes.AlreadyExists,
			"tenant-x":      codes.NotFound,
			"tenant-y":      codes.NotFound,
			"tenant-none":   codes.NotFound,
			"tenant-gone":   codes.FailedPrecondition,
			"tenant-orphan": codes.FailedPrecondition,
		} {
			if _, err := create(s, tenant, "click-"+tenant); status.Code(err) != code {
				t.Fatalf("%s: error = %v, want %s", tenant, err, code)
			}
		}
		for name, req := range map[string]*foundryv1.CreateFoundryOntologyRequest{
			"no name":      {WorkspaceId: "ws-1", Name: "  ", TenantId: "tenant-9", ClientToken: "c"},
			"a long name":  {WorkspaceId: "ws-1", Name: strings.Repeat("n", 121), TenantId: "tenant-9", ClientToken: "c"},
			"no token":     {WorkspaceId: "ws-1", Name: "n", TenantId: "tenant-9"},
			"a long token": {WorkspaceId: "ws-1", Name: "n", TenantId: "tenant-9", ClientToken: strings.Repeat("t", 65)},
			"no tenant":    {WorkspaceId: "ws-1", Name: "n", ClientToken: "c"},
		} {
			if _, err := s.CreateFoundryOntology(ctx, req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s: error = %v", name, err)
			}
		}
	})

	// The ontology shows its newest load job, the load reads go to, the job
	// the website polls and its documents: the uploaded ones of the current
	// load's version's scope.
	t.Run("lists ontologies with their loads, live job and documents", func(t *testing.T) {
		s, store, _ := foundryService(t)
		finished := time.Now()
		store.loads = append(store.loads, &foundrymodel.FoundryLoad{
			ID: "load-1", OntologyID: "ont-1", VersionID: "v-1", Version: 4, Status: foundrymodel.FoundryLoadReady, FinishedAt: &finished,
		})
		loadID := "load-1"
		store.ontologies["ont-1"].CurrentLoadID = &loadID
		store.jobs = append(store.jobs,
			&foundrymodel.FoundryGraphJob{ID: "load-1", LoadID: "load-1", OntologyID: "ont-1", Kind: foundrymodel.FoundryGraphJobLoad,
				Status: foundrymodel.FoundryGraphJobSucceeded, FinishedAt: &finished},
			&foundrymodel.FoundryGraphJob{ID: "job-2", LoadID: "load-1", OntologyID: "ont-1", Kind: foundrymodel.FoundryGraphJobExtract,
				Status: foundrymodel.FoundryGraphJobRunning, Phase: foundrymodel.FoundryGraphJobDocuments,
				Progress: foundrymodel.FoundryGraphJobProgress{DocumentsDone: 1, DocumentsTotal: 3}})
		scope := []string{"set-general"}
		store.versions = append(store.versions, &foundrymodel.OntologyVersion{ID: "v-1", WorkspaceID: "ws-1", OntologyID: "ont-1",
			Format: foundrymodel.OntologyFormatAgentBundle, Definition: json.RawMessage(`{}`), DocumentSetIDs: &scope})
		store.documents = append(store.documents,
			&foundrymodel.FoundryDocument{ID: "d1", WorkspaceID: "ws-1", SetID: "set-general", Status: foundrymodel.FoundryDocumentUploaded},
			&foundrymodel.FoundryDocument{ID: "d2", WorkspaceID: "ws-1", SetID: "set-general", Status: foundrymodel.FoundryDocumentDeleted},
			&foundrymodel.FoundryDocument{ID: "d3", WorkspaceID: "ws-1", SetID: "set-other", Status: foundrymodel.FoundryDocumentUploaded},
			&foundrymodel.FoundryDocument{ID: "d4", WorkspaceID: "ws-1", SetID: "set-general", Status: foundrymodel.FoundryDocumentPendingUpload})
		list, err := s.ListFoundryOntologies(ctx, &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1"})
		if err != nil || len(list.GetOntologies()) != 1 {
			t.Fatalf("list = (%+v, %v)", list, err)
		}
		o := list.GetOntologies()[0]
		if o.GetLastLoad().GetJobId() != "load-1" || o.GetLastLoad().GetStatus() != "succeeded" || o.GetLastLoad().GetFinishedAt() == nil ||
			o.GetCounts().GetDocuments() != 1 || o.GetDatabaseName() != "billing-db" {
			t.Fatalf("ontology = %+v: the last load is the newest load job, not the extract", o)
		}
		if c := o.GetCurrentLoad(); c.GetId() != "load-1" || c.GetVersion() != "foundry-v4" || c.GetFinishedAt() == nil {
			t.Fatalf("current load = %+v", c)
		}
		if a := o.GetActiveJob(); a.GetKind() != "extract" || a.GetLoadId() != "load-1" || a.GetPhase() != "documents" ||
			a.GetProgress().GetDocumentsTotal() != 3 {
			t.Fatalf("active job = %+v", a)
		}
		store.jobs[1].Status = foundrymodel.FoundryGraphJobSucceeded
		got, err := s.GetFoundryOntology(ctx, &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || got.GetOntology().GetActiveJob() != nil {
			t.Fatalf("with nothing live = (%+v, %v)", got, err)
		}
		if _, err := s.ListFoundryOntologies(ctx, &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1", PageToken: "bad"}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("bad page token: %v", err)
		}
		if _, err := s.GetFoundryOntology(ctx, &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-2", OntologyId: "ont-1"}); status.Code(err) != codes.NotFound {
			t.Fatalf("another workspace's ontology: %v", err)
		}
	})

	// Deleting cancels the live job and deletes the ontology's loads from
	// the database, which keeps everything else; until they are gone, no
	// other ontology may be bound to it.
	t.Run("deletes the ontology and its loads only", func(t *testing.T) {
		s, store, _ := foundryService(t)
		store.loads = append(store.loads, &foundrymodel.FoundryLoad{ID: "load-1", OntologyID: "ont-1", Status: foundrymodel.FoundryLoadWriting})
		store.jobs = append(store.jobs, &foundrymodel.FoundryGraphJob{ID: "load-1", LoadID: "load-1", OntologyID: "ont-1", Kind: foundrymodel.FoundryGraphJobLoad,
			Status: foundrymodel.FoundryGraphJobRunning})
		resp, err := s.DeleteFoundryOntology(ctx, &foundryv1.DeleteFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || resp.GetOntology().GetId() != "ont-1" {
			t.Fatalf("delete while a load runs = (%+v, %v)", resp, err)
		}
		if store.jobs[0].Status != foundrymodel.FoundryGraphJobCancelled || len(store.deletesQueued) != 1 || store.deletesQueued[0] == "" {
			t.Fatalf("job %s, deletes queued %v", store.jobs[0].Status, store.deletesQueued)
		}
		if _, ok := store.databases["tenant-1"]; !ok {
			t.Fatal("the database went with the ontology")
		}
		if _, err := s.GetFoundryOntology(ctx, &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); status.Code(err) != codes.NotFound {
			t.Fatalf("a deleted ontology: %v", err)
		}
		if _, err := s.DeleteFoundryOntology(ctx, &foundryv1.DeleteFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); status.Code(err) != codes.NotFound {
			t.Fatalf("deleted twice: %v", err)
		}
		_, err = create(s, "tenant-1", "click-again")
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "still being removed") {
			t.Fatalf("binding the database while its loads drain: %v", err)
		}
		delete(store.draining, "ont-1")
		if _, err := create(s, "tenant-1", "click-again"); err != nil {
			t.Fatalf("the drained database takes a new ontology: %v", err)
		}
	})

	// A load whose delete failed for good keeps the deleted ontology's hold
	// on the database, since a new ontology would take it for its own data.
	// Nothing retries it by itself; asking for the database again does, and
	// is refused until the retry has run.
	t.Run("retries a failed delete when the database is asked for again", func(t *testing.T) {
		s, store, _ := foundryService(t)
		now := time.Now()
		store.ontologies["ont-1"].DeletedAt = &now
		store.draining["ont-1"] = true
		store.loads = append(store.loads, &foundrymodel.FoundryLoad{ID: "load-1", OntologyID: "ont-1", Status: foundrymodel.FoundryLoadDeleteFailed})
		_, err := create(s, "tenant-1", "click-again")
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "still being removed") {
			t.Fatalf("binding a database a failed delete holds: %v", err)
		}
		if store.load("load-1").Status != foundrymodel.FoundryLoadDeleting || len(store.deletesQueued) != 1 || store.deletesQueued[0] == "" {
			t.Fatalf("load %s, deletes queued %v", store.load("load-1").Status, store.deletesQueued)
		}

		// The retry runs: asking again queues nothing more.
		store.jobs = append(store.jobs, &foundrymodel.FoundryGraphJob{ID: store.deletesQueued[0], LoadID: "load-1", OntologyID: "ont-1",
			Kind: foundrymodel.FoundryGraphJobDelete, Status: foundrymodel.FoundryGraphJobRunning})
		if _, err := create(s, "tenant-1", "click-again"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("binding while the retry runs: %v", err)
		}
		if len(store.deletesQueued) != 1 {
			t.Fatalf("deletes queued %v, want the one retry", store.deletesQueued)
		}

		// A retry the database can't queue is the server's failure, not a refusal.
		store.resumeErr = errors.New("connection refused")
		if _, err := create(s, "tenant-1", "click-again"); status.Code(err) != codes.Internal {
			t.Fatalf("a failed retry: %v", err)
		}
	})
}

func TestFoundryListErrors(t *testing.T) {
	ctx := context.Background()
	lists := map[string]func(s *Service, pageToken string) error{
		"ontologies": func(s *Service, pageToken string) error {
			_, err := s.ListFoundryOntologies(ctx, &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1", PageToken: pageToken})
			return err
		},
		"documents": func(s *Service, pageToken string) error {
			_, err := s.ListFoundryDocuments(ctx, &foundryv1.ListFoundryDocumentsRequest{WorkspaceId: "ws-1", PageToken: pageToken})
			return err
		},
		"an ontology's documents": func(s *Service, pageToken string) error {
			_, err := s.ListFoundryOntologyDocuments(ctx, &foundryv1.ListFoundryOntologyDocumentsRequest{
				WorkspaceId: "ws-1", OntologyId: "ont-1", PageToken: pageToken,
			})
			return err
		},
	}
	// A page token no list handed out is the caller's to fix; a database
	// failure is not, and clients retry an internal error but not a bad request.
	for name, list := range lists {
		t.Run(name+": a foreign page token is a bad request", func(t *testing.T) {
			s, _, _ := foundryService(t)
			if err := list(s, "bad"); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("error = %v", err)
			}
		})
		t.Run(name+": a database failure is internal", func(t *testing.T) {
			s, store, _ := foundryService(t)
			store.listErr = errors.New("connection refused")
			if err := list(s, ""); status.Code(err) != codes.Internal {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestFoundryDocumentRPCs(t *testing.T) {
	ctx := context.Background()
	sum := strings.Repeat("ab", 32)
	create := func(s *Service, name, mediaType, token string, size int64) (*foundryv1.CreateFoundryDocumentResponse, error) {
		return s.CreateFoundryDocument(ports.WithActor(ctx, person), &foundryv1.CreateFoundryDocumentRequest{
			WorkspaceId: "ws-1", Name: name,
			MediaType: mediaType, SizeBytes: size, Sha256: sum, ClientToken: token, SetId: "set-general",
		})
	}

	// The browser gets a PUT of exactly what it declared, under the
	// workspace's prefix; a repeat says it found the document already there.
	t.Run("creates a pending document with a presigned upload", func(t *testing.T) {
		s, store, bucket := foundryService(t)
		resp, err := create(s, "contract.md", "text/markdown", "up-1", 120)
		if err != nil {
			t.Fatal(err)
		}
		doc := resp.GetDocument()
		if doc.GetStatus() != "pending_upload" || doc.GetSizeBytes() != 120 || resp.GetUpload().GetMethod() != "PUT" ||
			resp.GetUpload().GetHeaders()["Content-Type"] != "text/markdown" || resp.GetUpload().GetExpiresAt() == nil || resp.GetExisting() {
			t.Fatalf("response = %+v", resp)
		}
		stored := store.documents[0]
		if stored.WorkspaceID != "ws-1" || stored.S3Key != document.Key("ws-1", stored.ID) || len(bucket.presigned) != 1 ||
			stored.CreatedByUserID == nil || *stored.CreatedByUserID != "user-1" {
			t.Fatalf("stored = %+v, presigned %v", stored, bucket.presigned)
		}
		again, err := create(s, "contract.md", "text/markdown", "up-1", 120)
		if err != nil || again.GetDocument().GetId() != doc.GetId() || again.GetUpload() == nil || !again.GetExisting() {
			t.Fatalf("retry before the bytes are in = (%+v, %v)", again, err)
		}
		sameBytes, err := create(s, "copy.md", "text/markdown", "up-2", 120)
		if err != nil || sameBytes.GetDocument().GetId() != doc.GetId() || !sameBytes.GetExisting() {
			t.Fatalf("same bytes = (%+v, %v)", sameBytes, err)
		}
		store.documents[0].Status = foundrymodel.FoundryDocumentUploaded
		done, err := create(s, "contract.md", "text/markdown", "up-1", 120)
		if err != nil || done.GetUpload() != nil || !done.GetExisting() {
			t.Fatalf("once the bytes are in, no upload: (%+v, %v)", done, err)
		}
		if _, err := s.CreateFoundryDocument(ctx, &foundryv1.CreateFoundryDocumentRequest{
			WorkspaceId: "ws-1", Name: "contract.md", MediaType: "text/markdown", SizeBytes: 120, Sha256: strings.Repeat("cd", 32),
			ClientToken: "up-1", SetId: "set-general",
		}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("a token used for other bytes: %v", err)
		}
	})

	// Every type Foundry stores is taken up to the 1 MiB cap, its PUT signed
	// with that type; a deployment's media_types may take fewer.
	t.Run("takes every stored type", func(t *testing.T) {
		s, _, _ := foundryService(t)
		for i, tc := range []struct{ name, mediaType string }{
			{"a.md", "text/markdown"}, {"a.markdown", "text/markdown"}, {"a.mdx", "text/mdx"}, {"a.txt", "text/plain"}, {"a.json", "application/json"},
			{"a.csv", "text/csv"}, {"a.tsv", "text/tab-separated-values"}, {"a.jsonl", "application/x-ndjson"},
			{"a.ndjson", "application/x-ndjson"}, {"a.parquet", "application/vnd.apache.parquet"}, {"A.PDF", "application/pdf"},
		} {
			resp, err := s.CreateFoundryDocument(ctx, &foundryv1.CreateFoundryDocumentRequest{
				WorkspaceId: "ws-1", Name: tc.name, MediaType: tc.mediaType, SizeBytes: 1 << 20, Sha256: fmt.Sprintf("%064x", i),
				ClientToken: "t-" + tc.name, SetId: "set-general",
			})
			if err != nil || resp.GetDocument().GetMediaType() != tc.mediaType || resp.GetUpload().GetHeaders()["Content-Type"] != tc.mediaType {
				t.Fatalf("%s = (%+v, %v)", tc.name, resp, err)
			}
		}
		s.deps.Cfg.Foundry.Documents.MediaTypes = []foundrymodel.FoundryMediaType{foundrymodel.FoundryMediaMarkdown}
		if _, err := create(s, "b.pdf", "application/pdf", "t-off", 10); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("a type the deployment leaves out: %v", err)
		}
	})

	// Only the types Foundry stores, of bounded size, whose declared type matches the name, are taken.
	t.Run("refuses what it can't store", func(t *testing.T) {
		s, store, _ := foundryService(t)
		for name, args := range map[string]struct {
			name, mediaType string
			size            int64
		}{
			"a Word document":     {"contract.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", 10},
			"a mismatched type":   {"contract.md", "text/plain", 10},
			"a PDF named as text": {"contract.txt", "application/pdf", 10},
			"an empty document":   {"contract.md", "text/markdown", 0},
			"text past 1 MiB":     {"contract.md", "text/markdown", 1<<20 + 1},
			"a table past 3 MiB":  {"orders.csv", "text/csv", 3<<20 + 1},
			"a PDF past 3 MiB":    {"contract.pdf", "application/pdf", 3<<20 + 1},
			"a path":              {"a/contract.md", "text/markdown", 10},
			"a backslash":         {`a\contract.md`, "text/markdown", 10},
		} {
			if _, err := create(s, args.name, args.mediaType, "t-"+name, args.size); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s: error = %v", name, err)
			}
		}
		if _, err := create(s, `a\contract.md`, "text/markdown", "t", 10); !strings.Contains(err.Error(), "slash, backslash or control character") {
			t.Fatalf("the name's rule: %v", err)
		}
		for name, req := range map[string]*foundryv1.CreateFoundryDocumentRequest{
			"a bad checksum": {WorkspaceId: "ws-1", Name: "a.md", MediaType: "text/markdown", SizeBytes: 1, Sha256: "ABC", ClientToken: "t", SetId: "set-general"},
			"no set":         {WorkspaceId: "ws-1", Name: "a.md", MediaType: "text/markdown", SizeBytes: 1, Sha256: sum, ClientToken: "t"},
			"no workspace":   {Name: "a.md", MediaType: "text/markdown", SizeBytes: 1, Sha256: sum, ClientToken: "t", SetId: "set-general"},
		} {
			if _, err := s.CreateFoundryDocument(ctx, req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if _, err := s.CreateFoundryDocument(ctx, &foundryv1.CreateFoundryDocumentRequest{
			WorkspaceId: "ws-2", Name: "a.md", MediaType: "text/markdown", SizeBytes: 1, Sha256: sum, ClientToken: "t", SetId: "set-general",
		}); status.Code(err) != codes.NotFound {
			t.Fatalf("another workspace's set: %v", err)
		}
		for i := range 200 {
			store.documents = append(store.documents, &foundrymodel.FoundryDocument{ID: fmt.Sprint(i), WorkspaceID: "ws-1", Status: foundrymodel.FoundryDocumentUploaded})
		}
		if _, err := create(s, "one-more.md", "text/markdown", "t-cap", 10); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("past the workspace cap: %v", err)
		}
		store.documents = nil
		s.deps.Documents = nil
		if _, err := create(s, "a.md", "text/markdown", "t-x", 10); status.Code(err) != codes.Unavailable {
			t.Fatalf("without a bucket: %v", err)
		}
	})

	// An upload whose URL expired before its bytes came was abandoned: it
	// must not hold one of the workspace's places for good. Asked for again,
	// it needs a place again.
	t.Run("counts abandoned uploads only when they are asked for again", func(t *testing.T) {
		s, store, _ := foundryService(t)
		ttl := s.deps.Cfg.Foundry.Documents.UploadTTL
		for i := range 200 {
			store.documents = append(store.documents, &foundrymodel.FoundryDocument{
				ID: fmt.Sprint(i), WorkspaceID: "ws-1", SetID: "set-general", Status: foundrymodel.FoundryDocumentPendingUpload,
				ClientToken: new(fmt.Sprint("old-", i)), SHA256: fmt.Sprintf("%064x", i), CreatedAt: time.Now().Add(-ttl - time.Minute),
			})
		}
		if _, err := create(s, "fresh.md", "text/markdown", "t-fresh", 10); err != nil {
			t.Fatalf("abandoned uploads filled the cap: %v", err)
		}
		if again, err := create(s, "fresh.md", "text/markdown", "t-fresh", 10); err != nil || again.GetUpload() == nil {
			t.Fatalf("the new upload asked for again = (%+v, %v)", again, err)
		}
		for i := range 199 {
			store.documents[i].Status = foundrymodel.FoundryDocumentUploaded
		}
		revive := func() error {
			_, err := s.CreateFoundryDocument(ctx, &foundryv1.CreateFoundryDocumentRequest{
				WorkspaceId: "ws-1", Name: "old.md", MediaType: "text/markdown", SizeBytes: 10,
				Sha256: fmt.Sprintf("%064x", 199), ClientToken: "old-199", SetId: "set-general",
			})
			return err
		}
		if err := revive(); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("an abandoned upload asked for at the cap: %v", err)
		}
		store.documents[0].Status = foundrymodel.FoundryDocumentDeleted
		if err := revive(); err != nil {
			t.Fatalf("an abandoned upload asked for under the cap: %v", err)
		}
	})

	// Completing checks the bytes, then hands the store what every ontology
	// reading the set needs: the requester, a job id each, the budget, and
	// the databases the caller may write. reads answers per ontology.
	t.Run("completes an upload once its bytes check out", func(t *testing.T) {
		s, store, bucket := foundryService(t)
		resp, err := create(s, "contract.md", "text/markdown", "up-1", 120)
		if err != nil {
			t.Fatal(err)
		}
		id := resp.GetDocument().GetId()
		// writes may write proj-1's databases.
		writes := ports.Actor{Source: ports.SourceUser, UserID: "user-1", WorkspaceID: "ws-1", QueryWrite: ports.SelectedProjects([]string{"proj-1"})}
		complete := func(caller ports.Actor) (*foundryv1.FoundryDocumentReadsResponse, error) {
			return s.CompleteFoundryDocumentUpload(ports.WithActor(ctx, caller), &foundryv1.CompleteFoundryDocumentUploadRequest{
				WorkspaceId: "ws-1", Id: id,
			})
		}
		for err, code := range map[error]codes.Code{
			document.ErrMissing: codes.FailedPrecondition, fmt.Errorf("%w: size", document.ErrMismatch): codes.FailedPrecondition,
			errors.New("s3 down"): codes.Unavailable,
		} {
			bucket.verifyErr = err
			if _, got := complete(writes); status.Code(got) != code {
				t.Fatalf("verify %v: error = %v, want %s", err, got, code)
			}
		}
		bucket.verifyErr = nil
		store.reads = []foundrymodel.FoundryDocumentRead{
			{OntologyID: "ont-1", Outcome: foundrymodel.FoundryDocumentReadQueued}, {OntologyID: "ont-3", Outcome: foundrymodel.FoundryDocumentReadSkipped},
		}
		done, err := complete(writes)
		if err != nil || done.GetDocument().GetStatus() != "uploaded" || len(done.GetReads()) != 2 ||
			done.GetReads()[0].GetOutcome() != foundryv1.FoundryDocumentReadOutcome_FOUNDRY_DOCUMENT_READ_OUTCOME_QUEUED ||
			done.GetReads()[1].GetOutcome() != foundryv1.FoundryDocumentReadOutcome_FOUNDRY_DOCUMENT_READ_OUTCOME_SKIPPED {
			t.Fatalf("complete = (%+v, %v)", done, err)
		}
		fan := store.fans[0]
		first, second := fan.NewJobID(), fan.NewJobID()
		if *fan.RequestedByUserID != "user-1" || first == second || fan.Budget.CapMicros == 0 ||
			!fan.CanWrite("proj-1") || fan.CanWrite("proj-2") {
			t.Fatalf("fan-out handed over = %+v", fan)
		}
		if again, err := complete(writes); err != nil || again.GetDocument().GetStatus() != "uploaded" || len(store.fans) != 1 ||
			len(again.GetReads()) != 0 {
			t.Fatalf("completing twice = (%+v, %v)", again, err)
		}
		other, err := s.CreateFoundryDocument(ctx, &foundryv1.CreateFoundryDocumentRequest{
			WorkspaceId: "ws-1", Name: "other.md", MediaType: "text/markdown", SizeBytes: 7, Sha256: strings.Repeat("ef", 32),
			ClientToken: "up-2", SetId: "set-general",
		})
		if err != nil {
			t.Fatal(err)
		}
		id = other.GetDocument().GetId()
		if _, err := complete(person); err != nil || store.fans[1].CanWrite("proj-1") {
			t.Fatalf("no write scope writes no database: %v", err)
		}
	})

	// A move is an upload into its new set: the same fan-out, after the
	// document is checked.
	t.Run("moves a document to another set", func(t *testing.T) {
		s, store, _ := foundryService(t)
		store.sets = append(store.sets, &foundrymodel.FoundryDocumentSet{ID: "set-contracts", WorkspaceID: "ws-1", Name: "Contracts"})
		store.documents = append(store.documents,
			&foundrymodel.FoundryDocument{ID: "doc-1", WorkspaceID: "ws-1", SetID: "set-general", Status: foundrymodel.FoundryDocumentUploaded},
			&foundrymodel.FoundryDocument{ID: "doc-2", WorkspaceID: "ws-1", SetID: "set-general", Status: foundrymodel.FoundryDocumentDeleted})
		store.reads = []foundrymodel.FoundryDocumentRead{{OntologyID: "ont-1", Outcome: foundrymodel.FoundryDocumentReadHeld}}
		move := func(id, set string) (*foundryv1.FoundryDocumentReadsResponse, error) {
			return s.MoveFoundryDocument(ctx, &foundryv1.MoveFoundryDocumentRequest{WorkspaceId: "ws-1", Id: id, SetId: set})
		}
		moved, err := move("doc-1", "set-contracts")
		if err != nil || moved.GetDocument().GetSetId() != "set-contracts" || len(moved.GetReads()) != 1 ||
			moved.GetReads()[0].GetOutcome() != foundryv1.FoundryDocumentReadOutcome_FOUNDRY_DOCUMENT_READ_OUTCOME_HELD || len(store.fans) != 1 {
			t.Fatalf("move = (%+v, %v)", moved, err)
		}
		inSet, err := s.ListFoundryDocuments(ctx, &foundryv1.ListFoundryDocumentsRequest{WorkspaceId: "ws-1", SetId: "set-contracts"})
		if err != nil || len(inSet.GetDocuments()) != 1 || inSet.GetDocuments()[0].GetId() != "doc-1" {
			t.Fatalf("the set's documents = (%+v, %v)", inSet, err)
		}
		for name, tc := range map[string]struct {
			id, set string
			code    codes.Code
		}{
			"a deleted document": {"doc-2", "set-contracts", codes.FailedPrecondition},
			"a missing document": {"doc-9", "set-contracts", codes.NotFound},
			"a missing set":      {"doc-1", "set-gone", codes.NotFound},
			"no set":             {"doc-1", "", codes.InvalidArgument},
			"no document id":     {"", "set-contracts", codes.InvalidArgument},
		} {
			if _, err := move(tc.id, tc.set); status.Code(err) != tc.code {
				t.Fatalf("%s: %v, want %s", name, err, tc.code)
			}
		}
		now := time.Now()
		store.ontologies["ont-1"].DeletedAt = &now
		if _, err := s.MoveFoundryOntologyDocument(ctx, &foundryv1.MoveFoundryOntologyDocumentRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "doc-1",
			SetId: "set-general"}); status.Code(err) != codes.NotFound {
			t.Fatalf("the alias of a deleted ontology: %v", err)
		}
	})

	// A document page shows the ontologies a document is associated with:
	// those whose current load holds it, and those whose reading version has
	// its set in scope, each with its extraction there.
	t.Run("shows a document with the ontologies it is associated with", func(t *testing.T) {
		s, store, bucket := foundryService(t)
		resp, err := create(s, "contract.md", "text/markdown", "up-1", 120)
		if err != nil {
			t.Fatal(err)
		}
		id := resp.GetDocument().GetId()
		got, err := s.GetFoundryDocument(ctx, &foundryv1.GetFoundryDocumentRequest{WorkspaceId: "ws-1", Id: id})
		if err != nil || len(got.GetDocument().GetOntologies()) != 0 {
			t.Fatalf("no ontology reads it yet = (%+v, %v)", got, err)
		}
		agentVersion(t, store, "v-routes", "[extractions.docs]\ndocuments = [\"set-general\"]\n", "set-general")
		agentVersion(t, store, "v-other", "", "set-other")
		current := "v-routes"
		store.ontologies["ont-1"].CurrentVersionID = &current
		got, err = s.GetFoundryDocument(ctx, &foundryv1.GetFoundryDocumentRequest{WorkspaceId: "ws-1", Id: id})
		if err != nil || len(got.GetDocument().GetOntologies()) != 1 || !got.GetDocument().GetOntologies()[0].GetRouted() ||
			got.GetDocument().GetOntologies()[0].GetExtraction() != nil {
			t.Fatalf("read by the current version = (%+v, %v)", got, err)
		}
		// Its current load's version reads another set now, but the load holds it.
		store.loads = append(store.loads, &foundrymodel.FoundryLoad{ID: "load-1", OntologyID: "ont-1", VersionID: "v-other", Status: foundrymodel.FoundryLoadReady})
		load := "load-1"
		store.ontologies["ont-1"].CurrentLoadID = &load
		got, err = s.GetFoundryDocument(ctx, &foundryv1.GetFoundryDocumentRequest{WorkspaceId: "ws-1", Id: id})
		if err != nil || len(got.GetDocument().GetOntologies()) != 0 {
			t.Fatalf("out of the reading scope, not held = (%+v, %v)", got, err)
		}
		store.members["load-1"] = []string{id}
		store.extracted("load-1", &foundrymodel.FoundryDocumentExtraction{DocumentID: id, Status: foundrymodel.FoundryExtractionExtracted,
			Counts: foundrymodel.FoundryExtractionCounts{Facts: 4}})
		got, err = s.GetFoundryDocument(ctx, &foundryv1.GetFoundryDocumentRequest{WorkspaceId: "ws-1", Id: id})
		if err != nil || len(got.GetDocument().GetOntologies()) != 1 || got.GetDocument().GetOntologies()[0].GetRouted() ||
			got.GetDocument().GetOntologies()[0].GetExtraction().GetFacts() != 4 || got.GetDocument().GetExtraction() != nil {
			t.Fatalf("held by the current load = (%+v, %v)", got, err)
		}
		if _, err := s.GetFoundryDocument(ctx, &foundryv1.GetFoundryDocumentRequest{WorkspaceId: "ws-2", Id: id}); status.Code(err) != codes.NotFound {
			t.Fatalf("another workspace's document: %v", err)
		}

		// An ontology's own view shows any document of the workspace with its extraction, or none.
		inOntology, err := s.GetFoundryOntologyDocument(ctx, &foundryv1.GetFoundryOntologyDocumentRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: id})
		if err != nil || inOntology.GetDocument().GetExtraction().GetStatus() != "extracted" {
			t.Fatalf("the ontology's document = (%+v, %v)", inOntology, err)
		}
		store.documents = append(store.documents, &foundrymodel.FoundryDocument{ID: "doc-elsewhere", WorkspaceID: "ws-1", SetID: "set-other",
			Status: foundrymodel.FoundryDocumentUploaded})
		inOntology, err = s.GetFoundryOntologyDocument(ctx, &foundryv1.GetFoundryOntologyDocumentRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			Id: "doc-elsewhere"})
		if err != nil || inOntology.GetDocument().GetExtraction() != nil {
			t.Fatalf("a document the load doesn't hold still opens = (%+v, %v)", inOntology, err)
		}
		if _, err := s.GetFoundryOntologyDocument(ctx, &foundryv1.GetFoundryOntologyDocumentRequest{WorkspaceId: "ws-2", OntologyId: "ont-2",
			Id: id}); status.Code(err) != codes.NotFound {
			t.Fatalf("outside the workspace: %v", err)
		}

		deleted, err := s.DeleteFoundryDocument(ctx, &foundryv1.DeleteFoundryDocumentRequest{WorkspaceId: "ws-1", Id: id})
		if err != nil || deleted.GetDocument().GetStatus() != "deleted" || len(bucket.prefixes) != 1 || bucket.prefixes[0] != "ws/ws-1/documents/"+id+"/" {
			t.Fatalf("delete = (%+v, %v), removed %v", deleted, err, bucket.prefixes)
		}
		if _, err := s.CompleteFoundryDocumentUpload(ctx, &foundryv1.CompleteFoundryDocumentUploadRequest{WorkspaceId: "ws-1", Id: id}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("completing a deleted document: %v", err)
		}
	})

	// A delete marks the document first, so no worker records a read of it
	// after, then removes its whole prefix: the original, its read and its
	// extractions. A removal that fails is retried by deleting again, which
	// finishes it.
	t.Run("deletes a document's whole prefix after marking it", func(t *testing.T) {
		s, store, bucket := foundryService(t)
		store.documents = append(store.documents, &foundrymodel.FoundryDocument{ID: "doc-1", WorkspaceID: "ws-1", SetID: "set-general",
			MediaType: foundrymodel.FoundryMediaCSV, Status: foundrymodel.FoundryDocumentUploaded, S3Key: document.Key("ws-1", "doc-1")})
		bucket.prefixErr = errors.New("s3 down")
		del := func() (*foundryv1.FoundryDocumentResponse, error) {
			return s.DeleteFoundryDocument(ctx, &foundryv1.DeleteFoundryDocumentRequest{WorkspaceId: "ws-1", Id: "doc-1"})
		}
		if _, err := del(); status.Code(err) != codes.Unavailable {
			t.Fatalf("a failed removal: %v", err)
		}
		if doc, _ := store.GetFoundryDocument(ctx, "ws-1", "doc-1"); doc.Status != foundrymodel.FoundryDocumentDeleted {
			t.Fatalf("the document is %s after a failed removal; it is marked first", doc.Status)
		}
		bucket.prefixErr = nil
		got, err := del()
		if err != nil || got.GetDocument().GetStatus() != "deleted" || len(bucket.prefixes) != 1 || bucket.prefixes[0] != "ws/ws-1/documents/doc-1/" {
			t.Fatalf("delete again = (%+v, %v), removed %v", got, err, bucket.prefixes)
		}
	})

	// An ontology lists what it reads, each with its current load's
	// extraction; its filter is checked here and applied by the store.
	t.Run("lists an ontology's documents", func(t *testing.T) {
		s, store, _ := foundryService(t)
		agentVersion(t, store, "v-1", "", "set-general")
		store.loads = append(store.loads, &foundrymodel.FoundryLoad{ID: "load-1", OntologyID: "ont-1", VersionID: "v-1", Status: foundrymodel.FoundryLoadReady})
		store.documents = append(store.documents,
			&foundrymodel.FoundryDocument{ID: "d-in", WorkspaceID: "ws-1", SetID: "set-general", Status: foundrymodel.FoundryDocumentUploaded},
			&foundrymodel.FoundryDocument{ID: "d-held", WorkspaceID: "ws-1", SetID: "set-other", Status: foundrymodel.FoundryDocumentUploaded},
			&foundrymodel.FoundryDocument{ID: "d-out", WorkspaceID: "ws-1", SetID: "set-other", Status: foundrymodel.FoundryDocumentUploaded})
		ids := func(resp *foundryv1.ListFoundryDocumentsResponse) []string {
			var out []string
			for _, d := range resp.GetDocuments() {
				out = append(out, d.GetId())
			}
			return out
		}
		list, err := s.ListFoundryOntologyDocuments(ctx, &foundryv1.ListFoundryOntologyDocumentsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || len(list.GetDocuments()) != 0 {
			t.Fatalf("with no version, nothing = (%+v, %v)", list, err)
		}
		load := "load-1"
		store.ontologies["ont-1"].CurrentLoadID = &load
		store.members["load-1"] = []string{"d-held"}
		store.extracted("load-1", &foundrymodel.FoundryDocumentExtraction{DocumentID: "d-held", Status: foundrymodel.FoundryExtractionOutOfScope})
		list, err = s.ListFoundryOntologyDocuments(ctx, &foundryv1.ListFoundryOntologyDocumentsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			Status: "out_of_scope"})
		if err != nil || strings.Join(ids(list), ",") != "d-in,d-held" || list.GetDocuments()[1].GetExtraction().GetStatus() != "out_of_scope" {
			t.Fatalf("list = (%+v, %v)", list, err)
		}
		if _, err := s.ListFoundryOntologyDocuments(ctx, &foundryv1.ListFoundryOntologyDocumentsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			Status: "everything"}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("an unknown status: %v", err)
		}
		all, err := s.ListFoundryDocuments(ctx, &foundryv1.ListFoundryDocumentsRequest{WorkspaceId: "ws-1"})
		if err != nil || len(all.GetDocuments()) != 3 || all.GetDocuments()[0].GetExtraction() != nil {
			t.Fatalf("the workspace's documents carry no extraction = (%+v, %v)", all, err)
		}
	})

	// A document is renamed by the rules of an upload's name, until it is deleted.
	t.Run("renames a document", func(t *testing.T) {
		s, store, _ := foundryService(t)
		store.documents = append(store.documents,
			&foundrymodel.FoundryDocument{ID: "doc-1", WorkspaceID: "ws-1", Name: "a.md", SetID: "set-general", Status: foundrymodel.FoundryDocumentUploaded},
			&foundrymodel.FoundryDocument{ID: "doc-2", WorkspaceID: "ws-1", Name: "b.md", SetID: "set-general", Status: foundrymodel.FoundryDocumentDeleted})
		rename := func(id, name string) (*foundryv1.FoundryDocumentResponse, error) {
			return s.UpdateFoundryDocument(ctx, &foundryv1.UpdateFoundryDocumentRequest{WorkspaceId: "ws-1", Id: id, Name: name})
		}
		renamed, err := rename("doc-1", "  Master agreement  ")
		if err != nil || renamed.GetDocument().GetName() != "Master agreement" {
			t.Fatalf("rename = (%+v, %v): trimmed, and its extension may go", renamed, err)
		}
		for name, tc := range map[string]struct {
			id, name string
			code     codes.Code
		}{
			"an empty name":      {"doc-1", "   ", codes.InvalidArgument},
			"a long name":        {"doc-1", strings.Repeat("n", 256), codes.InvalidArgument},
			"a slash":            {"doc-1", "a/b.md", codes.InvalidArgument},
			"a backslash":        {"doc-1", `a\b.md`, codes.InvalidArgument},
			"a control":          {"doc-1", "a\tb.md", codes.InvalidArgument},
			"a deleted document": {"doc-2", "c.md", codes.FailedPrecondition},
			"a missing document": {"doc-9", "c.md", codes.NotFound},
		} {
			if _, err := rename(tc.id, tc.name); status.Code(err) != tc.code {
				t.Fatalf("%s: %v, want %s", name, err, tc.code)
			}
		}
		if _, err := rename("doc-1", strings.Repeat("é", 255)); err != nil {
			t.Fatalf("255 characters: %v", err)
		}
	})

	// The Data document page reads the text through the backend: the
	// bucket lets a browser PUT, not GET.
	t.Run("reads an uploaded document's text", func(t *testing.T) {
		s, store, bucket := foundryService(t)
		store.documents = append(store.documents,
			&foundrymodel.FoundryDocument{ID: "doc-1", WorkspaceID: "ws-1", SetID: "set-general", MediaType: foundrymodel.FoundryMediaMarkdown,
				Status: foundrymodel.FoundryDocumentUploaded, S3Key: "k1", SizeBytes: 12},
			&foundrymodel.FoundryDocument{ID: "doc-2", WorkspaceID: "ws-1", SetID: "set-general", MediaType: foundrymodel.FoundryMediaMarkdown,
				Status: foundrymodel.FoundryDocumentPendingUpload, S3Key: "k2"},
			&foundrymodel.FoundryDocument{ID: "doc-3", WorkspaceID: "ws-1", SetID: "set-general", MediaType: foundrymodel.FoundryMediaMarkdown,
				Status: foundrymodel.FoundryDocumentUploaded, S3Key: "k3", SizeBytes: s.deps.Cfg.Foundry.Documents.MaxBytes + 1})
		bucket.content = map[string]string{"k1": "# Contract\n\xff"}
		read := func(id string) (*foundryv1.GetFoundryDocumentContentResponse, error) {
			return s.GetFoundryDocumentContent(ctx, &foundryv1.GetFoundryDocumentContentRequest{WorkspaceId: "ws-1", Id: id})
		}
		got, err := read("doc-1")
		if err != nil || got.GetText() != "# Contract\n�" {
			t.Fatalf("text = (%q, %v): bytes that are not UTF-8 read as U+FFFD", got.GetText(), err)
		}
		// JSON, CSV, TSV and JSONL are text too; Parquet is read as bytes.
		store.documents = append(store.documents,
			&foundrymodel.FoundryDocument{ID: "doc-csv", WorkspaceID: "ws-1", SetID: "set-general", MediaType: foundrymodel.FoundryMediaCSV,
				Status: foundrymodel.FoundryDocumentUploaded, S3Key: "k-csv", SizeBytes: 8},
			&foundrymodel.FoundryDocument{ID: "doc-parquet", WorkspaceID: "ws-1", SetID: "set-general", MediaType: foundrymodel.FoundryMediaParquet,
				Status: foundrymodel.FoundryDocumentUploaded, S3Key: "k-parquet", SizeBytes: 8})
		bucket.content["k-csv"] = "id,name\n"
		if got, err := read("doc-csv"); err != nil || got.GetText() != "id,name\n" {
			t.Fatalf("csv = (%q, %v)", got.GetText(), err)
		}
		if _, err := read("doc-parquet"); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Parquet documents have no text; use :bytes") {
			t.Fatalf("a Parquet file's text: %v", err)
		}
		for name, tc := range map[string]struct {
			id   string
			code codes.Code
		}{
			"a pending upload":   {"doc-2", codes.FailedPrecondition},
			"past the cap":       {"doc-3", codes.FailedPrecondition},
			"a missing document": {"doc-9", codes.NotFound},
		} {
			if _, err := read(tc.id); status.Code(err) != tc.code {
				t.Fatalf("%s: %v, want %s", name, err, tc.code)
			}
		}
		bucket.openErr = fmt.Errorf("get: %w", document.ErrMissing)
		if _, err := read("doc-1"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("a missing original: %v", err)
		}
		bucket.openErr = errors.New("s3 down")
		if _, err := read("doc-1"); status.Code(err) != codes.Unavailable {
			t.Fatalf("s3 down: %v", err)
		}
		s.deps.Documents = nil
		if _, err := read("doc-1"); status.Code(err) != codes.Unavailable {
			t.Fatalf("without a bucket: %v", err)
		}
	})

	// A PDF's text is what its read stored, as loads read it, never its
	// original's bytes; until the read succeeds the page is told why there
	// is none.
	t.Run("reads a PDF's text", func(t *testing.T) {
		s, store, bucket := foundryService(t)
		text := "ws/ws-1/documents/doc-pdf/read/standard-ab.txt"
		pages := 2
		pdf := func(id string, read *foundrymodel.FoundryFileRead) *foundrymodel.FoundryDocument {
			return &foundrymodel.FoundryDocument{ID: id, WorkspaceID: "ws-1", SetID: "set-general", MediaType: foundrymodel.FoundryMediaPDF,
				Status: foundrymodel.FoundryDocumentUploaded, S3Key: "ws/ws-1/documents/" + id + "/original", SizeBytes: 9, Read: read}
		}
		store.documents = append(store.documents,
			pdf("doc-pdf", &foundrymodel.FoundryFileRead{Status: foundrymodel.FoundryFileReadSucceeded, ArtifactKey: &text, PageCount: &pages}),
			pdf("doc-reading", &foundrymodel.FoundryFileRead{Status: foundrymodel.FoundryFileReadRunning}),
			pdf("doc-scan", &foundrymodel.FoundryFileRead{Status: foundrymodel.FoundryFileReadFailed,
				Error: &foundrymodel.FoundryFileReadError{Code: foundrymodel.FoundryFileReadUnsupported, Message: "Scanned PDF: no text to read"}}))
		bucket.content = map[string]string{"ws/ws-1/documents/doc-pdf/original": "%PDF-1.7\n", text: "Invoice 1042\n\fTerms\n"}
		read := func(id string) (*foundryv1.GetFoundryDocumentContentResponse, error) {
			return s.GetFoundryDocumentContent(ctx, &foundryv1.GetFoundryDocumentContentRequest{WorkspaceId: "ws-1", Id: id})
		}
		if got, err := read("doc-pdf"); err != nil || got.GetText() != "Invoice 1042\n\fTerms\n" {
			t.Fatalf("text = (%q, %v)", got.GetText(), err)
		}
		for id, want := range map[string]string{
			"doc-reading": "still being read",
			"doc-scan":    "Scanned PDF: no text to read",
		} {
			if _, err := read(id); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: %v, want %q", id, err, want)
			}
		}
		bucket.openErr = fmt.Errorf("get: %w", document.ErrMissing)
		if _, err := read("doc-pdf"); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "the PDF's text is gone") {
			t.Fatalf("a missing text: %v", err)
		}
		bucket.openErr = errors.New("s3 down")
		if _, err := read("doc-pdf"); status.Code(err) != codes.Unavailable {
			t.Fatalf("s3 down: %v", err)
		}
	})

	// The Data document page reads any document's bytes through the backend,
	// with its type, as it reads text.
	t.Run("reads an uploaded document's bytes", func(t *testing.T) {
		s, store, bucket := foundryService(t)
		pdf := "%PDF-1.7\n\xff\x00"
		store.documents = append(store.documents,
			&foundrymodel.FoundryDocument{ID: "doc-1", WorkspaceID: "ws-1", SetID: "set-general", MediaType: foundrymodel.FoundryMediaPDF,
				Status: foundrymodel.FoundryDocumentUploaded, S3Key: "k1", SizeBytes: int64(len(pdf))},
			&foundrymodel.FoundryDocument{ID: "doc-2", WorkspaceID: "ws-1", SetID: "set-general", MediaType: foundrymodel.FoundryMediaPDF,
				Status: foundrymodel.FoundryDocumentDeleted, S3Key: "k2"},
			&foundrymodel.FoundryDocument{ID: "doc-3", WorkspaceID: "ws-1", SetID: "set-general", MediaType: foundrymodel.FoundryMediaParquet,
				Status: foundrymodel.FoundryDocumentUploaded, S3Key: "k3", SizeBytes: s.deps.Cfg.Foundry.Documents.MaxTableBytes + 1})
		bucket.content = map[string]string{"k1": pdf}
		read := func(id string) (*foundryv1.GetFoundryDocumentBytesResponse, error) {
			return s.GetFoundryDocumentBytes(ctx, &foundryv1.GetFoundryDocumentBytesRequest{WorkspaceId: "ws-1", Id: id})
		}
		got, err := read("doc-1")
		if err != nil || got.GetMediaType() != "application/pdf" || string(got.GetData()) != pdf {
			t.Fatalf("bytes = (%+v, %v): the original, unchanged, with its type", got, err)
		}
		for name, tc := range map[string]struct {
			id   string
			code codes.Code
		}{
			"a deleted document": {"doc-2", codes.FailedPrecondition},
			"past the cap":       {"doc-3", codes.FailedPrecondition},
			"a missing document": {"doc-9", codes.NotFound},
			"no document id":     {"", codes.InvalidArgument},
		} {
			if _, err := read(tc.id); status.Code(err) != tc.code {
				t.Fatalf("%s: %v, want %s", name, err, tc.code)
			}
		}
		bucket.openErr = fmt.Errorf("get: %w", document.ErrMissing)
		if _, err := read("doc-1"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("a missing original: %v", err)
		}
		bucket.openErr = errors.New("s3 down")
		if _, err := read("doc-1"); status.Code(err) != codes.Unavailable {
			t.Fatalf("s3 down: %v", err)
		}
	})

	// The Data layout is the website's object, kept as it is, behind a
	// revision so two tabs never lose each other's change unknowingly.
	t.Run("reads and writes the Data layout", func(t *testing.T) {
		s, _, _ := foundryService(t)
		got, err := s.GetFoundryDataLayout(ctx, &foundryv1.GetFoundryDataLayoutRequest{WorkspaceId: "ws-1"})
		if err != nil || got.GetRevision() != 0 || len(got.GetLayout().GetFields()) != 0 {
			t.Fatalf("no layout = (%+v, %v)", got, err)
		}
		layout, err := structpb.NewStruct(map[string]any{"v": 1, "order": map[string]any{"root": []any{"kind:stripe"}}})
		if err != nil {
			t.Fatal(err)
		}
		put := func(l *structpb.Struct, expected int64) (*foundryv1.FoundryDataLayoutResponse, error) {
			return s.PutFoundryDataLayout(ports.WithActor(ctx, person), &foundryv1.PutFoundryDataLayoutRequest{
				WorkspaceId: "ws-1", Layout: l, ExpectedRevision: expected,
			})
		}
		written, err := put(layout, 0)
		if err != nil || written.GetRevision() != 1 || written.GetLayout().GetFields()["v"].GetNumberValue() != 1 {
			t.Fatalf("first write = (%+v, %v)", written, err)
		}
		if _, err := put(layout, 0); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("a stale first write: %v", err)
		}
		got, err = s.GetFoundryDataLayout(ctx, &foundryv1.GetFoundryDataLayoutRequest{WorkspaceId: "ws-1"})
		if err != nil || got.GetRevision() != 1 || got.GetLayout().GetFields()["order"] == nil {
			t.Fatalf("read back = (%+v, %v)", got, err)
		}
		big, err := structpb.NewStruct(map[string]any{"names": strings.Repeat("n", maxDataLayoutBytes)})
		if err != nil {
			t.Fatal(err)
		}
		for name, tc := range map[string]struct {
			layout   *structpb.Struct
			expected int64
		}{
			"no layout":           {nil, 1},
			"past 64 KiB":         {big, 1},
			"a negative revision": {layout, -1},
		} {
			if _, err := put(tc.layout, tc.expected); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if _, err := s.PutFoundryDataLayout(ctx, &foundryv1.PutFoundryDataLayoutRequest{Layout: layout}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("no workspace: %v", err)
		}
	})

	// The limit is on compact JSON, whatever spacing protojson chose on the
	// build: 16,000 commas would add 16,000 bytes if their spaces counted.
	// {"ids":["a",…×16000],"pad":"x…"} is 4·16000 + len(pad) + 18 bytes.
	t.Run("measures and stores the Data layout as compact JSON", func(t *testing.T) {
		s, store, _ := foundryService(t)
		ids := make([]any, 16000)
		for i := range ids {
			ids[i] = "a"
		}
		layoutOf := func(size int) *structpb.Struct {
			l, err := structpb.NewStruct(map[string]any{"ids": ids, "pad": strings.Repeat("x", size-4*len(ids)-18)})
			if err != nil {
				t.Fatal(err)
			}
			return l
		}
		_, err := s.PutFoundryDataLayout(ctx, &foundryv1.PutFoundryDataLayoutRequest{WorkspaceId: "ws-1", Layout: layoutOf(maxDataLayoutBytes)})
		if err != nil {
			t.Fatalf("exactly 64 KiB: %v", err)
		}
		stored := store.layouts["ws-1"].Layout
		if len(stored) != maxDataLayoutBytes || bytes.Contains(stored, []byte(", ")) || !json.Valid(stored) {
			t.Fatalf("stored %d bytes, spaced %t", len(stored), bytes.Contains(stored, []byte(", ")))
		}
		_, err = s.PutFoundryDataLayout(ctx, &foundryv1.PutFoundryDataLayoutRequest{WorkspaceId: "ws-1", Layout: layoutOf(maxDataLayoutBytes + 1), ExpectedRevision: 1})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "65537 bytes") {
			t.Fatalf("a byte past 64 KiB: %v", err)
		}
		// A layout keeps <, > and & as they are; only spacing changes.
		marked, err := structpb.NewStruct(map[string]any{"name": "R&D <draft>"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutFoundryDataLayout(ctx, &foundryv1.PutFoundryDataLayoutRequest{WorkspaceId: "ws-1", Layout: marked, ExpectedRevision: 1}); err != nil {
			t.Fatal(err)
		}
		if got := string(store.layouts["ws-1"].Layout); got != `{"name":"R&D <draft>"}` {
			t.Fatalf("stored %s", got)
		}
	})
}

func TestFoundryLoadRPCs(t *testing.T) {
	ctx := context.Background()
	gen := "gen-1"
	draft := func(status string) *foundrymodel.OntologyVersion {
		def, _ := json.Marshal(map[string]any{"format": "agent_bundle", "status": status, "ontology_toml": "", "mappings_toml": "",
			"rules": map[string]int{"count": 2, "evaluated": 2}})
		return &foundrymodel.OntologyVersion{
			ID: "v-" + status, WorkspaceID: "ws-1", OntologyID: "ont-1", Version: 3, Format: foundrymodel.OntologyFormatAgentBundle,
			Source: foundrymodel.OntologySourceGenerated, GenerationID: &gen, Definition: def,
		}
	}
	load := func(s *Service, version, token string) (*foundryv1.FoundryLoadResponse, error) {
		return s.CreateFoundryLoad(ports.WithActor(ctx, person), &foundryv1.CreateFoundryLoadRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: version, ClientToken: token,
		})
	}
	// ended stands for the worker: the load job ends and its load is ready (and current, when current is set).
	ended := func(store *foundryStore, loadID string, status foundrymodel.FoundryLoadStatus, current bool) {
		finished := time.Now()
		store.load(loadID).Status, store.load(loadID).FinishedAt = status, &finished
		for _, j := range store.jobs {
			if j.LoadID == loadID {
				j.Status = foundrymodel.FoundryGraphJobSucceeded
			}
		}
		if current {
			store.ontologies["ont-1"].CurrentLoadID = &loadID
		}
	}

	// A finished agent draft becomes a new load, whose id is its job's; the same click returns the same load.
	t.Run("queues a load of a finished draft", func(t *testing.T) {
		s, store, _ := foundryService(t)
		store.versions = append(store.versions, draft("done"))
		resp, err := load(s, "v-done", "load-1")
		if err != nil {
			t.Fatal(err)
		}
		l := resp.GetLoad()
		if l.GetId() == "" || l.GetStatus() != "writing" || l.GetVersionId() != "v-done" || l.GetVersion() != "foundry-v3" || l.GetCurrent() ||
			l.GetFinishedAt() != nil || len(l.GetJobs()) != 1 {
			t.Fatalf("load = %+v", l)
		}
		job := l.GetJobs()[0]
		if job.GetId() != l.GetId() || job.GetLoadId() != l.GetId() || job.GetKind() != "load" || job.GetStatus() != "queued" ||
			job.GetPhase() != "provisioning" || job.GetProgress() == nil {
			t.Fatalf("job = %+v", job)
		}
		if *store.jobs[0].RequestedByUserID != "user-1" {
			t.Fatalf("stored job = %+v", store.jobs[0])
		}
		if again, err := load(s, "v-done", "load-1"); err != nil || again.GetLoad().GetId() != l.GetId() {
			t.Fatalf("retry = (%+v, %v)", again, err)
		}
		if _, err := load(s, "v-other", "load-1"); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("token reused for another version: %v", err)
		}
		if _, err := load(s, "v-done", "load-2"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("a second live load: %v", err)
		}
		got, err := s.GetFoundryLoad(ctx, &foundryv1.GetFoundryLoadRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: l.GetId()})
		if err != nil || got.GetLoad().GetId() != l.GetId() {
			t.Fatalf("get = (%+v, %v)", got, err)
		}
		if _, err := s.GetFoundryLoad(ctx, &foundryv1.GetFoundryLoadRequest{WorkspaceId: "ws-2", OntologyId: "ont-2", Id: l.GetId()}); status.Code(err) != codes.NotFound {
			t.Fatalf("another ontology's load: %v", err)
		}
		if _, err := s.GetFoundryLoad(ctx, &foundryv1.GetFoundryLoadRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("no id: %v", err)
		}
	})

	// A new load is what the user waits for; a delete in progress gives way to it.
	t.Run("queues a load in place of a live delete", func(t *testing.T) {
		s, store, _ := foundryService(t)
		store.versions = append(store.versions, draft("done"))
		store.loads = append(store.loads, &foundrymodel.FoundryLoad{ID: "old", OntologyID: "ont-1", Status: foundrymodel.FoundryLoadDeleting})
		store.jobs = append(store.jobs, &foundrymodel.FoundryGraphJob{ID: "del", LoadID: "old", OntologyID: "ont-1", Kind: foundrymodel.FoundryGraphJobDelete,
			Status: foundrymodel.FoundryGraphJobRunning})
		resp, err := load(s, "v-done", "load-1")
		if err != nil || resp.GetLoad().GetStatus() != "writing" {
			t.Fatalf("load = (%+v, %v)", resp, err)
		}
		if store.jobs[0].Status != foundrymodel.FoundryGraphJobCancelled || store.load("old").Status != foundrymodel.FoundryLoadDeleting {
			t.Fatalf("delete %s, its load %s", store.jobs[0].Status, store.load("old").Status)
		}
	})

	// Only a draft whose tables are named and whose build finished can be ingested.
	t.Run("refuses a version it can't load", func(t *testing.T) {
		s, store, _ := foundryService(t)
		manual := &foundrymodel.OntologyVersion{ID: "v-manual", OntologyID: "ont-1", Format: foundrymodel.OntologyFormatFoundryV1, Definition: []byte(`{}`)}
		orphan := draft("done")
		orphan.ID, orphan.GenerationID = "v-orphan", nil
		// A draft whose rules were not all evaluated against data, or that records no evaluation, has untested rules.
		unevaluated, unrecorded := draft("done"), draft("done")
		unevaluated.ID, unevaluated.Definition = "v-unevaluated", []byte(`{"format":"agent_bundle","status":"done","rules":{"count":3,"evaluated":2}}`)
		unrecorded.ID, unrecorded.Definition = "v-unrecorded", []byte(`{"format":"agent_bundle","status":"done","rules":null}`)
		store.versions = append(store.versions, manual, orphan, draft("needs_input"), draft("budget_exhausted"), unevaluated, unrecorded)
		for _, version := range []string{"v-manual", "v-orphan", "v-needs_input", "v-budget_exhausted", "v-unevaluated", "v-unrecorded"} {
			if _, err := load(s, version, "t-"+version); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("%s: error = %v", version, err)
			}
		}
		if _, err := load(s, "v-missing", "t-missing"); status.Code(err) != codes.NotFound {
			t.Fatalf("a missing version: %v", err)
		}
		// A definition that doesn't read is Helix's fault, not the caller's.
		broken := draft("done")
		broken.ID, broken.Definition = "v-broken", []byte(`{`)
		store.versions = append(store.versions, broken)
		if _, err := load(s, "v-broken", "t-broken"); status.Code(err) != codes.Internal {
			t.Fatalf("a definition that doesn't read: %v", err)
		}
		if _, err := load(s, "v-done", ""); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("no client token: %v", err)
		}
	})

	// A workspace past its month's budget queues no more paid work.
	t.Run("refuses a load past the monthly budget", func(t *testing.T) {
		s, store, _ := foundryService(t)
		store.versions = append(store.versions, draft("done"))
		store.spent = int64(s.deps.Cfg.Foundry.Graph.MonthlyUSD * 1e6)
		if _, err := load(s, "v-done", "load-1"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("past the budget: %v", err)
		}
	})

	// The Loads page lists every load not yet deleted, newest first, with
	// its jobs and which one reads go to.
	t.Run("lists loads with their jobs and the current one", func(t *testing.T) {
		s, store, _ := foundryService(t)
		store.versions = append(store.versions, draft("done"))
		first, err := load(s, "v-done", "load-1")
		if err != nil {
			t.Fatal(err)
		}
		ended(store, first.GetLoad().GetId(), foundrymodel.FoundryLoadReady, true)
		store.jobs = append(store.jobs, &foundrymodel.FoundryGraphJob{ID: "x-1", LoadID: first.GetLoad().GetId(), OntologyID: "ont-1",
			Kind: foundrymodel.FoundryGraphJobExtract, Status: foundrymodel.FoundryGraphJobSucceeded})
		second, err := load(s, "v-done", "load-2")
		if err != nil {
			t.Fatal(err)
		}
		store.loads = append(store.loads, &foundrymodel.FoundryLoad{ID: "gone", OntologyID: "ont-1", Status: foundrymodel.FoundryLoadDeleted})
		list, err := s.ListFoundryLoads(ctx, &foundryv1.ListFoundryLoadsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || len(list.GetLoads()) != 2 {
			t.Fatalf("list = (%+v, %v)", list, err)
		}
		newest, oldest := list.GetLoads()[0], list.GetLoads()[1]
		if newest.GetId() != second.GetLoad().GetId() || newest.GetCurrent() || oldest.GetId() != first.GetLoad().GetId() || !oldest.GetCurrent() ||
			oldest.GetStatus() != "ready" || oldest.GetFinishedAt() == nil || len(oldest.GetJobs()) != 2 || oldest.GetJobs()[0].GetKind() != "extract" {
			t.Fatalf("loads = %+v", list.GetLoads())
		}
		if _, err := s.GetFoundryLoad(ctx, &foundryv1.GetFoundryLoadRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "gone"}); status.Code(err) != codes.NotFound {
			t.Fatalf("a deleted load: %v", err)
		}
		store.listErr = errors.New("connection refused")
		if _, err := s.ListFoundryLoads(ctx, &foundryv1.ListFoundryLoadsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); status.Code(err) != codes.Internal {
			t.Fatalf("a database failure: %v", err)
		}
	})

	// A current load gains an extract per upload for as long as it is
	// current: the Loads page, which polls, gets the newest of them and the
	// count and spend of all, so its rows stay small.
	t.Run("shows a load's newest jobs and the totals of all", func(t *testing.T) {
		s, store, _ := foundryService(t)
		store.versions = append(store.versions, draft("done"))
		first, err := load(s, "v-done", "load-1")
		if err != nil {
			t.Fatal(err)
		}
		loadID := first.GetLoad().GetId()
		ended(store, loadID, foundrymodel.FoundryLoadReady, true)
		violations := uint64(3)
		store.load(loadID).IngestViolations = &violations
		for i := range foundryLoadJobsShown + 5 {
			store.jobs = append(store.jobs, &foundrymodel.FoundryGraphJob{ID: fmt.Sprintf("x-%d", i), LoadID: loadID, OntologyID: "ont-1",
				Kind: foundrymodel.FoundryGraphJobExtract, Status: foundrymodel.FoundryGraphJobSucceeded, Spend: foundrymodel.FoundryGraphJobSpend{USDMicros: 1000}})
		}
		list, err := s.ListFoundryLoads(ctx, &foundryv1.ListFoundryLoadsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || len(list.GetLoads()) != 1 {
			t.Fatalf("list = (%+v, %v)", list, err)
		}
		got, err := s.GetFoundryLoad(ctx, &foundryv1.GetFoundryLoadRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: loadID})
		if err != nil {
			t.Fatal(err)
		}
		for name, l := range map[string]*foundryv1.FoundryLoad{"listed": list.GetLoads()[0], "got": got.GetLoad()} {
			if len(l.GetJobs()) != foundryLoadJobsShown || l.GetJobs()[0].GetId() != fmt.Sprintf("x-%d", foundryLoadJobsShown+4) ||
				l.GetJobCount() != foundryLoadJobsShown+6 || l.GetUsdMicros() != int64(foundryLoadJobsShown+5)*1000 {
				t.Fatalf("%s: %d jobs, the newest %s, %d in all spending %d", name, len(l.GetJobs()), l.GetJobs()[0].GetId(),
					l.GetJobCount(), l.GetUsdMicros())
			}
			// The ingest report's count is the load's, unset until counted.
			if l.IngestViolations == nil || l.GetIngestViolations() != 3 {
				t.Fatalf("%s: ingest violations %v, want 3", name, l.IngestViolations)
			}
		}
		store.load(loadID).IngestViolations = nil
		if got, _ := s.GetFoundryLoad(ctx, &foundryv1.GetFoundryLoadRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: loadID}); got.GetLoad().IngestViolations != nil {
			t.Fatalf("an uncounted load shows %d violations", got.GetLoad().GetIngestViolations())
		}
	})

	// Going back to an older load: it becomes current and the store is
	// handed an extract (with its requester) for what it lacks. A load that
	// can't serve reads is refused with why.
	t.Run("sets a ready or partial load current", func(t *testing.T) {
		s, store, _ := foundryService(t)
		store.versions = append(store.versions, draft("done"))
		first, _ := load(s, "v-done", "load-1")
		ended(store, first.GetLoad().GetId(), foundrymodel.FoundryLoadPartial, false)
		second, _ := load(s, "v-done", "load-2")
		setCurrent := func(id string) (*foundryv1.FoundryLoadResponse, error) {
			return s.SetCurrentFoundryLoad(ports.WithActor(ctx, person), &foundryv1.SetCurrentFoundryLoadRequest{
				WorkspaceId: "ws-1", OntologyId: "ont-1", Id: id,
			})
		}
		_, err := setCurrent(second.GetLoad().GetId())
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "ready or partial") {
			t.Fatalf("a load being written: %v", err)
		}
		resp, err := setCurrent(first.GetLoad().GetId())
		if err != nil || !resp.GetLoad().GetCurrent() || resp.GetLoad().GetStatus() != "partial" {
			t.Fatalf("set current = (%+v, %v)", resp, err)
		}
		if len(store.completions) != 1 || store.completions[0].ID == "" || *store.completions[0].RequestedByUserID != "user-1" {
			t.Fatalf("catch-up extract handed over = %+v", store.completions)
		}
		for id, code := range map[string]codes.Code{"missing": codes.NotFound, "": codes.InvalidArgument} {
			if _, err := setCurrent(id); status.Code(err) != code {
				t.Fatalf("%q: %v, want %s", id, err, code)
			}
		}
	})

	// Deleting a load runs in the background; the load says deleting until
	// then. The current load, and one being written, stay.
	t.Run("deletes a load that is neither current nor written", func(t *testing.T) {
		s, store, _ := foundryService(t)
		store.versions = append(store.versions, draft("done"))
		first, _ := load(s, "v-done", "load-1")
		ended(store, first.GetLoad().GetId(), foundrymodel.FoundryLoadReady, true)
		second, _ := load(s, "v-done", "load-2")
		remove := func(id string) (*foundryv1.FoundryLoadResponse, error) {
			return s.DeleteFoundryLoad(ctx, &foundryv1.DeleteFoundryLoadRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: id})
		}
		for id, says := range map[string]string{first.GetLoad().GetId(): "current load", second.GetLoad().GetId(): "writing"} {
			if _, err := remove(id); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), says) {
				t.Fatalf("delete %s: %v, want it refused saying %q", id, err, says)
			}
		}
		ended(store, second.GetLoad().GetId(), foundrymodel.FoundryLoadReady, true)
		resp, err := remove(first.GetLoad().GetId())
		if err != nil || resp.GetLoad().GetStatus() != "deleting" || resp.GetLoad().GetCurrent() {
			t.Fatalf("delete = (%+v, %v)", resp, err)
		}
		if len(store.deletesQueued) != 1 || store.deletesQueued[0] == "" {
			t.Fatalf("deletes queued = %v", store.deletesQueued)
		}
		for id, code := range map[string]codes.Code{"missing": codes.NotFound, "": codes.InvalidArgument} {
			if _, err := remove(id); status.Code(err) != code {
				t.Fatalf("%q: %v, want %s", id, err, code)
			}
		}
		if _, err := s.DeleteFoundryLoad(ctx, &foundryv1.DeleteFoundryLoadRequest{WorkspaceId: "ws-2", OntologyId: "ont-1", Id: "x"}); status.Code(err) != codes.NotFound {
			t.Fatalf("another workspace's ontology: %v", err)
		}
	})
}

func (s *foundryStore) SetFoundryOntologyAutoRefresh(_ context.Context, workspaceID, id string, on bool) error {
	o, ok := s.ontologies[id]
	if !ok || o.WorkspaceID != workspaceID || o.DeletedAt != nil {
		return foundrystore.ErrNotFound
	}
	o.AutoRefresh = on
	if !on {
		o.RefreshDueAt = nil
	}
	return nil
}

func TestFoundryOntologyRefreshSetting(t *testing.T) {
	ctx := context.Background()
	newOntology := func(t *testing.T) (*Service, *foundryStore, string) {
		t.Helper()
		s, store, _ := foundryService(t)
		resp, err := s.CreateFoundryOntology(ctx, &foundryv1.CreateFoundryOntologyRequest{
			WorkspaceId: "ws-1", Name: "Support", TenantId: "tenant-9", ClientToken: "click-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		id := resp.GetOntology().GetId()
		store.ontologies[id].AutoRefresh = true
		return s, store, id
	}
	// A customer whose own data hangs off Foundry's nodes can stop refreshes deleting it.
	t.Run("turns refresh off and forgets an owed one", func(t *testing.T) {
		s, store, id := newOntology(t)
		due := time.Now().Add(-time.Minute)
		store.ontologies[id].RefreshDueAt = &due
		off := false
		resp, err := s.UpdateFoundryOntology(ctx, &foundryv1.UpdateFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: id, AutoRefresh: &off})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetOntology().GetAutoRefresh() || resp.GetOntology().GetRefreshDueAt() != nil || store.ontologies[id].AutoRefresh {
			t.Fatalf("ontology = %+v", resp.GetOntology())
		}
	})
	t.Run("refuses an update that changes nothing", func(t *testing.T) {
		s, _, id := newOntology(t)
		_, err := s.UpdateFoundryOntology(ctx, &foundryv1.UpdateFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: id})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("UpdateFoundryOntology error = %v", err)
		}
	})
	// A refresh that is due but not queued says why, so the website doesn't
	// show a stuck graph. Only a live job holds one back: a spent month
	// doesn't, as a refresh with nothing to spend replays at $0.
	t.Run("says why a due refresh waits", func(t *testing.T) {
		s, store, id := newOntology(t)
		due := time.Now().Add(-time.Minute)
		store.ontologies[id].RefreshDueAt = &due
		store.spent = int64(s.deps.Cfg.Foundry.Graph.MonthlyUSD * 1e6)
		resp, err := s.GetFoundryOntology(ctx, &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: id})
		if err != nil || resp.GetOntology().GetRefreshWaiting() != "" || !resp.GetOntology().GetAutoRefresh() {
			t.Fatalf("GetFoundryOntology with the month spent = (%+v, %v)", resp, err)
		}
		store.jobs = append(store.jobs, &foundrymodel.FoundryGraphJob{ID: "job-1", OntologyID: id, Kind: foundrymodel.FoundryGraphJobLoad,
			Status: foundrymodel.FoundryGraphJobRunning, Phase: foundrymodel.FoundryGraphJobIngest})
		resp, err = s.GetFoundryOntology(ctx, &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: id})
		if err != nil || resp.GetOntology().GetRefreshWaiting() != "busy" {
			t.Fatalf("GetFoundryOntology with a job live = (%+v, %v)", resp, err)
		}
	})
}

// The loads list tells a refresh from a load someone started.
func TestFoundryLoadOrigin(t *testing.T) {
	load := &foundrymodel.FoundryLoad{ID: "l1", Version: 2, Status: foundrymodel.FoundryLoadReady, Origin: foundrymodel.FoundryLoadByRefresh, CreatedAt: time.Now()}
	if got := foundryLoadToProto(load, &foundrymodel.FoundryOntology{}, foundrymodel.FoundryLoadJobs{}, nil).GetOrigin(); got != "refresh" {
		t.Fatalf("origin = %q", got)
	}
}
