package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Dashboards and their public snapshots
// (migrations/20261201000000_foundry_dashboards.sql). The API decides who may
// do what; the store keeps each write to the revision it was made from, an
// edit to the editor's rights at the moment it is made, and the workspace's
// caps under the workspace's lock.

// foundryDashboardColumns are a dashboard's, with its share's (d and s);
// spec is the given expression, the spec or NULL for a list.
func foundryDashboardColumns(spec string) string {
	return `d.id, d.workspace_id, d.name, d.description, ` + spec + `, d.revision, d.created_by_user_id, d.updated_by_user_id, d.client_token,
       d.pinned_at, d.pinned_by_user_id, d.created_at, d.updated_at,
       s.token_sha256, s.token_sealed, s.token_key_id, s.spec_revision, s.ontology_ids, s.published_by_user_id, s.published_at, s.first_published_at,
       s.expires_at`
}

const foundryDashboardFrom = ` FROM foundry_dashboards d LEFT JOIN foundry_dashboard_shares s ON s.dashboard_id = d.id`

// CreateFoundryDashboard adds d, at revision 1 and unpinned, unless the
// workspace has max dashboards already (a FoundryDashboardLimit refusal), in
// one transaction holding the workspace's lock. ErrConflict: its creator has
// one with d's client token, which the caller answers with that one.
func (s *mysqlDB) CreateFoundryDashboard(ctx context.Context, d *model.FoundryDashboard, max int) error {
	switch {
	case d.ID == "" || d.WorkspaceID == "" || d.CreatedByUserID == "":
		return fmt.Errorf("foundry dashboard is missing an id")
	case len(d.Spec) == 0 || max < 1:
		return fmt.Errorf("foundry dashboard %s needs a spec and a positive cap", d.ID)
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, d.WorkspaceID); err != nil {
			return err
		}
		// The same create again answers the dashboard it made, even at the
		// cap that dashboard filled.
		var count int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_dashboards WHERE workspace_id=? AND created_by_user_id=? AND client_token=?`,
			d.WorkspaceID, d.CreatedByUserID, d.ClientToken).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return ErrConflict
		}
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_dashboards WHERE workspace_id=?`, d.WorkspaceID).Scan(&count); err != nil {
			return err
		}
		if count >= max {
			return model.FoundryDashboardRefusal{Reason: model.FoundryDashboardLimit,
				Detail: fmt.Sprintf("the workspace has %d dashboards, as many as it may: delete one first", count)}
		}
		_, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_dashboards
  (id, workspace_id, name, description, spec, created_by_user_id, updated_by_user_id, client_token)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, d.ID, d.WorkspaceID, d.Name, d.Description, string(d.Spec), d.CreatedByUserID, d.CreatedByUserID, d.ClientToken)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		return err
	})
}

func (s *mysqlDB) GetFoundryDashboard(ctx context.Context, workspaceID, id string) (*model.FoundryDashboard, error) {
	return scanFoundryDashboard(s.db.QueryRowContext(ctx, "SELECT "+foundryDashboardColumns("d.spec")+foundryDashboardFrom+`
WHERE d.id=? AND d.workspace_id=?`, id, workspaceID))
}

func (s *mysqlDB) GetFoundryDashboardByClientToken(ctx context.Context, workspaceID, userID, clientToken string) (*model.FoundryDashboard, error) {
	return scanFoundryDashboard(s.db.QueryRowContext(ctx, "SELECT "+foundryDashboardColumns("d.spec")+foundryDashboardFrom+`
WHERE d.workspace_id=? AND d.created_by_user_id=? AND d.client_token=?`, workspaceID, userID, clientToken))
}

// ListFoundryDashboards returns the workspace's dashboards without their
// specs, pinned first, then most recently changed: only the pinned ones with
// pinnedOnly. A workspace has at most its cap of them, so one list holds all.
func (s *mysqlDB) ListFoundryDashboards(ctx context.Context, workspaceID string, pinnedOnly bool) ([]*model.FoundryDashboard, error) {
	q := "SELECT " + foundryDashboardColumns("NULL") + foundryDashboardFrom + ` WHERE d.workspace_id=?`
	if pinnedOnly {
		q += ` AND d.pinned_at IS NOT NULL`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY d.pinned_at IS NULL, d.pinned_at, d.updated_at DESC, d.id DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundryDashboard)
}

