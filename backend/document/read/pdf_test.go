package read

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// documentPDFium is PDFium as the document worker runs it. Compiling it
// takes a second or two, so the tests share it; each read still gets an
// instance of its own, so no scenario sees another's.
var documentPDFium = NewPDFium(PDFMemoryLimit)

// pdfs reads PDFs from s as the document worker does, at its default limits.
func pdfs(s *store) *PDFs {
	return &PDFs{Store: s, PDFium: documentPDFium, Limits: PDFLimits{MaxPages: 150, MaxTextBytes: 1 << 20}}
}

// encrypted is a trailer whose /Encrypt dictionary needs a user password:
// its /U entry matches no empty password.
const encrypted = "/Encrypt << /Filter /Standard /V 1 /R 2 " +
	"/O <0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20> " +
	"/U <2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40> /P -4 >> " +
	"/ID [<00112233445566778899aabbccddeeff> <00112233445566778899aabbccddeeff>]"

// scanned is a page with only an image on it, as a scanner makes.
const scanned = pdfPage("q 100 0 0 100 72 600 cm /Im1 Do Q\n")

// textOK extracts data and wants text.
func textOK(t *testing.T, p *PDFs, data []byte) (string, int) {
	t.Helper()
	text, pages, bad, err := p.Text(context.Background(), data)
	if err != nil || bad != nil {
		t.Fatalf("bad = %+v, err = %v", bad, err)
	}
	return text, pages
}

// textBad extracts data and wants it unreadable.
func textBad(t *testing.T, p *PDFs, data []byte) model.FoundryFileReadError {
	t.Helper()
	text, _, bad, err := p.Text(context.Background(), data)
	if err != nil || bad == nil {
		t.Fatalf("text = %q, err = %v; want unreadable", text, err)
	}
	return bad.Error
}

