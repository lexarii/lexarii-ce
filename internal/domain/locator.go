// SPDX-License-Identifier: AGPL-3.0-or-later

// Package domain holds Lexarii's entities and value types.
// It imports nothing from the rest of the application.
package domain

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Format identifies a book file's format.
//
// Only EPUB and PDF are locatable: they have extractable text with stable
// positions, so a Locator can point into them. The image formats are library
// citizens but not retrieval citizens - they can be shelved and read, but there
// is nothing to chunk, cite, or highlight. Locator.Validate rejects them, which
// is what stops a locator being minted for a format that cannot support one.
type Format string

const (
	FormatEPUB Format = "epub"
	FormatPDF  Format = "pdf"
	FormatCBZ  Format = "cbz"
	FormatCBR  Format = "cbr"
	FormatMOBI Format = "mobi"
	FormatAZW3 Format = "azw3"
)

// Locatable reports whether positions inside this format can be addressed by a
// Locator, and therefore whether it can carry chunks, citations, and highlights.
func (f Format) Locatable() bool {
	return f == FormatEPUB || f == FormatPDF
}

// ErrInvalidLocator is returned when a locator string cannot be parsed.
var ErrInvalidLocator = errors.New("invalid locator")

// Locator is a stable pointer to a position inside a BookFile.
//
// Every reference to a place in a book uses one: reading position, highlight
// anchors, chunk boundaries, and chat citations. Three invariants hold, and the
// rest of the system is built on them:
//
//  1. Stable across re-ingest. The same file always yields the same locators,
//     which is why extraction must be deterministic.
//  2. Independent of presentation. Font size, window width, device, rotation,
//     and theme cannot change a locator. This is why reading position is never
//     stored as a scroll percentage.
//  3. Resolvable both to a rendered position (by the reader) and to a character
//     range (by the indexer).
//
// Changing the wire format below is a coordinated change across ingest, the
// reader, and retrieval - never a local one. Unstable locators orphan every
// highlight and rot every stored citation.
//
// Wire format:
//
//	epub:<spineIndex>/<elementPath>:<charOffset>
//	pdf:<page>:<charOffset>
type Locator struct {
	Format Format `json:"format"`

	// SpineIndex is the zero-based position in the EPUB spine, which is the
	// reading order. The TOC is not the reading order. EPUB only.
	SpineIndex int `json:"spineIndex,omitempty"`

	// ElementPath is a slash-separated path of indexed element names within the
	// spine document, e.g. "body[1]/div[2]/p[5]". EPUB only.
	ElementPath string `json:"elementPath,omitempty"`

	// Page is the one-based page number. PDF only.
	Page int `json:"page,omitempty"`

	// CharOffset is the character offset within the addressed element or page.
	CharOffset int `json:"charOffset"`
}

// NewEPUBLocator builds a locator into an EPUB spine item.
func NewEPUBLocator(spineIndex int, elementPath string, charOffset int) Locator {
	return Locator{
		Format:      FormatEPUB,
		SpineIndex:  spineIndex,
		ElementPath: elementPath,
		CharOffset:  charOffset,
	}
}

// NewPDFLocator builds a locator into a PDF page.
func NewPDFLocator(page, charOffset int) Locator {
	return Locator{Format: FormatPDF, Page: page, CharOffset: charOffset}
}

// Validate reports whether the locator is internally consistent.
func (l Locator) Validate() error {
	if l.CharOffset < 0 {
		return fmt.Errorf("%w: negative char offset %d", ErrInvalidLocator, l.CharOffset)
	}
	switch l.Format {
	case FormatEPUB:
		if l.SpineIndex < 0 {
			return fmt.Errorf("%w: negative spine index %d", ErrInvalidLocator, l.SpineIndex)
		}
		if l.ElementPath == "" {
			return fmt.Errorf("%w: empty element path", ErrInvalidLocator)
		}
		if strings.ContainsAny(l.ElementPath, ":") {
			return fmt.Errorf("%w: element path may not contain ':'", ErrInvalidLocator)
		}
	case FormatPDF:
		if l.Page < 1 {
			return fmt.Errorf("%w: page %d is not one-based", ErrInvalidLocator, l.Page)
		}
	default:
		return fmt.Errorf("%w: unknown format %q", ErrInvalidLocator, l.Format)
	}
	return nil
}

// String renders the canonical wire form. The zero Locator renders as "".
func (l Locator) String() string {
	switch l.Format {
	case FormatEPUB:
		return fmt.Sprintf("epub:%d/%s:%d", l.SpineIndex, l.ElementPath, l.CharOffset)
	case FormatPDF:
		return fmt.Sprintf("pdf:%d:%d", l.Page, l.CharOffset)
	default:
		return ""
	}
}