// lockFoundryDashboard locks a dashboard of the workspace until the
// transaction ends, refusing it unless it is at revision expected (0: any).
func (s *mysqlDB) lockFoundryDashboard(ctx context.Context, workspaceID, id string, expected int64) (*model.FoundryDashboard, error) {
	d, err := scanFoundryDashboard(s.db.QueryRowContext(ctx, "SELECT "+foundryDashboardColumns("d.spec")+foundryDashboardFrom+`
WHERE d.id=? AND d.workspace_id=? FOR UPDATE`, id, workspaceID))
	if err != nil {
		return nil, err
	}
	if expected != 0 && d.Revision != expected {
		return nil, model.FoundryDashboardRefusal{Reason: model.FoundryDashboardChanged,
			Detail: fmt.Sprintf("the dashboard changed since revision %d: it is at %d now; read it again", expected, d.Revision)}
	}
	return d, nil
}

// editable refuses an editor who may not change d.
func editable(d *model.FoundryDashboard, editor model.FoundryDashboardEditor) error {
	switch {
	case editor.Manager:
		return nil
	case d.CreatedByUserID != editor.UserID:
		return model.FoundryDashboardRefusal{Reason: model.FoundryDashboardNotEditable,
			Detail: "only an owner or admin changes another person's dashboard"}
	case d.PinnedAt != nil:
		return model.FoundryDashboardRefusal{Reason: model.FoundryDashboardNotEditable,
			Detail: "the dashboard is pinned for the whole workspace: only an owner or admin changes it now"}
	}
	return nil
}

// UpdateFoundryDashboard makes u on the dashboard at revision expected, as
// editor, and moves it to the next revision. ErrNotFound when it is gone; a
// FoundryDashboardChanged refusal at another revision; FoundryDashboardNotEditable
// when the editor may not change it now.
func (s *mysqlDB) UpdateFoundryDashboard(ctx context.Context, workspaceID, id string, editor model.FoundryDashboardEditor, expected int64,
	u model.FoundryDashboardUpdate) error {
	if expected < 1 || (u.Name == nil && u.Description == nil && len(u.Spec) == 0) {
		return fmt.Errorf("foundry dashboard update needs a revision and a change")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		d, err := tx.lockFoundryDashboard(ctx, workspaceID, id, expected)
		if err != nil {
			return err
		}
		if err := editable(d, editor); err != nil {
			return err
		}
		name, description, spec := d.Name, d.Description, d.Spec
		if u.Name != nil {
			name = *u.Name
		}
		if u.Description != nil {
			description = *u.Description
		}
		if len(u.Spec) > 0 {
			spec = u.Spec
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_dashboards
SET name=?, description=?, spec=?, revision=revision+1, updated_by_user_id=?, updated_at=NOW(6) WHERE id=?`,
			name, description, string(spec), editor.UserID, id)
		return err
	})
}

// SetFoundryDashboardPinned pins the dashboard for the whole workspace, or
// unpins it, as userID, when it is at revision expected (a
// FoundryDashboardChanged refusal otherwise: what is pinned is what its
// pinner saw); its revision stays. Pinning one pinned already, or unpinning
// one that isn't, changes nothing. A workspace pins at most maxPinned (a
// FoundryDashboardLimit refusal), counted under its lock.
func (s *mysqlDB) SetFoundryDashboardPinned(ctx context.Context, workspaceID, id, userID string, expected int64, pinned bool, maxPinned int) error {
	if expected < 1 {
		return fmt.Errorf("pinning foundry dashboard %s needs the revision it was read at", id)
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, workspaceID); err != nil {
			return err
		}
		d, err := tx.lockFoundryDashboard(ctx, workspaceID, id, expected)
		if err != nil || (d.PinnedAt != nil) == pinned {
			return err
		}
		if !pinned {
			_, err := tx.db.ExecContext(ctx, `UPDATE foundry_dashboards SET pinned_at=NULL, pinned_by_user_id=NULL WHERE id=?`, id)
			return err
		}
		var count int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_dashboards WHERE workspace_id=? AND pinned_at IS NOT NULL`,
			workspaceID).Scan(&count); err != nil {
			return err
		}
		if count >= maxPinned {
			return model.FoundryDashboardRefusal{Reason: model.FoundryDashboardLimit,
				Detail: fmt.Sprintf("the workspace has %d pinned dashboards, as many as it may: unpin one first", count)}
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_dashboards SET pinned_at=NOW(6), pinned_by_user_id=? WHERE id=?`, userID, id)
		return err
	})
}

// DeleteFoundryDashboard deletes the dashboard and its public snapshot, as
// editor: ErrNotFound when it is gone, FoundryDashboardNotEditable when the
// editor may not change it, or it is published and they manage nothing
// (taking its link down is an owner's or admin's, as unpublishing is).
func (s *mysqlDB) DeleteFoundryDashboard(ctx context.Context, workspaceID, id string, editor model.FoundryDashboardEditor) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		d, err := tx.lockFoundryDashboard(ctx, workspaceID, id, 0)
		if err != nil {
			return err
		}
		if err := editable(d, editor); err != nil {
			return err
		}
		if d.Share != nil && !editor.Manager {
			return model.FoundryDashboardRefusal{Reason: model.FoundryDashboardNotEditable,
				Detail: "the dashboard is published at a public link: only an owner or admin deletes it"}
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_dashboard_shares WHERE dashboard_id=?`, id); err != nil {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `DELETE FROM foundry_dashboards WHERE id=?`, id)
		return err
	})
}

