package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/agentquestion"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The Assistant's threads, turns, transcript and actions
// (migrations/20261130110000_foundry_agent.sql). A turn is claimed, leased,
// renewed and settled as a question is (foundry_questions.go); its events and
// transcript are written only while its worker holds its lease.

const foundryAgentThreadColumns = `id, workspace_id, user_id, kind, title, runtime, runtime_session_id, created_at, updated_at`

const foundryAgentTurnColumns = `id, workspace_id, thread_id, user_id, runtime, kind, input, automation_run_id, automation_node_id, page_context,
       asked, answers, status, error, client_token,
       lease_owner, lease_until, attempts, next_attempt_at, max_usd_micros, reserved_micros, usd_micros, lost_micros, input_tokens,
       output_tokens, steps, last_seq, created_at, started_at, finished_at`

const foundryAgentActionColumns = `id, turn_id, thread_id, workspace_id, user_id, operation, target, payload, payload_hash, display,
       target_named_by_user, confirmation_id, confirmation_token_sealed, confirmation_key_id, status, result, error, expires_at,
       decided_by, decided_at, created_at`

const ownedFoundryAgentTurn = ` WHERE id=? AND status='running' AND lease_owner=?`

// dueFoundryAgentTurn is a turn a claim may take: queued and due, or running
// under a lease that ran out.
const dueFoundryAgentTurn = `((status='queued' AND next_attempt_at <= NOW(6)) OR (status='running' AND lease_until < NOW(6)))`

// claimFoundryAgentTurnTries bounds the turns one claim looks at.
const claimFoundryAgentTurnTries = 20

// CreateFoundryAgentTurn queues t, a person's message or their answers, under
// the workspace's limits, in one transaction holding the workspace's lock,
// then the thread's, with first, when not nil, as its first event:
//   - thread is the new thread t starts, or nil for t.ThreadID, which must be
//     the user's own live thread (ErrNotFound otherwise);
//   - answers (t.Answers, checked by the caller against the questions) answer
//     the thread's newest turn, t.Input, which ended done having asked its
//     person (FoundryAgentQuestionsClosed otherwise: answered, or the thread
//     went on);
//   - the user has no turn with t's client token (ErrConflict: a double
//     click, which the caller answers with that turn);
//   - the thread has no live turn and no action running (FoundryAgentThreadBusy);
//     a click that ran past model.FoundryAgentExecutingFor is ended unknown;
//   - the user sent fewer than limits.PerHour messages and answers in the
//     last hour;
//   - what the month's turns settled, plus t's cap, is within limits.CapMicros.
//
// t takes its thread's runtime. t.MaxUSDMicros is an 'api' turn's cap: a
// helper's turn runs on its owner's subscription, and is stored with a cap
// of 0, spending nothing of the month's budget.
func (s *mysqlDB) CreateFoundryAgentTurn(ctx context.Context, t *model.FoundryAgentTurn, thread *model.FoundryAgentThread, limits model.FoundryAgentLimits,
	first json.RawMessage) error {
	answering := t.Kind == model.FoundryAgentTurnAnswers
	switch {
	case t.ID == "" || t.WorkspaceID == "" || t.ThreadID == "" || t.UserID == "":
		return fmt.Errorf("foundry agent turn is missing an id")
	case t.Kind != model.FoundryAgentTurnMessage && !answering || strings.TrimSpace(t.Input) == "" || t.MaxUSDMicros <= 0:
		return fmt.Errorf("foundry agent turn %s is no message or answers with a cap", t.ID)
	case answering != (t.Answers != nil) || answering && thread != nil || t.Asked != nil:
		return fmt.Errorf("foundry agent turn %s: answers come exactly with an answers turn, of a thread that asked", t.ID)
	case thread != nil && (thread.ID != t.ThreadID || thread.WorkspaceID != t.WorkspaceID || thread.UserID != t.UserID):
		return fmt.Errorf("foundry agent turn %s isn't of its new thread", t.ID)
	case limits.PerHour < 1 || limits.CapMicros <= 0:
		return fmt.Errorf("foundry agent limits must be positive")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, t.WorkspaceID); err != nil {
			return err
		}
		runtime := model.FoundryAgentRuntimeAPI
		if thread != nil {
			if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_agent_threads (id, workspace_id, user_id, title, runtime)
VALUES (?, ?, ?, ?, ?)`, thread.ID, thread.WorkspaceID, thread.UserID, thread.Title, string(thread.Runtime)); err != nil {
				return err
			}
			runtime = thread.Runtime
		} else {
			if err := tx.lockFoundryAgentThread(ctx, t.WorkspaceID, t.UserID, t.ThreadID); err != nil {
				return err
			}
			if err := tx.db.QueryRowContext(ctx, `SELECT runtime FROM foundry_agent_threads WHERE id=?`, t.ThreadID).Scan(&runtime); err != nil {
				return err
			}
		}
		maxMicros := t.MaxUSDMicros
		if runtime != model.FoundryAgentRuntimeAPI {
			maxMicros = 0
		}
		if err := tx.busyFoundryAgentThread(ctx, t.ThreadID); err != nil {
			if sent, sentErr := tx.sentFoundryAgentTurn(ctx, t); sentErr != nil || sent {
				// A second click that waited on the first's thread lock finds its turn.
				return cmp.Or(sentErr, ErrConflict)
			}
			return err
		}
		if answering {
			if err := tx.openFoundryAgentQuestions(ctx, t.ThreadID, t.Input); err != nil {
				if sent, sentErr := tx.sentFoundryAgentTurn(ctx, t); sentErr != nil || sent {
					return cmp.Or(sentErr, ErrConflict)
				}
				return err
			}
		}
		var lastHour int
		var settled int64
		if err := tx.db.QueryRowContext(ctx, `SELECT
  (SELECT COUNT(*) FROM foundry_agent_turns WHERE workspace_id=? AND user_id=? AND kind IN ('message', 'answers') AND created_at >= NOW(6) - INTERVAL 1 HOUR),
  (SELECT COALESCE(SUM(usd_micros), 0) FROM foundry_agent_turns WHERE workspace_id=? AND charged_at >= ? AND automation_run_id IS NULL)`,
			t.WorkspaceID, t.UserID, t.WorkspaceID, limits.Since.UTC()).Scan(&lastHour, &settled); err != nil {
			return err
		}
		switch {
		case lastHour >= limits.PerHour:
			return model.FoundryAgentRefusal{Reason: model.FoundryAgentPerHour,
				Detail: fmt.Sprintf("you have sent %d messages in the last hour; try again later", lastHour)}
		case maxMicros > 0 && settled+maxMicros > limits.CapMicros:
			return model.FoundryAgentRefusal{Reason: model.FoundryAgentBudget,
				Detail: fmt.Sprintf("the workspace has spent this month's $%.2f of the Assistant", float64(limits.CapMicros)/1e6)}
		}
		var answers json.RawMessage
		if answering {
			raw, err := json.Marshal(t.Answers)
			if err != nil {
				return fmt.Errorf("encode foundry agent turn %s's answers: %w", t.ID, err)
			}
			answers = raw
		}
		_, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_agent_turns
  (id, workspace_id, thread_id, user_id, runtime, kind, input, page_context, answers, status, client_token, max_usd_micros)
SELECT ?, ?, ?, ?, runtime, ?, ?, ?, ?, 'queued', ?, ? FROM foundry_agent_threads WHERE id=?`,
			t.ID, t.WorkspaceID, t.ThreadID, t.UserID, string(t.Kind), t.Input, nullJSON(t.PageContext), nullJSON(answers), t.ClientToken, maxMicros, t.ThreadID)
		if isDuplicateKey(err) {
			if live, liveErr := tx.liveFoundryAgentTurn(ctx, t.ThreadID); liveErr == nil && live {
				return model.FoundryAgentRefusal{Reason: model.FoundryAgentThreadBusy, Detail: "the Assistant is still answering in this thread"}
			}
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if first != nil {
			if err := tx.appendFoundryAgentEvents(ctx, t.ID, []json.RawMessage{first}); err != nil {
				return err
			}
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_agent_threads SET updated_at=NOW(6) WHERE id=?`, t.ThreadID)
		return err
	})
}

// openFoundryAgentQuestions refuses answers to askedID unless it is the
// locked thread's newest turn, done, and asked its person: the questions it
// asked close once anything follows it (its answers among them).
func (s *mysqlDB) openFoundryAgentQuestions(ctx context.Context, threadID, askedID string) error {
	var newest, status string
	var asked bool
	err := s.db.QueryRowContext(ctx, `SELECT id, status, asked IS NOT NULL FROM foundry_agent_turns
WHERE thread_id=? ORDER BY created_at DESC, id DESC LIMIT 1`, threadID).Scan(&newest, &status, &asked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if newest != askedID || status != string(model.FoundryAgentTurnDone) || !asked {
		return model.FoundryAgentRefusal{Reason: model.FoundryAgentQuestionsClosed,
			Detail: "these questions are no longer open: they were answered, or the conversation went on since"}
	}
	return nil
}

// AskFoundryAgentQuestions records what turnID asked its person, once, with
// event, while workerID holds its lease (ErrNotFound otherwise). Asking the
// same again records nothing more (a replayed attempt); asking anything else
// once it asked is ErrConflict. An automation's turn asks no one.
func (s *mysqlDB) AskFoundryAgentQuestions(ctx context.Context, turnID, workerID string, asked *model.FoundryAgentAsked, event json.RawMessage) error {
	if err := asked.Validate(); err != nil {
		return fmt.Errorf("foundry agent turn %s: %w", turnID, err)
	}
	encoded, err := json.Marshal(asked)
	if err != nil {
		return fmt.Errorf("encode foundry agent turn %s's questions: %w", turnID, err)
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.ownFoundryAgentTurn(ctx, turnID, workerID); err != nil {
			return err
		}
		var kind string
		var before sql.NullString
		if err := tx.db.QueryRowContext(ctx, `SELECT kind, asked FROM foundry_agent_turns WHERE id=?`, turnID).Scan(&kind, &before); err != nil {
			return err
		}
		switch {
		case kind == string(model.FoundryAgentTurnAutomation):
			return fmt.Errorf("foundry agent turn %s is an automation's, which asks no one", turnID)
		case before.Valid:
			var was model.FoundryAgentAsked
			if err := json.Unmarshal([]byte(before.String), &was); err != nil {
				return err
			}
			if again, _ := json.Marshal(&was); string(again) == string(encoded) {
				return nil
			}
			return ErrConflict
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_agent_turns SET asked=CAST(? AS JSON) WHERE id=?`, string(encoded), turnID); err != nil {
			return err
		}
		return tx.appendFoundryAgentEvents(ctx, turnID, []json.RawMessage{event})
	})
}

