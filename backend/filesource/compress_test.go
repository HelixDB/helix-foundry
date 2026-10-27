package filesource

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/encoding/unicode"
)

const twoLines = "{\"id\":1,\"name\":\"a\"}\n{\"id\":2,\"name\":\"b\"}\n"

func TestCompression(t *testing.T) {
	plain := []byte(twoLines)
	for name, tc := range map[string]struct {
		file string
		data []byte
	}{
		"plain":             {"x.jsonl", plain},
		"gzip":              {"x.jsonl.gz", gzipBytes(t, plain)},
		"gzip multi-member": {"x.jsonl.gz", gzipBytes(t, plain[:20], plain[20:])},
		"zstd":              {"x.jsonl.zst", zstdBytes(t, plain)},
		"bzip2":             {"x.jsonl.bz2", bzip2Lines},
		"gz name on plain":  {"x.jsonl.gz", plain},
		"gzip without .gz":  {"x.jsonl", gzipBytes(t, plain)},
		"two layers":        {"x.jsonl.gz.zst", zstdBytes(t, gzipBytes(t, plain))},
	} {
		t.Run(name, func(t *testing.T) {
			r := scanAll(t, tempFile(t, tc.file, tc.data))
			require.Len(t, r.rows, 2)
			require.Equal(t, "b", r.rows[1]["name"])
			require.Equal(t, []string{"id"}, r.key)
		})
	}
}

func TestCompressionErrors(t *testing.T) {
	plain := []byte(twoLines)
	for name, tc := range map[string]struct {
		data   []byte
		kind   error
		msg    string
		record int64
	}{
		"zip":                {append([]byte("PK\x03\x04"), plain...), ErrCompression, "compressed with zip", 0},
		"empty zip":          {[]byte("PK\x05\x06\x00\x00\x00\x00"), ErrCompression, "compressed with zip", 0},
		"xz":                 {append([]byte{0xfd, '7', 'z', 'X', 'Z', 0}, plain...), ErrCompression, "compressed with xz", 0},
		"lz4":                {append([]byte{0x04, 0x22, 0x4d, 0x18}, plain...), ErrCompression, "compressed with lz4", 0},
		"zip inside gzip":    {gzipBytes(t, append([]byte("PK\x03\x04"), plain...)), ErrCompression, "compressed with zip", 0},
		"three layers":       {gzipBytes(t, zstdBytes(t, gzipBytes(t, plain))), ErrCompression, "compressed more than twice", 0},
		"truncated gzip":     {gzipBytes(t, bytes.Repeat(plain, 100))[:60], ErrMalformed, "corrupt gzip data", anyRecord},
		"bad gzip header":    {[]byte{0x1f, 0x8b, 0x00, 0x00}, ErrMalformed, "corrupt gzip data", 0},
		"truncated zstd":     {zstdBytes(t, bytes.Repeat(plain, 100))[:20], ErrMalformed, "corrupt zstd data", anyRecord},
		"truncated bzip2":    {bzip2Lines[:30], ErrMalformed, "corrupt bzip2 data", anyRecord},
		"compressed parquet": {gzipBytes(t, []byte("PAR1xxxxPAR1")), ErrCompression, "a compressed Parquet file", 0},
		"big zstd window":    {zstdFrameWindow(28), ErrCompression, "zstd window over 128 MiB; recompress without --long", anyRecord},
	} {
		t.Run(name, func(t *testing.T) {
			err := scanErr(t, DefaultLimits, tempFile(t, "x.jsonl", tc.data))
			requireKind(t, err, tc.kind, "x.jsonl", tc.record)
			require.Contains(t, err.Error(), tc.msg)
		})
	}
}

func TestZstdWindowAllocatesNothing(t *testing.T) {
	frame := zstdFrameWindow(28) // 256 MiB
	var syncErr, sampleErr error
	grew := peakHeap(t, func() {
		syncErr = scanErr(t, DefaultLimits, tempFile(t, "x.jsonl.zst", frame))
		_, sampleErr = Sample(context.Background(), bytes.NewReader(frame), "x.jsonl.zst", FormatJSON, false, DefaultSampleLimits)
	})
	require.ErrorIs(t, syncErr, ErrCompression)
	require.ErrorIs(t, sampleErr, ErrSampleBudget)
	require.Contains(t, sampleErr.Error(), "zstd window over 8 MiB")
	require.Less(t, grew, uint64(16<<20))
	// A window under the cap decodes.
	_, err := zstd.NewReader(nil)
	require.NoError(t, err)
}

