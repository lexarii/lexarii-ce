// SPDX-License-Identifier: AGPL-3.0-or-later

// Package rag turns books into something that can be searched by meaning, and
// answers questions from what it finds.
//
// The whole design rests on one rule: every chunk knows where it came from. A
// retrieved passage that cannot be opened in the reader is a quotation with no
// source, and an answer built from those is indistinguishable from one the model
// made up.
package rag

import (
	"strings"
	"unicode"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// MaxChars is the hard ceiling on a chunk, in characters.
//
// Characters, not tokens, because a token estimate cannot bound this corpus.
// Measured against the real embedding model over real chunks: chars/4 was under
// the truth by 2x on one book and the obvious improvement - counting runs and
// punctuation - was over by 4.6x on a contents page of dot leaders while still
// 1.7x under on another. The variance is inherent, because how text tokenises
// depends on the tokeniser's vocabulary and this library is full of run-together
// PDF extractions, dot leaders, and mathematics.
//
// So the estimate is used for SIZING - making passages roughly equal - and this
// is the guard. At worst about half a token per character, 2,000 characters is
// comfortably inside the 2,048-token batch the embedding server is serving with.
const MaxChars = 2000

// StrategyVersion identifies how chunks were made.
//
// Stored with every chunk, because changing the strategy changes what retrieval
// returns, and a library half-chunked one way and half the other answers
// questions inconsistently with no way to tell why. Bumping this makes every
// existing chunk visibly stale rather than silently mixed.
// Version 2 bounds chunks by characters as well as tokens, after the embedding
// server rejected passages whose real token count was three times the estimate.
const StrategyVersion = 2

// Unit is one piece of a book in reading order: a spine item for EPUB, a page
// for PDF.
type Unit struct {
	// Index is the unit's position, which is the `major` half of a locator: the
	// spine index for EPUB, or the ONE-BASED page number for PDF.
	Index int
	// Title is the chapter or section heading this unit belongs to, when known.
	Title string
	// Blocks are the unit's paragraphs in reading order.
	Blocks []Block
}

// Block is one paragraph-level element, with the path that identifies it.
//
// Chunks anchor to blocks rather than to a character offset within a whole
// chapter, because that is what the rest of the system already means by a
// position: an annotation anchors to "this paragraph, this far in", and a
// citation that resolves the same way opens the reader exactly where the
// passage is. An offset into a chapter's concatenated text would be a second,
// incompatible notion of where something is.
type Block struct {
	// Path identifies the element within its unit, in the same form the reader
	// uses: "section[1]/div[1]/p[2]". Required for EPUB; empty for PDF, which
	// has pages rather than elements.
	Path string
	Text string
}

// Chunk is a passage of a book, with the position it came from.
type Chunk struct {
	Seq          int
	Start        domain.Locator
	End          domain.Locator
	ChapterTitle string
	Content      string
	TokenCount   int
}

// Options tune chunking.
type Options struct {
	// Format decides what a locator's `major` means.
	Format domain.Format
	// TargetTokens is the size a chunk aims for. Zero uses DefaultTargetTokens.
	TargetTokens int
	// MaxTokens is the hard ceiling. Zero uses DefaultMaxTokens.
	MaxTokens int
	// OverlapTokens is how much of the previous chunk each chunk repeats. Zero
	// uses DefaultOverlapTokens; a negative value disables overlap.
	OverlapTokens int
	// MaxChars is the hard ceiling in characters. Zero uses MaxChars.
	MaxCharacters int
}

// Chunk sizing.
//
// The ceiling is set by the embedding server, not by taste: a model served with
// a 2,048-token context silently truncates anything longer, so a chunk that
// overruns is stored complete and embedded partial - the worst combination,
// because retrieval then matches on text the vector never saw. The target sits
// well below that so a bad token estimate cannot cross it.
const (
	DefaultTargetTokens  = 450
	DefaultMaxTokens     = 900
	DefaultOverlapTokens = 60
)

// Split turns a book's units into chunks.
//
// Chunks may span units, because a PDF page is far smaller than a useful
// passage and one chunk per page would retrieve half-sentences. They break on
// paragraph boundaries wherever possible: a passage that starts mid-sentence
// reads as nonsense when it is quoted back as a citation.
func Split(units []Unit, opts Options) []Chunk {
	target, maxTokens, overlap := opts.sizes()
	maxChars := opts.MaxCharacters
	if maxChars <= 0 {
		maxChars = MaxChars
	}

	var out []Chunk
	var current []piece
	var currentTokens int

	flush := func() {
		if len(current) == 0 {
			return
		}
		out = append(out, assemble(current, len(out), opts.Format))

		// Carry the tail of this chunk into the next one, so a passage that
		// straddles a boundary is still retrievable whole from one side of it.
		current, currentTokens = tail(current, overlap)
	}

	currentChars := 0
	for _, unit := range units {
		for _, para := range blockPieces(unit) {
			// A paragraph bigger than the ceiling on its own - a long listing,
			// a table flattened into text - is split rather than dropped or
			// stored oversized.
			for _, part := range splitOversized(para, maxTokens, maxChars) {
				overToken := currentTokens > 0 && currentTokens+part.tokens > target
				overChars := currentChars > 0 && currentChars+len(part.text) > maxChars
				if overToken || overChars {
					flush()
					currentChars = charsOf(current)
				}

				// The overlap carried by flush() is text, and it counts. Without
				// this the ceiling is exceeded by whatever was carried - measured
				// at 2,241 characters against a 2,000 limit - and the ceiling is
				// the only real guard there is, because the token estimate cannot
				// be trusted to agree with the model's tokeniser.
				if currentChars+len(part.text) > maxChars {
					current, currentTokens, currentChars = nil, 0, 0
				}

				current = append(current, part)
				currentTokens += part.tokens
				currentChars += len(part.text)

				if currentTokens >= maxTokens || currentChars >= maxChars {
					flush()
					currentChars = charsOf(current)
				}
			}
		}
	}

	// The final chunk, unless what is left is only the overlap carried from the
	// previous one - which would store the same text twice with no new content.
	if len(current) > 0 && !onlyOverlap(current) {
		out = append(out, assemble(current, len(out), opts.Format))
	}
	return out
}

func (o Options) sizes() (target, maxTokens, overlap int) {
	target = o.TargetTokens
	if target <= 0 {
		target = DefaultTargetTokens
	}
	maxTokens = o.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	if maxTokens < target {
		maxTokens = target
	}

	switch {
	case o.OverlapTokens < 0:
		overlap = 0
	case o.OverlapTokens == 0:
		overlap = DefaultOverlapTokens
	default:
		overlap = o.OverlapTokens
	}
	// Overlap larger than the target would make every chunk mostly a copy of
	// the one before it.
	if overlap >= target {
		overlap = target / 4
	}
	return target, maxTokens, overlap
}

// piece is one paragraph, with where it came from.
type piece struct {
	unit    int
	title   string
	path    string // the block's element path within its unit
	offset  int    // character offset of this text within its block
	text    string
	tokens  int
	carried bool // true when this piece is overlap from the previous chunk
}

// blockPieces turns a unit's blocks into pieces, keeping each one's path and
// its offset within that block.
func blockPieces(unit Unit) []piece {
	var out []piece
	for _, block := range unit.Blocks {
		// The offset is measured against the block's ORIGINAL text, so trimming
		// is accounted for rather than ignored: a locator off by the leading
		// whitespace of every paragraph is off by a little, everywhere.
		lead := len(block.Text) - len(strings.TrimLeft(block.Text, " \t\n\r"))
		text := scrub(strings.TrimSpace(block.Text))
		if text == "" {
			continue
		}
		out = append(out, piece{
			unit:   unit.Index,
			title:  unit.Title,
			path:   block.Path,
			offset: lead,
			text:   text,
			tokens: EstimateTokens(text),
		})
	}
	return out
}

// charsOf totals the text length of the pieces carried into a chunk.
func charsOf(pieces []piece) int {
	total := 0
	for _, p := range pieces {
		total += len(p.text)
	}
	return total
}

// splitOversized breaks a paragraph that exceeds either ceiling on its own.
func splitOversized(p piece, maxTokens, maxChars int) []piece {
	if p.tokens <= maxTokens && len(p.text) <= maxChars {
		return []piece{p}
	}

	// Sentence boundaries first: a chunk that begins mid-sentence is unusable as
	// a quotation, which is what a citation is.
	sentences := splitSentences(p.text)

	var out []piece
	var buf strings.Builder
	start := p.offset

	emit := func() {
		text := strings.TrimSpace(buf.String())
		if text == "" {
			return
		}
		out = append(out, piece{
			unit: p.unit, title: p.title, path: p.path, offset: start,
			text: text, tokens: EstimateTokens(text),
		})
		start += buf.Len()
		buf.Reset()
	}

	for _, s := range sentences {
		// Text with no sentence punctuation at all - a code listing, a flattened
		// table, a language this crude rule does not fit - still has to be cut,
		// or a single paragraph is stored whole and embedded truncated.
		for _, part := range splitOnWords(s, maxTokens, maxChars) {
			overToken := EstimateTokens(buf.String()+part) > maxTokens
			overChars := buf.Len()+len(part) > maxChars
			if buf.Len() > 0 && (overToken || overChars) {
				emit()
			}
			buf.WriteString(part)
		}
	}
	emit()
	return out
}

// splitOnWords breaks text at word boundaries so that no piece exceeds the
// ceiling. The last resort, used when sentence boundaries did not help.
func splitOnWords(s string, maxTokens, maxChars int) []string {
	if EstimateTokens(s) <= maxTokens && len(s) <= maxChars {
		return []string{s}
	}

	var out []string
	var buf strings.Builder
	for _, field := range strings.SplitAfter(s, " ") {
		overToken := EstimateTokens(buf.String()+field) > maxTokens
		overChars := buf.Len()+len(field) > maxChars
		if buf.Len() > 0 && (overToken || overChars) {
			out = append(out, buf.String())
			buf.Reset()
		}
		// A single "word" longer than the ceiling - a base64 blob, a minified
		// line, a run of dot leaders - is cut on a rune boundary rather than
		// left oversized.
		for EstimateTokens(field) > maxTokens || len(field) > maxChars {
			cut := min(maxTokens*4, maxChars)
			for cut > 0 && cut < len(field) && field[cut]&0xC0 == 0x80 {
				cut--
			}
			if cut <= 0 || cut >= len(field) {
				break
			}
			out = append(out, field[:cut])
			field = field[cut:]
		}
		buf.WriteString(field)
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

// splitSentences breaks text after sentence-ending punctuation, keeping the
// punctuation and the following space with the sentence they belong to.
func splitSentences(text string) []string {
	var out []string
	start := 0

	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '.' && runes[i] != '!' && runes[i] != '?' {
			continue
		}
		// Followed by whitespace, or it is an abbreviation, a decimal, or a
		// version number rather than the end of a sentence.
		if i+1 >= len(runes) || !unicode.IsSpace(runes[i+1]) {
			continue
		}
		end := i + 1
		for end < len(runes) && unicode.IsSpace(runes[end]) {
			end++
		}
		out = append(out, string(runes[start:end]))
		start = end
		i = end - 1
	}

	if start < len(runes) {
		out = append(out, string(runes[start:]))
	}
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

// assemble turns accumulated pieces into a chunk.
func assemble(pieces []piece, seq int, format domain.Format) Chunk {
	first, last := pieces[0], pieces[len(pieces)-1]

	var b strings.Builder
	for i, p := range pieces {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p.text)
	}
	content := b.String()

	return Chunk{
		Seq:          seq,
		Start:        locator(format, first.unit, first.path, first.offset),
		End:          locator(format, last.unit, last.path, last.offset+len(last.text)),
		ChapterTitle: first.title,
		Content:      content,
		TokenCount:   EstimateTokens(content),
	}
}

// locator builds a position, putting the unit index where the format expects.
//
// A PDF page is one-based and an EPUB spine index is zero-based, which is not a
// detail that can be papered over: a chunk stored under the wrong convention
// opens the reader one page out, every time.
func locator(format domain.Format, unit int, path string, offset int) domain.Locator {
	l := domain.Locator{Format: format, CharOffset: offset}
	if format == domain.FormatPDF {
		l.Page = unit
		return l
	}
	l.SpineIndex = unit
	l.ElementPath = path
	return l
}

// tail returns the pieces to carry into the next chunk as overlap.
func tail(pieces []piece, overlap int) ([]piece, int) {
	if overlap <= 0 {
		return nil, 0
	}

	var carried []piece
	total := 0
	for i := len(pieces) - 1; i >= 0; i-- {
		// Only real content is carried forward. Carrying overlap that was itself
		// carried would let the same paragraph propagate through a whole book.
		if pieces[i].carried {
			break
		}
		// A piece larger than the whole overlap budget is not carried at all.
		//
		// Carrying it anyway is how a chunk ends up over the ceiling: the next
		// chunk starts already full, and the first real paragraph added to it
		// pushes the total past the limit. The ceiling exists because the
		// embedding server truncates beyond it, so overrunning is not cosmetic.
		if total+pieces[i].tokens > overlap {
			break
		}
		p := pieces[i]
		p.carried = true
		carried = append([]piece{p}, carried...)
		total += p.tokens
	}
	return carried, total
}

// onlyOverlap reports whether every piece was carried from the previous chunk,
// which would make this chunk a duplicate with no new text.
func onlyOverlap(pieces []piece) bool {
	for _, p := range pieces {
		if !p.carried {
			return false
		}
	}
	return true
}

// scrub removes characters that cannot be stored or are not worth storing.
//
// A NUL byte is the important one: extracted book text contains them - PDF text
// layers and sanitised EPUB markup both produce them - and Postgres text cannot
// hold one at all, so a single NUL anywhere in a 900-page book failed the whole
// file with "invalid byte sequence for encoding UTF8". Six real books in the
// test library did exactly that.
//
// Other control characters go too. They are invisible to a reader, meaningless
// to an embedding model, and they cost tokens.
func scrub(s string) string {
	if !strings.ContainsFunc(s, isJunk) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isJunk(r) {
			// Replaced with a space rather than removed, so that offsets shift
			// as little as possible and words either side do not run together.
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func isJunk(r rune) bool {
	switch r {
	case '\n', '\t', '\r':
		return false
	case '\uFEFF', '\uFFFD':
		return true
	}
	return unicode.IsControl(r)
}

// EstimateTokens approximates how many tokens a string is.
//
// For SIZING ONLY - making passages roughly comparable. It is not a bound and
// must not be used as one: measured against the real model over real chunks
// from this library it was 2x under on one book, and a more elaborate rule that
// counted punctuation separately was 4.6x over on a page of dot leaders while
// still 1.7x under elsewhere. How text tokenises depends on a vocabulary this
// code does not have.
//
// MaxChars is the guard. This is the ruler.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	if n := len(s) / 4; n > 0 {
		return n
	}
	return 1
}
