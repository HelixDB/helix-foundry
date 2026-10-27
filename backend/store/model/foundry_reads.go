package model

import "time"

// foundry_reads.go is reading a document once, at upload
// (foundry_document_reads): a table file's rows, or a PDF's text. A text
// document is never read this way; it is extracted as it is.

// FoundryFileReadMode is how deeply a document is read. Table files and
// PDFs have a standard read; a PDF's deep read (not yet offered) would be a
// second row.
type FoundryFileReadMode string

const (
	FoundryFileReadStandard FoundryFileReadMode = "standard"
	FoundryFileReadDeep     FoundryFileReadMode = "deep"
)

// FoundryFileReadStatus is where a read is.
type FoundryFileReadStatus string

const (
	// FoundryFileReadQueued waits for a worker from NextAttemptAt.
	FoundryFileReadQueued FoundryFileReadStatus = "queued"
	// FoundryFileReadRunning is claimed: a worker reads it until
	// ClaimedUntil, and a claim that lapses is claimed again.
	FoundryFileReadRunning FoundryFileReadStatus = "running"
	// FoundryFileReadSubmitted is a PDF handed to a parsing service under
	// JobID. Nothing submits yet: the standard read is in-process.
	FoundryFileReadSubmitted FoundryFileReadStatus = "submitted"
	// FoundryFileReadSucceeded holds what the file gave, at ArtifactKey.
	// It never changes after.
	FoundryFileReadSucceeded FoundryFileReadStatus = "succeeded"
	// FoundryFileReadFailed could not read the file, for good: Error says why.
	FoundryFileReadFailed FoundryFileReadStatus = "failed"
)

// FoundryFileReadErrorCode is why a file could not be read. An extraction of
// the document records the same code.
type FoundryFileReadErrorCode string

const (
	// FoundryFileReadTooLarge is a file past a reading limit.
	FoundryFileReadTooLarge FoundryFileReadErrorCode = "too_large"
	// FoundryFileReadUnsupported is a file that is not what its type says,
	// that the worker could not get through, or a PDF with no text to read
	// (encrypted, or a scan).
	FoundryFileReadUnsupported FoundryFileReadErrorCode = "unsupported"
)

// FoundryFileReadError is why a read failed: its code, and what to fix, for
// the user.
type FoundryFileReadError struct {
	Code    FoundryFileReadErrorCode
	Message string
}

// FoundryFileRead is one read of a document (a foundry_document_reads row).
type FoundryFileRead struct {
	DocumentID  string
	WorkspaceID string
	Mode        FoundryFileReadMode
	Status      FoundryFileReadStatus
	// Attempts counts the claims since the read last ended one in order: a
	// claim the worker never ended (the file killed it) leaves it counted,
	// and a lapsed read with MaxFoundryFileReadLapses of them fails.
	Attempts      int
	NextAttemptAt time.Time
	// ClaimToken and ClaimedUntil are the claim of a running read; only its
	// holder may end it.
	ClaimToken   *string
	ClaimedUntil *time.Time
	// JobID is a PDF's parse job, once submitted.
	JobID *string
	// PageCount is a PDF's pages; RowCount a table file's rows, once read.
	PageCount *int
	RowCount  *int64
	// USDMicros is what reading it cost: nothing, for a read in-process.
	USDMicros int64
	// TextSHA256 is the SHA-256 (lowercase hex) of a PDF's text, once read.
	TextSHA256 *string
	// ArtifactKey is where what it gave lives, set exactly once succeeded: a
	// table file's rows as gzipped JSON Lines under its read/ prefix, or its
	// original when that is sent as it is; a PDF's text under its read/
	// prefix (document.TextKey).
	ArtifactKey *string
	// Error is set exactly once failed.
	Error *FoundryFileReadError
	// LastError is what the last attempt that failed said, while it waits to
	// be read again; once failed, Error's message.
	LastError         *string
	RequestedByUserID *string
	CreatedAt         time.Time
	FinishedAt        *time.Time
}

// MaxFoundryFileReadLapses is how many claims of a read may lapse in a row
// before it fails as unsupported: a file that keeps killing the worker would
// otherwise restart the pod forever.
const MaxFoundryFileReadLapses = 3

// Retrying reports a queued read that failed before and waits to be read
// again.
func (r *FoundryFileRead) Retrying() bool {
	return r.Status == FoundryFileReadQueued && r.LastError != nil
}

