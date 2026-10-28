package foundrygraphworker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// changeLog is what an apply reads to send a table as its changes.
type changeLog interface {
	ListFoundryLoadSources(ctx context.Context, loadIDs []string) (map[string][]model.FoundryLoadSource, error)
	ListConnectorChangeBatchesAfter(ctx context.Context, workspaceID, connectorID, table string, afterSeq, toSeq int64) (
		[]*model.ConnectorChangeBatch, error)
	ListConnectors(ctx context.Context, workspaceID string) ([]*model.Connector, error)
	ListConnectorTables(ctx context.Context, workspaceID, connectorID string) ([]*model.ConnectorTable, error)
}

// tableChange is one pinned table of an apply beside the position its load
// holds it at; position is nil for a load from before positions were kept.
type tableChange struct {
	pin      model.FoundryLoadSource
	position *model.FoundryLoadSource
	table    *model.ConnectorTable
	kind     string
}

// changed reports whether the load holds the table at another snapshot.
func (c tableChange) changed() bool { return c.position == nil || c.position.S3Key != c.pin.S3Key }

// fitsChangeFile reports whether a changed table's change batches can stand
// in for its snapshot: every batch from the load's position to the pin is
// there, numbered without a gap, of upserts and deletes by key; the table
// is not windowed, whose rows leave without a delete; its mappings key it by
// its primary key, the key a delete line carries, and nest no mapping under
// it; nothing looks it up, since a lookup reads its whole index; nothing its
// mappings look up changed, since its rows' hashes hold what they looked up;
// and it changed in fewer rows than half its own.
func fitsChangeFile(c tableChange, batches []*model.ConnectorChangeBatch, mapped *ontologyagent.MappedTable, changed map[string]bool) bool {
	if c.position == nil || c.table == nil || c.position.ChangeSeq >= c.pin.ChangeSeq || !c.changed() ||
		int64(len(batches)) != c.pin.ChangeSeq-c.position.ChangeSeq || connector.WindowedTable(c.kind, c.pin.Table) {
		return false
	}
	var rows int64
	for i, b := range batches {
		if b.Seq != c.position.ChangeSeq+int64(i)+1 || b.Kind != model.ConnectorChangesBatch || b.S3Key == nil {
			return false
		}
		rows += b.Inserts + b.Updates + b.Deletes
	}
	if rows*2 >= c.table.RowCount {
		return false
	}
	var primaryKey []string
	if json.Unmarshal(c.table.PrimaryKey, &primaryKey) != nil || len(primaryKey) == 0 {
		return false
	}
	if mapped == nil || mapped.Nested || mapped.LookedUp || len(mapped.RowKeys) == 0 {
		return false
	}
	for _, key := range mapped.RowKeys {
		if !sameColumns(key, primaryKey) {
			return false
		}
	}
	return !slices.ContainsFunc(mapped.LooksUp, func(source string) bool { return changed[source] })
}

// sameColumns compares two keys as sets: a change line's key is an object.
func sameColumns(a, b []string) bool {
	return len(a) == len(b) && !slices.ContainsFunc(a, func(c string) bool { return !slices.Contains(b, c) })
}