// lockFoundryAgentThread locks the user's live thread: ErrNotFound when it
// is another's, deleted or absent.
func (s *mysqlDB) lockFoundryAgentThread(ctx context.Context, workspaceID, userID, threadID string) error {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM foundry_agent_threads
WHERE id=? AND workspace_id=? AND user_id=? AND deleted_at IS NULL FOR UPDATE`, threadID, workspaceID, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// sentFoundryAgentTurn reports whether the user has a turn with t's client token.
func (s *mysqlDB) sentFoundryAgentTurn(ctx context.Context, t *model.FoundryAgentTurn) (bool, error) {
	if t.ClientToken == nil {
		return false, nil
	}
	var sent int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_agent_turns WHERE workspace_id=? AND user_id=? AND client_token=?`,
		t.WorkspaceID, t.UserID, *t.ClientToken).Scan(&sent)
	return sent > 0, err
}

// busyFoundryAgentThread refuses a locked thread with a live turn or a click
// running, ending first the clicks that ran too long to still be running.
//
// Its reads are plain, so the transaction must read nothing plainly before
// it holds the thread's lock: a plain read fixes the transaction's snapshot,
// and one fixed before the lock misses the send or click that held it. Every
// write that makes a thread busy holds that lock.
func (s *mysqlDB) busyFoundryAgentThread(ctx context.Context, threadID string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_agent_actions
SET status='unknown', error='the action stopped without saying how it ended; check its result before running it again'
WHERE thread_id=? AND status='executing' AND decided_at < NOW(6) - INTERVAL ? MICROSECOND`,
		threadID, model.FoundryAgentExecutingFor.Microseconds()); err != nil {
		return err
	}
	var running int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_agent_actions WHERE thread_id=? AND status='executing'`,
		threadID).Scan(&running); err != nil {
		return err
	}
	if running > 0 {
		return model.FoundryAgentRefusal{Reason: model.FoundryAgentThreadBusy, Detail: "an action of this thread is running"}
	}
	live, err := s.liveFoundryAgentTurn(ctx, threadID)
	if err != nil {
		return err
	}
	if live {
		return model.FoundryAgentRefusal{Reason: model.FoundryAgentThreadBusy, Detail: "the Assistant is still answering in this thread"}
	}
	return nil
}

