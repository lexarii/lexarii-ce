// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pdf extracts text and metadata from PDF files.
//
// PDFs in a real library fall into two very different groups: documents with a
// text layer, which extract cleanly, and scans, which are page images with no
// text at all. Ingesting the second group as empty books is the failure mode
// worth designing against - an empty book looks ingested, searches find
// nothing, and the user has no idea why. Scans are detected and marked
// needs_ocr instead.
package pdf

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/ledongthuc/pdf"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// ErrNotPDF reports a file that cannot be read as a PDF.
var ErrNotPDF = errors.New("not a readable pdf")

// ErrEncrypted reports a password-protected PDF. This is a legitimate state to
// report to the user, not a parse failure to hide.
var ErrEncrypted = errors.New("pdf is encrypted")

// Scanned-PDF detection thresholds.
//
// A text-layer PDF averages hundreds to thousands of characters per page. A
// scan averages zero, or a handful of stray characters from a watermark or an
// OCR attempt that produced nothing usable.
const (
	// MinCharsPerPage is the average below which a document is treated as a scan.
	MinCharsPerPage = 32
	// MinTotalChars guards very short documents, where an average is meaningless.
	MinTotalChars = 200
)

// MaxPages caps extraction. A malformed PDF can claim an absurd page count, and
// a scan of a library should not stall for minutes on one hostile file.
const MaxPages = 20_000

