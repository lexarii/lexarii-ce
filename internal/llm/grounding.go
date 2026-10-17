// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import "strings"

// Grounded reports whether a value the model produced can be found in the text
// it was shown.
//
// This exists because the model's own confidence is worthless as a signal.
// Measured against a real library: of 1,576 claims gemma4-12b made, 1,560 came
// back at exactly 1.00 and the remaining 16 at 0.90. The bar for applying a
// value unreviewed is 0.60, so it has never once blocked anything. That is not
// the model being right, it is the model not distinguishing - and a number
// that is always the same is worse than no number, because the code around it
// reads as though something is being checked.
//
// A book's title is printed in the book. So is its subtitle, its series and
// its edition. If the model's answer is nowhere in the pages it was given, it
// came from somewhere else - the filename, another book, or nothing at all.
//
// Measured on 25 books from the same library: 21 titles appear in the text and
// 4 do not, so unlike the confidence score this actually separates one claim
// from another. It is not precise enough to throw an answer away - two of
// those four look correct, and extraction from a PDF is lossy enough that a
// real title can genuinely be missing from the sample - which is why an
// ungrounded value is recorded as a SUGGESTION for a person to accept rather
// than being applied or discarded. The cost of being wrong is one row in a
// review screen.
func Grounded(sample, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.TrimSpace(sample) == "" {
		return false
	}

	// Verbatim, ignoring case, punctuation and spacing - which is how the same
	// title survives being typeset one way and extracted another.
	if strings.Contains(squash(sample), squash(value)) {
		return true
	}

	// Or nearly all of its real words. A model reasonably writes "Kotlin
	// Cookbook: A Problem-Focused Approach" for a page that breaks the line
	// between "Problem" and "Focused", and a hyphen that became a line break
	// should not make a true answer look invented.
	words := strings.Fields(spaced(value))
	haystack := spaced(sample)
	found, significant := 0, 0
	for _, word := range words {
		// Short words carry no evidence: "of", "the", "a" appear in every
		// book ever printed.
		if len(word) < 3 {
			continue
		}
		significant++
		if strings.Contains(haystack, word) {
			found++
		}
	}
	return significant > 0 && float64(found)/float64(significant) >= groundedWordShare
}

// groundedWordShare is how much of a value has to be present.
//
// Not all of it: an extractor drops a ligature or splits a hyphenated word
// often enough that demanding every word would flag correct answers on the
// books whose extraction is worst - which are exactly the books enrichment is
// for.
const groundedWordShare = 0.8

// squash reduces text to its letters and digits.
func squash(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if isWordRune(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// spaced reduces text to lowercase words separated by single spaces.
func spaced(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if isWordRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return b.String()
}

func isWordRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	// Anything outside ASCII is kept: an accented letter is part of a word,
	// and dropping it would break every title that is not in English.
	case r >= 128:
		return true
	default:
		return false
	}
}