// flatePDF is a one-page PDF whose content stream is compressed to a few
// KiB and inflates to more than inflated bytes, past any small memory cap.
func flatePDF(t *testing.T, inflated int) []byte {
	t.Helper()
	var content bytes.Buffer
	w := zlib.NewWriter(&content)
	if _, err := w.Write([]byte("BT /F1 12 Tf 72 720 Td (x) Tj ET\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte(" "), inflated)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b := &pdfBuilder{}
	catalog := b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [4 0 R] /Count 1 >>")
	b.add(fmt.Sprintf("<< /Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream", content.Len(), content.String()))
	b.add("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
		"/Resources << /Font << /F1 << /Type /Font /Subtype /Type1 /BaseFont /Helvetica >> >> >> /Contents 3 0 R >>")
	return b.bytes(catalog, "")
}

func TestPDFText(t *testing.T) {
	// A born-digital PDF reads as its lines, one a line, in order.
	t.Run("reads a born-digital PDF's lines", func(t *testing.T) {
		text, pages := textOK(t, pdfs(nil), textPDF("", pdfPage(lines(72, "Invoice 1042", "Acme Corp (Berlin)", "Total: 10.50 EUR"))))
		if want := "Invoice 1042\nAcme Corp (Berlin)\nTotal: 10.50 EUR\n"; text != want || pages != 1 {
			t.Fatalf("text = %q over %d pages, want %q over 1", text, pages, want)
		}
	})

	// Two columns written one after the other read a column at a time, not
	// line by line across the page.
	t.Run("reads a column before the next", func(t *testing.T) {
		page := pdfPage(lines(72, "Left one", "Left two", "Left three") + lines(320, "Right one", "Right two", "Right three"))
		text, _ := textOK(t, pdfs(nil), textPDF("", page))
		if want := "Left one\nLeft two\nLeft three\nRight one\nRight two\nRight three\n"; text != want {
			t.Fatalf("text = %q, want %q", text, want)
		}
	})

	// Pages are split by one form feed each, an empty page included, so page
	// N is always the text after the (N-1)th form feed.
	t.Run("splits pages with form feeds", func(t *testing.T) {
		text, pages := textOK(t, pdfs(nil), textPDF("",
			pdfPage(lines(72, "Page one")), scanned, pdfPage(lines(72, "Page three", "", "", "after a gap"))))
		if want := "Page one\n\f\fPage three\nafter a gap\n"; text != want || pages != 3 {
			t.Fatalf("text = %q over %d pages, want %q over 3", text, pages, want)
		}
		if got := strings.Split(text, "\f"); len(got) != pages {
			t.Fatalf("%d pages split into %d", pages, len(got))
		}
	})

	// Text in WinAnsi (é, ï, €) and in a CID font with a ToUnicode map (東京)
	// both read as the characters they are, in UTF-8.
	t.Run("keeps multibyte text", func(t *testing.T) {
		page := pdfPage(lines(72, "Caf\xe9 na\xefve \x80 5") + "BT /F2 12 Tf 72 600 Td <000100020003> Tj ET\n")
		text, _ := textOK(t, pdfs(nil), textPDF("", page))
		if want := "Café naïve € 5\n東京🚀\n"; text != want || !utf8.ValidString(text) {
			t.Fatalf("text = %q, want %q", text, want)
		}
	})

	// A PDF that needs a password can't be read: it says to upload it
	// without one.
	t.Run("refuses an encrypted PDF", func(t *testing.T) {
		bad := textBad(t, pdfs(nil), textPDF(encrypted, pdfPage(lines(72, "secret"))))
		if bad.Code != model.FoundryFileReadUnsupported || !strings.Contains(bad.Message, "encrypted") {
			t.Fatalf("error = %+v", bad)
		}
	})

	// A damaged file, or one that is no PDF at all, is unsupported.
	t.Run("refuses a damaged file", func(t *testing.T) {
		whole := textPDF("", pdfPage(lines(72, "cut short")))
		for _, tc := range []struct {
			name string
			data []byte
		}{
			{"not a PDF", []byte("name,amount\nAcme,10\n")},
			{"a header and nothing else", []byte("%PDF-1.7\n")},
			{"garbage after the header", []byte("%PDF-1.7\n\x00\x01\x02 this is not a PDF body")},
			{"empty", nil},
			{"cut before its pages", whole[:40]},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if bad := textBad(t, pdfs(nil), tc.data); bad.Code != model.FoundryFileReadUnsupported {
					t.Fatalf("error = %+v", bad)
				}
			})
		}
	})

	// A PDF with no pages has nothing to read.
	t.Run("refuses a PDF with no pages", func(t *testing.T) {
		bad := textBad(t, pdfs(nil), textPDF(""))
		if bad.Code != model.FoundryFileReadUnsupported || !strings.Contains(bad.Message, "no pages") {
			t.Fatalf("error = %+v", bad)
		}
	})

	// A PDF with no text layer, every page an image, has nothing to read: it
	// says so, and stays a PDF a deep read could take.
	t.Run("refuses a scanned PDF", func(t *testing.T) {
		bad := textBad(t, pdfs(nil), textPDF("", scanned, scanned))
		if bad.Code != model.FoundryFileReadUnsupported || bad.Message != ScannedPDF {
			t.Fatalf("error = %+v", bad)
		}
	})

	// The page cap is checked before any page is read: 150 pages read, 151
	// are too large.
	t.Run("refuses more pages than the cap", func(t *testing.T) {
		pages := make([]pdfPage, 151)
		for i := range pages {
			pages[i] = pdfPage(lines(72, fmt.Sprintf("Page %d", i+1)))
		}
		if _, n := textOK(t, pdfs(nil), textPDF("", pages[:150]...)); n != 150 {
			t.Fatalf("%d pages", n)
		}
		bad := textBad(t, pdfs(nil), textPDF("", pages...))
		if bad.Code != model.FoundryFileReadTooLarge || !strings.Contains(bad.Message, "151 pages") {
			t.Fatalf("error = %+v", bad)
		}
	})

	// Text past what a load extracts of a document is too large to read,
	// however few its pages.
	t.Run("refuses text past the cap", func(t *testing.T) {
		page := make([]string, 40) // 100 KB a page
		for i := range page {
			page[i] = strings.Repeat("word ", 500)
		}
		pages := make([]pdfPage, 11)
		for i := range pages {
			pages[i] = pdfPage(lines(72, page...))
		}
		if text, _ := textOK(t, pdfs(nil), textPDF("", pages[:10]...)); len(text) > 1<<20 {
			t.Fatalf("read %d bytes, past the cap", len(text))
		}
		bad := textBad(t, pdfs(nil), textPDF("", pages...))
		if bad.Code != model.FoundryFileReadTooLarge || !strings.Contains(bad.Message, "1024 KiB") {
			t.Fatalf("error = %+v", bad)
		}
	})

	// PDFium gets only so much memory: a stream that inflates past it fails
	// that file, and the next file reads in a fresh instance.
	t.Run("refuses a file that needs more memory than the cap", func(t *testing.T) {
		small := pdfs(nil)
		small.PDFium = NewPDFium(32 << 20)
		if bad := textBad(t, small, flatePDF(t, 64<<20)); bad.Code != model.FoundryFileReadUnsupported {
			t.Fatalf("error = %+v", bad)
		}
		if text, _ := textOK(t, small, textPDF("", pdfPage(lines(72, "next")))); text != "next\n" {
			t.Fatalf("text = %q", text)
		}
	})

	// A read whose context has ended starts no PDFium at all, and stores
	// nothing: the worker gives it back or leaves it to lapse.
	t.Run("reads nothing once its context has ended", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, bad, err := pdfs(nil).Text(ctx, textPDF("", pdfPage(lines(72, "a")))); !errors.Is(err, context.Canceled) || bad != nil {
			t.Fatalf("bad = %+v, err = %v", bad, err)
		}
		s := newStore()
		doc := s.upload("a.pdf", model.FoundryMediaPDF, textPDF("", pdfPage(lines(72, "a"))))
		if outcome, err := pdfs(s).Read(ctx, doc); !errors.Is(err, context.Canceled) || s.puts != 0 {
			t.Fatalf("outcome = %+v, err = %v, %d puts", outcome, err, s.puts)
		}
	})

	// A read that runs out of time is killed where it is and is the
	// worker's failure, not the file's; the next read starts afresh.
	t.Run("stops at its deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, _, bad, err := pdfs(nil).Text(ctx, flatePDF(t, 300<<20))
		if !errors.Is(err, context.DeadlineExceeded) || bad != nil {
			t.Fatalf("bad = %+v, err = %v", bad, err)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("stopped after %s", took)
		}
		if text, _ := textOK(t, pdfs(nil), textPDF("", pdfPage(lines(72, "next")))); text != "next\n" {
			t.Fatalf("text = %q", text)
		}
	})
}

