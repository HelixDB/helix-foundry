package events

import (
	"fmt"
	"slices"
)

// rowSlack is the most a row exceeds its line by beyond the schema's null
// fill: a null occurred_at (`,"occurred_at":null`, 19 bytes) less the data
// wrapper (`,"data":` and its closing brace, 9).
const rowSlack = 10

// warnPercent is how full a source is when every answer starts to warn.
const warnPercent = 80

// Caps are the limits every table has, foundry.sync.max_rows_per_table and
// max_table_bytes; past either, a sync fails for good. Config validates both
// positive, and max_table_bytes at most 4 GiB.
type Caps struct {
	MaxRows, MaxBytes int64
}

// Capacity is how full a source's table will be once a sync has merged every
// batch waiting, against the caps. Both counts are upper bounds: a resent id
// counts again, and every waiting piece counts the padding of every field.
type Capacity struct {
	Rows, Bytes int64
	Caps
}

// Percent is how full the fuller of the two counts is, rounded down.
func (c Capacity) Percent() int {
	return int(max(c.Rows*100/c.MaxRows, c.Bytes*100/c.MaxBytes))
}

// Warning is what every send, list and page tells people from 80% full, or
// "" below. It names the fuller count, rows on a tie.
func (c Capacity) Warning() string {
	if c.Percent() < warnPercent {
		return ""
	}
	held := fmt.Sprintf("%s of %s pieces", grouped(c.Rows), grouped(c.MaxRows))
	if c.Bytes*100/c.MaxBytes > c.Rows*100/c.MaxRows {
		held = fmt.Sprintf("%s of %s bytes", grouped(c.Bytes), grouped(c.MaxBytes))
	}
	return fmt.Sprintf("This source is %d%% full (%s). Once it is full, sends are refused: send new pieces to a new source before then.", c.Percent(), held)
}

// Holding is what a source holds, as the caps count it.
type Holding struct {
	// TableRows is the row_count of its events table in connector_tables,
	// and TableBytes the uncompressed size of that table's snapshot (ISIZE);
	// both 0 before its first sync.
	TableRows, TableBytes int64
	// TableFields are the names of that table's stored columns. Every row
	// carries each, so a schema field missing here is padded into every row
	// at the next merge.
	TableFields []string
	// PendingPieces and PendingBytes are the sums of events and bytes of the
	// batches above its cursor, which no sync has merged yet.
	PendingPieces, PendingBytes int64
}

// Capacity is how full the source's table will be once the waiting batches
// are merged under schema s: the table's rows and bytes, the padding of s's
// fields the table's rows lack, and each waiting piece's line, its padding
// and rowSlack.
func (h Holding) Capacity(s Schema, caps Caps) Capacity {
	// ISIZE is the size mod 2^32, and config caps a table at 4 GiB
	// (config.MaxSnapshotBytes): a table with rows that reads as empty holds
	// exactly 4 GiB.
	table := h.TableBytes
	if h.TableRows > 0 && table == 0 {
		table = 1 << 32
	}
	var unpadded int64
	for _, f := range s.fields {
		if !slices.Contains(h.TableFields, f.Name) {
			unpadded += nullFill(f)
		}
	}
	return Capacity{
		Rows:  h.TableRows + h.PendingPieces,
		Bytes: table + h.TableRows*unpadded + h.PendingBytes + h.PendingPieces*(s.NullFill()+rowSlack),
		Caps:  caps,
	}
}

// Project is the capacity once send is stored, with after the schema its
// merge gives (Merged.Schema), or a *Refusal of kind Full when that passes a
// cap: refusing the send keeps every later sync of the source within the
// caps. Run it before the batch object is written, and again under the
// source's row lock with the values read there: sends that committed in
// between count only there.
func (h Holding) Project(send Send, after Schema, caps Caps) (Capacity, error) {
	held := h.TableRows + h.PendingPieces
	h.PendingPieces += int64(len(send.pieces))
	h.PendingBytes += send.bytes
	c := h.Capacity(after, caps)
	switch {
	case c.Rows > caps.MaxRows:
		return Capacity{}, refuse(Full, "This source holds %s pieces, and one source holds at most %s: this send's %s would pass that. Send to a new source.", grouped(held), grouped(caps.MaxRows), grouped(int64(len(send.pieces))))
	case c.Bytes > caps.MaxBytes:
		return Capacity{}, refuse(Full, "This source's table would hold %s bytes with this send, and one source holds at most %s. Send to a new source.", grouped(c.Bytes), grouped(caps.MaxBytes))
	}
	return c, nil
}
