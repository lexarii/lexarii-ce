// SPDX-License-Identifier: AGPL-3.0-or-later

package pdf

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// buildPDF assembles a minimal but genuinely valid PDF from a list of object
// bodies, computing the cross-reference table offsets as it goes.
//
// Objects are numbered from 1 in the order given, so object N is objects[N-1]
// and references between them can be written as "N 0 R".
func buildPDF(t *testing.T, objects []string, rootObj int) []byte {
	t.Helper()

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	// A binary comment marks the file as containing binary data. Real producers
	// emit it and some readers sniff for it.
	buf.Write([]byte{'%', 0xE2, 0xE3, 0xCF, 0xD3, '\n'})

	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}

	xrefOffset := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	// Entry 0 is always the head of the free list.
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}

	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, rootObj, xrefOffset)

	return buf.Bytes()
}

// stream builds a content stream object with a correct /Length.
func stream(content string) string {
	return fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)
}

// textContent draws lines of text on a page, one per line, top to bottom.
func textContent(lines ...string) string {
	var b strings.Builder
	b.WriteString("BT\n/F1 12 Tf\n")
	y := 720
	for _, line := range lines {
		// Escape the characters that are special inside a PDF literal string.
		escaped := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`).Replace(line)
		fmt.Fprintf(&b, "1 0 0 1 72 %d Tm\n(%s) Tj\n", y, escaped)
		y -= 16
	}
	b.WriteString("ET\n")
	return b.String()
}

// newTextPDF builds a PDF whose pages carry the given lines of real text.
func newTextPDF(t *testing.T, pages [][]string, info string) []byte {
	t.Helper()

	// Object layout: 1 catalog, 2 pages tree, then per page a page object and a
	// contents stream, then the font, then optionally the info dictionary.
	firstPageObj := 3
	kids := make([]string, 0, len(pages))
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", firstPageObj+i*2))
	}
	fontObj := firstPageObj + len(pages)*2

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)),
	}
	for i, lines := range pages {
		contentsObj := firstPageObj + i*2 + 1
		objects = append(objects,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
				"/Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>", fontObj, contentsObj),
			stream(textContent(lines...)),
		)
	}
	objects = append(objects, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")

	if info != "" {
		objects = append(objects, info)
	}

	// The trailer is written last and carries byte offsets, so /Info has to be
	// baked in rather than patched into finished bytes.
	if info != "" {
		return rebuildWithInfo(t, objects, len(objects))
	}
	return buildPDF(t, objects, 1)
}

// rebuildWithInfo is buildPDF with an /Info entry in the trailer.
func rebuildWithInfo(t *testing.T, objects []string, infoObj int) []byte {
	t.Helper()

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	buf.Write([]byte{'%', 0xE2, 0xE3, 0xCF, 0xD3, '\n'})

	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}

	xrefOffset := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, infoObj, xrefOffset)

	return buf.Bytes()
}

// newScannedPDF builds a PDF with pages but effectively no text layer, which is
// what a scan looks like.
func newScannedPDF(t *testing.T, pageCount int) []byte {
	t.Helper()

	pages := make([][]string, pageCount)
	for i := range pages {
		// A scan is not always literally empty: a watermark or a failed OCR pass
		// leaves a few stray characters behind.
		pages[i] = []string{"."}
	}
	return newTextPDF(t, pages, "")
}