func TestNormalizePage(t *testing.T) {
	// What PDFium writes becomes plain lines: \n breaks, no controls but
	// those, no trailing spaces, and at most one blank line in a row.
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"CRLF and CR breaks", "one\r\ntwo\rthree", "one\ntwo\nthree\n"},
		{"tabs become spaces", "a\tb", "a b\n"},
		{"controls go, form feeds included", "a\x00b\x02c\x0bd\fe\u0085f\u007f", "abcdef\n"},
		{"trailing spaces go", "a  \nb \t", "a\nb\n"},
		{"blank lines collapse to one", "a\n\n\n\n b", "a\n\n b\n"},
		{"lines of spaces are blank", "a\n   \n \t \nb", "a\n\nb\n"},
		{"no blank line at the ends", "\n\n a\n\n\n", " a\n"},
		{"invalid UTF-8 goes", "caf\xc3\x28 \xff東", "caf( 東\n"},
		{"a page of nothing is empty", " \r\n\t\f\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizePage(tc.raw); got != tc.want {
				t.Fatalf("normalizePage(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// FuzzNormalizePage checks what any page text normalises to: valid UTF-8
// with no control but \n, never a form feed, every line ending in \n with
// no space before it, and no two blank lines in a row.
func FuzzNormalizePage(f *testing.F) {
	for _, seed := range []string{"", "a\r\nb", "\f\x00\t", "a\n\n\n\nb", "caf\xc3\x28", " \n \n x \n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got := normalizePage(raw)
		switch {
		case !utf8.ValidString(got):
			t.Fatalf("%q is not UTF-8", got)
		case strings.ContainsFunc(got, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }):
			t.Fatalf("%q holds a control character", got)
		case got != "" && !strings.HasSuffix(got, "\n"), strings.HasPrefix(got, "\n"), strings.Contains(got, "\n\n\n"):
			t.Fatalf("%q has stray blank lines", got)
		case strings.Contains(got, " \n"):
			t.Fatalf("%q has trailing spaces", got)
		}
	})
}

