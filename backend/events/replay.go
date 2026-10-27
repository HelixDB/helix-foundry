package events

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"time"
)

// SameSend reports whether a stored batch object, as its store returns it
// (gzip), holds exactly the send's pieces: in the same order, each with the
// same id, the same occurred_at instant (or none) and the same data after
// json.Compact. received_at may differ: a retry arrives later. A send of
// another size never matches, so compare the batch row's events and bytes
// with Send.Pieces and Send.Bytes first, and read the object only when they
// are equal. An error is an object that could not be read whole, or one no
// send could have written (ErrCorrupt).
func SameSend(object io.Reader, send Send) (bool, error) {
	gz, err := gzip.NewReader(object)
	if err != nil {
		return false, fmt.Errorf("events: read batch object: %w", err)
	}
	// One byte past the send's size tells a longer object apart without
	// reading it all; a shorter one ends early, its checksum checked.
	body, err := io.ReadAll(io.LimitReader(gz, send.bytes+1))
	if err != nil {
		return false, fmt.Errorf("events: read batch object: %w", err)
	}
	if int64(len(body)) != send.bytes {
		return false, nil
	}
	lines := bytes.SplitAfter(body, []byte("\n"))
	// A whole object ends with a newline, so the split leaves one empty tail.
	if len(lines) != len(send.pieces)+1 || len(lines[len(lines)-1]) != 0 {
		return false, nil
	}
	for i, p := range send.pieces {
		l, err := DecodeLine(lines[i])
		if err != nil {
			return false, err
		}
		occurred := ""
		if p.occurred != nil {
			occurred = p.occurred.Format(time.RFC3339Nano)
		}
		if l.id != p.id || l.occurred != occurred || !bytes.Equal(l.data, p.data) {
			return false, nil
		}
	}
	return true, nil
}