// FoundryFileReadState is what reading a document has come to, as the
// website shows it.
type FoundryFileReadState string

const (
	// FoundryFileReadReady is a document ontologies can read: text, or a
	// file whose read succeeded.
	FoundryFileReadReady FoundryFileReadState = "ready"
	// FoundryFileReadReading is a file being read, or waiting to be.
	FoundryFileReadReading FoundryFileReadState = "reading"
	// FoundryFileReadFailedState is a file that could not be read.
	FoundryFileReadFailedState FoundryFileReadState = "failed"
)

// ReadState is what reading d has come to. A text document is always
// ready; a table file or a PDF is as its read is, and one with no read yet
// (an upload before reads were queued) is reading. The database holds no
// other type or status; one that slipped through reads as failed rather
// than ready.
func (d *FoundryDocument) ReadState() FoundryFileReadState {
	switch d.MediaType.ReadKind() {
	case FoundryReadText:
		return FoundryFileReadReady
	case FoundryReadTable, FoundryReadPDF:
	default:
		return FoundryFileReadFailedState
	}
	status := FoundryFileReadQueued
	if d.Read != nil {
		status = d.Read.Status
	}
	switch status {
	case FoundryFileReadSucceeded:
		return FoundryFileReadReady
	case FoundryFileReadQueued, FoundryFileReadRunning, FoundryFileReadSubmitted:
		return FoundryFileReadReading
	case FoundryFileReadFailed:
	}
	return FoundryFileReadFailedState
}

// TextKey is where the text ontologies read of d lives, which loads,
// generations and the document page read: a text document's original, or
// the text a PDF's read stored (UTF-8, pages split by form feeds). False for
// a table file, which is read as a table, and for a PDF whose read has not
// succeeded.
func (d *FoundryDocument) TextKey() (string, bool) {
	switch d.MediaType.ReadKind() {
	case FoundryReadText:
		return d.S3Key, true
	case FoundryReadPDF:
		if d.Read == nil || d.Read.Status != FoundryFileReadSucceeded || d.Read.ArtifactKey == nil {
			return "", false
		}
		return *d.Read.ArtifactKey, true
	}
	return "", false
}

// FoundryFileReadWake is what a PDF's read hands the ontologies whose loads
// hold the document once it ends, as an upload's completion does
// (FoundryDocumentFanOut): an id per extract it queues, and the month's
// budget. Each extract is requested by the person who uploaded the PDF, so
// it draws on a person's cap (FoundryGraphBudget.CapFor), as the upload's
// own extract would have.
type FoundryFileReadWake struct {
	NewJobID func() string
	Budget   FoundryGraphBudget
}

// FoundryFileReadClaim is a read a worker claimed, with its document.
type FoundryFileReadClaim struct {
	Read     *FoundryFileRead
	Document *FoundryDocument
}

// FoundryFileReadOutcome ends a claim: FoundryTableRead, FoundryPDFRead,
// FoundryFileUnreadable, FoundryFileReadRetry or FoundryFileReadReleased,
// nothing else, as the unexported method seals it.
type FoundryFileReadOutcome interface {
	fileReadOutcome()
}

// FoundryTableRead is a table file read: Rows rows, at ArtifactKey.
type FoundryTableRead struct {
	ArtifactKey string
	Rows        int64
}

// FoundryPDFRead is a PDF read as text: Pages pages, its text at
// ArtifactKey, whose SHA-256 (lowercase hex) is TextSHA256.
type FoundryPDFRead struct {
	ArtifactKey string
	Pages       int
	TextSHA256  string
}

// FoundryFileUnreadable fails the read for good: the file is not what its
// type says, or past a reading limit.
type FoundryFileUnreadable struct {
	Error FoundryFileReadError
}

// FoundryFileReadRetry gives the read back to the queue until At, after a
// failure that is not the file's (storage, the database); Message says what.
// The claim ended in order, so it is not counted as a lapse.
type FoundryFileReadRetry struct {
	At      time.Time
	Message string
}

// FoundryFileReadReleased gives the read back to the queue as it was, due
// now: the worker is stopping. The claim does not count.
type FoundryFileReadReleased struct{}

func (FoundryTableRead) fileReadOutcome()        {}
func (FoundryPDFRead) fileReadOutcome()          {}
func (FoundryFileUnreadable) fileReadOutcome()   {}
func (FoundryFileReadRetry) fileReadOutcome()    {}
func (FoundryFileReadReleased) fileReadOutcome() {}