func (s *mysqlDB) liveFoundryAgentTurn(ctx context.Context, threadID string) (bool, error) {
	var live int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_agent_turns WHERE live_thread=?`, threadID).Scan(&live)
	return live > 0, err
}

func (s *mysqlDB) GetFoundryAgentThread(ctx context.Context, workspaceID, id string) (*model.FoundryAgentThread, error) {
	return scanFoundryAgentThread(s.db.QueryRowContext(ctx, "SELECT "+foundryAgentThreadColumns+`
FROM foundry_agent_threads WHERE id=? AND workspace_id=? AND deleted_at IS NULL`, id, workspaceID))
}

// ListFoundryAgentThreads returns the user's live chats of the workspace,
// most recently active first, a page of limit at a time: never the threads
// their automations' runs run in.
func (s *mysqlDB) ListFoundryAgentThreads(ctx context.Context, workspaceID, userID, pageToken string, limit int) ([]*model.FoundryAgentThread, string, error) {
	conds := []string{"workspace_id=?", "user_id=?", "kind='chat'", "deleted_at IS NULL"}
	args := []any{workspaceID, userID}
	if pageToken != "" {
		cursor, err := decodeTimeIDCursor(pageToken)
		if err != nil {
			return nil, "", err
		}
		conds = append(conds, "(updated_at < ? OR (updated_at = ? AND id < ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryAgentThreadColumns+" FROM foundry_agent_threads"+whereClause(conds)+
		fmt.Sprintf(" ORDER BY updated_at DESC, id DESC LIMIT %d", limit+1), args...)
	if err != nil {
		return nil, "", err
	}
	out, err := scanAll(rows, scanFoundryAgentThread)
	if err != nil || len(out) <= limit {
		return out, "", err
	}
	next, err := encodeTimeIDCursor(out[limit-1].UpdatedAt, out[limit-1].ID)
	return out[:limit], next, err
}

// DeleteFoundryAgentThread deletes the user's thread and cancels its live
// turn, charging what that turn reserved; ErrNotFound when it isn't theirs.
func (s *mysqlDB) DeleteFoundryAgentThread(ctx context.Context, workspaceID, userID, id string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.lockFoundryAgentThread(ctx, workspaceID, userID, id); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_agent_turns
SET status='cancelled', lease_owner=NULL, lease_until=NULL, usd_micros=usd_micros+reserved_micros, reserved_micros=0, finished_at=NOW(6)
WHERE thread_id=? AND status IN ('queued', 'running')`, id); err != nil {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_agent_threads SET deleted_at=NOW(6) WHERE id=?`, id)
		return err
	})
}

func (s *mysqlDB) GetFoundryAgentTurn(ctx context.Context, workspaceID, id string) (*model.FoundryAgentTurn, error) {
	return scanFoundryAgentTurn(s.db.QueryRowContext(ctx, "SELECT "+foundryAgentTurnColumns+`
FROM foundry_agent_turns WHERE id=? AND workspace_id=?`, id, workspaceID))
}

func (s *mysqlDB) GetFoundryAgentTurnByClientToken(ctx context.Context, workspaceID, userID, clientToken string) (*model.FoundryAgentTurn, error) {
	return scanFoundryAgentTurn(s.db.QueryRowContext(ctx, "SELECT "+foundryAgentTurnColumns+`
FROM foundry_agent_turns WHERE workspace_id=? AND user_id=? AND client_token=?`, workspaceID, userID, clientToken))
}

// ListFoundryAgentTurns returns the thread's newest limit turns, oldest first.
func (s *mysqlDB) ListFoundryAgentTurns(ctx context.Context, threadID string, limit int) ([]*model.FoundryAgentTurn, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryAgentTurnColumns+`
FROM foundry_agent_turns WHERE thread_id=? ORDER BY created_at DESC, id DESC LIMIT ?`, threadID, limit)
	if err != nil {
		return nil, err
	}
	out, err := scanAll(rows, scanFoundryAgentTurn)
	slices.Reverse(out)
	return out, err
}

// CancelFoundryAgentTurn ends the user's queued or running turn as cancelled,
// charging what it reserved; its attempt loses its lease and records nothing.
// Cancelling a cancelled turn returns it; ErrConflict means it ended otherwise.
func (s *mysqlDB) CancelFoundryAgentTurn(ctx context.Context, workspaceID, userID, id string) (*model.FoundryAgentTurn, error) {
	var out *model.FoundryAgentTurn
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		t, err := scanFoundryAgentTurn(tx.db.QueryRowContext(ctx, "SELECT "+foundryAgentTurnColumns+`
FROM foundry_agent_turns WHERE id=? AND workspace_id=? AND user_id=? FOR UPDATE`, id, workspaceID, userID))
		switch {
		case err != nil:
			return err
		case t.Status == model.FoundryAgentTurnCancelled:
			out = t
			return nil
		case !t.Status.Live():
			return ErrConflict
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_agent_turns
SET status='cancelled', lease_owner=NULL, lease_until=NULL, usd_micros=usd_micros+reserved_micros, reserved_micros=0, finished_at=NOW(6)
WHERE id=?`, id); err != nil {
			return err
		}
		out, err = scanFoundryAgentTurn(tx.db.QueryRowContext(ctx, "SELECT "+foundryAgentTurnColumns+` FROM foundry_agent_turns WHERE id=?`, id))
		return err
	})
	return out, err
}

