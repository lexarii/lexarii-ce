// SPDX-License-Identifier: AGPL-3.0-or-later

package metadata

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return New(server.URL, "", time.Second)
}

// A library with no catalogue configured is the default and is working
// correctly, exactly as one with no model is.
func TestNoCatalogueIsNotAnError(t *testing.T) {
	if c := New("", "", 0); c != nil {
		t.Fatalf("a client was built with no base URL: %+v", c)
	}
	var c *Client
	if _, err := c.ByISBN(t.Context(), "9780201896831"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}

func TestByISBN(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("bibkeys"); got != "ISBN:9780201896831" {
			t.Errorf("bibkeys = %q", got)
		}
		// OpenLibrary asks for a User-Agent and rate-limits harder without one.
		if r.Header.Get("User-Agent") == "" {
			t.Error("no User-Agent sent")
		}
		_, _ = w.Write([]byte(`{"ISBN:9780201896831":{
			"title":"The Art of Computer Programming",
			"subtitle":"Fundamental Algorithms",
			"url":"https://openlibrary.org/books/OL1M",
			"number_of_pages":650,
			"publish_date":"March 1968",
			"authors":[{"name":"Donald E. Knuth"}],
			"publishers":[{"name":"Addison-Wesley"}],
			"identifiers":{"isbn_10":["0-201-89683-4"],"isbn_13":["978-0-201-89683-1"]}
		}}`))
	})

	book, err := c.ByISBN(t.Context(), "978-0-201-89683-1")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if book.Title != "The Art of Computer Programming" || book.Subtitle != "Fundamental Algorithms" {
		t.Errorf("title/subtitle = %q / %q", book.Title, book.Subtitle)
	}
	if book.Publisher != "Addison-Wesley" || book.Year != 1968 || book.PageCount != 650 {
		t.Errorf("publisher/year/pages = %q / %d / %d", book.Publisher, book.Year, book.PageCount)
	}
	// Punctuation stripped, both forms kept.
	if book.ISBN13 != "9780201896831" || book.ISBN10 != "0201896834" {
		t.Errorf("isbn13/10 = %q / %q", book.ISBN13, book.ISBN10)
	}
	if len(book.Authors) != 1 || book.Authors[0] != "Donald E. Knuth" {
		t.Errorf("authors = %v", book.Authors)
	}
}

