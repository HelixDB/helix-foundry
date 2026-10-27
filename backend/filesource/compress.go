package filesource

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"io"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// maxLayers is how many compression layers a file may have (a .csv.gz.zst).
const maxLayers = 2

const peekSize = 64 << 10

type codec uint8

const (
	codecNone codec = iota
	codecGzip
	codecZstd
	codecBzip2
)

var (
	magicGzip      = []byte{0x1f, 0x8b}
	magicZstd      = []byte{0x28, 0xb5, 0x2f, 0xfd}
	magicBzip2     = []byte("BZh")
	bzip2Block     = []byte{0x31, 0x41, 0x59, 0x26, 0x53, 0x59}
	bzip2End       = []byte{0x17, 0x72, 0x45, 0x38, 0x50, 0x90}
	magicParquet   = []byte("PAR1")
	unsupportedFmt = []struct {
		magic []byte
		name  string
	}{
		{[]byte("PK\x03\x04"), "zip"},
		{[]byte("PK\x05\x06"), "zip"},
		{[]byte{0xfd, '7', 'z', 'X', 'Z', 0x00}, "xz"},
		{[]byte{0x04, 0x22, 0x4d, 0x18}, "lz4"},
		{[]byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}, "7z"},
	}
)

// sniff names the codec of a stream from its first bytes. A codec Helix
// does not read comes back by name.
func sniff(head []byte) (codec, string) {
	switch {
	case bytes.HasPrefix(head, magicGzip):
		return codecGzip, ""
	case bytes.HasPrefix(head, magicZstd):
		return codecZstd, ""
	case len(head) >= 10 && bytes.HasPrefix(head, magicBzip2) && head[3] >= '1' && head[3] <= '9' &&
		(bytes.Equal(head[4:10], bzip2Block) || bytes.Equal(head[4:10], bzip2End)):
		return codecBzip2, ""
	}
	for _, u := range unsupportedFmt {
		if bytes.HasPrefix(head, u.magic) {
			return codecNone, u.name
		}
	}
	return codecNone, ""
}

// Compression names the compression a file's first bytes show, "" for
// none: gzip, zstd or bzip2, which Scan reads, or zip, xz, lz4 or 7z, which
// it refuses. A caller that takes only plain files refuses any of them
// before Scan decompresses one.
func Compression(head []byte) string {
	c, unsupported := sniff(head)
	if unsupported != "" || c == codecNone {
		return unsupported
	}
	return c.String()
}

// decoding are the knobs of one decompression.
type decoding struct {
	// maxWindow caps a zstd window; windowErr is the kind returned over it.
	maxWindow uint64
	windowErr error
	// cut marks an input that is a prefix of the object: a stream ending
	// early ends the sample instead of failing.
	cut bool
}

// decompress peels up to two compression layers, chosen by magic bytes,
// never by extension. close releases the zstd decoders.
func decompress(r io.Reader, d decoding) (_ *bufio.Reader, close func(), err error) {
	var closers []func()
	closeAll := func() {
		for _, c := range closers {
			c()
		}
	}
	defer func() {
		if err != nil {
			closeAll()
		}
	}()
	src := &sourceReader{r: r}
	br := bufio.NewReaderSize(src, peekSize)
	for layer := 0; ; layer++ {
		head, _ := br.Peek(10) // a short or failing peek is plain data or a later read error
		c, unsupported := sniff(head)
		if unsupported != "" {
			return nil, nil, because(ErrCompression, "compressed with %s; Helix reads gzip, zstd and bzip2", unsupported)
		}
		if c == codecNone {
			if layer > 0 && bytes.HasPrefix(head, magicParquet) {
				return nil, nil, because(ErrCompression, "a compressed Parquet file; store Parquet files uncompressed, their pages are compressed already")
			}
			return br, closeAll, nil
		}
		if layer == maxLayers {
			return nil, nil, because(ErrCompression, "compressed more than twice")
		}
		var next io.Reader
		switch c {
		case codecGzip:
			zr, err := gzip.NewReader(br)
			if err != nil {
				return nil, nil, d.codecError("gzip", err, src)
			}
			next = zr
		case codecZstd:
			zr, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(d.maxWindow))
			if err != nil {
				return nil, nil, d.codecError("zstd", err, src)
			}
			closers = append(closers, zr.Close)
			next = zr
		case codecBzip2:
			next = bzip2.NewReader(br)
		}
		br = bufio.NewReaderSize(&codecReader{r: next, name: c.String(), d: d, src: src}, peekSize)
	}
}

func (c codec) String() string {
	switch c {
	case codecGzip:
		return "gzip"
	case codecZstd:
		return "zstd"
	case codecBzip2:
		return "bzip2"
	}
	return "none"
}

// sourceReader keeps the first error the compressed input itself returned.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && s.err == nil {
		s.err = err
	}
	return n, err
}

// codecReader turns a decompressor's errors into this package's kinds.
type codecReader struct {
	r    io.Reader
	name string
	d    decoding
	src  *sourceReader
}

func (c *codecReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = c.d.codecError(c.name, err, c.src)
	}
	return n, err
}

// codecError names a decompressor's error. One the input passed on, such
// as a broken connection, is not corrupt data and comes back as it is.
func (d decoding) codecError(name string, err error, src *sourceReader) error {
	switch {
	case src.err != nil:
		return src.err
	case errors.Is(err, zstd.ErrWindowSizeExceeded), errors.Is(err, zstd.ErrDecoderSizeExceeded):
		return because(d.windowErr, "zstd window over %s; recompress without --long", sizeText(int64(d.maxWindow)))
	case d.cut:
		return errCut
	}
	return &reason{kind: ErrMalformed, msg: "corrupt " + name + " data", cause: err}
}

// charset decides how a decompressed text stream is decoded to UTF-8.
type charset uint8

const (
	// charsetDetect strips a UTF-8 BOM and transcodes UTF-16 with a BOM.
	charsetDetect charset = iota
	// charsetWindows1252 reads a CSV that is not valid UTF-8.
	charsetWindows1252
)

// decodeText turns br into UTF-8.
func decodeText(br *bufio.Reader, cs charset) io.Reader {
	head, _ := br.Peek(3) // a short peek is a short file
	switch {
	case bytes.HasPrefix(head, []byte{0xef, 0xbb, 0xbf}):
		_, _ = br.Discard(3) // peeked above
	case bytes.HasPrefix(head, []byte{0xff, 0xfe}):
		return transform.NewReader(br, unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM).NewDecoder())
	case bytes.HasPrefix(head, []byte{0xfe, 0xff}):
		return transform.NewReader(br, unicode.UTF16(unicode.BigEndian, unicode.ExpectBOM).NewDecoder())
	}
	if cs == charsetWindows1252 {
		return transform.NewReader(br, charmap.Windows1252.NewDecoder())
	}
	return br
}
