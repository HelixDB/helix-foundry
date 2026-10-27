package events

import (
	"encoding/binary"
	"fmt"
)

// ISIZE is the uncompressed size of a gzip file read from its last bytes,
// such as a ranged GET of bytes=-4: the ISIZE field ending its trailer (RFC
// 1952 §2.3.1), the last member's size mod 2^32. A snapshot is one member
// (snapshot.Writer) of at most max_table_bytes, which config caps at 4 GiB,
// so for a snapshot it is exact but at exactly 4 GiB, which reads as 0
// (Holding.Capacity counts that case for a table with rows).
func ISIZE(tail []byte) (int64, error) {
	if len(tail) < 4 {
		return 0, fmt.Errorf("events: a gzip file ends in a 4-byte ISIZE; read %d bytes", len(tail))
	}
	return int64(binary.LittleEndian.Uint32(tail[len(tail)-4:])), nil
}
