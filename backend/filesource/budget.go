package filesource

import (
	"context"
	"errors"
	"io"
)

// recordSlack is how far past a record's limit the budget reader lets a
// decoder read ahead; the exact record size is checked once it is decoded.
const recordSlack = 64 << 10

// errCut ends a sample whose input was cut short; it is never returned.
var errCut = errors.New("filesource: sample cut")

// budget is the decompressed bytes one pass may still read, shared by every
// file of a table so many small bombs cannot add up.
type budget struct {
	left, limit int64
	// cut makes running out end the input (a sample) instead of failing.
	cut bool
}

func newBudget(limit int64) *budget { return &budget{left: limit, limit: limit} }

// budgetReader feeds the stdlib decoders. Without it encoding/json would
// buffer a huge string whole, and encoding/csv a newline-free line or an
// unbalanced quote to the end of the file. It never hands out more than the
// remaining budget, fails once a decoder reads more than maxRecord past the
// last record boundary, and stops when ctx ends.
type budgetReader struct {
	ctx       context.Context
	r         io.Reader
	total     *budget
	maxRecord int64
	// read is the bytes handed out; boundary is where the last record ended.
	read, boundary int64
	err            error
}

func newBudgetReader(ctx context.Context, r io.Reader, total *budget, maxRecord int) *budgetReader {
	return &budgetReader{ctx: ctx, r: r, total: total, maxRecord: int64(maxRecord) + recordSlack}
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if err := b.ctx.Err(); err != nil {
		return 0, b.fail(err)
	}
	room := b.maxRecord - (b.read - b.boundary)
	if room <= 0 {
		return 0, b.fail(because(ErrRecordTooLarge, "a record over %s", sizeText(b.maxRecord-recordSlack)))
	}
	if b.total.left <= 0 {
		return b.exhausted()
	}
	p = p[:min(int64(len(p)), room, b.total.left)]
	n, err := b.r.Read(p)
	b.read += int64(n)
	b.total.left -= int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		b.err = err
	}
	return n, err
}

// exhausted is the budget running out: the end of the input if nothing is
// left to read, else a cut sample or a table too large to read.
func (b *budgetReader) exhausted() (int, error) {
	var one [1]byte
	for {
		n, err := b.r.Read(one[:])
		switch {
		case n == 0 && errors.Is(err, io.EOF):
			return 0, io.EOF
		case n == 0 && err == nil:
			continue
		case n == 0:
			return 0, b.fail(err)
		case b.total.cut:
			return 0, b.fail(errCut)
		}
		return 0, b.fail(because(ErrTooLarge, "over %s once decompressed", sizeText(b.total.limit)))
	}
}

func (b *budgetReader) fail(err error) error {
	b.err = err
	return err
}

// mark records that a record ended at offset, in bytes handed out.
func (b *budgetReader) mark(offset int64) { b.boundary = offset }

// cause is the reader's own error, which decoders may wrap or replace.
func (b *budgetReader) cause() error { return b.err }
