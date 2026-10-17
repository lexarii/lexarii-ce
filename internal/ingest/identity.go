// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"strings"
	"unicode"
)

// Deciding whether two author rows are one person is the second half of the
// composite-author problem. Splitting "Kevlin Henney and Trisha Gee" created
// real people; it did not notice that "M. Tabatabaian" and "Mehrzad
// Tabatabaian" are the same one, or that "Jon Manning" and "Jonathon Manning"
// are.
//
// The temptation is to reach for trigram similarity, and on the real library
// that is actively dangerous. Measured over 137 authors, the closest pairs are:
//
//	Jonathon Manning ~ Jon Manning          0.71   same person
//	R. Bronson       ~ G. Bronson           0.67   DIFFERENT people
//	Dawn Griffiths   ~ David Griffiths      0.63   DIFFERENT people
//	M. Tabatabaian   ~ Mehrzad Tabatabaian  0.58   same person
//	R. R. Gupta      ~ Anuj Gupta           0.46   DIFFERENT people
//
// Similarity gets three of five wrong, and gets them wrong in the expensive
// direction: merging Dawn and David Griffiths - a married pair who co-author
// the same three books - destroys two people's bibliographies to tidy up a
// list. Leaving a duplicate is cheap. Merging two people is not, and it is
// unrecoverable once the rows are gone.
//
// So the rule is structural rather than statistical: surnames must match
// exactly, and the first forenames must be compatible - identical, or one an
// initial or a shortening of the other. A caller that can see co-authorship
// should apply DifferentByCoauthorship as well, which catches both dangerous
// pairs above on its own.

// Name is an author's name, split into the parts that decide identity.
type Name struct {
	// Surname includes any particles: "de Graaf", "van der Berg".
	Surname string
	// Forenames are everything before the surname, in order. An initial keeps
	// its full token ("R.") so it can be told from a one-letter name.
	Forenames []string
	// Original is the name as it was written.
	Original string
}

// ParseName splits a name into forenames and surname.
//
// Accepts both orders - "Costa, Peter" and "Peter Costa" - because publisher
// metadata uses whichever the cataloguer preferred.
func ParseName(raw string) Name {
	original := strings.Join(strings.Fields(raw), " ")
	out := Name{Original: original}
	if original == "" {
		return out
	}

	// "Surname, Forenames": the comma says which half is which, so there is
	// nothing to infer.
	if comma := strings.Index(original, ","); comma >= 0 {
		out.Surname = strings.TrimSpace(original[:comma])
		out.Forenames = strings.Fields(original[comma+1:])
		return out
	}

	parts := strings.Fields(original)
	if len(parts) == 1 {
		out.Surname = parts[0]
		return out
	}

	// Particles travel with the surname: "Robert de Graaf" is Graaf, not de.
	last := len(parts) - 1
	first := last
	for first > 1 && particles[strings.ToLower(strings.TrimSuffix(parts[first-1], "."))] {
		first--
	}
	out.Surname = strings.Join(parts[first:], " ")
	out.Forenames = parts[:first]
	return out
}

// SameAuthor reports whether two names are one person.
//
// Conservative by construction: it answers false whenever it cannot tell, which
// leaves a duplicate rather than destroying a bibliography.
func SameAuthor(a, b Name) bool {
	if a.Original == "" || b.Original == "" {
		return false
	}
	if fold(a.Surname) != fold(b.Surname) {
		return false
	}

	// A surname alone matches anyone who shares it, which is not evidence of
	// anything: "Robbins" and "Arnold Robbins" may well be the same person, but
	// "Robbins" and "Nelson Robbins" are equally consistent with it.
	if len(a.Forenames) == 0 || len(b.Forenames) == 0 {
		return false
	}

	// Only the FIRST forename decides. Middle names and initials come and go
	// between editions - "Nelson H. F. Beebe" and "Nelson Beebe" are one person
	// - so requiring them to agree would split people rather than join them.
	return compatible(a.Forenames[0], b.Forenames[0])
}

// minShortening is how much of a forename a shortening must share.
//
// "Jon" for "Jonathon" is three. Two would make "Jo" match both "John" and
// "Joan", which is exactly the kind of confident wrong answer this is built to
// avoid.
const minShortening = 3

// compatible reports whether two forenames can belong to the same person.
func compatible(x, y string) bool {
	a, b := fold(x), fold(y)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}

	aInitial, bInitial := isInitial(x), isInitial(y)
	switch {
	case aInitial && bInitial:
		// Two initials that differ are two people: R. Bronson and G. Bronson
		// co-wrote the same book.
		return false
	case aInitial:
		return a[0] == b[0]
	case bInitial:
		return b[0] == a[0]
	}

	// Neither is an initial, so one must be a shortening of the other: "Jon" of
	// "Jonathon". "Dawn" and "David" share only a letter and are two people.
	if len(a) < minShortening || len(b) < minShortening {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// isInitial reports whether a token is an initial rather than a name: "R.",
// "R", "J.-P.".
func isInitial(token string) bool {
	letters := 0
	for _, r := range token {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return letters == 1
}

// fold normalises a name part for comparison: case, punctuation, accents left
// alone deliberately - "Müller" and "Muller" are a question this does not
// pretend to answer.
func fold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// DifferentByCoauthorship reports whether two names appear together on the same
// work, which is strong evidence they are two people.
//
// Nobody is credited twice on their own book. This is what tells Dawn Griffiths
// from David Griffiths, who write the Head First books together and share a
// surname, a publisher, and three titles.
func DifferentByCoauthorship(sharedWorks int) bool { return sharedWorks > 0 }
