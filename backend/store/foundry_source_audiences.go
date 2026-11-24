package store

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
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

// storedAudienceRead is what foundry_source_audiences keeps of a
// connector's last read: when the last one that worked was and its digest
// (nil for none), and when a problem kept was found (nil for none).
type storedAudienceRead struct {
	readAt, problemAt *time.Time
	digest            []byte
}

// lockSourceAudiences locks a live connector of the workspace FOR UPDATE,
// which orders every write of its audiences, and a DeleteConnector, one
// after another, then reads what its last read stored. ErrNotFound means the
// workspace has no such live connector.
func (s *mysqlDB) lockSourceAudiences(ctx context.Context, workspaceID, connectorID string) (storedAudienceRead, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM connectors WHERE id=? AND workspace_id=? AND deleted_at IS NULL FOR UPDATE`,
		connectorID, workspaceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return storedAudienceRead{}, ErrNotFound
	}
	if err != nil {
		return storedAudienceRead{}, fmt.Errorf("lock connector %s: %w", connectorID, err)
	}
	var out storedAudienceRead
	var read, problem sql.NullTime
	err = s.db.QueryRowContext(ctx, `SELECT read_at, problem_at, digest FROM foundry_source_audiences WHERE connector_id=?`, connectorID).
		Scan(&read, &problem, &out.digest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return storedAudienceRead{}, nil
	case err != nil:
		return storedAudienceRead{}, fmt.Errorf("read connector %s audiences: %w", connectorID, err)
	}
	if read.Valid {
		out.readAt = &read.Time
	}
	if problem.Valid {
		out.problemAt = &problem.Time
	}
	return out, nil
}

// audienceDigest is the SHA-256 of a in one canonical form: its people by
// email, then its items by key, each with whom it lists. Two reads that
// share the same items with the same people have the same digest.
func audienceDigest(a connector.Audiences) []byte {
	var b bytes.Buffer
	for _, e := range slices.SortedFunc(maps.Keys(a.People), func(x, y connector.Email) int { return cmp.Compare(x.String(), y.String()) }) {
		fmt.Fprintf(&b, "p %s %s\n", strconv.Quote(e.String()), strconv.FormatBool(a.People[e].Guest))
	}
	for _, key := range slices.Sorted(maps.Keys(a.Items)) {
		audience := a.Items[key]
		fmt.Fprintf(&b, "i %s %s", strconv.Quote(key), strconv.FormatBool(audience.Kind() == connector.AudienceEveryone))
		for _, e := range audience.Members() {
			fmt.Fprintf(&b, " %s", strconv.Quote(e.String()))
		}
		b.WriteString("\n")
	}
	sum := sha256.Sum256(b.Bytes())
	return sum[:]
}

// ReplaceSourceAudiences stores a, the audiences of a live connector of the
// workspace read at readAt, in place of every one stored before, in one
// transaction. A read whose digest matches the stored one's only moves
// read_at, so a sync that finds nothing changed writes one row. A problem
// found after readAt stays; one found before it goes, since this read
// worked. A read no newer than the one stored changes nothing, so a late
// writer never brings back older audiences. An item key is 1 to 255
// characters. ErrNotFound means the workspace has no such live connector.
func (s *mysqlDB) ReplaceSourceAudiences(ctx context.Context, workspaceID, connectorID string, readAt time.Time, a connector.Audiences) error {
	for key := range a.Items {
		if key == "" || utf8.RuneCountInString(key) > 255 {
			return fmt.Errorf("store connector %s audiences: an item key is empty or longer than 255 characters", connectorID)
		}
	}
	readAt = readAt.UTC().Truncate(time.Microsecond) // as DATETIME(6) keeps it
	digest := audienceDigest(a)
	return s.inTx(ctx, func(tx *mysqlDB) error {
		stored, err := tx.lockSourceAudiences(ctx, workspaceID, connectorID)
		if err != nil {
			return err
		}
		if stored.readAt != nil && !stored.readAt.Before(readAt) {
			return nil
		}
		keepProblem := stored.problemAt != nil && stored.problemAt.After(readAt)
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_source_audiences (connector_id, workspace_id, read_at, digest) VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE read_at=VALUES(read_at), digest=VALUES(digest), problem=IF(?, problem, NULL), problem_at=IF(?, problem_at, NULL)`,
			connectorID, workspaceID, readAt, digest, keepProblem, keepProblem); err != nil {
			return fmt.Errorf("store connector %s audiences: %w", connectorID, err)
		}
		if bytes.Equal(stored.digest, digest) {
			return nil
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

// SourceAudienceRead is when a live connector of the workspace last read
// its audiences and when a read since failed, each nil for none, so a sync
// reads them again only once they are due. ErrNotFound means the workspace
// has no such live connector.
func (s *mysqlDB) SourceAudienceRead(ctx context.Context, workspaceID, connectorID string) (model.FoundrySourceAudienceRead, error) {
	var read, problem sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT a.read_at, a.problem_at FROM connectors c
LEFT JOIN foundry_source_audiences a ON a.connector_id = c.id
WHERE c.id=? AND c.workspace_id=? AND c.deleted_at IS NULL`, connectorID, workspaceID).Scan(&read, &problem)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return model.FoundrySourceAudienceRead{}, ErrNotFound
	case err != nil:
		return model.FoundrySourceAudienceRead{}, fmt.Errorf("read connector %s audiences: %w", connectorID, err)
	}
	var out model.FoundrySourceAudienceRead
	if read.Valid {
		out.ReadAt = &read.Time
	}
	if problem.Valid {
		out.ProblemAt = &problem.Time
	}
	return out, nil
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
		stored, err := tx.lockSourceAudiences(ctx, workspaceID, connectorID)
		if err != nil {
			return err
		}
		if stored.readAt != nil && !stored.readAt.Before(at) || stored.problemAt != nil && stored.problemAt.After(at) {
			return nil
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_source_audiences (connector_id, workspace_id, problem, problem_at) VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE problem=VALUES(problem), problem_at=VALUES(problem_at)`, connectorID, workspaceID, problem, at); err != nil {
			return fmt.Errorf("record connector %s audience problem: %w", connectorID, err)
		}
		return nil
	})
}

