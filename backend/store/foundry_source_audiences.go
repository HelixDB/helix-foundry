package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// sourceAudienceChunk is how many rows one INSERT of a connector's
// audiences writes: a Slack workspace's people or private channels' members
// may be tens of thousands.
const sourceAudienceChunk = 500

// maxSourceAudienceProblem is the longest problem kept, in characters, as
// foundry_source_audiences.problem holds it.
const maxSourceAudienceProblem = 1024

// lockSourceAudiences locks a live connector of the workspace FOR UPDATE,
// which orders every write of its audiences, and a DeleteConnector, one
// after another, then reads when its audiences were last read and when the
// problem kept was found (each nil for none). ErrNotFound means the
// workspace has no such live connector.
func (s *mysqlDB) lockSourceAudiences(ctx context.Context, workspaceID, connectorID string) (readAt, problemAt *time.Time, err error) {
	var id string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM connectors WHERE id=? AND workspace_id=? AND deleted_at IS NULL FOR UPDATE`,
		connectorID, workspaceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("lock connector %s: %w", connectorID, err)
	}
	var read, problem sql.NullTime
	err = s.db.QueryRowContext(ctx, `SELECT read_at, problem_at FROM foundry_source_audiences WHERE connector_id=?`, connectorID).
		Scan(&read, &problem)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("read connector %s audiences: %w", connectorID, err)
	}
	if read.Valid {
		readAt = &read.Time
	}
	if problem.Valid {
		problemAt = &problem.Time
	}
	return readAt, problemAt, nil
}

// ReplaceSourceAudiences stores a, the audiences of a live connector of the
// workspace read at readAt, in place of every one stored before, in one
// transaction. A problem found after readAt stays; one found before it goes,
// since this read worked. A read no newer than the one stored changes
// nothing, so a late writer never brings back older audiences. An item key
// is 1 to 255 characters. ErrNotFound means the workspace has no such live
// connector.
func (s *mysqlDB) ReplaceSourceAudiences(ctx context.Context, workspaceID, connectorID string, readAt time.Time, a connector.Audiences) error {
	for key := range a.Items {
		if key == "" || utf8.RuneCountInString(key) > 255 {
			return fmt.Errorf("store connector %s audiences: an item key is empty or longer than 255 characters", connectorID)
		}
	}
	readAt = readAt.UTC().Truncate(time.Microsecond) // as DATETIME(6) keeps it
	return s.inTx(ctx, func(tx *mysqlDB) error {
		stored, problemAt, err := tx.lockSourceAudiences(ctx, workspaceID, connectorID)
		if err != nil {
			return err
		}
		if stored != nil && !stored.Before(readAt) {
			return nil
		}
		keepProblem := problemAt != nil && problemAt.After(readAt)
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_source_audiences (connector_id, workspace_id, read_at) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE read_at=VALUES(read_at), problem=IF(?, problem, NULL), problem_at=IF(?, problem_at, NULL)`,
			connectorID, workspaceID, readAt, keepProblem, keepProblem); err != nil {
			return fmt.Errorf("store connector %s audiences: %w", connectorID, err)
		}
		for _, table := range []string{"foundry_source_item_members", "foundry_source_items", "foundry_source_people"} {
			if _, err := tx.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE connector_id=?`, connectorID); err != nil {
				return fmt.Errorf("clear connector %s %s: %w", connectorID, table, err)
			}
		}
		emails := slices.SortedFunc(maps.Keys(a.People), func(x, y connector.Email) int { return cmp.Compare(x.String(), y.String()) })
		var people, items, members [][]any
		for _, e := range emails {
			people = append(people, []any{connectorID, e.String(), a.People[e].Guest})
		}
		for _, key := range slices.Sorted(maps.Keys(a.Items)) {
			audience := a.Items[key]
			items = append(items, []any{connectorID, key, audience.Kind() == connector.AudienceEveryone})
			for _, e := range audience.Members() {
				members = append(members, []any{connectorID, key, e.String()})
			}
		}
		for _, insert := range []struct {
			head string
			rows [][]any
		}{
			{"INSERT INTO foundry_source_people (connector_id, email, guest)", people},
			{"INSERT INTO foundry_source_items (connector_id, item_key, everyone)", items},
			{"INSERT INTO foundry_source_item_members (connector_id, item_key, email)", members},
		} {
			for chunk := range slices.Chunk(insert.rows, sourceAudienceChunk) {
				values := strings.TrimSuffix(strings.Repeat("("+idPlaceholders(len(chunk[0]))+"),", len(chunk)), ",")
				if _, err := tx.db.ExecContext(ctx, insert.head+" VALUES "+values, slices.Concat(chunk...)...); err != nil {
					return fmt.Errorf("store connector %s audiences: %w", connectorID, err)
				}
			}
		}
		return nil
	})
}

// RecordSourceAudienceProblem keeps why a read of a live connector's
// audiences at at failed, worded for the customer (cut to 1,024
// characters), beside the audiences stored before, which stay. A read that
// worked at or after at, or a problem found after it, keeps what is stored.
// ErrNotFound means the workspace has no such live connector.
func (s *mysqlDB) RecordSourceAudienceProblem(ctx context.Context, workspaceID, connectorID string, at time.Time, problem string) error {
	if problem = strings.TrimSpace(problem); problem == "" {
		return fmt.Errorf("record connector %s audience problem: the problem is empty", connectorID)
	}
	if runes := []rune(problem); len(runes) > maxSourceAudienceProblem {
		problem = string(runes[:maxSourceAudienceProblem])
	}
	at = at.UTC().Truncate(time.Microsecond) // as DATETIME(6) keeps it
	return s.inTx(ctx, func(tx *mysqlDB) error {
		readAt, problemAt, err := tx.lockSourceAudiences(ctx, workspaceID, connectorID)
		if err != nil {
			return err
		}
		if readAt != nil && !readAt.Before(at) || problemAt != nil && problemAt.After(at) {
			return nil
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_source_audiences (connector_id, workspace_id, problem, problem_at) VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE problem=VALUES(problem), problem_at=VALUES(problem_at)`, connectorID, workspaceID, problem, at); err != nil {
			return fmt.Errorf("record connector %s audience problem: %w", connectorID, err)
		}
		return nil
	})
}

