package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// isDeadlock reports MySQL error 1213: InnoDB rolled the transaction back
// to break a deadlock, so running it again from the start may succeed.
func isDeadlock(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1213
}

// isDuplicateKey reports MySQL error 1062: a unique key refused the write.
func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// rowScanner is the *sql.Row / *sql.Rows shared interface used by scan helpers.
type rowScanner interface {
	Scan(dest ...any) error
}

// execOrNotFound runs an exec statement and returns ErrNotFound when it affected no rows.
// Callers use it for guarded writes (e.g. `UPDATE … WHERE id=? AND status='pending'`) where a
// 0-row result means the target row was absent or its WHERE guard didn't hold.
func execOrNotFound(ctx context.Context, db Executor, q string, args ...any) error {
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func nullJSON(j json.RawMessage) any {
	if len(j) == 0 {
		return nil
	}
	return string(j)
}

func joinComma(parts []string) string {
	return strings.Join(parts, ", ")
}

// whereClause renders AND conditions as " WHERE a AND b", or "" when there are none.
func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// idPlaceholders is n comma-separated placeholders, for an IN list.
func idPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toAnySlice(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

type timeIDCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

func encodeTimeIDCursor(createdAt time.Time, id string) (string, error) {
	raw, err := json.Marshal(timeIDCursor{CreatedAt: createdAt.UTC(), ID: id})
	if err != nil {
		return "", fmt.Errorf("encode page token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// decodeTimeIDCursor reads a token encodeTimeIDCursor made; any other is
// ErrInvalidPageToken.
func decodeTimeIDCursor(token string) (timeIDCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return timeIDCursor{}, fmt.Errorf("%w: %v", ErrInvalidPageToken, err)
	}
	var cursor timeIDCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return timeIDCursor{}, fmt.Errorf("%w: %v", ErrInvalidPageToken, err)
	}
	if cursor.CreatedAt.IsZero() || strings.TrimSpace(cursor.ID) == "" {
		return timeIDCursor{}, fmt.Errorf("%w: missing cursor fields", ErrInvalidPageToken)
	}
	return cursor, nil
}
