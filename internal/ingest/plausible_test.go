// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import "testing"

func TestPlausibleTitleRejectsProducerArtefacts(t *testing.T) {
	// Every one of these was either observed in a real 167-book library or is a
	// documented output of a common PDF producer.
	junk := []string{
		"Algebra_P1_Book.indb",
		"flight.indb",
		"linear.indb",
		"special.indb",
		"Microsoft Word - chapter1.doc",
		"Microsoft Word - Draft 3 FINAL.docx",
		"Microsoft PowerPoint - lecture.pptx",
		"book.tex",
		"thesis.dvi",
		"manuscript.qxd",
		"output.pdf",
		"Untitled document",
		"untitled-1",
		"Document1",
		"chapter_01_final",
		"book-v2_draft",
		"0123456789abcdef0123",
		"3f2504e0-4f89-11d3-9a0c-0305e82c3301",
		"/Users/someone/Documents/book.indd",
		"",
		"   ",
	}
	for _, title := range junk {
		t.Run(title, func(t *testing.T) {
			if PlausibleTitle(title) {
				t.Fatalf("PlausibleTitle(%q) = true; junk like this becomes a permanent publisher fact", title)
			}
		})
	}
}

// The test must be conservative: a real title that looks unusual is far cheaper
// than a junk title that cannot be displaced.
func TestPlausibleTitleAcceptsRealTitles(t *testing.T) {
	real := []string{
		"The Art of Computer Programming",
		"Algorithmic Thinking",
		"97 Things Every Java Programmer Should Know",
		"Gödel, Escher, Bach: An Eternal Golden Braid",
		"C++ Primer",
		"Dune",
		"Ulysses",
		"1984",
		"R for Data Science",
		"Lisp",
		"bash Cookbook",
		"Structure and Interpretation of Computer Programs",
		// Subtitles with punctuation must survive.
		"Bayesian Statistics the Fun Way: Understanding Statistics and Probability",
		"Head First C#",
		// A version number in a real title is fine when there are spaces.
		"Python 3 Standard Library by Example",
		// Non-Latin scripts must not be mistaken for junk.
		"算法导论",
		"Введение в анализ",
	}
	for _, title := range real {
		t.Run(title, func(t *testing.T) {
			if !PlausibleTitle(title) {
				t.Fatalf("PlausibleTitle(%q) = false; a real title was rejected", title)
			}
		})
	}
}

func TestPlausibleAuthor(t *testing.T) {
	junk := []string{
		"", "   ", "Administrator", "admin", "USER", "unknown", "Owner",
		"Microsoft Word", "pdfTeX", "Ghostscript", "Acrobat", "default",
		`CORP\jsmith`, "/home/build/user", "0123456789abcdef0123",
	}
	for _, name := range junk {
		t.Run("junk/"+name, func(t *testing.T) {
			if PlausibleAuthor(name) {
				t.Fatalf("PlausibleAuthor(%q) = true", name)
			}
		})
	}

	real := []string{
		"Donald Knuth", "Ursula K. Le Guin", "Kevlin Henney and Trisha Gee",
		"Norman Matloff", "Tilman M. Davies", "Bjarne Stroustrup",
		// A surname that happens to contain a junk word must still pass.
		"Adam Userton", "Jean-Luc Administrateur",
	}
	for _, name := range real {
		t.Run("real/"+name, func(t *testing.T) {
			if !PlausibleAuthor(name) {
				t.Fatalf("PlausibleAuthor(%q) = false; a real author was rejected", name)
			}
		})
	}
}

// Rejecting is not guessing. The field is left empty exactly as if the file had
// declared nothing, which is what lets enrichment and the user fill it later.
func TestRejectionLeavesTheFieldEmptyNotGuessed(t *testing.T) {
	var m Metadata
	title := "flight.indb"

	if PlausibleTitle(title) {
		m.Title = title
		m.SetProvenance("title", "embedded")
	}

	if m.Title != "" {
		t.Errorf("Title = %q, want empty", m.Title)
	}
	if _, recorded := m.Provenance["title"]; recorded {
		t.Error("a rejected title must not carry provenance; that is what makes it replaceable")
	}
}

// Real author strings from a 167-book library that an over-broad path check
// rejected. Kept as regression seeds.
func TestPlausibleAuthorAcceptsSlashSeparatedLists(t *testing.T) {
	observed := []string{
		"Abi Aghayere/ Jason Vigil",
		"G. Bronson/ R. Bronson/ M. Kieff",
		"S. H. Omran/ M. T. Chaichan/ H. M. Hussen/ N. G. Nacy/ L. J. Habeeb",
	}
	for _, name := range observed {
		t.Run(name, func(t *testing.T) {
			if !PlausibleAuthor(name) {
				t.Fatalf("PlausibleAuthor(%q) = false; a forward slash separates authors, it is not a path", name)
			}
		})
	}
}