// SourceSharingFor is what the reader whose email is email reads of each
// live connector of the workspace with audiences stored, by connector id:
// the items shared with everyone, unless the source knows the reader as a
// guest, and those that list them. An email that is no address
// (connector.ParseEmail), such as an agent key's empty one, is no one in
// any source: it sees only what is shared with everyone. Both reads see
// one moment of the store.
func (s *mysqlDB) SourceSharingFor(ctx context.Context, workspaceID, email string) ([]model.FoundrySourceSharing, error) {
	reader, _ := connector.ParseEmail(email) // the zero Email matches no one
	var out []model.FoundrySourceSharing
	err := s.inTx(ctx, func(tx *mysqlDB) error {
		rows, err := tx.db.QueryContext(ctx, `SELECT a.connector_id, a.read_at, COALESCE(p.guest, FALSE),
  (SELECT COUNT(*) FROM foundry_source_items i WHERE i.connector_id = a.connector_id)
FROM foundry_source_audiences a
JOIN connectors c ON c.id = a.connector_id AND c.workspace_id = a.workspace_id AND c.deleted_at IS NULL
LEFT JOIN foundry_source_people p ON p.connector_id = a.connector_id AND p.email = ?
WHERE a.workspace_id = ? ORDER BY a.connector_id`, reader.String(), workspaceID)
		if err != nil {
			return fmt.Errorf("read source sharing: %w", err)
		}
		out, err = scanAll(rows, func(row rowScanner) (model.FoundrySourceSharing, error) {
			var sharing model.FoundrySourceSharing
			var readAt sql.NullTime
			if err := row.Scan(&sharing.ConnectorID, &readAt, &sharing.Guest, &sharing.Items); err != nil {
				return sharing, err
			}
			if readAt.Valid {
				sharing.ReadAt = &readAt.Time
			}
			return sharing, nil
		})
		if err != nil {
			return fmt.Errorf("read source sharing: %w", err)
		}
		if len(out) == 0 {
			return nil
		}
		rows, err = tx.db.QueryContext(ctx, `SELECT i.connector_id, i.item_key FROM foundry_source_items i
JOIN foundry_source_audiences a ON a.connector_id = i.connector_id
LEFT JOIN foundry_source_people p ON p.connector_id = i.connector_id AND p.email = ?
WHERE a.workspace_id = ? AND (
  i.everyone AND NOT COALESCE(p.guest, FALSE) OR
  NOT i.everyone AND EXISTS (SELECT 1 FROM foundry_source_item_members m
    WHERE m.connector_id = i.connector_id AND m.item_key = i.item_key AND m.email = ?))
ORDER BY i.connector_id, i.item_key`, reader.String(), workspaceID, reader.String())
		if err != nil {
			return fmt.Errorf("read the items shared with a reader: %w", err)
		}
		visible, err := scanAll(rows, func(row rowScanner) (v [2]string, err error) {
			err = row.Scan(&v[0], &v[1])
			return v, err
		})
		if err != nil {
			return fmt.Errorf("read the items shared with a reader: %w", err)
		}
		at := map[string]int{}
		for i, sharing := range out {
			at[sharing.ConnectorID] = i
		}
		for _, v := range visible {
			// An item of a connector deleted since isn't the first query's.
			if i, ok := at[v[0]]; ok {
				out[i].Visible = append(out[i].Visible, v[1])
			}
		}
		return nil
	})
	return out, err
}

