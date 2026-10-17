// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import "strings"

// Subject tags arrive from publisher metadata and from the model, and they
// accumulate near-duplicates: a library of 168 books grew 215 tags, of which
// about 150 name exactly one book. Most of that long tail is real - aerodynamics
// and cheminformatics are genuinely one book each - but some of it is the same
// subject written twice, and a tag that names one book is a dead end in the UI,
// because clicking it shows you the book you were already looking at.
//
// The obvious tool is embedding similarity, and it is the wrong one for the
// same reason it was wrong for author names. "machine learning" and "deep
// learning" are as close as two phrases get, and merging them destroys a
// distinction the reader cares about. Similarity measures relatedness; merging
// requires sameness.
//
// So the rule here is structural and deliberately narrow: one tag's words must
// be a subset of the other's, and every extra word must carry no topical
// content of its own. "computer programming" is programming; "systems
// programming" is NOT, and neither is "abstract algebra" algebra.

// genericModifiers are words that qualify a subject without changing it.
//
// The list is short on purpose and each entry has been checked against real
// tags. "systems" was tried and removed: it turns "systems programming", a
// distinct field, into plain programming. "applied" and "advanced" are absent
// for the same reason - applied mathematics is its own subject.
var genericModifiers = map[string]bool{
	"computer":     true,
	"computing":    true,
	"programming":  true,
	"utilities":    true,
	"techniques":   true,
	"fundamentals": true,
	"essentials":   true,
	"basics":       true,
	"introduction": true,
	"guide":        true,
	"handbook":     true,
}

// RedundantSubject reports whether `specific` is `general` with only
// content-free words added, and so should collapse into it.
//
// Both tags must already exist for a caller to act on this: the rule says two
// names are the same subject, never that a subject should be renamed to one
// nobody has used. That is what stops "audio programming" from collapsing into
// an "audio" tag that does not exist.
func RedundantSubject(specific, general string) bool {
	a := words(general)
	b := words(specific)
	if len(a) == 0 || len(b) == 0 || len(b) <= len(a) {
		return false
	}

	// Every word of the general tag must appear in the specific one.
	for w := range a {
		if !b[w] {
			return false
		}
	}
	// And everything left over must be a word that means nothing on its own.
	for w := range b {
		if !a[w] && !genericModifiers[w] {
			return false
		}
	}
	return true
}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(s)) {
		w = strings.Trim(w, ".,:;()")
		if w != "" {
			out[w] = true
		}
	}
	return out
}
