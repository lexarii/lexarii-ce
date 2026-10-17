// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// citationPattern matches a bracketed passage number: [1], [12].
//
// Deliberately strict. Books are full of brackets - "[sic]", "[1]" as the
// book's own reference, "[0..n]" in a code sample - and a loose pattern would
// find citations in the quoted material rather than in the answer about it.
var citationPattern = regexp.MustCompile(`\[(\d{1,3})\]`)

// CitedNumbers returns the passage numbers an answer referred to, in the order
// they first appear, ignoring anything outside the range that was offered.
//
// Out-of-range markers are dropped rather than reported: a model citing [17]
// when it was given ten passages has made something up, and the honest handling
// is for that citation not to exist. A number that pointed at nothing would be
// worse than none, because the reader would follow it.
func CitedNumbers(answer string, available int) []int {
	if available <= 0 {
		return nil
	}

	var out []int
	seen := make(map[int]bool)

	for _, match := range citationPattern.FindAllStringSubmatch(answer, -1) {
		n, err := strconv.Atoi(match[1])
		if err != nil || n < 1 || n > available {
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// InventedCitations returns markers that point at passages which were never
// offered.
//
// Worth surfacing rather than only discarding: a model inventing citations is a
// sign the answer is drifting away from its sources, and that is exactly what
// this whole design exists to make visible.
func InventedCitations(answer string, available int) []int {
	var out []int
	seen := make(map[int]bool)

	for _, match := range citationPattern.FindAllStringSubmatch(answer, -1) {
		n, err := strconv.Atoi(match[1])
		if err != nil || (n >= 1 && n <= available) {
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// Some models cite in their own notation whatever the prompt asks for. The
// gpt-oss family writes 【1】, or 【1†L10-L20】 with a dagger and a line range
// from its training format, and occasionally [1†L10-L20] with ASCII brackets.
// Nothing downstream reads those - CitedNumbers, the stored citations, the chips
// in the browser - so an answer cited that way kept its sources on the screen
// and lost every link from the text to them.
//
// NormaliseCitations rewrites them to [n] before anything parses, stores or
// sends the answer, so there is one notation everywhere and the client never
// has to know which model wrote the text.
//
// The rules, exactly:
//
//   - 【n】 becomes [n]. Up to two spaces either side of the number are
//     tolerated: 【 1 】.
//   - 【n†anything】 becomes [n]. The locator after the dagger is dropped: it
//     points into the model's own view of the prompt, not into the book.
//   - [n†anything] becomes [n].
//   - [n] is left alone, and so is every other bracket.
//
// n is one to three ASCII digits, as CitedNumbers requires. What follows the
// dagger is at most maxCitationSuffix characters and contains no bracket and
// no line break, so a marker can never swallow the text around it. Adjacent
// markers - 【1】【2】, [1†a][2†b] - are separate markers and become [1][2].
//
// Known skew, accepted: 【1】 is also ordinary punctuation in Chinese and
// Japanese, so a CJK quotation with a bracketed numeral in it becomes [1] and
// may be read as a citation. Rare in an answer, and a model that writes 【1】
// at all is far more often citing.
//
// Range checking is not done here. 【17】 from a model given ten passages
// becomes [17], and CitedNumbers drops it exactly as it drops a [17].
var citationVariant = regexp.MustCompile(
	`【 {0,2}(\d{1,3}) {0,2}(?:†[^\[\]【】\n]{0,` + strconv.Itoa(maxCitationSuffix) + `})?】` +
		`|\[(\d{1,3})†[^\[\]【】\n]{0,` + strconv.Itoa(maxCitationSuffix) + `}\]`)

// maxCitationSuffix bounds the locator after a dagger. Longer than any real one
// ("L1234-L1290" is eleven), short enough that a stray dagger in prose does not
// start a marker that runs on through a paragraph.
const maxCitationSuffix = 40

// maxCitationRunes is the longest a variant marker can be: 【, two spaces,
// three digits, two spaces, the dagger, the suffix, 】.
const maxCitationRunes = 1 + 2 + 3 + 2 + 1 + maxCitationSuffix + 1

// NormaliseCitations rewrites the citation notations some models use to the
// [n] the rest of the system reads. See citationVariant for the rules.
func NormaliseCitations(text string) string {
	if !strings.ContainsAny(text, "【†") {
		return text
	}
	return citationVariant.ReplaceAllStringFunc(text, func(marker string) string {
		m := citationVariant.FindStringSubmatch(marker)
		n := m[1]
		if n == "" {
			n = m[2]
		}
		return "[" + n + "]"
	})
}

// citationStream normalises a streamed answer piece by piece.
//
// A marker can arrive split across deltas - "【1" in one and "†L4】" in the
// next - and neither half is recognisable alone. So the stream holds back the
// tail that could still turn into a marker and passes everything before it on,
// normalised. The text it emits, joined, is exactly NormaliseCitations of the
// whole answer, which is what is stored and what the final event carries: the
// person reads the same words while it streams as when they reopen the thread.
//
// The hold is short. It starts at the last opening bracket that has not been
// closed and is released as soon as a closing bracket, a line break, or more
// than maxCitationRunes of text shows it was never a marker.
type citationStream struct {
	emit    func(string)
	pending string
}

func newCitationStream(emit func(string)) *citationStream {
	return &citationStream{emit: emit}
}

// write takes one delta.
func (s *citationStream) write(delta string) {
	s.pending += delta
	cut := heldFrom(s.pending)
	if cut == 0 {
		return
	}
	ready := s.pending[:cut]
	s.pending = s.pending[cut:]
	s.emit(NormaliseCitations(ready))
}

// flush releases whatever is still held, at the end of the answer.
func (s *citationStream) flush() {
	if s.pending == "" {
		return
	}
	ready := s.pending
	s.pending = ""
	s.emit(NormaliseCitations(ready))
}

// heldFrom returns where the part of text that must wait for more begins:
// len(text) when none of it has to.
//
// No marker contains a bracket other than its own two, so a marker that is not
// finished yet starts at the LAST opening bracket. Everything before that is
// settled. Splitting there is safe for the same reason: no marker can span it.
func heldFrom(text string) int {
	// A delta can in principle end inside a multi-byte character, and the
	// first byte of 【 alone matches nothing.
	end := len(text)
	if start := lastRuneStart(text); start < end && !utf8.FullRuneInString(text[start:]) {
		end = start
	}

	open := max(strings.LastIndexByte(text[:end], '['), strings.LastIndex(text[:end], "【"))
	if open < 0 {
		return end
	}
	tail := text[open:end]
	if strings.ContainsAny(tail, "]】\n") || utf8.RuneCountInString(tail) >= maxCitationRunes {
		return end
	}
	return open
}

// lastRuneStart is the index of the first byte of the last character.
func lastRuneStart(text string) int {
	i := len(text)
	for i > 0 && i > len(text)-utf8.UTFMax {
		i--
		if utf8.RuneStart(text[i]) {
			return i
		}
	}
	return len(text)
}
