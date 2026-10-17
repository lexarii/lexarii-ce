// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import "testing"

func TestParseName(t *testing.T) {
	tests := []struct {
		raw      string
		surname  string
		forename string
	}{
		{"Peter Costa", "Costa", "Peter"},
		{"Costa, Peter", "Costa", "Peter"},
		{"Nelson H. F. Beebe", "Beebe", "Nelson"},
		{"R. R. Gupta", "Gupta", "R."},
		{"Robert de Graaf", "de Graaf", "Robert"},
		{"de Graaf, Robert", "de Graaf", "Robert"},
		{"van der Berg, Willem", "van der Berg", "Willem"},
		{"Plato", "Plato", ""},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got := ParseName(tt.raw)
			if got.Surname != tt.surname {
				t.Fatalf("surname = %q, want %q", got.Surname, tt.surname)
			}
			first := ""
			if len(got.Forenames) > 0 {
				first = got.Forenames[0]
			}
			if first != tt.forename {
				t.Fatalf("first forename = %q, want %q", first, tt.forename)
			}
		})
	}
}

// Every pair here is a real pair of rows from a 137-author library, including
// the three that trigram similarity gets wrong.
func TestSameAuthorOnRealPairs(t *testing.T) {
	tests := []struct {
		a, b string
		same bool
		why  string
	}{
		{"Jonathon Manning", "Jon Manning", true, "Jon is a shortening of Jonathon"},
		{"M. Tabatabaian", "Mehrzad Tabatabaian", true, "M. is the initial of Mehrzad"},

		{"R. Bronson", "G. Bronson", false, "two different initials are two people"},
		{"Dawn Griffiths", "David Griffiths", false, "neither name shortens to the other"},
		{"R. R. Gupta", "Anuj Gupta", false, "R. is not the initial of Anuj"},

		// Middle names come and go between editions and must not split a person.
		{"Nelson H. F. Beebe", "Nelson Beebe", true, "middle initials are not identity"},
		{"Arnold Robbins", "Arnold Robbins", true, "identical"},

		// Sort-name form is the same person written the other way round.
		{"Costa, Peter", "Peter Costa", true, "same name, different order"},
		{"de Graaf, Robert", "Robert de Graaf", true, "particles travel with the surname"},

		// A bare surname is consistent with everyone who has it, which is not
		// evidence.
		{"Robbins", "Arnold Robbins", false, "a surname alone identifies nobody"},

		// Different surnames are never the same person.
		{"Alice Smith", "Alice Jones", false, "different surnames"},

		// The short-prefix trap: two would match Joan to John.
		{"Jo Smith", "John Smith", false, "two letters is not a shortening"},
		{"Joan Smith", "John Smith", false, "Joan and John are two people"},
		{"Dan Smith", "Danielle Smith", true, "a three-letter prefix is accepted, knowingly"},
	}

	for _, tt := range tests {
		t.Run(tt.a+" vs "+tt.b, func(t *testing.T) {
			got := SameAuthor(ParseName(tt.a), ParseName(tt.b))
			if got != tt.same {
				t.Fatalf("SameAuthor(%q, %q) = %v, want %v (%s)",
					tt.a, tt.b, got, tt.same, tt.why)
			}
		})
	}
}

func TestSameAuthorIsSymmetric(t *testing.T) {
	pairs := [][2]string{
		{"M. Tabatabaian", "Mehrzad Tabatabaian"},
		{"Dawn Griffiths", "David Griffiths"},
		{"Jon Manning", "Jonathon Manning"},
		{"Robbins", "Arnold Robbins"},
	}
	for _, p := range pairs {
		forward := SameAuthor(ParseName(p[0]), ParseName(p[1]))
		back := SameAuthor(ParseName(p[1]), ParseName(p[0]))
		if forward != back {
			t.Fatalf("SameAuthor(%q,%q)=%v but reversed=%v", p[0], p[1], forward, back)
		}
	}
}

// Co-authorship overrides everything: nobody is credited twice on one book.
func TestCoauthorshipMeansDifferentPeople(t *testing.T) {
	if !DifferentByCoauthorship(1) {
		t.Fatal("sharing a work must mean two people")
	}
	if DifferentByCoauthorship(0) {
		t.Fatal("sharing no work is not evidence either way")
	}
}