// PublishFoundryDashboard stores share as its dashboard's public snapshot,
// when the dashboard is still at share.SpecRevision (a FoundryDashboardChanged
// refusal otherwise), with its link ending as expiry says. A dashboard
// published already keeps its token, and the first time it was published,
// unless its link has expired: an expired link stays dead, so the publish
// makes a new one from share's token, as a first publish does. It returns
// the share as stored, without its snapshot.
//
// The snapshot was read before this transaction, so under the locks of the
// ontologies it read (taken first, in id order, as deleting an ontology and
// turning People enrichment off take them) it is refused,
// FoundryDashboardUnreadable, when one of them was deleted meanwhile or is
// queued to be purged of enrichment's profiles: either would leave data
// public that Foundry has promised to remove. Both of those delete the
// shares that read the ontology, so one committed after this one removes it.
func (s *mysqlDB) PublishFoundryDashboard(ctx context.Context, share *model.FoundryDashboardShare, expiry model.FoundryDashboardLinkExpiry) (
	*model.FoundryDashboardShare, error) {
	switch {
	case expiry.Keep && expiry.At != nil:
		return nil, fmt.Errorf("foundry dashboard share %s both keeps its expiry and sets one", share.DashboardID)
	case share.DashboardID == "" || share.WorkspaceID == "" || share.PublishedByUserID == "" || len(share.TokenSHA256) != 64:
		return nil, fmt.Errorf("foundry dashboard share is missing its dashboard, publisher or token")
	case len(share.Snapshot) == 0 || len(share.TokenSealed) == 0 || share.TokenKeyID == "" || share.SpecRevision < 1:
		return nil, fmt.Errorf("foundry dashboard share %s is missing its snapshot, sealed token or revision", share.DashboardID)
	}
	ontologyIDs := slices.Compact(slices.Sorted(slices.Values(share.OntologyIDs)))
	ontologies, err := json.Marshal(ontologyIDs)
	if err != nil {
		return nil, err
	}
	var stored *model.FoundryDashboardShare
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		for _, ontologyID := range ontologyIDs {
			o, err := tx.lockFoundryOntology(ctx, share.WorkspaceID, ontologyID)
			if errors.Is(err, ErrNotFound) || err == nil && o.DeletedAt != nil {
				return model.FoundryDashboardRefusal{Reason: model.FoundryDashboardUnreadable,
					Detail: "an ontology its tiles read was deleted while they were read"}
			}
			if err != nil {
				return err
			}
			var purging int
			if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_enrichment_purges WHERE ontology_id=? AND done_at IS NULL`,
				ontologyID).Scan(&purging); err != nil {
				return err
			}
			if purging > 0 {
				return model.FoundryDashboardRefusal{Reason: model.FoundryDashboardUnreadable,
					Detail: fmt.Sprintf("People enrichment was turned off, and %s is being loaded again without its profiles: publish once that load is current", o.Name)}
			}
		}
		if _, err := tx.lockFoundryDashboard(ctx, share.WorkspaceID, share.DashboardID, share.SpecRevision); err != nil {
			return err
		}
		// An expired link is never revived: its row goes, and the insert
		// below makes a new link.
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_dashboard_shares WHERE dashboard_id=? AND expires_at IS NOT NULL AND expires_at <= NOW(6)`,
			share.DashboardID); err != nil {
			return err
		}
		keep := ""
		if !expiry.Keep {
			keep = ", expires_at=VALUES(expires_at)"
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_dashboard_shares
  (dashboard_id, workspace_id, token_sha256, token_sealed, token_key_id, snapshot, spec_revision, ontology_ids, published_by_user_id,
   published_at, first_published_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(6), NOW(6), ?)
ON DUPLICATE KEY UPDATE snapshot=VALUES(snapshot), spec_revision=VALUES(spec_revision), ontology_ids=VALUES(ontology_ids),
  published_by_user_id=VALUES(published_by_user_id), published_at=VALUES(published_at)`+keep,
			share.DashboardID, share.WorkspaceID, share.TokenSHA256, share.TokenSealed, share.TokenKeyID, string(share.Snapshot), share.SpecRevision,
			string(ontologies), share.PublishedByUserID, expiry.At); err != nil {
			return err
		}
		d, err := tx.lockFoundryDashboard(ctx, share.WorkspaceID, share.DashboardID, 0)
		if err != nil {
			return err
		}
		stored = d.Share
		return nil
	})
	return stored, err
}

// UnpublishFoundryDashboard deletes the dashboard's public snapshot, and so
// its link: ErrNotFound when it has none.
func (s *mysqlDB) UnpublishFoundryDashboard(ctx context.Context, workspaceID, id string) error {
	return execOrNotFound(ctx, s.db, `DELETE FROM foundry_dashboard_shares WHERE dashboard_id=? AND workspace_id=?`, id, workspaceID)
}

// GetSharedFoundryDashboard returns the public snapshot whose token has
// tokenSHA256, with its snapshot: ErrNotFound when there is none, or its
// workspace is deleted or being deleted. Whether it may be shown (its
// expiry, its workspace's settings) is the caller's to decide.
func (s *mysqlDB) GetSharedFoundryDashboard(ctx context.Context, tokenSHA256 string) (*model.FoundryDashboardShare, error) {
	var share model.FoundryDashboardShare
	var snapshot, ontologies string
	var expiresAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT s.dashboard_id, s.workspace_id, s.token_sha256, s.token_sealed, s.token_key_id, s.snapshot,
       s.spec_revision, s.ontology_ids, s.published_by_user_id, s.published_at, s.first_published_at, s.expires_at
FROM foundry_dashboard_shares s JOIN `+s.workspaces.Table()+` w ON w.id = s.workspace_id
WHERE s.token_sha256=? AND w.deleted_at IS NULL AND w.status NOT IN ('deleting', 'deleted')`, tokenSHA256).Scan(
		&share.DashboardID, &share.WorkspaceID, &share.TokenSHA256, &share.TokenSealed, &share.TokenKeyID, &snapshot, &share.SpecRevision,
		&ontologies, &share.PublishedByUserID, &share.PublishedAt, &share.FirstPublishedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	share.Snapshot = json.RawMessage(snapshot)
	if expiresAt.Valid {
		share.ExpiresAt = &expiresAt.Time
	}
	if err := json.Unmarshal([]byte(ontologies), &share.OntologyIDs); err != nil {
		return nil, fmt.Errorf("read share %s's ontologies: %w", share.DashboardID, err)
	}
	return &share, nil
}

// deleteFoundryEnrichedShares deletes the workspace's public snapshots that
// read an ontology any of whose loads, live or deleted since, applied People
// enrichment's profiles: turning enrichment off removes them from what
// anyone sees, a snapshot read of such a load included.
func (s *mysqlDB) deleteFoundryEnrichedShares(ctx context.Context, workspaceID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM foundry_dashboard_shares
WHERE workspace_id=? AND EXISTS (
  SELECT 1 FROM foundry_load_enrichments e JOIN foundry_ontologies o ON o.id = e.ontology_id
  WHERE o.workspace_id = foundry_dashboard_shares.workspace_id AND e.applied_key <> ''
    AND JSON_CONTAINS(foundry_dashboard_shares.ontology_ids, JSON_QUOTE(o.id)))`, workspaceID)
	return err
}

// deleteFoundryOntologyShares deletes the public snapshots that read the
// ontology: they go with it, as everything read of it does.
func (s *mysqlDB) deleteFoundryOntologyShares(ctx context.Context, workspaceID, ontologyID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM foundry_dashboard_shares WHERE workspace_id=? AND JSON_CONTAINS(ontology_ids, JSON_QUOTE(?))`,
		workspaceID, ontologyID)
	return err
}