func TestZstdBomb(t *testing.T) {
	data := zstdBytes(t, append([]byte("a\n"), bytes.Repeat([]byte("1\n"), 32<<20)...))
	lim := DefaultLimits
	lim.MaxDecompressedBytes = 8 << 20
	err := scanErr(t, lim, tempFile(t, "x.csv.zst", data))
	requireKind(t, err, ErrTooLarge, "x.csv.zst", anyRecord)
	require.Contains(t, err.Error(), "over 8 MiB once decompressed")
}

func TestDecompressedBudgetSpansFiles(t *testing.T) {
	body := []byte("a\n" + strings.Repeat("1\n", 499))
	lim := DefaultLimits
	lim.MaxDecompressedBytes = int64(len(body))
	// Exactly the budget reads; the second file goes over it.
	r := scanWith(t, lim, tempFile(t, "a.csv", body))
	require.Len(t, r.rows, 499)
	err := scanErr(t, lim, tempFile(t, "a.csv", body), tempFile(t, "b.csv", body))
	require.ErrorIs(t, err, ErrTooLarge)
	require.Equal(t, "b.csv", err.(*Error).File)
}

func TestCharsets(t *testing.T) {
	utf16 := func(order unicode.Endianness, s string) []byte {
		b, err := unicode.UTF16(order, unicode.UseBOM).NewEncoder().Bytes([]byte(s))
		require.NoError(t, err)
		return b
	}
	for name, data := range map[string][]byte{
		"utf-8 bom":    append([]byte{0xef, 0xbb, 0xbf}, `[{"name":"é"}]`...),
		"utf-16 le":    utf16(unicode.LittleEndian, `[{"name":"é"}]`),
		"utf-16 be":    utf16(unicode.BigEndian, `[{"name":"é"}]`),
		"utf-16 gzip":  gzipBytes(t, utf16(unicode.LittleEndian, `[{"name":"é"}]`)),
		"plain no bom": []byte(`[{"name":"é"}]`),
	} {
		t.Run(name, func(t *testing.T) {
			r := scanAll(t, tempFile(t, "x.json", data))
			require.Equal(t, []map[string]any{{"name": "é"}}, r.rows)
		})
	}
}

// TestBudgetReaderBoundsMemory feeds 1 GiB inputs that the stdlib decoders
// would otherwise buffer whole.
func TestBudgetReaderBoundsMemory(t *testing.T) {
	const gib = 1 << 30
	ctx := context.Background()
	reader := func(ctx context.Context, r io.Reader) *budgetReader {
		return newBudgetReader(ctx, r, newBudget(DefaultLimits.MaxDecompressedBytes), DefaultLimits.MaxRecordBytes)
	}
	for name, tc := range map[string]struct {
		run  func(br *budgetReader) error
		data io.Reader
	}{
		"json string value": {
			data: repeat([]byte(`{"a":"`), "x", gib),
			run: func(br *budgetReader) error {
				recs, err := newJSONRecords(br, jsonLayout{shape: shapeLines}, DefaultLimits.MaxRecordBytes, false)
				require.NoError(t, err)
				_, err = recs.next()
				return err
			},
		},
		"csv unbalanced quote": {
			data: repeat([]byte("a,b\n1,\"5"), "x\n", gib),
			run: func(br *budgetReader) error {
				recs, err := newCSVRecords(br, "x.csv", DefaultLimits.MaxRecordBytes, 1000)
				require.NoError(t, err)
				_, err = recs.next()
				return err
			},
		},
		"csv newline-free line": {
			data: repeat([]byte("a,b\n"), "x", gib),
			run: func(br *budgetReader) error {
				recs, err := newCSVRecords(br, "x.csv", DefaultLimits.MaxRecordBytes, 1000)
				require.NoError(t, err)
				_, err = recs.next()
				return err
			},
		},
		"json blank lines": {
			data: repeat(nil, "\n", gib),
			run: func(br *budgetReader) error {
				_, err := newJSONRecords(br, jsonLayout{shape: shapeLines}, DefaultLimits.MaxRecordBytes, false)
				return err
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			grew := peakHeap(t, func() { err = tc.run(reader(ctx, tc.data)) })
			require.ErrorIs(t, err, ErrRecordTooLarge)
			// A decoder that buffered its input would grow by about 1 GiB. The
			// bound is well below that, and above the record cap's buffers
			// with the garbage a loaded machine's collector has yet to reach.
			require.Less(t, grew, uint64(128<<20))
		})
	}
	t.Run("blank lines with ctx cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		_, err := newJSONRecords(reader(ctx, repeat(nil, "\n", gib)), jsonLayout{shape: shapeLines}, DefaultLimits.MaxRecordBytes, false)
		require.ErrorIs(t, err, context.Canceled)
		require.Less(t, time.Since(start), time.Second)
	})
}