func TestByISBNNotFound(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		// OpenLibrary answers 200 with an empty object rather than 404.
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := c.ByISBN(t.Context(), "9780201896831"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// A title search is a guess, and a catalogue's first result for a common title
// is a different book often enough that taking it would write wrong years into
// somebody's library.
func TestByTitleRefusesAMismatch(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"docs":[
			{"title":"Programming Rust: A Completely Different Book","author_name":["Someone Else"],"first_publish_year":2001},
			{"title":"Programming Ruby","author_name":["Dave Thomas"],"first_publish_year":2004}
		]}`))
	})
	if _, err := c.ByTitle(t.Context(), "Programming Rust", []string{"Jim Blandy"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound - neither result is this book", err)
	}
}

// The catalogue writes "Donald E. Knuth" where an EPUB wrote "Knuth, Donald
// E.". Neither is wrong, and a comparison that calls them different people
// rejects every correct answer.
func TestByTitleMatchesAcrossNameOrder(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("title"); got != "The Art of Computer Programming" {
			t.Errorf("title = %q", got)
		}
		_, _ = w.Write([]byte(`{"docs":[{
			"title":"The Art of Computer Programming",
			"author_name":["Donald E. Knuth"],
			"first_publish_year":1968,
			"publisher":["Addison-Wesley"],
			"isbn":["0201896834","9780201896831"],
			"number_of_pages_median":650,
			"key":"/works/OL1W"
		}]}`))
	})

	book, err := c.ByTitle(t.Context(), "The Art of Computer Programming", []string{"Knuth, Donald E."})
	if err != nil {
		t.Fatalf("ByTitle: %v", err)
	}
	if book.Year != 1968 || book.Publisher != "Addison-Wesley" {
		t.Errorf("year/publisher = %d / %q", book.Year, book.Publisher)
	}
	if book.ISBN13 != "9780201896831" || book.ISBN10 != "0201896834" {
		t.Errorf("isbn13/10 = %q / %q", book.ISBN13, book.ISBN10)
	}
}

// Titles differ in punctuation and articles between catalogues, and a person
// would call these the same book.
func TestTitleComparison(t *testing.T) {
	same := [][2]string{
		// A leading article is dropped as a WORD. Dropping it after folding
		// takes the "a" off "Art of Computer Programming".
		{"The Art of Computer Programming", "Art of Computer Programming"},
		{"C# 8.0 in a Nutshell", "C#8.0 in a Nutshell"},
		{"Algorithms, Fourth Edition", "Algorithms Fourth Edition"},
	}
	for _, pair := range same {
		if !sameTitle(pair[0], pair[1]) {
			t.Errorf("%q and %q read as different books", pair[0], pair[1])
		}
	}
	if sameTitle("Programming Rust", "Programming Ruby") {
		t.Error("two different books read as the same")
	}
	if sameTitle("", "") {
		t.Error("two empty titles matched")
	}
}

func TestYearIn(t *testing.T) {
	for in, want := range map[string]int{
		"1968": 1968, "March 1968": 1968, "1968-03-01": 1968,
		"c. 2004": 2004, "": 0, "no year here": 0, "12": 0,
	} {
		if got := yearIn(in); got != want {
			t.Errorf("yearIn(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestNormaliseISBN(t *testing.T) {
	for in, want := range map[string]string{
		"978-0-201-89683-1": "9780201896831",
		"0 201 89683 4":     "0201896834",
		"080442957X":        "080442957X",
		"12345":             "",
		"":                  "",
	} {
		if got := normaliseISBN(in); got != want {
			t.Errorf("normaliseISBN(%q) = %q, want %q", in, got, want)
		}
	}
}

// A catalogue is a stranger. Its answer is decoded into memory on a machine in
// somebody's house, so it is bounded rather than trusted.
func TestAnEnormousAnswerIsRefused(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"docs":[{"title":"` + strings.Repeat("x", 3<<20) + `"}]}`))
	})
	if _, err := c.ByTitle(t.Context(), "anything", nil); err == nil {
		t.Error("a 3MB answer was accepted")
	}
}

func TestRateLimitIsSaidPlainly(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.ByISBN(t.Context(), "9780201896831")
	if err == nil || !strings.Contains(err.Error(), "rate-limiting") {
		t.Errorf("err = %v", err)
	}
}

// A catalogue writes the subtitle into the title and a book's own metadata
// usually does not. Demanding exact equality rejected the right record for
// most books in a real library.
func TestATitleMatchesItsOwnSubtitledForm(t *testing.T) {
	matching := [][2]string{
		{"Head First Agile", "Head First Agile: A Brain-Friendly Guide to Agile Principles, Ideas, and Real-World Practices"},
		{"The Go Programming Language", "The Go Programming Language"},
		// Either way round: ours may be the long one.
		{"Fluent Python: Clear, Concise, and Effective Programming", "Fluent Python"},
		// Still ignoring case, punctuation and a leading article.
		{"the pragmatic programmer", "The Pragmatic Programmer: Your Journey to Mastery"},
	}
	for _, pair := range matching {
		if !sameTitle(pair[0], pair[1]) {
			t.Errorf("sameTitle(%q, %q) = false, want true", pair[0], pair[1])
		}
	}

	// And the thing a prefix rule would get wrong: a different book whose
	// name begins with this one's. Writing its year into the library would be
	// worse than leaving the field empty.
	different := [][2]string{
		{"Linear Algebra", "Linear Algebra Done Right"},
		{"Refactoring", "Refactoring to Patterns"},
		{"Python Cookbook", "Python Cookbook Recipes for Mastering Python 3"},
	}
	for _, pair := range different {
		if sameTitle(pair[0], pair[1]) {
			t.Errorf("sameTitle(%q, %q) = true, want false: that is a different book", pair[0], pair[1])
		}
	}
}