// ClaimFoundryAgentTurn leases the oldest due turn of claim.Runtime to workerID, deciding it
// under its workspace's lock as ClaimFoundryQuestion decides a question: a
// month that can't fit it fails it; a workspace running claim.Limits.Running
// turns, or whose reservations leave too little, defers it; otherwise it
// runs, reserving its cap. A turn of a deleted thread is cancelled. Nil means
// none is due, or none of the first few could run. A reservation an attempt
// left unsettled (its lease ran out) is charged, and counted lost.
func (s *mysqlDB) ClaimFoundryAgentTurn(ctx context.Context, workerID string, claim model.FoundryAgentClaim) (*model.FoundryAgentTurn, error) {
	if workerID == "" || claim.Runtime == "" || claim.Lease <= 0 || claim.Defer <= 0 || claim.Limits.CapMicros <= 0 || claim.Limits.Running < 1 ||
		claim.Runtime == model.FoundryAgentRuntimeAPI && (claim.Automations.CapMicros <= 0 || claim.Automations.Running < 1) {
		return nil, fmt.Errorf("invalid foundry agent turn claim")
	}
	for range claimFoundryAgentTurnTries {
		var candidate, workspaceID string
		err := s.db.QueryRowContext(ctx, `SELECT id, workspace_id FROM foundry_agent_turns WHERE runtime=? AND `+dueFoundryAgentTurn+`
ORDER BY next_attempt_at, id LIMIT 1`, string(claim.Runtime)).Scan(&candidate, &workspaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("find a due foundry agent turn: %w", err)
		}
		var claimed *model.FoundryAgentTurn
		err = s.WithTx(ctx, func(txDB Store) error {
			tx := txDB.(*mysqlDB)
			claimed = nil
			gone := tx.workspaces.Lock(ctx, tx.db, workspaceID)
			if gone != nil && !errors.Is(gone, ErrNotFound) {
				return gone
			}
			t, err := scanFoundryAgentTurn(tx.db.QueryRowContext(ctx, "SELECT "+foundryAgentTurnColumns+`
FROM foundry_agent_turns WHERE id=? AND `+dueFoundryAgentTurn+` FOR UPDATE`, candidate))
			switch {
			case errors.Is(err, ErrNotFound):
				return nil
			case err != nil:
				return err
			case gone != nil:
				return tx.endFoundryAgentTurn(ctx, t.ID, model.FoundryAgentTurnFailed, "the workspace was deleted")
			}
			var deleted bool
			if err := tx.db.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM foundry_agent_threads WHERE id=?`,
				t.ThreadID).Scan(&deleted); err != nil {
				return err
			}
			if deleted {
				return tx.endFoundryAgentTurn(ctx, t.ID, model.FoundryAgentTurnCancelled, "the thread was deleted")
			}
			// A turn is reserved against its own pool: people's or automations'.
			l, pool := claim.Limits, "the Assistant"
			automation := t.AutomationRunID != nil
			if automation {
				l, pool = claim.Automations, "automations"
			}
			var running int
			var settled, reserved int64
			if err := tx.db.QueryRowContext(ctx, `SELECT
  COUNT(CASE WHEN status='running' AND lease_until >= NOW(6) THEN 1 END),
  COALESCE(SUM(CASE WHEN charged_at >= ? THEN usd_micros END), 0),
  COALESCE(SUM(CASE WHEN charged_at >= ? THEN reserved_micros END), 0)
FROM foundry_agent_turns WHERE workspace_id=? AND id<>? AND (status='running' OR charged_at >= ?) AND (automation_run_id IS NOT NULL) = ?`,
				l.Since.UTC(), l.Since.UTC(), t.WorkspaceID, t.ID, l.Since.UTC(), automation).Scan(&running, &settled, &reserved); err != nil {
				return err
			}
			own := t.Spend.USDMicros + t.ReservedMicros + t.MaxUSDMicros
			switch {
			case settled+own > l.CapMicros:
				return tx.endFoundryAgentTurn(ctx, t.ID, model.FoundryAgentTurnFailed,
					fmt.Sprintf("the workspace has spent this month's $%.2f of %s", float64(l.CapMicros)/1e6, pool))
			case running >= l.Running || settled+reserved+own > l.CapMicros:
				_, err := tx.db.ExecContext(ctx, `UPDATE foundry_agent_turns
SET status='queued', lease_owner=NULL, lease_until=NULL, usd_micros=usd_micros+reserved_micros,
    lost_micros=lost_micros+reserved_micros, reserved_micros=0, next_attempt_at=NOW(6) + INTERVAL ? MICROSECOND
WHERE id=?`, claim.Defer.Microseconds(), t.ID)
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_agent_turns
SET status='running', attempts=attempts+1, lease_owner=?, lease_until=?, usd_micros=usd_micros+reserved_micros,
    lost_micros=lost_micros+reserved_micros, reserved_micros=max_usd_micros, charged_at=NOW(6), started_at=COALESCE(started_at, NOW(6))
WHERE id=?`, workerID, time.Now().UTC().Add(claim.Lease), t.ID); err != nil {
				return err
			}
			claimed, err = scanFoundryAgentTurn(tx.db.QueryRowContext(ctx, "SELECT "+foundryAgentTurnColumns+` FROM foundry_agent_turns WHERE id=?`, t.ID))
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("claim foundry agent turn: %w", err)
		}
		if claimed != nil {
			return claimed, nil
		}
	}
	return nil, nil
}

// endFoundryAgentTurn ends a live turn for good, charging any reservation.
func (s *mysqlDB) endFoundryAgentTurn(ctx context.Context, id string, status model.FoundryAgentTurnStatus, why string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_agent_turns
SET status=?, error=?, lease_owner=NULL, lease_until=NULL, usd_micros=usd_micros+reserved_micros, reserved_micros=0, finished_at=NOW(6)
WHERE id=?`, string(status), why, id)
	return err
}

// RenewFoundryAgentTurnLease extends the lease; ErrNotFound means workerID
// lost it, to another worker, a cancel or the thread's deletion.
func (s *mysqlDB) RenewFoundryAgentTurnLease(ctx context.Context, id, workerID string, lease time.Duration) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_agent_turns SET lease_until=?`+ownedFoundryAgentTurn,
		time.Now().UTC().Add(lease), id, workerID)
}

// AppendFoundryAgentEvents stores events after the turn's newest while
// workerID holds its lease; ErrNotFound means it lost it, and nothing was
// stored.
func (s *mysqlDB) AppendFoundryAgentEvents(ctx context.Context, id, workerID string, events []json.RawMessage) error {
	if len(events) == 0 {
		return nil
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.ownFoundryAgentTurn(ctx, id, workerID); err != nil {
			return err
		}
		return tx.appendFoundryAgentEvents(ctx, id, events)
	})
}

