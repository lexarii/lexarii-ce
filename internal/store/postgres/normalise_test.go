// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"strings"
	"testing"
)

func TestSortTitle(t *testing.T) {
	tests := map[string]string{
		"The Dispossessed":                   "Dispossessed",
		"A Brief History of Time":            "Brief History of Time",
		"An Introduction to Category Theory": "Introduction to Category Theory",
		"Gödel, Escher, Bach":                "Gödel, Escher, Bach",
		"the c programming language":         "c programming language",
		"":                                   "",
		"   ":                                "",
		// "Theory" starts with "The" but is not an article.
		"Theory of Everything": "Theory of Everything",
		// A bare article is not worth stripping to nothing.
		"The": "The",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := SortTitle(in); got != want {
				t.Fatalf("SortTitle(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestSortName(t *testing.T) {
	tests := map[string]string{
		"Donald Knuth":          "Knuth, Donald",
		"Ursula K. Le Guin":     "Le Guin, Ursula K.",
		"Vincent van Gogh":      "van Gogh, Vincent",
		"John von Neumann":      "von Neumann, John",
		"Simone de Beauvoir":    "de Beauvoir, Simone",
		"Martin Luther King Jr": "King Jr, Martin Luther",
		"Homer":                 "Homer",
		"Ritchie, Dennis":       "Ritchie, Dennis", // already sorted, left alone
		"":                      "",
		"  Donald   Knuth  ":    "Knuth, Donald", // whitespace collapsed
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := SortName(in); got != want {
				t.Fatalf("SortName(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestTagSlug(t *testing.T) {
	tests := map[string]string{
		"Machine Learning":          "machine-learning",
		"machine learning":          "machine-learning",
		"Machine-Learning":          "machine-learning",
		"MACHINE  LEARNING":         "machine-learning",
		"  Machine Learning  ":      "machine-learning",
		"Computer Science":          "computer-science",
		"Science Fiction & Fantasy": "science-fiction-fantasy",
		"19th century":              "19th-century",
		"Gödel":                     "gödel",
		"":                          "",
		"---":                       "",
		"!!!":                       "",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := TagSlug(in); got != want {
				t.Fatalf("TagSlug(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

// The whole point of slugs: near-duplicates must collapse to one tag.
func TestTagSlugCollapsesNearDuplicates(t *testing.T) {
	variants := []string{
		"machine learning", "Machine Learning", "MACHINE LEARNING",
		"Machine-Learning", "  machine   learning  ", "machine_learning",
	}
	want := TagSlug(variants[0])
	for _, v := range variants {
		if got := TagSlug(v); got != want {
			t.Errorf("TagSlug(%q) = %q, want %q - near-duplicates must collapse", v, got, want)
		}
	}
}

// ...but genuinely different subjects must not be collapsed together.
func TestTagSlugKeepsDistinctSubjectsApart(t *testing.T) {
	distinct := []string{"c", "c++", "c#"}
	seen := make(map[string]string, len(distinct))
	for _, name := range distinct {
		slug := TagSlug(name)
		if other, clash := seen[slug]; clash {
			t.Fatalf("%q and %q both slug to %q; they are different languages", name, other, slug)
		}
		seen[slug] = name
	}
}

func TestSearchKey(t *testing.T) {
	tests := map[string]string{
		"linear algebra":  "linearalgebra",
		"Linear Algebra":  "linearalgebra",
		"LINEAR  ALGEBRA": "linearalgebra",
		"linear-algebra":  "linearalgebra",
		"C++ Primer":      "cprimer",
		"97 Things":       "97things",
		"":                "",
		"!!!":             "",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := SearchKey(in); got != want {
				t.Fatalf("SearchKey(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

// The point of the whole mechanism: a query with spaces must match a filename
// that has none.
func TestSearchKeyMatchesRunTogetherFilenames(t *testing.T) {
	filename := SearchKey("appliedlinearalgebraandoptimizationusingmatlab.pdf")
	for _, query := range []string{"linear algebra", "Linear Algebra", "optimization", "matlab"} {
		if !strings.Contains(filename, SearchKey(query)) {
			t.Errorf("SearchKey(%q) = %q is not inside %q", query, SearchKey(query), filename)
		}
	}
	// And must not match something unrelated.
	if strings.Contains(filename, SearchKey("bayesian statistics")) {
		t.Error("an unrelated query matched")
	}
}

func TestStemKey(t *testing.T) {
	tests := map[string]string{
		"/library/done/97thingseveryjavaprogrammershouldknow.pdf": "97thingseveryjavaprogrammershouldknow",
		"/library/97thingseveryjavaprogrammershouldknow.epub":     "97thingseveryjavaprogrammershouldknow",
		"/library/97thingseveryjavaprogrammershouldknow.mobi":     "97thingseveryjavaprogrammershouldknow",
		"algebraessentials.epub":                                  "algebraessentials",
		"/a/b/The Art of Computer Programming.pdf":                "theartofcomputerprogramming",
		"/a/b/the-art-of-computer-programming.epub":               "theartofcomputerprogramming",
		`C:\books\Some_Book.epub`:                                 "somebook",
		"/library/no-extension":                                   "noextension",
		"":                                                        "",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := StemKey(in); got != want {
				t.Fatalf("StemKey(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

// The signal the merge rule relies on: the same book in three formats, in two
// different directories, reduces to one stem.
func TestStemKeyUnifiesFormatsAcrossDirectories(t *testing.T) {
	paths := []string{
		"/home/adam/Downloads/books/done/97thingseveryjavaprogrammershouldknow.pdf",
		"/home/adam/Downloads/books/97thingseveryjavaprogrammershouldknow.epub",
		"/home/adam/Downloads/books/97thingseveryjavaprogrammershouldknow.mobi",
	}
	want := StemKey(paths[0])
	for _, p := range paths {
		if got := StemKey(p); got != want {
			t.Errorf("StemKey(%q) = %q, want %q", p, got, want)
		}
	}
	if StemKey("/library/bayesianstatisticsthefunway.pdf") == want {
		t.Error("unrelated books share a stem")
	}
}