// ListSourceAudiences is the audiences of each live connector of the
// workspace with any stored, by connector id: when they were read, the
// problem kept, its items' counts and the source's people.
func (s *mysqlDB) ListSourceAudiences(ctx context.Context, workspaceID string) ([]model.FoundrySourceAudience, error) {
	var out []model.FoundrySourceAudience
	err := s.inTx(ctx, func(tx *mysqlDB) error {
		rows, err := tx.db.QueryContext(ctx, `SELECT a.connector_id, a.read_at, a.problem, a.problem_at,
  (SELECT COUNT(*) FROM foundry_source_items i WHERE i.connector_id = a.connector_id AND i.everyone),
  (SELECT COUNT(*) FROM foundry_source_items i WHERE i.connector_id = a.connector_id AND NOT i.everyone)
FROM foundry_source_audiences a
JOIN connectors c ON c.id = a.connector_id AND c.workspace_id = a.workspace_id AND c.deleted_at IS NULL
WHERE a.workspace_id = ? ORDER BY a.connector_id`, workspaceID)
		if err != nil {
			return fmt.Errorf("list source audiences: %w", err)
		}
		out, err = scanAll(rows, func(row rowScanner) (model.FoundrySourceAudience, error) {
			var a model.FoundrySourceAudience
			var readAt, problemAt sql.NullTime
			var problem sql.NullString
			if err := row.Scan(&a.ConnectorID, &readAt, &problem, &problemAt, &a.EveryoneItems, &a.MembersItems); err != nil {
				return a, err
			}
			if readAt.Valid {
				a.ReadAt = &readAt.Time
			}
			if problem.Valid && problemAt.Valid {
				a.Problem = &model.FoundrySourceAudienceProblem{Message: problem.String, At: problemAt.Time}
			}
			return a, nil
		})
		if err != nil {
			return fmt.Errorf("list source audiences: %w", err)
		}
		if len(out) == 0 {
			return nil
		}
		rows, err = tx.db.QueryContext(ctx, `SELECT p.connector_id, p.email, p.guest FROM foundry_source_people p
JOIN foundry_source_audiences a ON a.connector_id = p.connector_id
WHERE a.workspace_id = ? ORDER BY p.connector_id, p.email`, workspaceID)
		if err != nil {
			return fmt.Errorf("list source people: %w", err)
		}
		type person struct {
			connectorID string
			model.FoundrySourcePerson
		}
		people, err := scanAll(rows, func(row rowScanner) (p person, err error) {
			err = row.Scan(&p.connectorID, &p.Email, &p.Guest)
			return p, err
		})
		if err != nil {
			return fmt.Errorf("list source people: %w", err)
		}
		at := map[string]int{}
		for i, a := range out {
			at[a.ConnectorID] = i
		}
		for _, p := range people {
			if i, ok := at[p.connectorID]; ok {
				out[i].People = append(out[i].People, p.FoundrySourcePerson)
			}
		}
		return nil
	})
	return out, err
}