// FinishFoundryAgentText stores events, the text a model call of the turn
// ended with, after the turn's newest, and deletes the call's text_delta
// events, which they supersede: one transaction, while workerID holds the
// lease, so a reader sees the deltas or the text, never neither. A poller
// past some of the deltas reads on from the text. ErrNotFound means the
// lease was lost, and nothing changed.
func (s *mysqlDB) FinishFoundryAgentText(ctx context.Context, id, workerID string, step int, events []json.RawMessage) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.ownFoundryAgentTurn(ctx, id, workerID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_agent_events
WHERE turn_id=? AND JSON_UNQUOTE(JSON_EXTRACT(event, '$.type'))='text_delta' AND JSON_EXTRACT(event, '$.step')=?`, id, step); err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		return tx.appendFoundryAgentEvents(ctx, id, events)
	})
}

// ownFoundryAgentTurn locks the turn while workerID holds its lease.
func (s *mysqlDB) ownFoundryAgentTurn(ctx context.Context, id, workerID string) error {
	var held string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM foundry_agent_turns`+ownedFoundryAgentTurn+` FOR UPDATE`, id, workerID).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// appendFoundryAgentEvents stores events after the locked turn's newest.
func (s *mysqlDB) appendFoundryAgentEvents(ctx context.Context, id string, events []json.RawMessage) error {
	var last int
	if err := s.db.QueryRowContext(ctx, `SELECT last_seq FROM foundry_agent_turns WHERE id=? FOR UPDATE`, id).Scan(&last); err != nil {
		return err
	}
	args := make([]any, 0, 3*len(events))
	for i, e := range events {
		args = append(args, id, last+1+i, string(e))
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_agent_events (turn_id, seq, event) VALUES `+
		strings.TrimSuffix(strings.Repeat("(?, ?, ?), ", len(events)), ", "), args...); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_agent_turns SET last_seq=? WHERE id=?`, last+len(events), id)
	return err
}

// ListFoundryAgentEvents returns up to limit of the turn's events after
// afterSeq, in order.
func (s *mysqlDB) ListFoundryAgentEvents(ctx context.Context, turnID string, afterSeq, limit int) ([]model.FoundryAgentEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, event, created_at FROM foundry_agent_events
WHERE turn_id=? AND seq > ? ORDER BY seq LIMIT ?`, turnID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FoundryAgentEvent
	for rows.Next() {
		var e model.FoundryAgentEvent
		var event string
		if err := rows.Scan(&e.Seq, &event, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Event = json.RawMessage(event)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AppendFoundryAgentMessages adds messages to the end of the turn's thread's
// transcript while workerID holds the turn's lease, numbering them; it keeps
// the thread's newest keep messages. ErrNotFound means the lease was lost.
func (s *mysqlDB) AppendFoundryAgentMessages(ctx context.Context, turnID, workerID string, messages []model.FoundryAgentMessage, keep int) error {
	if len(messages) == 0 {
		return nil
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.ownFoundryAgentTurn(ctx, turnID, workerID); err != nil {
			return err
		}
		var threadID string
		var last int
		if err := tx.db.QueryRowContext(ctx, `SELECT t.thread_id, COALESCE((SELECT MAX(seq) FROM foundry_agent_messages m WHERE m.thread_id = t.thread_id), 0)
FROM foundry_agent_turns t WHERE t.id=?`, turnID).Scan(&threadID, &last); err != nil {
			return err
		}
		args := make([]any, 0, 6*len(messages))
		for i, m := range messages {
			args = append(args, threadID, last+1+i, turnID, string(m.Role), string(m.Content), m.EstTokens)
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_agent_messages (thread_id, seq, turn_id, role, content, est_tokens) VALUES `+
			strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?, ?), ", len(messages)), ", "), args...); err != nil {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_agent_messages WHERE thread_id=? AND seq <= ?`, threadID, last+len(messages)-keep)
		return err
	})
}

// ListFoundryAgentMessages returns the thread's transcript, oldest first.
func (s *mysqlDB) ListFoundryAgentMessages(ctx context.Context, threadID string) ([]model.FoundryAgentMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, turn_id, role, content, est_tokens FROM foundry_agent_messages
WHERE thread_id=? ORDER BY seq`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FoundryAgentMessage
	for rows.Next() {
		var m model.FoundryAgentMessage
		var role, content string
		if err := rows.Scan(&m.Seq, &m.TurnID, &role, &content, &m.EstTokens); err != nil {
			return nil, err
		}
		m.Role, m.Content = model.FoundryAgentRole(role), json.RawMessage(content)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SettleFoundryAgentTurn ends workerID's attempt with o, releasing its
// reservation and adding what it spent; ErrNotFound means it lost the lease.
func (s *mysqlDB) SettleFoundryAgentTurn(ctx context.Context, id, workerID string, o model.FoundryAgentTurnOutcome) error {
	if err := o.Validate(); err != nil {
		return fmt.Errorf("settle foundry agent turn %s: %w", id, err)
	}
	if o.RetryAt != nil {
		given := 0
		if o.Uncounted {
			given = 1
		}
		return execOrNotFound(ctx, s.db, `UPDATE foundry_agent_turns
SET status='queued', lease_owner=NULL, lease_until=NULL, next_attempt_at=?, attempts=attempts-?, error=?, reserved_micros=0,
    usd_micros=usd_micros+?, input_tokens=input_tokens+?, output_tokens=output_tokens+?, steps=steps+?`+ownedFoundryAgentTurn,
			o.RetryAt.UTC(), given, nullString(o.Error), o.Spend.USDMicros, o.Spend.InputTokens, o.Spend.OutputTokens, o.Steps, id, workerID)
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_agent_turns
SET status=?, error=?, lease_owner=NULL, lease_until=NULL, reserved_micros=0, finished_at=NOW(6),
    usd_micros=usd_micros+?, input_tokens=input_tokens+?, output_tokens=output_tokens+?, steps=steps+?`+ownedFoundryAgentTurn,
		string(o.Status), nullString(o.Error), o.Spend.USDMicros, o.Spend.InputTokens, o.Spend.OutputTokens, o.Steps, id, workerID)
}