func TestSplitContributors(t *testing.T) {
	tests := []struct {
		raw  string
		want []string
	}{
		{"Abi Aghayere/ Jason Vigil", []string{"Abi Aghayere", "Jason Vigil"}},
		{"G. Bronson/ R. Bronson/ M. Kieff", []string{"G. Bronson", "R. Bronson", "M. Kieff"}},
		{"Kernighan; Ritchie", []string{"Kernighan", "Ritchie"}},
		{"Donald Knuth", []string{"Donald Knuth"}},

		// A lone comma after a surname is ONE person in sort-name form.
		{"Knuth, Donald", []string{"Knuth, Donald"}},
		{"Costa, Peter", []string{"Costa, Peter"}},
		{"de Graaf, Robert", []string{"de Graaf, Robert"}},
		{"van der Berg, Willem", []string{"van der Berg, Willem"}},

		// A lone comma after a FULL name is separating two people.
		{"Joseph Albahari, Eric Johannsen", []string{"Joseph Albahari", "Eric Johannsen"}},
		{"Ken Anjyo, Hiroyuki Ochiai", []string{"Ken Anjyo", "Hiroyuki Ochiai"}},

		// Real rows from the library, which is what this is for.
		{"Kevlin Henney and Trisha Gee", []string{"Kevlin Henney", "Trisha Gee"}},
		{"Arnold Robbins & Nelson H. F. Beebe", []string{"Arnold Robbins", "Nelson H. F. Beebe"}},
		{"Arnold Robbins, Elbert Hannah & Linda Lamb",
			[]string{"Arnold Robbins", "Elbert Hannah", "Linda Lamb"}},
		{"Benjamin Bengfort, Rebecca Bilbro, and Tony Ojeda",
			[]string{"Benjamin Bengfort", "Rebecca Bilbro", "Tony Ojeda"}},
		{"Randal L. Schwartz, brian d foy, and Tom Phoenix",
			[]string{"Randal L. Schwartz", "brian d foy", "Tom Phoenix"}},
		{"Shelley Powers, Jerry Peek, Tim O'Reilly & Mike Loukides",
			[]string{"Shelley Powers", "Jerry Peek", "Tim O'Reilly", "Mike Loukides"}},
		{"Mars Geldard, Jonathon Manning, Paris Buttfield-Addison, and Tim Nugent",
			[]string{"Mars Geldard", "Jonathon Manning", "Paris Buttfield-Addison", "Tim Nugent"}},

		// A degree is not a person.
		{"Phil Winder, Ph.D.", []string{"Phil Winder"}},
		{"Roger W. Pryor, PhD", []string{"Roger W. Pryor"}},
		{"Paul Troncone and Carl Albing, PhD", []string{"Paul Troncone", "Carl Albing"}},

		// "and" is a word, not a substring: these names contain it and survive.
		{"Alexander Anderson", []string{"Alexander Anderson"}},
		{"Sandy Normandy", []string{"Sandy Normandy"}},

		{"", nil},
		{" / / ", nil},
		{"PhD", nil},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got := SplitContributors(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("SplitContributors(%q) = %v, want %v", tt.raw, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("SplitContributors(%q)[%d] = %q, want %q", tt.raw, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// Calibrated against a real 173-book library, where exactly two author names
// were a single token and both were junk: "radha" on five unrelated books, and
// "Sys" on one.
func TestPlausibleAuthorRejectsMachineAccounts(t *testing.T) {
	for _, junk := range []string{
		"radha",    // a Unix account, credited with five books
		"Sys",      // a machine account
		"sys",      //
		"toshiba",  // the name a laptop was sold under
		"hp",       //
		"root",     //
		"jsmith",   // a login, not a name
		"someuser", //
	} {
		if PlausibleAuthor(junk) {
			t.Errorf("accepted %q as an author", junk)
		}
	}
}

// The rule is one lowercase token, and it must not reach further than that.
// Mononyms are real, and so is a name its owner writes in lower case.
func TestPlausibleAuthorKeepsRealNames(t *testing.T) {
	for _, name := range []string{
		"Homer",             // a mononym, capitalised
		"Colette",           //
		"Voltaire",          //
		"bell hooks",        // deliberately lower case, but two tokens
		"Arnold Robbins",    //
		"Dawn Griffiths",    //
		"Kevlin Henney",     //
		"Olgha B. Davis",    //
		"Abi Aghayere",      //
		"Joseph Albahari",   //
		"李伟",                // no spaces, and not lowercase-able
		"Хорхе Луис Борхес", //
	} {
		if !PlausibleAuthor(name) {
			t.Errorf("rejected a real author: %q", name)
		}
	}
}