// changeFiles sends each table of an apply that fits (fitsChangeFile) as its
// changes since the load's position: the batches between, merged into one
// file under the job's directory, with the snapshot as the source's file
// still. Anything that goes wrong reading the log sends that table's
// snapshot instead, which the agent diffs whole; an apply never fails for it.
func changeFiles(ctx context.Context, db changeLog, files snapshot.Store, workDir string, job *model.FoundryGraphJob, mappingsTOML string,
	sources []ontologyagent.Source) []ontologyagent.Source {
	logger := log.From(ctx).With("job", job.ID)
	mapped, err := ontologyagent.MappedTables(mappingsTOML)
	if err != nil {
		logger.Warn("graph job: the mappings can't be read for change files", "err", err)
		return sources
	}
	positions, err := db.ListFoundryLoadSources(ctx, []string{job.LoadID})
	if err != nil {
		logger.Warn("graph job: read the load's positions failed", "err", err)
		return sources
	}
	held := map[[2]string]*model.FoundryLoadSource{}
	for i, p := range positions[job.LoadID] {
		held[[2]string{p.ConnectorID, p.Table}] = &positions[job.LoadID][i]
	}
	connectors, err := db.ListConnectors(ctx, job.WorkspaceID)
	if err != nil {
		logger.Warn("graph job: read the connectors failed", "err", err)
		return sources
	}
	kinds := map[string]string{}
	for _, c := range connectors {
		kinds[c.ID] = c.Kind
	}
	tables := map[[2]string]*model.ConnectorTable{}
	bySource := map[string]tableChange{}
	changed := map[string]bool{}
	for _, pin := range job.Sources {
		if _, read := tables[[2]string{pin.ConnectorID, ""}]; !read {
			rows, err := db.ListConnectorTables(ctx, job.WorkspaceID, pin.ConnectorID)
			if err != nil {
				logger.Warn("graph job: read the connector's tables failed", "connector", pin.ConnectorID, "err", err)
				return sources
			}
			tables[[2]string{pin.ConnectorID, ""}] = nil
			for _, t := range rows {
				tables[[2]string{pin.ConnectorID, t.TableName}] = t
			}
		}
		c := tableChange{pin: pin, position: held[[2]string{pin.ConnectorID, pin.Table}],
			table: tables[[2]string{pin.ConnectorID, pin.Table}], kind: kinds[pin.ConnectorID]}
		id := ontologyagent.TableSourceID(pin.ConnectorID, pin.Table)
		bySource[id], changed[id] = c, c.changed()
	}
	out := slices.Clone(sources)
	for i, s := range out {
		c, ok := bySource[s.ID]
		if !ok || !c.changed() || c.position == nil {
			continue
		}
		batches, err := db.ListConnectorChangeBatchesAfter(ctx, job.WorkspaceID, c.pin.ConnectorID, c.pin.Table, c.position.ChangeSeq, c.pin.ChangeSeq)
		if err != nil {
			logger.Warn("graph job: read the table's change batches failed", "table", c.pin.Table, "err", err)
			continue
		}
		if !fitsChangeFile(c, batches, mapped[s.ID], changed) {
			continue
		}
		keys := make([]string, len(batches))
		for j, b := range batches {
			keys[j] = *b.S3Key
		}
		rel := filepath.Join(jobsDir, job.ID, "changes", s.ID+".jsonl")
		if err := os.MkdirAll(filepath.Dir(filepath.Join(workDir, rel)), 0o755); err != nil {
			logger.Warn("graph job: stage the change file failed", "table", c.pin.Table, "err", err)
			continue
		}
		lines, err := snapshot.MergeBatches(ctx, files, keys, filepath.Join(workDir, rel))
		if err != nil {
			logger.Warn("graph job: merge the table's change batches failed; its snapshot goes instead", "table", c.pin.Table, "err", err)
			continue
		}
		out[i].Changes = &ontologyagent.ChangeFile{Path: rel, FromSeq: c.position.ChangeSeq, ToSeq: c.pin.ChangeSeq}
		changeFileRows.Add(float64(lines))
		logger.Info("graph job: a table goes as its changes", "table", c.pin.Table, "from_seq", c.position.ChangeSeq,
			"to_seq", c.pin.ChangeSeq, "changes", lines)
	}
	return out
}

// withoutChanges sends the snapshot of each source whose stream the agent
// holds at another position than its change file starts from.
func withoutChanges(sources []ontologyagent.Source, positions []ontologyagent.StoredPosition) ([]ontologyagent.Source, error) {
	out := slices.Clone(sources)
	for _, p := range positions {
		id, _, _ := cutStream(p.Stream)
		i := slices.IndexFunc(out, func(s ontologyagent.Source) bool { return s.ID == id })
		if i < 0 || out[i].Changes == nil {
			return nil, fmt.Errorf("the agent's stream %q is no change file Helix sent", p.Stream)
		}
		out[i].Changes = nil
	}
	return out, nil
}

// cutStream splits a stream into its source and mapping (U+001F between).
func cutStream(stream string) (source, mapping string, ok bool) {
	for i := range len(stream) {
		if stream[i] == 0x1f {
			return stream[:i], stream[i+1:], true
		}
	}
	return stream, "", false
}