// SetFoundryAgentThreadSession keeps, as the turn's thread's, the session of
// its helper's CLI that the thread's next turn resumes, while workerID holds
// the turn's lease; empty forgets it. ErrNotFound means the lease was lost.
func (s *mysqlDB) SetFoundryAgentThreadSession(ctx context.Context, turnID, workerID, sessionID string) error {
	if len(sessionID) > 128 {
		return fmt.Errorf("foundry agent session id is longer than 128 bytes")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.ownFoundryAgentTurn(ctx, turnID, workerID); err != nil {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_agent_threads h JOIN foundry_agent_turns t ON t.thread_id = h.id
SET h.runtime_session_id=? WHERE t.id=?`, nullString(sessionID), turnID)
		return err
	})
}

// EndUnclaimedFoundryAgentTurns fails runtime's turns that waited for a
// claim since before, with why: queued and due since then, or running under
// a lease that ran out then. Only a helper's runtime: Foundry's worker
// claims its own. A reservation is charged as any end charges it (a
// helper's turns reserve nothing). It returns how many it ended.
func (s *mysqlDB) EndUnclaimedFoundryAgentTurns(ctx context.Context, runtime model.FoundryAgentRuntime, before time.Time, why string) (int64, error) {
	if !runtime.Subscription() || why == "" {
		return 0, fmt.Errorf("end unclaimed foundry agent turns: a helper's runtime, and why")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_agent_turns
SET status='failed', error=?, lease_owner=NULL, lease_until=NULL, usd_micros=usd_micros+reserved_micros, reserved_micros=0, finished_at=NOW(6)
WHERE runtime=? AND ((status='queued' AND next_attempt_at < ?) OR (status='running' AND lease_until < ?))`,
		why, string(runtime), before.UTC(), before.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RecordFoundryAgentAction stores a, prepared by the turn workerID holds, and
// the event card makes of it, together. The same payload prepared again in
// the turn (a replayed attempt) returns the action already stored, and no
// event. A thread waits on at most maxPending live cards
// (FoundryAgentTooManyCards). An automation's turn records an action its
// owner approved for the step as executing, decided by them (DecidedBy):
// it runs at once, waiting on no one, and FinishFoundryAgentAction ends it.
// ErrNotFound means the lease was lost.
func (s *mysqlDB) RecordFoundryAgentAction(ctx context.Context, workerID string, a *model.FoundryAgentAction, maxPending int,
	card func(*model.FoundryAgentAction) (json.RawMessage, error)) (*model.FoundryAgentAction, error) {
	approved := a.Status == model.FoundryAgentActionExecuting
	if a.ID == "" || a.TurnID == "" || !approved && a.Status != model.FoundryAgentActionPrepared || len(a.PayloadHash) != 64 || len(a.TokenSealed) == 0 ||
		approved && a.DecidedBy == nil {
		return nil, fmt.Errorf("foundry agent action is incomplete")
	}
	var out *model.FoundryAgentAction
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.ownFoundryAgentTurn(ctx, a.TurnID, workerID); err != nil {
			return err
		}
		existing, err := scanFoundryAgentAction(tx.db.QueryRowContext(ctx, "SELECT "+foundryAgentActionColumns+`
FROM foundry_agent_actions WHERE turn_id=? AND payload_hash=?`, a.TurnID, a.PayloadHash))
		switch {
		case err == nil:
			out = existing
			return nil
		case !errors.Is(err, ErrNotFound):
			return err
		}
		var pending int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_agent_actions
WHERE thread_id=? AND status='prepared' AND expires_at > NOW(6)`, a.ThreadID).Scan(&pending); err != nil {
			return err
		}
		if pending >= maxPending && !approved {
			return model.FoundryAgentRefusal{Reason: model.FoundryAgentTooManyCards,
				Detail: fmt.Sprintf("%d actions already wait for the person; let them run or dismiss those first", pending)}
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_agent_actions
  (id, turn_id, thread_id, workspace_id, user_id, operation, target, payload, payload_hash, display, target_named_by_user,
   confirmation_id, confirmation_token_sealed, confirmation_key_id, status, expires_at, decided_by, decided_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, IF(?, NOW(6), NULL))`,
			a.ID, a.TurnID, a.ThreadID, a.WorkspaceID, a.UserID, a.Operation, a.Target, string(a.Payload), a.PayloadHash, string(a.Display),
			a.TargetNamedByUser, a.ConfirmationID, a.TokenSealed, a.KeyID, string(a.Status), a.ExpiresAt.UTC(), a.DecidedBy, approved); err != nil {
			return err
		}
		if out, err = scanFoundryAgentAction(tx.db.QueryRowContext(ctx, "SELECT "+foundryAgentActionColumns+`
FROM foundry_agent_actions WHERE id=?`, a.ID)); err != nil {
			return err
		}
		event, err := card(out)
		if err != nil {
			return err
		}
		return tx.appendFoundryAgentEvents(ctx, a.TurnID, []json.RawMessage{event})
	})
	return out, err
}

func (s *mysqlDB) GetFoundryAgentAction(ctx context.Context, workspaceID, id string) (*model.FoundryAgentAction, error) {
	return scanFoundryAgentAction(s.db.QueryRowContext(ctx, "SELECT "+foundryAgentActionColumns+`
FROM foundry_agent_actions WHERE id=? AND workspace_id=?`, id, workspaceID))
}

// GetFoundryAgentActionByPayload returns the action a turn prepared for the
// payload payloadHash names: a turn prepares each payload once.
func (s *mysqlDB) GetFoundryAgentActionByPayload(ctx context.Context, turnID, payloadHash string) (*model.FoundryAgentAction, error) {
	return scanFoundryAgentAction(s.db.QueryRowContext(ctx, "SELECT "+foundryAgentActionColumns+`
FROM foundry_agent_actions WHERE turn_id=? AND payload_hash=?`, turnID, payloadHash))
}

// ListFoundryAgentActions returns the thread's actions, oldest first.
func (s *mysqlDB) ListFoundryAgentActions(ctx context.Context, threadID string) ([]*model.FoundryAgentAction, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryAgentActionColumns+`
FROM foundry_agent_actions WHERE thread_id=? ORDER BY created_at, id`, threadID)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundryAgentAction)
}

