// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"fmt"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// Result is what a parser produces from one file: the canonical text with
// stable positions, the structure, the cover, and whatever metadata the file
// actually claims.
//
// Parsers do not guess, infer, or enrich. If a file says nothing about its
// author, the author is empty - not a value derived from the filename. Filling
// gaps is the enrichment stage's job, and keeping the two apart is what lets
// publisher metadata reliably beat a model's guess later.
type Result struct {
	Format   domain.Format
	Metadata Metadata

	// Spine is the reading order. For EPUB this is the OPF spine, which is not
	// the same as the table of contents.
	Spine []SpineItem

	// UnitText is the extracted plain text of each reading-order unit, parallel
	// to Spine: UnitText[i] is the text of Spine[i]. Empty when a format carries
	// no extractable text (CBZ) or when extraction has not run.
	//
	// The Locator `major` for a unit is NOT always its index. EPUB spine indices
	// are zero-based and match directly; PDF pages are one-based, so unit i is
	// page i+1. Use MajorFor rather than assuming.
	UnitText []string

	// NeedsOCR reports a document with pages but effectively no extractable
	// text: a scan. It is a library citizen but not a retrieval citizen until
	// it is OCRed, and it must never be ingested as an empty book.
	NeedsOCR bool

	// TOC is the navigational structure, which may be partial, out of order, or
	// absent entirely.
	TOC []TOCEntry

	Cover *Cover

	// Warnings records recoverable problems: skipped archive entries, an
	// unreadable TOC, a missing cover. A file that produces warnings is still
	// ingested; the warnings surface in the UI.
	Warnings []string
}

// Metadata is what the file claims about itself.
type Metadata struct {
	Title       string
	Subtitle    string
	Authors     []Contributor
	Language    string // BCP-47
	Publisher   string
	PublishedOn string // as claimed: may be a year, a date, or junk
	ISBN13      string
	ISBN10      string
	Description string
	Series      string
	SeriesIndex *float64
	Subjects    []string

	// Provenance records where each field came from, keyed by field name
	// ("title", "authors", ...). A field absent from this map was not found.
	Provenance map[string]domain.MetadataSource
}

// Contributor is a person credited in the file's metadata.
type Contributor struct {
	Name     string
	SortName string
	Role     domain.AuthorRole
}

// SpineItem is one document in reading order.
type SpineItem struct {
	// Index is the zero-based position in reading order and is the `major`
	// component of every Locator into this item.
	Index int
	// Path is the sanitised archive path of the document.
	Path string
	// ID is the OPF manifest id, used to match the TOC back to the spine.
	ID string
	// MediaType is the declared content type.
	MediaType string
	// Linear is false for supplementary content that readers may skip
	// (advertisements, alternate versions). It stays in the spine and keeps its
	// index, because dropping it would renumber every locator after it.
	Linear bool
}

// TOCEntry is one navigation point. Children nest arbitrarily deep.
type TOCEntry struct {
	Title string
	// Target is the sanitised archive path this entry points at.
	Target string
	// Fragment is the element id within the target, empty when the entry points
	// at the whole document.
	Fragment string
	// SpineIndex is the matching spine position, or -1 when the target is not in
	// the spine - which happens in real books and is not an error.
	SpineIndex int
	Children   []TOCEntry
}

// Cover is an extracted cover image.
type Cover struct {
	// Path is the sanitised archive path it came from.
	Path      string
	MediaType string
	Data      []byte
}

// MajorFor converts a zero-based reading-order unit index into the `major`
// component of a Locator for this format.
//
// EPUB spine indices are zero-based; PDF page numbers are one-based. Getting
// this wrong shifts every locator in a book by one, which silently corrupts
// every highlight and citation, so it lives in one place.
func (r *Result) MajorFor(unitIndex int) int {
	if r.Format == domain.FormatPDF {
		return unitIndex + 1
	}
	return unitIndex
}

// AddWarning records a recoverable problem. Parsers call this instead of
// failing: one bad TOC must not cost the user the whole book.
func (r *Result) AddWarning(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// SetProvenance records where a metadata field came from.
func (m *Metadata) SetProvenance(field string, source domain.MetadataSource) {
	if m.Provenance == nil {
		m.Provenance = make(map[string]domain.MetadataSource)
	}
	m.Provenance[field] = source
}
