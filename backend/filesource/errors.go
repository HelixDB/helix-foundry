package filesource

import (
	"errors"
	"fmt"
	"slices"
)

// The kinds of unreadable data. Each error this package returns wraps one of
// them, so callers branch with errors.Is; the message says what to fix.
var (
	ErrMalformed      = errors.New("malformed data")
	ErrNotObject      = errors.New("not a JSON object")
	ErrTooDeep        = errors.New("nested too deeply")
	ErrTooManyColumns = errors.New("too many columns")
	ErrRecordTooLarge = errors.New("record too large")
	ErrTooLarge       = errors.New("too large once decompressed")
	ErrCompression    = errors.New("unsupported compression")
	ErrAmbiguousJSON  = errors.New("several lists of records")
	ErrExtraFields    = errors.New("more fields than the header")
	ErrParquet        = errors.New("unreadable Parquet")
	ErrSampleBudget   = errors.New("over the sample budget")
)

// kinds are the kinds of unreadable data, which [Error] carries.
var kinds = []error{ErrMalformed, ErrNotObject, ErrTooDeep, ErrTooManyColumns, ErrRecordTooLarge, ErrTooLarge,
	ErrCompression, ErrAmbiguousJSON, ErrExtraFields, ErrParquet, ErrSampleBudget}

// isUnreadable reports err as one of the kinds of unreadable data. Any other
// error (a context's, a failed read or open) is not the data's fault, and
// reading again may get past it.
func isUnreadable(err error) bool {
	return slices.ContainsFunc(kinds, func(kind error) bool { return errors.Is(err, kind) })
}

// Error is unreadable data in one file. Its message is written for the
// customer: "<file> record <n>: <reason>", or "<file>: <reason>" when the
// problem is not in one record.
type Error struct {
	File string
	// Record is the 1-based record within the file; 0 is the file as a whole.
	Record int64
	Err    error
}

func (e *Error) Error() string {
	if e.Record > 0 {
		return fmt.Sprintf("%s record %d: %v", e.File, e.Record, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.File, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// reason is a kind of unreadable data with a specific message, so the
// message reads naturally while errors.Is still finds the kind. cause is a
// library's own error, kept for logs and errors.Is but never in the message.
type reason struct {
	kind  error
	msg   string
	cause error
}

func (r *reason) Error() string { return r.msg }

func (r *reason) Unwrap() []error {
	if r.cause == nil {
		return []error{r.kind}
	}
	return []error{r.kind, r.cause}
}

func because(kind error, format string, args ...any) error {
	return &reason{kind: kind, msg: fmt.Sprintf(format, args...)}
}

// sizeText writes a byte count the way limits are configured: whole binary
// units when exact, otherwise bytes.
func sizeText(n int64) string {
	for _, u := range []struct {
		shift uint
		name  string
	}{{30, "GiB"}, {20, "MiB"}, {10, "KiB"}} {
		if n >= 1<<u.shift && n%(1<<u.shift) == 0 {
			return fmt.Sprintf("%d %s", n>>u.shift, u.name)
		}
	}
	return fmt.Sprintf("%d bytes", n)
}