// ParseLocator parses the canonical wire form produced by String.
func ParseLocator(s string) (Locator, error) {
	scheme, rest, ok := strings.Cut(s, ":")
	if !ok {
		return Locator{}, fmt.Errorf("%w: missing format prefix in %q", ErrInvalidLocator, s)
	}

	switch Format(scheme) {
	case FormatEPUB:
		body, offStr, ok := strings.Cut(rest, ":")
		if !ok {
			return Locator{}, fmt.Errorf("%w: missing char offset in %q", ErrInvalidLocator, s)
		}
		idxStr, path, ok := strings.Cut(body, "/")
		if !ok {
			return Locator{}, fmt.Errorf("%w: missing element path in %q", ErrInvalidLocator, s)
		}
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			return Locator{}, fmt.Errorf("%w: spine index in %q: %v", ErrInvalidLocator, s, err)
		}
		off, err := strconv.Atoi(offStr)
		if err != nil {
			return Locator{}, fmt.Errorf("%w: char offset in %q: %v", ErrInvalidLocator, s, err)
		}
		l := NewEPUBLocator(idx, path, off)
		return l, l.Validate()

	case FormatPDF:
		pageStr, offStr, ok := strings.Cut(rest, ":")
		if !ok {
			return Locator{}, fmt.Errorf("%w: missing char offset in %q", ErrInvalidLocator, s)
		}
		page, err := strconv.Atoi(pageStr)
		if err != nil {
			return Locator{}, fmt.Errorf("%w: page in %q: %v", ErrInvalidLocator, s, err)
		}
		off, err := strconv.Atoi(offStr)
		if err != nil {
			return Locator{}, fmt.Errorf("%w: char offset in %q: %v", ErrInvalidLocator, s, err)
		}
		l := NewPDFLocator(page, off)
		return l, l.Validate()

	default:
		return Locator{}, fmt.Errorf("%w: unknown format %q", ErrInvalidLocator, scheme)
	}
}

// Compare orders two locators within the same BookFile: negative if l precedes
// other, zero if equal, positive if it follows. Comparing locators of different
// formats is meaningless and reports 0, so callers must not mix formats.
//
// This is a TOTAL order - it always returns an answer, falling back to the
// char offset when the structure itself cannot decide (see CompareDecided).
// That fallback is a reasonable heuristic for picking a "later" position out
// of two - reading-position sync uses exactly this to resolve conflicts by
// taking the furthest position - but it is not a claim that the order is
// actually known. Code that must not accept a range it cannot prove is
// forwards - Range.Validate is the reason CompareDecided exists at all -
// needs the decided flag and must call CompareDecided directly rather than
// infer "decided" from this method's result.
func (l Locator) Compare(other Locator) int {
	if cmp, decided := l.CompareDecided(other); decided {
		return cmp
	}
	return cmpInt(l.CharOffset, other.CharOffset)
}

// CompareDecided is Compare, but honest about whether the document's own
// structure could actually settle the order, which Compare's fallback to the
// char offset hides: two locators in different elements have char offsets
// that are not commensurable, and pretending otherwise is exactly the bug
// this method exists to avoid repeating.
//
// decided is false when the two locators are of different formats, or - for
// EPUB - when their element paths diverge at segments naming different tags,
// or one path is a strict prefix of the other (a block's own text can fall
// before, between, or after the blocks nested inside it; the path alone does
// not say which). It is true whenever the spine index differs, the page
// differs, the paths are structurally identical (offset decides), or the
// paths diverge at same-named siblings, ordered by ebook-ingest's one-based,
// per-tag index - the case that matters most in practice, because it is what
// lets a highlight run from one paragraph into the next one over: "p[9]" to
// "p[10]" is decided and forwards, even though it sorts the other way as a
// plain string.
//
// Range.Validate calls this directly, and rejects a range only when it is
// decided AND backwards - mirroring the database CHECK constraint in
// migration 00015 exactly, which accepts any range whose start and end paths
// are not textually identical. Compare, which folds "cannot tell" into
// "offset decides" for its own heuristic purposes, would refuse ranges the
// database - and a person highlighting across two real paragraphs - would
// accept, which is exactly the bug this method exists to let Validate avoid.
func (l Locator) CompareDecided(other Locator) (cmp int, decided bool) {
	if l.Format != other.Format {
		return 0, false
	}
	switch l.Format {
	case FormatEPUB:
		if c := cmpInt(l.SpineIndex, other.SpineIndex); c != 0 {
			return c, true
		}
		pathCmp, pathDecided := compareElementPath(l.ElementPath, other.ElementPath)
		if !pathDecided {
			return 0, false
		}
		if pathCmp != 0 {
			return pathCmp, true
		}
		return cmpInt(l.CharOffset, other.CharOffset), true
	case FormatPDF:
		if c := cmpInt(l.Page, other.Page); c != 0 {
			return c, true
		}
		return cmpInt(l.CharOffset, other.CharOffset), true
	default:
		return 0, false
	}
}

