package read

import (
	"bytes"
	"fmt"
	"strings"
)

// pdf_fixtures_test.go writes the small PDFs the extraction goldens read,
// object by object, so each one is exactly what its test says it is.

// pdfBuilder collects a PDF's objects; object n is objects[n-1].
type pdfBuilder struct {
	objects []string
}

// add appends an object and returns its number.
func (b *pdfBuilder) add(object string) int {
	b.objects = append(b.objects, object)
	return len(b.objects)
}

// stream is a stream object of data with the given dictionary entries.
func stream(entries string, data string) string {
	return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", entries, len(data), data)
}

// bytes writes the PDF whose catalog is object root, with trailer entries
// extra beside /Size and /Root, and a cross-reference table.
func (b *pdfBuilder) bytes(root int, extra string) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(b.objects))
	for i, object := range b.objects {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(b.objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root %d 0 R %s >>\nstartxref\n%d\n%%%%EOF\n", len(b.objects)+1, root, extra, xref)
	return out.Bytes()
}

// pdfPage is one page: its content stream, drawn with /F1 (Helvetica,
// WinAnsi), /F2 (a CID font whose ToUnicode maps <0001> to 東, <0002> to
// 京 and <0003> to 🚀, outside the BMP) and /Im1 (a one-pixel grey image).
type pdfPage string

// textPDF is a PDF of pages, each a content stream that may use the fonts
// and image pdfPage names. trailer adds trailer entries.
func textPDF(trailer string, pages ...pdfPage) []byte {
	b := &pdfBuilder{}
	catalog := b.add("") // filled once the page tree's number is known
	tree := b.add("")
	helvetica := b.add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	toUnicode := b.add(stream("", strings.Join([]string{
		"/CIDInit /ProcSet findresource begin", "12 dict begin", "begincmap",
		"/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def",
		"/CMapName /Adobe-Identity-UCS def", "/CMapType 2 def",
		"1 begincodespacerange", "<0000> <FFFF>", "endcodespacerange",
		"3 beginbfchar", "<0001> <6771>", "<0002> <4EAC>", "<0003> <D83DDE80>", "endbfchar",
		"endcmap", "CMapName currentdict /CMap defineresource pop", "end", "end",
	}, "\n")))
	descriptor := b.add("<< /Type /FontDescriptor /FontName /HelixCJK /Flags 4 /FontBBox [0 0 1000 1000] /ItalicAngle 0 " +
		"/Ascent 1000 /Descent 0 /CapHeight 1000 /StemV 80 >>")
	cid := b.add(fmt.Sprintf("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /HelixCJK "+
		"/CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /FontDescriptor %d 0 R /DW 1000 >>", descriptor))
	cjk := b.add(fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /HelixCJK /Encoding /Identity-H "+
		"/DescendantFonts [%d 0 R] /ToUnicode %d 0 R >>", cid, toUnicode))
	image := b.add(stream("/Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8", "\x80"))
	resources := fmt.Sprintf("<< /Font << /F1 %d 0 R /F2 %d 0 R >> /XObject << /Im1 %d 0 R >> >>", helvetica, cjk, image)
	var kids []string
	for _, content := range pages {
		contents := b.add(stream("", string(content)))
		page := b.add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Resources %s /Contents %d 0 R >>",
			tree, resources, contents))
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
	}
	b.objects[catalog-1] = fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", tree)
	b.objects[tree-1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids))
	return b.bytes(catalog, trailer)
}

// lines is a page of Helvetica lines down from the top left at x, one per
// line; each line is WinAnsi bytes, escaped for a PDF string.
func lines(x int, text ...string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "BT /F1 12 Tf 16 TL %d 720 Td\n", x)
	for _, line := range text {
		escaped := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`).Replace(line)
		fmt.Fprintf(&out, "(%s) Tj T*\n", escaped)
	}
	out.WriteString("ET\n")
	return out.String()
}