func scanFoundryDashboard(scanner rowScanner) (*model.FoundryDashboard, error) {
	var d model.FoundryDashboard
	var spec, clientToken, pinnedBy sql.NullString
	var pinnedAt sql.NullTime
	var tokenSHA256, tokenKeyID, ontologies, publishedBy sql.NullString
	var tokenSealed []byte
	var specRevision sql.NullInt64
	var publishedAt, firstPublishedAt, expiresAt sql.NullTime
	if err := scanner.Scan(&d.ID, &d.WorkspaceID, &d.Name, &d.Description, &spec, &d.Revision, &d.CreatedByUserID, &d.UpdatedByUserID,
		&clientToken, &pinnedAt, &pinnedBy, &d.CreatedAt, &d.UpdatedAt,
		&tokenSHA256, &tokenSealed, &tokenKeyID, &specRevision, &ontologies, &publishedBy, &publishedAt, &firstPublishedAt,
		&expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if spec.Valid {
		d.Spec = json.RawMessage(spec.String)
	}
	if clientToken.Valid {
		d.ClientToken = &clientToken.String
	}
	if pinnedAt.Valid && pinnedBy.Valid {
		d.PinnedAt, d.PinnedByUserID = &pinnedAt.Time, &pinnedBy.String
	}
	if tokenSHA256.Valid {
		d.Share = &model.FoundryDashboardShare{
			DashboardID: d.ID, WorkspaceID: d.WorkspaceID, TokenSHA256: tokenSHA256.String, TokenSealed: tokenSealed, TokenKeyID: tokenKeyID.String,
			SpecRevision: specRevision.Int64, PublishedByUserID: publishedBy.String, PublishedAt: publishedAt.Time, FirstPublishedAt: firstPublishedAt.Time,
		}
		if expiresAt.Valid {
			d.Share.ExpiresAt = &expiresAt.Time
		}
		if err := json.Unmarshal([]byte(ontologies.String), &d.Share.OntologyIDs); err != nil {
			return nil, fmt.Errorf("read dashboard %s's share: %w", d.ID, err)
		}
	}
	return &d, nil
}

// GetFoundryDashboardSettings returns the workspace's dashboard settings: with
// no row, public links on, as an owner never changed them.
func (s *mysqlDB) GetFoundryDashboardSettings(ctx context.Context, workspaceID string) (model.FoundryDashboardSettings, error) {
	out := model.FoundryDashboardSettings{WorkspaceID: workspaceID, PublicLinks: true}
	var updatedAt time.Time
	err := s.db.QueryRowContext(ctx, `SELECT public_links, updated_by_user_id, updated_at FROM foundry_dashboard_settings WHERE workspace_id=?`,
		workspaceID).Scan(&out.PublicLinks, &out.UpdatedByUserID, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return out, nil
	case err != nil:
		return model.FoundryDashboardSettings{}, err
	}
	out.UpdatedAt = &updatedAt
	return out, nil
}

// SetFoundryDashboardSettings records an owner's change of the workspace's
// public links: on or off, by userID, now.
func (s *mysqlDB) SetFoundryDashboardSettings(ctx context.Context, workspaceID, userID string, publicLinks bool) error {
	if workspaceID == "" || userID == "" {
		return fmt.Errorf("foundry dashboard settings need a workspace and the owner who changes them")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_dashboard_settings (workspace_id, public_links, updated_by_user_id, updated_at)
VALUES (?, ?, ?, NOW(6))
ON DUPLICATE KEY UPDATE public_links=VALUES(public_links), updated_by_user_id=VALUES(updated_by_user_id), updated_at=VALUES(updated_at)`,
		workspaceID, publicLinks, userID)
	return err
}