// foundryAgentActionThread is the thread of the user's action, read outside
// the transaction that then locks it: an action never changes thread, and a
// read inside would fix the transaction's snapshot before the thread's lock
// (busyFoundryAgentThread).
func (s *mysqlDB) foundryAgentActionThread(ctx context.Context, workspaceID, userID, id string) (string, error) {
	var threadID string
	err := s.db.QueryRowContext(ctx, `SELECT thread_id FROM foundry_agent_actions WHERE id=? AND workspace_id=? AND user_id=?`,
		id, workspaceID, userID).Scan(&threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return threadID, err
}

// lockFoundryAgentAction locks the user's live thread, then their action of
// it: ErrNotFound when either isn't theirs. It reads nothing before the
// thread's lock.
func (s *mysqlDB) lockFoundryAgentAction(ctx context.Context, workspaceID, userID, threadID, id string) (*model.FoundryAgentAction, error) {
	if err := s.lockFoundryAgentThread(ctx, workspaceID, userID, threadID); err != nil {
		return nil, err
	}
	return scanFoundryAgentAction(s.db.QueryRowContext(ctx, "SELECT "+foundryAgentActionColumns+`
FROM foundry_agent_actions WHERE id=? AND thread_id=? AND user_id=? FOR UPDATE`, id, threadID, userID))
}

// ClaimFoundryAgentAction starts the user's click on their action: from
// prepared to executing, once, for the payload they saw (payloadHash), while
// its confirmation lives and its thread is idle. Refusals are a
// model.FoundryAgentRefusal; an action that expired is marked so.
// ErrNotFound means the action isn't theirs.
func (s *mysqlDB) ClaimFoundryAgentAction(ctx context.Context, workspaceID, userID, id, payloadHash string) (*model.FoundryAgentAction, error) {
	threadID, err := s.foundryAgentActionThread(ctx, workspaceID, userID, id)
	if err != nil {
		return nil, err
	}
	var out *model.FoundryAgentAction
	expired := false
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		out, expired = nil, false
		a, err := tx.lockFoundryAgentAction(ctx, workspaceID, userID, threadID, id)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		switch {
		case a.Status != model.FoundryAgentActionPrepared:
			return model.FoundryAgentRefusal{Reason: model.FoundryAgentNotPrepared, Detail: "the action is " + string(a.Shown(now))}
		case a.PayloadHash != payloadHash:
			return model.FoundryAgentRefusal{Reason: model.FoundryAgentChanged, Detail: "the action shown is not the one prepared; reload it"}
		case !now.Before(a.ExpiresAt):
			// The expiry commits; the caller is refused after it.
			expired = true
			_, err := tx.db.ExecContext(ctx, `UPDATE foundry_agent_actions SET status='expired', decided_at=NOW(6) WHERE id=?`, id)
			return err
		}
		if err := tx.busyFoundryAgentThread(ctx, a.ThreadID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_agent_actions SET status='executing', decided_by=?, decided_at=NOW(6) WHERE id=?`,
			userID, id); err != nil {
			return err
		}
		out, err = scanFoundryAgentAction(tx.db.QueryRowContext(ctx, "SELECT "+foundryAgentActionColumns+` FROM foundry_agent_actions WHERE id=?`, id))
		return err
	})
	if err == nil && expired {
		return nil, model.FoundryAgentRefusal{Reason: model.FoundryAgentExpired, Detail: "the action's confirmation expired; refresh it"}
	}
	return out, err
}

// FinishFoundryAgentAction ends a click that is executing with end and, when
// continuation is set, queues the action-result turn the agent goes on from
// with its first event, in the same transaction. continuation.MaxUSDMicros
// is an 'api' turn's cap: on a helper's thread it is stored as 0. The thread is idle (the
// claim checked, and sends wait for the click), so the turn is live alone;
// should one be live after all, the click still ends, with no continuation.
// ErrNotFound means the action isn't executing.
func (s *mysqlDB) FinishFoundryAgentAction(ctx context.Context, id string, end model.FoundryAgentActionEnd, continuation *model.FoundryAgentTurn,
	firstEvent json.RawMessage) error {
	if err := end.Validate(); err != nil {
		return fmt.Errorf("finish foundry agent action %s: %w", id, err)
	}
	// The thread's lock first, as a send and a click take it, then the
	// action's: the other order deadlocks against them.
	var threadID string
	err := s.db.QueryRowContext(ctx, `SELECT thread_id FROM foundry_agent_actions WHERE id=?`, id).Scan(&threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if continuation != nil && (continuation.Kind != model.FoundryAgentTurnActionResult || continuation.Input != id ||
		continuation.ThreadID != threadID || continuation.MaxUSDMicros <= 0) {
		return fmt.Errorf("finish foundry agent action %s: the continuation is not its result's turn", id)
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.db.QueryRowContext(ctx, `SELECT id FROM foundry_agent_threads WHERE id=? FOR UPDATE`, threadID).Scan(&threadID); err != nil {
			return err
		}
		if err := execOrNotFound(ctx, tx.db, `UPDATE foundry_agent_actions SET status=?, result=?, error=? WHERE id=? AND status='executing'`,
			string(end.Status), nullJSON(end.Result), nullString(end.Error), id); err != nil {
			return err
		}
		if continuation == nil {
			return nil
		}
		t := continuation
		// A helper's thread runs on its owner's subscription: its turns' cap is 0.
		_, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_agent_turns
  (id, workspace_id, thread_id, user_id, runtime, kind, input, status, max_usd_micros)
SELECT ?, ?, ?, ?, runtime, ?, ?, 'queued', IF(runtime='api', ?, 0) FROM foundry_agent_threads WHERE id=?`,
			t.ID, t.WorkspaceID, t.ThreadID, t.UserID, string(t.Kind), t.Input, t.MaxUSDMicros, t.ThreadID)
		if isDuplicateKey(err) {
			// A turn went live after all: the click's end stands, and the
			// agent reads the action's result with the thread's actions.
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_agent_threads SET updated_at=NOW(6) WHERE id=?`, t.ThreadID); err != nil {
			return err
		}
		return tx.appendFoundryAgentEvents(ctx, t.ID, []json.RawMessage{firstEvent})
	})
}

// DismissFoundryAgentAction records the user's no to their prepared (or
// expired) action; dismissing it twice returns it. Refusals are a
// model.FoundryAgentRefusal; ErrNotFound means it isn't theirs.
func (s *mysqlDB) DismissFoundryAgentAction(ctx context.Context, workspaceID, userID, id string) (*model.FoundryAgentAction, error) {
	threadID, err := s.foundryAgentActionThread(ctx, workspaceID, userID, id)
	if err != nil {
		return nil, err
	}
	var out *model.FoundryAgentAction
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		a, err := tx.lockFoundryAgentAction(ctx, workspaceID, userID, threadID, id)
		switch {
		case err != nil:
			return err
		case a.Status == model.FoundryAgentActionDismissed:
			out = a
			return nil
		case a.Status != model.FoundryAgentActionPrepared && a.Status != model.FoundryAgentActionExpired:
			return model.FoundryAgentRefusal{Reason: model.FoundryAgentNotPrepared, Detail: "the action is " + string(a.Shown(time.Now()))}
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_agent_actions SET status='dismissed', decided_by=?, decided_at=NOW(6) WHERE id=?`,
			userID, id); err != nil {
			return err
		}
		out, err = scanFoundryAgentAction(tx.db.QueryRowContext(ctx, "SELECT "+foundryAgentActionColumns+` FROM foundry_agent_actions WHERE id=?`, id))
		return err
	})
	return out, err
}