func TestPDFs(t *testing.T) {
	// A PDF's text is stored once, as UTF-8 plain text under a key holding
	// its SHA-256, and the read records that key, the hash and the pages;
	// reading it again stores nothing new and reads the same.
	t.Run("stores a PDF's text once", func(t *testing.T) {
		s := newStore()
		doc := s.upload("invoice.pdf", model.FoundryMediaPDF, textPDF("", pdfPage(lines(72, "Invoice 1042")), pdfPage(lines(72, "Terms"))))
		r := pdfs(s)
		outcome, err := r.Read(context.Background(), doc)
		if err != nil {
			t.Fatal(err)
		}
		read, ok := outcome.(model.FoundryPDFRead)
		if !ok {
			t.Fatalf("outcome = %+v, want a PDF read", outcome)
		}
		const text = "Invoice 1042\n\fTerms\n"
		sum := sha256.Sum256([]byte(text))
		want := model.FoundryPDFRead{
			ArtifactKey: document.TextKey("ws-1", "doc-1", model.FoundryFileReadStandard, hex.EncodeToString(sum[:])),
			Pages:       2, TextSHA256: hex.EncodeToString(sum[:]),
		}
		if read != want {
			t.Fatalf("read = %+v, want %+v", read, want)
		}
		if got := string(s.objects[read.ArtifactKey]); got != text || s.types[read.ArtifactKey] != "text/plain; charset=utf-8" {
			t.Fatalf("stored %q as %q", got, s.types[read.ArtifactKey])
		}
		again, err := r.Read(context.Background(), doc)
		if err != nil || again != outcome || len(s.objects) != 2 {
			t.Fatalf("read again %+v (%v), %d objects", again, err, len(s.objects))
		}
	})

	// A file whose bytes are gone (deleted under the worker) can't be read;
	// a store that fails, or holds other bytes than the upload's, is a
	// failure to retry, not the file's.
	t.Run("tells a missing file from a failing store", func(t *testing.T) {
		s := newStore()
		doc := s.upload("a.pdf", model.FoundryMediaPDF, textPDF("", pdfPage(lines(72, "a"))))
		delete(s.objects, doc.S3Key)
		outcome, err := pdfs(s).Read(context.Background(), doc)
		if bad, ok := outcome.(model.FoundryFileUnreadable); err != nil || !ok || bad.Error.Code != model.FoundryFileReadUnsupported {
			t.Fatalf("outcome = %+v, err = %v", outcome, err)
		}
		s = newStore()
		s.putErr = errors.New("s3 is down")
		doc = s.upload("a.pdf", model.FoundryMediaPDF, textPDF("", pdfPage(lines(72, "a"))))
		if outcome, err := pdfs(s).Read(context.Background(), doc); err == nil {
			t.Fatalf("outcome = %+v with no error", outcome)
		}
		s = newStore()
		doc = s.upload("a.pdf", model.FoundryMediaPDF, textPDF("", pdfPage(lines(72, "a"))))
		doc.SizeBytes++
		if outcome, err := pdfs(s).Read(context.Background(), doc); err == nil {
			t.Fatalf("outcome = %+v with no error", outcome)
		}
	})

	// An unreadable PDF stores nothing.
	t.Run("stores nothing of a PDF it can't read", func(t *testing.T) {
		s := newStore()
		doc := s.upload("scan.pdf", model.FoundryMediaPDF, textPDF("", scanned))
		outcome, err := pdfs(s).Read(context.Background(), doc)
		if bad, ok := outcome.(model.FoundryFileUnreadable); err != nil || !ok || bad.Error.Message != ScannedPDF {
			t.Fatalf("outcome = %+v, err = %v", outcome, err)
		}
		if s.puts != 0 {
			t.Fatalf("%d puts", s.puts)
		}
	})

	// The worker hands each reader its own kind only.
	t.Run("reads no other kind", func(t *testing.T) {
		s := newStore()
		if _, err := pdfs(s).Read(context.Background(), s.upload("a.csv", model.FoundryMediaCSV, []byte("id\n1\n"))); err == nil {
			t.Fatal("read a CSV as a PDF")
		}
	})
}
