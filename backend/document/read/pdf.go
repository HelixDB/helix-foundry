package read

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"

	"github.com/klippa-app/go-pdfium"
	pdfiumerrors "github.com/klippa-app/go-pdfium/errors"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// ScannedPDF is the message of a PDF with no text layer: every page is
// empty, as a scan's are. It stays stored and viewable; a deep read (OCR)
// is what would read it.
const ScannedPDF = "Scanned PDF: no text to read"

// PDFMemoryLimit is the memory PDFium may take for one PDF in the document
// worker's pod, which has 3 GiB and also runs the sync worker.
const PDFMemoryLimit = 512 << 20

// PDFium is PDFium compiled to WebAssembly (go-pdfium) and run by wazero,
// in-process and with no CGO. It is compiled once, on the first PDF (a
// second or two), and each PDF gets an instance of its own, with fresh
// memory, thrown away after. An instance is sandboxed: it sees no file
// system and writes nowhere, its memory is capped at maxMemory, and it is
// killed when its read's context ends. One PDF is read at a time.
type PDFium struct {
	maxMemory int64

	once sync.Once
	pool pdfium.Pool
	err  error
}

// NewPDFium is PDFium whose instances each take at most maxMemoryBytes, a
// multiple of the 64 KiB WebAssembly page; a PDF that needs more is
// unsupported. Nothing is compiled until the first PDF.
func NewPDFium(maxMemoryBytes int64) *PDFium {
	return &PDFium{maxMemory: maxMemoryBytes}
}

// instance is a fresh PDFium instance, made when ctx allows.
func (e *PDFium) instance(ctx context.Context) (pdfium.Pdfium, error) {
	e.once.Do(func() {
		e.pool, e.err = webassembly.Init(webassembly.Config{
			MaxIdle: 1, MaxTotal: 1,
			FSConfig: wazero.NewFSConfig(),
			RuntimeConfig: wazero.NewRuntimeConfig().
				WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling).
				WithMemoryLimitPages(uint32(e.maxMemory >> 16)).
				WithCloseOnContextDone(true),
			Stdout: io.Discard, Stderr: io.Discard,
		})
	})
	if e.err != nil {
		return nil, fmt.Errorf("read: compile PDFium: %w", e.err)
	}
	instance, err := e.pool.GetInstanceWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read: start PDFium: %w", err)
	}
	return instance, nil
}

// PDFLimits bound reading one PDF.
type PDFLimits struct {
	// MaxPages refuses a longer PDF as too_large before any page is read
	// (foundry.documents.worker.max_pages).
	MaxPages int
	// MaxTextBytes refuses a PDF whose text is longer as too_large: it is
	// what a load extracts of a document (foundry.documents.max_bytes).
	MaxTextBytes int64
}

// PDFs reads PDFs (model.FoundryReadPDF) to their text, with PDFium.
type PDFs struct {
	Store  Store
	PDFium *PDFium
	Limits PDFLimits
}

// Read reads a PDF and stores its text once:
//  1. Download its original whole, at most the size its upload declared.
//  2. Extract its text (Text).
//  3. Store the text as UTF-8 plain text at document.TextKey, which holds
//     the text's SHA-256: a retry that read other text (under another
//     PDFium) writes an object of its own, never one another read records.
//
// The outcome is model.FoundryPDFRead, or model.FoundryFileUnreadable for a
// file that is not a readable PDF (Text). A file whose original is gone is
// unsupported.
func (p *PDFs) Read(ctx context.Context, doc *model.FoundryDocument) (model.FoundryFileReadOutcome, error) {
	if doc.MediaType.ReadKind() != model.FoundryReadPDF {
		return nil, fmt.Errorf("read: %s is not a PDF but %s", doc.ID, doc.MediaType)
	}
	body, err := p.Store.Open(ctx, doc.S3Key)
	if errors.Is(err, document.ErrMissing) {
		return unreadable(model.FoundryFileReadUnsupported, "the file's bytes are gone"), nil
	}
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(body, doc.SizeBytes+1))
	_ = body.Close() // best-effort: only read
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != doc.SizeBytes {
		return nil, fmt.Errorf("read: document %s holds %d bytes, not the %d uploaded", doc.ID, len(data), doc.SizeBytes)
	}
	text, pages, bad, err := p.Text(ctx, data)
	if err != nil {
		return nil, err
	}
	if bad != nil {
		return *bad, nil
	}
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:])
	key := document.TextKey(doc.WorkspaceID, doc.ID, model.FoundryFileReadStandard, digest)
	if err := p.Store.PutOnce(ctx, key, "text/plain; charset=utf-8", strings.NewReader(text)); err != nil {
		return nil, err
	}
	return model.FoundryPDFRead{ArtifactKey: key, Pages: pages, TextSHA256: digest}, nil
}