// RefreshFoundryAgentAction gives the user's prepared or expired action the
// new confirmation prepared for the same payload, and its card as drawn
// again with it (display), waiting for them again. Refusals are a
// model.FoundryAgentRefusal; ErrNotFound means it isn't theirs.
func (s *mysqlDB) RefreshFoundryAgentAction(ctx context.Context, workspaceID, userID, id, payloadHash string, display json.RawMessage, confirmationID string,
	sealed []byte, keyID string, expiresAt time.Time) (*model.FoundryAgentAction, error) {
	if !json.Valid(display) {
		return nil, fmt.Errorf("refresh foundry agent action %s: its display isn't JSON", id)
	}
	threadID, err := s.foundryAgentActionThread(ctx, workspaceID, userID, id)
	if err != nil {
		return nil, err
	}
	var out *model.FoundryAgentAction
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		a, err := tx.lockFoundryAgentAction(ctx, workspaceID, userID, threadID, id)
		switch {
		case err != nil:
			return err
		case a.Status != model.FoundryAgentActionPrepared && a.Status != model.FoundryAgentActionExpired:
			return model.FoundryAgentRefusal{Reason: model.FoundryAgentNotPrepared, Detail: "the action is " + string(a.Shown(time.Now()))}
		case a.PayloadHash != payloadHash:
			return model.FoundryAgentRefusal{Reason: model.FoundryAgentChanged, Detail: "the refreshed action is not the one prepared"}
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_agent_actions
SET status='prepared', decided_by=NULL, decided_at=NULL, display=?, confirmation_id=?, confirmation_token_sealed=?, confirmation_key_id=?, expires_at=?
WHERE id=?`, string(display), confirmationID, sealed, keyID, expiresAt.UTC(), id); err != nil {
			return err
		}
		out, err = scanFoundryAgentAction(tx.db.QueryRowContext(ctx, "SELECT "+foundryAgentActionColumns+` FROM foundry_agent_actions WHERE id=?`, id))
		return err
	})
	return out, err
}

func scanFoundryAgentThread(scanner rowScanner) (*model.FoundryAgentThread, error) {
	var t model.FoundryAgentThread
	var runtime string
	var kind string
	var session sql.NullString
	if err := scanner.Scan(&t.ID, &t.WorkspaceID, &t.UserID, &kind, &t.Title, &runtime, &session, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.Runtime, t.RuntimeSessionID, t.Kind = model.FoundryAgentRuntime(runtime), session.String, model.FoundryAgentThreadKind(kind)
	return &t, nil
}

func scanFoundryAgentTurn(scanner rowScanner) (*model.FoundryAgentTurn, error) {
	var t model.FoundryAgentTurn
	var runtime, kind, status string
	var pageContext, asked, answers, errText, clientToken, leaseOwner, automationRun, automationNode sql.NullString
	var leaseUntil, startedAt, finishedAt sql.NullTime
	spend := &t.Spend
	if err := scanner.Scan(&t.ID, &t.WorkspaceID, &t.ThreadID, &t.UserID, &runtime, &kind, &t.Input, &automationRun, &automationNode, &pageContext,
		&asked, &answers, &status, &errText, &clientToken,
		&leaseOwner, &leaseUntil, &t.Attempts, &t.NextAttemptAt, &t.MaxUSDMicros, &t.ReservedMicros, &spend.USDMicros, &t.LostMicros, &spend.InputTokens,
		&spend.OutputTokens, &t.Steps, &t.LastSeq, &t.CreatedAt, &startedAt, &finishedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.Runtime, t.Kind, t.Status = model.FoundryAgentRuntime(runtime), model.FoundryAgentTurnKind(kind), model.FoundryAgentTurnStatus(status)
	if pageContext.Valid {
		t.PageContext = json.RawMessage(pageContext.String)
	}
	if asked.Valid {
		t.Asked = &model.FoundryAgentAsked{}
		if err := json.Unmarshal([]byte(asked.String), t.Asked); err != nil {
			return nil, fmt.Errorf("read foundry agent turn %s's questions: %w", t.ID, err)
		}
	}
	if answers.Valid {
		if err := json.Unmarshal([]byte(answers.String), &t.Answers); err != nil {
			return nil, fmt.Errorf("read foundry agent turn %s's answers: %w", t.ID, err)
		}
		if t.Answers == nil {
			t.Answers = []agentquestion.Answer{}
		}
	}
	for _, f := range []struct {
		src sql.NullString
		dst **string
	}{{errText, &t.Error}, {clientToken, &t.ClientToken}, {leaseOwner, &t.LeaseOwner}, {automationRun, &t.AutomationRunID},
		{automationNode, &t.AutomationNodeID}} {
		if f.src.Valid {
			*f.dst = &f.src.String
		}
	}
	for _, f := range []struct {
		src sql.NullTime
		dst **time.Time
	}{{leaseUntil, &t.LeaseUntil}, {startedAt, &t.StartedAt}, {finishedAt, &t.FinishedAt}} {
		if f.src.Valid {
			*f.dst = &f.src.Time
		}
	}
	return &t, nil
}

func scanFoundryAgentAction(scanner rowScanner) (*model.FoundryAgentAction, error) {
	var a model.FoundryAgentAction
	var status, payload, display string
	var result, errText, decidedBy sql.NullString
	var decidedAt sql.NullTime
	if err := scanner.Scan(&a.ID, &a.TurnID, &a.ThreadID, &a.WorkspaceID, &a.UserID, &a.Operation, &a.Target, &payload, &a.PayloadHash, &display,
		&a.TargetNamedByUser, &a.ConfirmationID, &a.TokenSealed, &a.KeyID, &status, &result, &errText, &a.ExpiresAt,
		&decidedBy, &decidedAt, &a.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.Status, a.Payload, a.Display = model.FoundryAgentActionStatus(status), json.RawMessage(payload), json.RawMessage(display)
	if result.Valid {
		a.Result = json.RawMessage(result.String)
	}
	if errText.Valid {
		a.Error = &errText.String
	}
	if decidedBy.Valid {
		a.DecidedBy = &decidedBy.String
	}
	if decidedAt.Valid {
		a.DecidedAt = &decidedAt.Time
	}
	return &a, nil
}