// Parse reads a PDF and returns its text, structure, and claimed metadata.
//
// Extraction is deterministic: the same bytes always produce the same page text
// and therefore the same locators.
func Parse(r io.ReaderAt, size int64) (res *ingest.Result, err error) {
	// The PDF parser indexes into slices derived from file contents and panics
	// on some malformed input. A bad file must fail against itself, not take
	// down the worker running the scan.
	defer func() {
		if v := recover(); v != nil {
			res, err = nil, fmt.Errorf("%w: parser panicked: %v", ErrNotPDF, v)
		}
	}()

	reader, err := pdf.NewReader(r, size)
	if err != nil {
		if isEncryptedErr(err) {
			return nil, fmt.Errorf("%w: %v", ErrEncrypted, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrNotPDF, err)
	}

	pageCount := reader.NumPage()
	if pageCount <= 0 {
		return nil, fmt.Errorf("%w: document reports %d pages", ErrNotPDF, pageCount)
	}

	out := &ingest.Result{Format: domain.FormatPDF}
	if pageCount > MaxPages {
		out.AddWarning("document claims %d pages; only the first %d were read", pageCount, MaxPages)
		pageCount = MaxPages
	}

	out.Spine = make([]ingest.SpineItem, 0, pageCount)
	out.UnitText = make([]string, 0, pageCount)

	totalChars := 0
	failedPages := 0

	for i := 1; i <= pageCount; i++ {
		text, perr := extractPage(reader, i)
		if perr != nil {
			// One unreadable page must not cost the reader the other 400. The
			// page keeps its slot so every later locator stays correct.
			failedPages++
			text = ""
		}

		out.Spine = append(out.Spine, ingest.SpineItem{
			Index:     i - 1,
			MediaType: "application/pdf",
			Linear:    true,
		})
		out.UnitText = append(out.UnitText, text)
		totalChars += countMeaningful(text)
	}

	if failedPages > 0 {
		out.AddWarning("%d of %d pages could not be extracted", failedPages, pageCount)
	}

	out.NeedsOCR = looksScanned(totalChars, pageCount)
	if out.NeedsOCR {
		out.AddWarning(
			"only %d characters extracted across %d pages: this looks like a scan and needs OCR before it can be searched",
			totalChars, pageCount)
	}

	out.Metadata = readMetadata(reader, out)
	out.TOC = readOutline(reader, pageCount, out)

	return out, nil
}

// extractPage returns the plain text of one page, in reading order.
func extractPage(reader *pdf.Reader, num int) (text string, err error) {
	// Per-page recovery as well as per-document: one malformed page is common in
	// otherwise fine files, and it should cost that page and nothing more.
	defer func() {
		if v := recover(); v != nil {
			text, err = "", fmt.Errorf("page %d panicked: %v", num, v)
		}
	}()

	page := reader.Page(num)
	if page.V.IsNull() {
		return "", fmt.Errorf("page %d is null", num)
	}

	rows, err := page.GetTextByRow()
	if err != nil {
		return "", fmt.Errorf("page %d: %w", num, err)
	}

	var b strings.Builder
	for _, row := range rows {
		for _, word := range row.Content {
			b.WriteString(word.S)
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// looksScanned reports whether a document has pages but effectively no text.
//
// The per-page average is the real signal, and the gap it has to straddle is
// enormous: a text-layer page carries one to three thousand characters, a
// scanned page carries zero or a few strays from a watermark. The absolute
// floor applies only to single-page documents, where the average and the total
// are the same number and a short-but-real document would otherwise be
// misjudged.
func looksScanned(totalChars, pageCount int) bool {
	if pageCount <= 0 {
		return false
	}
	if totalChars == 0 {
		return true
	}
	if pageCount == 1 {
		return totalChars < MinTotalChars
	}
	return totalChars/pageCount < MinCharsPerPage
}

// countMeaningful counts characters that are actual content. Whitespace is
// excluded so a page of blank lines does not read as a text layer.
func countMeaningful(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// readMetadata reads the document information dictionary.
//
// PDF metadata is famously unreliable: Title is routinely the LaTeX job name,
// the source filename, or empty. It is reported exactly as claimed, and never
// repaired or guessed at here - a bad value recorded as embedded metadata would
// outrank the truth forever.
func readMetadata(reader *pdf.Reader, res *ingest.Result) (m ingest.Metadata) {
	defer func() {
		if v := recover(); v != nil {
			res.AddWarning("document metadata could not be read: %v", v)
		}
	}()

	trailer := reader.Trailer()
	if trailer.IsNull() {
		return m
	}
	info := trailer.Key("Info")
	if info.IsNull() {
		return m
	}

	if title := cleanText(info.Key("Title")); title != "" {
		// An implausible title is rejected, not recorded. Because embedded
		// metadata outranks everything later, enshrining "Algebra_P1_Book.indb"
		// as a publisher fact would make it permanent.
		if ingest.PlausibleTitle(title) {
			m.Title = title
			m.SetProvenance("title", domain.SourceEmbedded)
		} else {
			res.AddWarning("ignoring implausible embedded title %q; it looks like a source filename or producer artefact", title)
		}
	}
	if author := cleanText(info.Key("Author")); author != "" {
		if ingest.PlausibleAuthor(author) {
			// A single Author string often holds several names separated by "/"
			// or ";". Those are unambiguous; commas are not, so they are left
			// alone.
			for _, name := range ingest.SplitContributors(author) {
				if ingest.PlausibleAuthor(name) {
					m.Authors = append(m.Authors, ingest.Contributor{Name: name, Role: domain.RoleAuthor})
				}
			}
			if len(m.Authors) > 0 {
				m.SetProvenance("authors", domain.SourceEmbedded)
			}
		} else {
			res.AddWarning("ignoring implausible embedded author %q", author)
		}
	}
	if subject := cleanText(info.Key("Subject")); subject != "" {
		m.Description = subject
		m.SetProvenance("description", domain.SourceEmbedded)
	}
	if keywords := cleanText(info.Key("Keywords")); keywords != "" {
		for _, kw := range strings.FieldsFunc(keywords, func(r rune) bool {
			return r == ',' || r == ';'
		}) {
			if kw = strings.TrimSpace(kw); kw != "" {
				m.Subjects = append(m.Subjects, kw)
			}
		}
		if len(m.Subjects) > 0 {
			m.SetProvenance("subjects", domain.SourceEmbedded)
		}
	}
	// CreationDate is a PDF date string (D:YYYYMMDDHHmmSS). Kept as claimed;
	// it is the date the file was produced, which is often not the publication
	// date, so normalising it would invent precision the file never had.
	if created := cleanText(info.Key("CreationDate")); created != "" {
		m.PublishedOn = created
		m.SetProvenance("publishedOn", domain.SourceEmbedded)
	}

	return m
}

// Outline reads only the document outline, without extracting any text.
//
// The reader needs a contents list every time a PDF is opened, and a full Parse
// of a 900-page book is seconds of text extraction that would be thrown away.
// The file stays the source of truth - the same arrangement as EPUB, whose TOC
// is also read from the book rather than copied into the database.
func Outline(r io.ReaderAt, size int64) (entries []ingest.TOCEntry, err error) {
	defer func() {
		if v := recover(); v != nil {
			entries, err = nil, fmt.Errorf("%w: parser panicked: %v", ErrNotPDF, v)
		}
	}()

	reader, err := pdf.NewReader(r, size)
	if err != nil {
		if isEncryptedErr(err) {
			return nil, fmt.Errorf("%w: %v", ErrEncrypted, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrNotPDF, err)
	}

	// Warnings are discarded here: a missing outline is the ordinary case and
	// the caller only wants the entries.
	var discard ingest.Result
	return readOutline(reader, reader.NumPage(), &discard), nil
}

// Sample reads the opening text of a PDF, stopping once it has enough.
//
// Enrichment sends a model a sample, never a book: a 1,100-page reference work
// does not fit in any context window worth paying for, and the answer is in the
// title page, the contents, and the introduction anyway. Extracting only what
// is needed also keeps enrichment from repeating the whole ingest.
func Sample(r io.ReaderAt, size int64, maxChars int) (text string, err error) {
	defer func() {
		if v := recover(); v != nil {
			text, err = "", fmt.Errorf("%w: parser panicked: %v", ErrNotPDF, v)
		}
	}()

	if maxChars <= 0 {
		return "", nil
	}

	reader, err := pdf.NewReader(r, size)
	if err != nil {
		if isEncryptedErr(err) {
			return "", fmt.Errorf("%w: %v", ErrEncrypted, err)
		}
		return "", fmt.Errorf("%w: %v", ErrNotPDF, err)
	}

	pageCount := reader.NumPage()
	if pageCount > MaxPages {
		pageCount = MaxPages
	}

	// Two slices, not one continuous read from page one.
	//
	// The front of a book is a title page, a copyright page, and very often a
	// contents list that runs for pages. Sixteen thousand characters of that
	// tells a model what the chapters are CALLED and nothing about what the book
	// is - and gemma4-12b, asked to describe a contents list, tries to
	// enumerate it and overruns its token budget. So: the opening, which is
	// where the title lives, and then a slice of actual prose from far enough in
	// to be past the apparatus.
	head := readPages(reader, 1, pageCount, maxChars/4)

	bodyStart := pageCount / 5
	if bodyStart < 2 {
		bodyStart = 2
	}
	body := readPages(reader, bodyStart, pageCount, maxChars-len(head))

	var b strings.Builder
	b.WriteString(head)
	if head != "" && body != "" {
		b.WriteString("\n\n")
	}
	b.WriteString(body)

	return truncateSample(b.String(), maxChars), nil
}

// readPages accumulates text from a run of pages, up to a character budget.
func readPages(reader *pdf.Reader, from, pageCount, budget int) string {
	if budget <= 0 || from > pageCount {
		return ""
	}

	var b strings.Builder
	read := 0
	for i := from; i <= pageCount && b.Len() < budget; i++ {
		// A page that will not extract is skipped rather than failing the
		// sample: a scanned title page in front of readable text is common, and
		// it is the readable text that answers the question.
		page, perr := extractPage(reader, i)
		if perr != nil {
			continue
		}
		page = strings.TrimSpace(page)
		if page == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(page)

		// A hard stop on pages as well as characters, so a document of ten
		// thousand nearly-empty pages cannot turn a sample into a full read.
		read++
		if read >= maxSamplePages {
			break
		}
	}
	return truncateSample(b.String(), budget)
}

// maxSamplePages bounds how far into a book the sampler will look for text.
const maxSamplePages = 40

// truncateSample cuts to a rune boundary, preferring a paragraph or sentence
// end so the model is not handed half a word.
func truncateSample(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	cut := maxChars
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	if idx := strings.LastIndexAny(s[:cut], ".\n"); idx > maxChars/2 {
		cut = idx + 1
	}
	return strings.TrimSpace(s[:cut])
}

// readOutline converts the PDF outline (bookmarks) into a TOC.
func readOutline(reader *pdf.Reader, pageCount int, res *ingest.Result) (entries []ingest.TOCEntry) {
	defer func() {
		if v := recover(); v != nil {
			res.AddWarning("document outline could not be read: %v", v)
			entries = nil
		}
	}()

	root := reader.Trailer().Key("Root")
	outlines := root.Key("Outlines")
	if outlines.Kind() != pdf.Dict {
		res.AddWarning("no outline found")
		return nil
	}

	// The outline is walked here rather than through reader.Outline(), which
	// keeps only titles and children and drops the destination - and a contents
	// list that cannot take you anywhere is decoration.
	dests := newDestinations(reader, root, pageCount)
	entries = walkOutline(outlines.Key("First"), dests, 0)
	if len(entries) == 0 {
		res.AddWarning("no outline found")
		return nil
	}

	if unresolved := countUnresolved(entries); unresolved > 0 {
		res.AddWarning("%d of %d outline entries do not resolve to a page",
			unresolved, countEntries(entries))
	}
	return entries
}

// maxOutlineDepth bounds the walk.
//
// The outline is a linked structure from an untrusted file, and a /First or
// /Next that points back at an ancestor is a cycle that would otherwise be
// walked until the process died.
const maxOutlineDepth = 32

// maxOutlineEntries bounds the total size of a TOC, for the same reason.
const maxOutlineEntries = 10000

func walkOutline(first pdf.Value, dests *destinations, depth int) []ingest.TOCEntry {
	if depth >= maxOutlineDepth {
		return nil
	}

	var out []ingest.TOCEntry
	seen := make(map[string]bool)

	for item := first; item.Kind() == pdf.Dict; item = item.Key("Next") {
		// A /Next chain that revisits an entry is a cycle. The printed object
		// reference is what identifies an entry here, since the library does not
		// expose object pointers.
		key := item.String()
		if seen[key] {
			break
		}
		seen[key] = true

		if len(out) >= maxOutlineEntries {
			break
		}

		title := strings.TrimSpace(item.Key("Title").Text())
		children := walkOutline(item.Key("First"), dests, depth+1)
		if title == "" && len(children) == 0 {
			continue
		}

		// SpineIndex carries the PAGE for a PDF, which is what `major` means for
		// this format everywhere else. Unresolved stays -1: an entry pointing at
		// the wrong page is worse than one pointing nowhere, because the reader
		// trusts it.
		page := -1
		if resolved, ok := dests.page(item); ok {
			page = resolved
		}

		out = append(out, ingest.TOCEntry{
			Title:      title,
			SpineIndex: page,
			Children:   children,
		})
	}
	return out
}

func countEntries(entries []ingest.TOCEntry) int {
	n := 0
	for _, e := range entries {
		n += 1 + countEntries(e.Children)
	}
	return n
}

func countUnresolved(entries []ingest.TOCEntry) int {
	n := 0
	for _, e := range entries {
		if e.SpineIndex < 0 {
			n++
		}
		n += countUnresolved(e.Children)
	}
	return n
}

// destinations resolves an outline entry to a page number.
//
// A destination names a page by object reference, and this library resolves
// references before handing them over, so there is no object id left to compare.
// Pages are therefore identified by their printed dictionary, which contains
// each page's own /Contents and /Resources references and is distinct in any
// real document.
type destinations struct {
	reader    *pdf.Reader
	root      pdf.Value
	pageCount int
	indexed   bool
	byPage    map[string]int
	// ambiguous marks dictionaries shared by more than one page - two identical
	// blank pages, say. Those resolve to nothing rather than to the first match.
	ambiguous map[string]bool

	// named is the flattened name tree, built only when a descent fails.
	named       map[string]pdf.Value
	namesLoaded bool
}

func newDestinations(reader *pdf.Reader, root pdf.Value, pageCount int) *destinations {
	return &destinations{
		reader:    reader,
		root:      root,
		pageCount: pageCount,
		byPage:    make(map[string]int),
		ambiguous: make(map[string]bool),
	}
}

// index builds the page lookup on first use, so a document whose outline never
// names a page - or has no outline at all - never pays for the walk.
//
// The page tree is walked ONCE rather than calling Page(i) for each page, which
// re-descends from the root every time. On a 1,156-page reference book that
// difference was eight seconds against a tenth of one, and this runs every time
// such a book is opened.
func (d *destinations) index() {
	if d.indexed || d.pageCount == 0 {
		return
	}
	d.indexed = true

	page := 0
	d.walkPages(d.reader.Trailer().Key("Root").Key("Pages"), &page, 0)
}

// maxPageTreeDepth bounds the descent. A /Kids array that points back at an
// ancestor is a cycle in a file we did not write.
const maxPageTreeDepth = 64

func (d *destinations) walkPages(node pdf.Value, page *int, depth int) {
	if depth >= maxPageTreeDepth || *page >= d.pageCount || node.Kind() != pdf.Dict {
		return
	}

	switch node.Key("Type").Name() {
	case "Page":
		*page++
		key := node.String()
		if existing, ok := d.byPage[key]; ok && existing != *page {
			d.ambiguous[key] = true
			return
		}
		d.byPage[key] = *page

	default:
		// "Pages", or a node with no /Type at all, which real files contain.
		kids := node.Key("Kids")
		for i := 0; i < kids.Len(); i++ {
			d.walkPages(kids.Index(i), page, depth+1)
		}
	}
}

// page resolves one outline entry to a 1-based page number.
func (d *destinations) page(item pdf.Value) (int, bool) {
	dest := item.Key("Dest")
	if dest.IsNull() {
		// The other half of the spec: an /A action rather than a /Dest. Only
		// GoTo is followed - a URI or a Launch action does not name a page in
		// this document, and following one would be worse than useless.
		action := item.Key("A")
		if action.Key("S").Name() != "GoTo" {
			return 0, false
		}
		dest = action.Key("D")
	}
	if dest.IsNull() {
		return 0, false
	}

	d.index()
	return d.resolve(dest, 0)
}

// maxDestIndirection bounds how far a named destination may chain.
const maxDestIndirection = 8

func (d *destinations) resolve(dest pdf.Value, depth int) (int, bool) {
	if depth >= maxDestIndirection {
		return 0, false
	}

	switch dest.Kind() {
	case pdf.Name, pdf.String:
		named := d.lookupNamed(dest)
		if named.IsNull() {
			return 0, false
		}
		return d.resolve(named, depth+1)

	case pdf.Dict:
		// A destination dictionary wraps the array under /D.
		return d.resolve(dest.Key("D"), depth+1)

	case pdf.Array:
		if dest.Len() == 0 {
			return 0, false
		}
		target := dest.Index(0)
		switch target.Kind() {
		case pdf.Integer:
			// A bare page index, 0-based, as remote destinations use.
			n := int(target.Int64()) + 1
			if n < 1 || n > d.pageCount {
				return 0, false
			}
			return n, true
		case pdf.Dict:
			key := target.String()
			if d.ambiguous[key] {
				return 0, false
			}
			if n, ok := d.byPage[key]; ok {
				return n, true
			}
		}
	}
	return 0, false
}

// lookupNamed follows a named destination, which lives either in the old
// /Dests dictionary or in the /Names name tree. Both are still in the wild.
func (d *destinations) lookupNamed(name pdf.Value) pdf.Value {
	key := name.Name()
	if name.Kind() == pdf.String {
		key = name.RawString()
	}
	if key == "" {
		return pdf.Value{}
	}

	// A name tree is sorted and carries /Limits on each node, so the ordinary
	// case is a descent of two or three levels. The whole tree is only flattened
	// when that descent fails, because /Limits is sometimes simply wrong in the
	// wild and a missing chapter is worse than a slow open.
	if !d.namesLoaded {
		if found := d.searchNameTree(d.root.Key("Names").Key("Dests"), key, 0); !found.IsNull() {
			return found
		}
		if legacy := d.root.Key("Dests").Key(key); !legacy.IsNull() {
			return legacy
		}
	}

	d.loadNames()
	return d.named[key]
}

// searchNameTree descends a name tree using /Limits.
//
// Returns null when the key is not found, which the caller treats as "the tree
// may be lying" rather than "the key does not exist".
func (d *destinations) searchNameTree(node pdf.Value, key string, depth int) pdf.Value {
	if depth >= maxNameTreeDepth || node.Kind() != pdf.Dict {
		return pdf.Value{}
	}

	if limits := node.Key("Limits"); limits.Kind() == pdf.Array && limits.Len() == 2 {
		lo := limits.Index(0).RawString()
		hi := limits.Index(1).RawString()
		if key < lo || key > hi {
			return pdf.Value{}
		}
	}

	if names := node.Key("Names"); names.Kind() == pdf.Array {
		// Flat pairs: key, value, key, value. The array is sorted, so this is a
		// binary search over the pair index. Reading the leaf whole and caching
		// it was tried and is slower: the names are not in document order, so
		// consecutive lookups do not land in the same leaf often enough to pay
		// for reading every pair in it.
		lo, hi := 0, names.Len()/2-1
		for lo <= hi {
			mid := (lo + hi) / 2
			found := names.Index(mid * 2).RawString()
			switch {
			case found == key:
				return names.Index(mid*2 + 1)
			case found < key:
				lo = mid + 1
			default:
				hi = mid - 1
			}
		}
		return pdf.Value{}
	}

	kids := node.Key("Kids")
	for i := 0; i < kids.Len(); i++ {
		if found := d.searchNameTree(kids.Index(i), key, depth+1); !found.IsNull() {
			return found
		}
	}
	return pdf.Value{}
}

// maxNamedDestinations bounds the flattened name tree.
const maxNamedDestinations = 100000

// maxNameTreeDepth bounds the descent through /Kids.
const maxNameTreeDepth = 64

// loadNames flattens the name tree into a map, once.
//
// Searching the tree per entry looked harmless and was not: a book whose 1,049
// outline entries all use named destinations searched the whole tree 1,049
// times, and opening it took five and a half seconds. /Limits would allow
// skipping subtrees during a search, but it is frequently wrong in real files,
// and reading the tree once is both faster than a correct search and immune to
// the lie.
func (d *destinations) loadNames() {
	if d.namesLoaded {
		return
	}
	d.namesLoaded = true
	d.named = make(map[string]pdf.Value)

	// The old /Dests dictionary, still produced by some tools.
	legacy := d.root.Key("Dests")
	if legacy.Kind() == pdf.Dict {
		for _, key := range legacy.Keys() {
			d.named[key] = legacy.Key(key)
		}
	}

	d.collectNames(d.root.Key("Names").Key("Dests"), 0)
}

func (d *destinations) collectNames(node pdf.Value, depth int) {
	if depth >= maxNameTreeDepth || node.Kind() != pdf.Dict || len(d.named) >= maxNamedDestinations {
		return
	}

	if names := node.Key("Names"); names.Kind() == pdf.Array {
		// Flat pairs: key, value, key, value.
		for i := 0; i+1 < names.Len(); i += 2 {
			key := names.Index(i).RawString()
			if key == "" {
				continue
			}
			if _, exists := d.named[key]; !exists {
				d.named[key] = names.Index(i + 1)
			}
		}
	}

	kids := node.Key("Kids")
	for i := 0; i < kids.Len(); i++ {
		d.collectNames(kids.Index(i), depth+1)
	}
}

// cleanText renders a PDF string value, dropping control characters that
// routinely appear in badly produced files.
//
// Text() rather than RawString(), and the difference is not cosmetic. A PDF
// text string is either PDFDocEncoding or UTF-16BE behind a FE FF byte-order
// mark, and every producer writes the second one as soon as the metadata
// contains a character Latin-1 cannot hold - an accent, a dash, anything CJK.
// RawString() hands back the bytes, so such a title arrived with the mark on
// the front and a NUL between every letter. Text() decodes both forms.
func cleanText(v pdf.Value) string {
	if v.IsNull() {
		return ""
	}
	if v.Kind() != pdf.String {
		return ""
	}
	raw := v.Text()
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, raw))
}

func isEncryptedErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "encrypt")
}
