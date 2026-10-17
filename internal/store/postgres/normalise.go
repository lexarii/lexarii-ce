// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// errNotFound is the package-local alias used when building wrapped errors.
var errNotFound = store.ErrNotFound

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// leadingArticles are stripped when building a sort title, so "The Dispossessed"
// files under D. Only English is handled: guessing at articles across languages
// without knowing the language does more harm than good, and the language field
// is frequently absent.
var leadingArticles = []string{"the ", "a ", "an "}

// SortTitle produces the shelf-ordering form of a title.
func SortTitle(title string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		return ""
	}
	lower := strings.ToLower(t)
	for _, article := range leadingArticles {
		if strings.HasPrefix(lower, article) {
			return strings.TrimSpace(t[len(article):])
		}
	}
	return t
}

// SortName produces a "Surname, Forenames" sort form from a display name.
//
// This is a heuristic and is treated as one: it is only ever used when the file
// did not supply a file-as value. Publisher-supplied sort names always win,
// because no heuristic handles "Ursula K. Le Guin", "Vincent van Gogh", and
// "Martin Luther King Jr." correctly at the same time.
func SortName(name string) string {
	n := strings.Join(strings.Fields(name), " ")
	if n == "" {
		return ""
	}

	// A name already in "Surname, Forenames" form is left alone.
	if strings.Contains(n, ",") {
		return n
	}

	parts := strings.Split(n, " ")
	if len(parts) == 1 {
		return n
	}

	// Generational suffixes belong with the surname, not at the front.
	suffixes := map[string]bool{"jr": true, "jr.": true, "sr": true, "sr.": true,
		"i": true, "ii": true, "iii": true, "iv": true}
	last := len(parts) - 1
	suffix := ""
	if last > 1 && suffixes[strings.ToLower(parts[last])] {
		suffix = " " + parts[last]
		last--
	}

	// Nobiliary particles travel with the surname: "van Gogh", "de Beauvoir",
	// "Le Guin", "von Neumann".
	particles := map[string]bool{"van": true, "von": true, "de": true, "del": true,
		"della": true, "di": true, "da": true, "du": true, "la": true, "le": true,
		"den": true, "der": true, "ten": true, "ter": true, "bin": true, "ibn": true}
	first := last
	for first > 1 && particles[strings.ToLower(parts[first-1])] {
		first--
	}

	surname := strings.Join(parts[first:last+1], " ") + suffix
	forenames := strings.Join(parts[:first], " ")
	if forenames == "" {
		return surname
	}
	return surname + ", " + forenames
}

// TagSlug normalises a tag name for deduplication.
//
// Without this a 5000-book library grows 5000 near-duplicate tags: "ml", "ML",
// "machine-learning", and "Machine Learning" are one subject, not four.
func TagSlug(name string) string {
	var b strings.Builder
	lastHyphen := true // suppress a leading hyphen

	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastHyphen = false
		case r == '+' || r == '#':
			// Preserved deliberately: "c++" and "c#" are different subjects from
			// "c", and collapsing them would merge three languages into one tag.
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}

	return strings.Trim(b.String(), "-")
}

// jsonValue encodes a provenance value for the jsonb column.
func jsonValue(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		// Every value passed here is a string or a []string, so this cannot
		// fail in practice; null is a truthful fallback rather than a crash.
		return []byte("null")
	}
	return data
}

// SearchKey reduces a string to bare lowercase alphanumerics.
//
// It is the Go side of the generated book_files.search_key column, and the two
// must stay in step: the column is
// regexp_replace(lower(path), '[^a-z0-9]', ”, 'g').
//
// This exists so a query can match a filename that has no separators.
// "linear algebra" becomes "linearalgebra", which is a substring of
// "appliedlinearalgebraandoptimizationusingmatlab.pdf" once that is normalised
// the same way.
func SearchKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// StemKey reduces a file path to its bare filename stem: directory and
// extension removed, then lowercased with all punctuation stripped.
//
// It is the Go side of the generated book_files.stem_key column and the two
// must stay in step. "97thingseveryjavaprogrammershouldknow.epub" and
// ".../done/97thingseveryjavaprogrammershouldknow.pdf" both reduce to
// "97thingseveryjavaprogrammershouldknow".
func StemKey(path string) string {
	base := path
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	return SearchKey(base)
}