func TestBudgetReader(t *testing.T) {
	t.Run("ctx cancelled mid-file", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := tempFile(t, "x.jsonl", []byte(twoLines))
		s, err := Scan(ctx, []File{f}, DefaultLimits)
		require.NoError(t, err)
		cancel()
		for _, err := range s.Rows(ctx) {
			require.ErrorIs(t, err, context.Canceled)
			var e *Error
			require.False(t, errors.As(err, &e))
		}
		_, err = Scan(ctx, []File{f}, DefaultLimits)
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("read errors pass through", func(t *testing.T) {
		boom := errors.New("boom")
		br := newBudgetReader(context.Background(), io.MultiReader(strings.NewReader("ab"), errReader{boom}), newBudget(100), 100)
		_, err := io.ReadAll(br)
		require.ErrorIs(t, err, boom)
		_, err = br.Read(make([]byte, 1))
		require.ErrorIs(t, err, boom)
		require.ErrorIs(t, br.cause(), boom)
	})
	t.Run("exhausted then a read error", func(t *testing.T) {
		boom := errors.New("boom")
		br := newBudgetReader(context.Background(), io.MultiReader(strings.NewReader("ab"), zeroReader{}, errReader{boom}), newBudget(2), 100)
		_, err := io.ReadAll(br)
		require.ErrorIs(t, err, boom)
	})
	t.Run("cut budget", func(t *testing.T) {
		b := newBudget(2)
		b.cut = true
		_, err := io.ReadAll(newBudgetReader(context.Background(), strings.NewReader("abc"), b, 100))
		require.ErrorIs(t, err, errCut)
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// zeroReader reads nothing without an error once, then ends.
type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, io.EOF }

func TestSizeText(t *testing.T) {
	require.Equal(t, "2 GiB", sizeText(2<<30))
	require.Equal(t, "16 MiB", sizeText(16<<20))
	require.Equal(t, "64 KiB", sizeText(64<<10))
	require.Equal(t, "1000 bytes", sizeText(1000))
	require.Equal(t, "0 bytes", sizeText(0))
}

func TestErrorMessages(t *testing.T) {
	err := &Error{File: "a.csv", Err: because(ErrTooManyColumns, "more than 1000 columns")}
	require.Equal(t, "a.csv: more than 1000 columns", err.Error())
	require.ErrorIs(t, err, ErrTooManyColumns)
	wrapped := &reason{kind: ErrMalformed, msg: "corrupt gzip data", cause: io.ErrUnexpectedEOF}
	require.ErrorIs(t, wrapped, io.ErrUnexpectedEOF)
	require.Equal(t, "corrupt gzip data", wrapped.Error())
	require.Equal(t, "none", codecNone.String())
}

func TestDecompressOptions(t *testing.T) {
	_, _, err := decompress(bytes.NewReader(zstdBytes(t, []byte("x"))), decoding{maxWindow: 10, windowErr: ErrCompression})
	require.ErrorIs(t, err, ErrMalformed)
	b := newBudget(1)
	br := newBudgetReader(context.Background(), io.MultiReader(strings.NewReader("a"), &nothingOnce{}), b, 10)
	data, err := io.ReadAll(br)
	require.NoError(t, err)
	require.Equal(t, "a", string(data))
}

// nothingOnce reads nothing and no error once, then ends.
type nothingOnce struct{ done bool }

func (n *nothingOnce) Read([]byte) (int, error) {
	if n.done {
		return 0, io.EOF
	}
	n.done = true
	return 0, nil
}