// SourceSharingFor is what reader reads of each live connector of the
// workspace with audiences stored, by connector id. A person (reader not
// nil) reads the items that list them, and those shared with everyone and
// the source's directory (Member) while the source has an account for them
// by that email that isn't a guest's: a person it has no account for, such
// as an offboarded one, or one whose email is the zero Email, reads only
// items that list them, which are none. A nil reader is no one in any source
// (an agent's key): it reads the items shared with everyone, and no
// directory. The reads see one moment of the store.
func (s *mysqlDB) SourceSharingFor(ctx context.Context, workspaceID string, reader *connector.Email) ([]model.FoundrySourceSharing, error) {
	agent, email := reader == nil, ""
	if reader != nil {
		email = reader.String()
	}
	var out []model.FoundrySourceSharing
	err := s.inTx(ctx, func(tx *mysqlDB) error {
		rows, err := tx.db.QueryContext(ctx, `SELECT a.connector_id, a.read_at, a.problem_at
FROM foundry_source_audiences a
JOIN connectors c ON c.id = a.connector_id AND c.workspace_id = a.workspace_id AND c.deleted_at IS NULL
WHERE a.workspace_id = ? ORDER BY a.connector_id`, workspaceID)
		if err != nil {
			return fmt.Errorf("read source sharing: %w", err)
		}
		out, err = scanAll(rows, func(row rowScanner) (model.FoundrySourceSharing, error) {
			var sharing model.FoundrySourceSharing
			var readAt, problemAt sql.NullTime
			if err := row.Scan(&sharing.ConnectorID, &readAt, &problemAt); err != nil {
				return sharing, err
			}
			if readAt.Valid {
				sharing.ReadAt = &readAt.Time
			}
			if problemAt.Valid {
				sharing.ProblemAt = &problemAt.Time
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
LEFT JOIN foundry_source_people p ON p.connector_id = i.connector_id AND p.email = ? AND ? <> ''
WHERE a.workspace_id = ? AND (
  i.everyone AND (? OR p.email IS NOT NULL AND NOT p.guest) OR
  NOT i.everyone AND ? <> '' AND EXISTS (SELECT 1 FROM foundry_source_item_members m
    WHERE m.connector_id = i.connector_id AND m.item_key = i.item_key AND m.email = ?))
ORDER BY i.connector_id, i.item_key`, email, email, workspaceID, agent, email, email)
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
		if email == "" {
			return nil
		}
		rows, err = tx.db.QueryContext(ctx, `SELECT p.connector_id FROM foundry_source_people p
JOIN foundry_source_audiences a ON a.connector_id = p.connector_id
WHERE a.workspace_id = ? AND p.email = ? AND NOT p.guest ORDER BY p.connector_id`, workspaceID, email)
		if err != nil {
			return fmt.Errorf("read the sources a reader has an account in: %w", err)
		}
		members, err := scanAll(rows, func(row rowScanner) (connectorID string, err error) {
			err = row.Scan(&connectorID)
			return connectorID, err
		})
		if err != nil {
			return fmt.Errorf("read the sources a reader has an account in: %w", err)
		}
		for _, id := range members {
			if i, ok := at[id]; ok {
				out[i].Member = true
			}
		}
		return nil
	})
	return out, err
}

// SourcePeopleByEmail is, for each of emails the source of a live connector
// of the workspace has an account for, those accounts: each connector's, and
// whether it knows them as a guest. An email no source has is left out.
func (s *mysqlDB) SourcePeopleByEmail(ctx context.Context, workspaceID string, emails []connector.Email) (map[connector.Email][]model.FoundrySourcePerson, error) {
	out := map[connector.Email][]model.FoundrySourcePerson{}
	byAddress := map[string]connector.Email{}
	for _, e := range emails {
		if e != (connector.Email{}) {
			byAddress[e.String()] = e
		}
	}
	addresses := slices.Sorted(maps.Keys(byAddress))
	for chunk := range slices.Chunk(addresses, sourceAudienceChunk) {
		args := []any{workspaceID}
		for _, a := range chunk {
			args = append(args, a)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT p.email, p.connector_id, p.guest FROM foundry_source_people p
JOIN foundry_source_audiences a ON a.connector_id = p.connector_id
JOIN connectors c ON c.id = a.connector_id AND c.workspace_id = a.workspace_id AND c.deleted_at IS NULL
WHERE a.workspace_id = ? AND p.email IN (`+idPlaceholders(len(chunk))+`) ORDER BY p.email, p.connector_id`, args...)
		if err != nil {
			return nil, fmt.Errorf("read source people: %w", err)
		}
		type row struct {
			email string
			model.FoundrySourcePerson
		}
		found, err := scanAll(rows, func(r rowScanner) (p row, err error) {
			err = r.Scan(&p.email, &p.ConnectorID, &p.Guest)
			return p, err
		})
		if err != nil {
			return nil, fmt.Errorf("read source people: %w", err)
		}
		for _, p := range found {
			e := byAddress[p.email]
			out[e] = append(out[e], p.FoundrySourcePerson)
		}
	}
	return out, nil
}

// ListSourceAudiences is the audiences of each live connector of the
// workspace with any stored, by connector id: when they were read, the
// problem kept and its items' counts.
func (s *mysqlDB) ListSourceAudiences(ctx context.Context, workspaceID string) ([]model.FoundrySourceAudience, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.connector_id, a.read_at, a.problem, a.problem_at,
  (SELECT COUNT(*) FROM foundry_source_items i WHERE i.connector_id = a.connector_id AND i.everyone),
  (SELECT COUNT(*) FROM foundry_source_items i WHERE i.connector_id = a.connector_id AND NOT i.everyone)
FROM foundry_source_audiences a
JOIN connectors c ON c.id = a.connector_id AND c.workspace_id = a.workspace_id AND c.deleted_at IS NULL
WHERE a.workspace_id = ? ORDER BY a.connector_id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list source audiences: %w", err)
	}
	out, err := scanAll(rows, func(row rowScanner) (model.FoundrySourceAudience, error) {
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
		return nil, fmt.Errorf("list source audiences: %w", err)
	}
	return out, nil
}