// compareElementPath orders two element paths within the same spine item.
//
// A path is a slash-separated list of "name[index]" segments, the index
// counting preceding siblings of the same tag name, one-based - the scheme
// ebook-ingest's elementStep produces and the reader resolves. Comparing the
// paths as plain strings is wrong: "p[10]" sorts before "p[9]" as text, so a
// highlight running from the 9th paragraph to the 10th reads as backwards
// even though the two are siblings in a document order the paths already
// encode.
//
// So paths are walked segment by segment. Where two segments name the same
// tag, the numeric index decides - that settles exactly the p[9]/p[10] case,
// because siblings of one tag share a parent and the index alone orders them.
// Where segments name different tags, or one path is a strict prefix of the
// other, there is no way to know which comes first without the document
// itself - a div's own text can fall before, between, or after the blocks
// nested inside it. Those cases report decided=false, the same narrowing
// migration 00015 applied to the database CHECK constraint for the same
// reason: a comparison that admits some backwards data it cannot detect is
// better than one that rejects forwards data it cannot understand. It is the
// CALLER's job - CompareDecided's - to treat "not decided" as "not decided"
// rather than quietly reading it as "equal".
func compareElementPath(a, b string) (cmp int, decided bool) {
	as := strings.Split(a, "/")
	bs := strings.Split(b, "/")
	n := len(as)
	if len(bs) < n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		aName, aIndex, aHasIndex := splitPathSegment(as[i])
		bName, bIndex, bHasIndex := splitPathSegment(bs[i])
		if aName != bName {
			return 0, false
		}
		if aHasIndex && bHasIndex {
			if c := cmpInt(aIndex, bIndex); c != 0 {
				return c, true
			}
			continue
		}
		if as[i] != bs[i] {
			return 0, false
		}
	}
	if len(as) != len(bs) {
		// One path is an ancestor of the other; document order between a
		// block's own text and the blocks nested inside it is not decidable
		// from the paths alone.
		return 0, false
	}
	// Every segment compared equal, pairwise, at the same length: the paths
	// are structurally identical.
	return 0, true
}

// splitPathSegment parses one "name[index]" path segment. ok is false for a
// segment with no bracketed index, in which case name is the whole segment
// and callers fall back to comparing it verbatim.
func splitPathSegment(seg string) (name string, index int, ok bool) {
	open := strings.IndexByte(seg, '[')
	if open < 0 || !strings.HasSuffix(seg, "]") {
		return seg, 0, false
	}
	idx, err := strconv.Atoi(seg[open+1 : len(seg)-1])
	if err != nil {
		return seg, 0, false
	}
	return seg[:open], idx, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Range is an inclusive-start, exclusive-end span between two locators in the
// same BookFile. Chunks, highlights, and citations are all ranges.
type Range struct {
	Start Locator `json:"start"`
	End   Locator `json:"end"`
}

// Contains reports whether a locator falls inside the range: at or after the
// start, strictly before the end.
//
// This is how a citation or a highlight is matched to the passage it belongs
// to without requiring the two to begin at exactly the same character. Chunk
// boundaries move whenever the chunking strategy changes; the sentence a
// person cared about does not.
//
// Deliberately uses Compare, not CompareDecided: a chunk very often starts at
// a heading and runs through the paragraphs nested under it, which is exactly
// the shape CompareDecided cannot order - a chunk's own start locator and a
// locator deep inside it are, structurally, "different branches of the
// document tree" to it, the same as any other undecided pair. Range.Validate
// must never accept a range it cannot prove is forwards, because a wrong
// answer there corrupts stored data; Contains has no such stake - it answers
// a read-only membership question, currently only for retrieval evaluation's
// own Recall@10/MRR scoring, where treating "cannot structurally tell" as
// "not contained" would silently undercount passages retrieval actually got
// right, every time the right chunk happens to start above the paragraph a
// question is really about. Compare's own offset fallback is a heuristic, not
// a proof, but a false "contained" here costs nothing a person notices, while
// a false "not contained" reads as a retrieval regression that never happened.
//
// A locator of a different format is never contained: Compare itself reports
// 0 for that pairing, which would otherwise read as "equal" and let a PDF
// locator fall inside an EPUB range, so that case is still guarded explicitly.
func (r Range) Contains(l Locator) bool {
	if l.Format != r.Start.Format || l.Format != r.End.Format {
		return false
	}
	return r.Start.Compare(l) <= 0 && l.Compare(r.End) < 0
}

// Validate reports whether the range is well formed and correctly ordered.
//
// Refuses only when the order is actually decided and runs backwards - see
// CompareDecided. This is what makes it agree with the database CHECK
// constraint in migration 00015: both accept a range whose start and end
// paths cannot be shown to be in the wrong order, rather than guessing from
// an offset that belongs to a different element entirely.
func (r Range) Validate() error {
	if err := r.Start.Validate(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := r.End.Validate(); err != nil {
		return fmt.Errorf("end: %w", err)
	}
	if r.Start.Format != r.End.Format {
		return fmt.Errorf("%w: mixed formats %q and %q", ErrInvalidLocator, r.Start.Format, r.End.Format)
	}
	if cmp, decided := r.Start.CompareDecided(r.End); decided && cmp > 0 {
		return fmt.Errorf("%w: start %s follows end %s", ErrInvalidLocator, r.Start, r.End)
	}
	return nil
}