// Text is a PDF's text and its page count. The text is each page's, in the
// order PDFium reads the page's content, normalised (normalizePage), with a
// form feed (\f) between pages: page N is the text after the (N-1)th form
// feed, and a page with no text is empty. A PDF that can't be read is
// model.FoundryFileUnreadable:
//   - damaged, not a PDF, or needing a password: unsupported;
//   - of no pages: unsupported;
//   - of more than Limits.MaxPages pages: too_large, before any page is read;
//   - whose text passes Limits.MaxTextBytes: too_large;
//   - with no text on any page (a scan): unsupported, ScannedPDF;
//   - that PDFium fails on part way (past its memory, say): unsupported.
//
// An error is not the file's: PDFium could not start, or ctx ended, which
// kills the read where it is.
func (p *PDFs) Text(ctx context.Context, data []byte) (text string, pages int, bad *model.FoundryFileUnreadable, err error) {
	// The pool hands out an idle instance whatever ctx, and AfterFunc kills
	// it in a goroutine of its own, which a short read can outrun: an ended
	// ctx starts no PDFium at all.
	if err := ctx.Err(); err != nil {
		return "", 0, nil, err
	}
	instance, err := p.PDFium.instance(ctx)
	if err != nil {
		return "", 0, nil, err
	}
	// Killing the instance interrupts PDFium mid-call; closing it otherwise
	// throws it away all the same, since instances are never reused.
	stop := context.AfterFunc(ctx, func() { _ = instance.Kill() })
	defer func() {
		if stop() {
			_ = instance.Close() // best-effort: the instance is dropped either way
		}
	}()
	fail := func(code model.FoundryFileReadErrorCode, format string, args ...any) (string, int, *model.FoundryFileUnreadable, error) {
		if ctx.Err() != nil {
			return "", 0, nil, ctx.Err()
		}
		bad := unreadable(code, format, args...)
		return "", 0, &bad, nil
	}
	opened, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
	switch {
	case errors.Is(err, pdfiumerrors.ErrPassword), errors.Is(err, pdfiumerrors.ErrSecurity):
		return fail(model.FoundryFileReadUnsupported, "the PDF is encrypted; upload it without a password")
	case err != nil:
		return fail(model.FoundryFileReadUnsupported, "the file is damaged or is not a PDF")
	}
	count, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: opened.Document})
	switch {
	case err != nil:
		return fail(model.FoundryFileReadUnsupported, "the PDF's pages could not be read; it may be damaged")
	case count.PageCount == 0:
		return fail(model.FoundryFileReadUnsupported, "the PDF has no pages")
	case count.PageCount > p.Limits.MaxPages:
		return fail(model.FoundryFileReadTooLarge, "the PDF has %d pages; Helix reads PDFs of up to %d", count.PageCount, p.Limits.MaxPages)
	}
	// A page's text is read whole with FPDFText_GetText, which decodes
	// UTF-16: go-pdfium's GetPageText reads it a code unit at a time, so a
	// character outside the BMP (an emoji, rarer CJK) would come out as two
	// U+FFFD.
	pageText := func(i int) (string, error) {
		textPage, err := instance.FPDFText_LoadPage(&requests.FPDFText_LoadPage{
			Page: requests.Page{ByIndex: &requests.PageByIndex{Document: opened.Document, Index: i}}})
		if err != nil {
			return "", err
		}
		defer func() { _, _ = instance.FPDFText_ClosePage(&requests.FPDFText_ClosePage{TextPage: textPage.TextPage}) }()
		chars, err := instance.FPDFText_CountChars(&requests.FPDFText_CountChars{TextPage: textPage.TextPage})
		if err != nil {
			return "", err
		}
		text, err := instance.FPDFText_GetText(&requests.FPDFText_GetText{TextPage: textPage.TextPage, Count: chars.Count})
		if err != nil {
			return "", err
		}
		return text.Text, nil
	}
	var out strings.Builder
	hasText := false
	for i := range count.PageCount {
		page, err := pageText(i)
		if err != nil {
			return fail(model.FoundryFileReadUnsupported, "page %d of the PDF could not be read: it is damaged or too complex", i+1)
		}
		if i > 0 {
			out.WriteByte('\f')
		}
		normalized := normalizePage(page)
		hasText = hasText || normalized != ""
		out.WriteString(normalized)
		if int64(out.Len()) > p.Limits.MaxTextBytes {
			return fail(model.FoundryFileReadTooLarge, "the PDF's text is more than %d KiB, which is as much of a document as Helix reads",
				p.Limits.MaxTextBytes>>10)
		}
	}
	if !hasText {
		return fail(model.FoundryFileReadUnsupported, ScannedPDF)
	}
	return out.String(), count.PageCount, nil, nil
}

// normalizePage is a page's text as it is stored: valid UTF-8; line breaks
// as \n (PDFium writes \r\n); a tab as a space; no other control character,
// \f included, so form feeds only ever separate pages; no space at the end
// of a line; no blank line at its start or end, and none twice in a row
// inside. A page with text ends with \n; one with none is "".
func normalizePage(raw string) string {
	raw = strings.ToValidUTF8(raw, "")
	raw = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\t", " ").Replace(raw)
	raw = strings.Map(func(r rune) rune {
		if r != '\n' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, raw)
	var out strings.Builder
	blank := false
	for line := range strings.Lines(raw) {
		line = strings.TrimRightFunc(line, unicode.IsSpace)
		if line == "" {
			blank = out.Len() > 0
			continue
		}
		if blank {
			out.WriteByte('\n')
			blank = false
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}
