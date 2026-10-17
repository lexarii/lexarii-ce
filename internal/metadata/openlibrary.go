// SPDX-License-Identifier: AGPL-3.0-or-later

// Package metadata looks a book up in an external catalogue.
//
// This is the one part of Lexarii that talks to a stranger, so it is off
// unless somebody turns it on. What it sends - an ISBN, or a title and an
// author - is a description of a book on the user's shelf, and a list of those
// is a personal thing. "No telemetry, no phoning home" is a promise this
// package could quietly break, so it does nothing at all until
// LEXARII_METADATA_BASE_URL is set.
//
// OpenLibrary is the default because its data is CC0, its API needs no key,
// and it can be self-hosted - so somebody who does not want to talk to
// archive.org can point this at their own copy.
//
// What comes back is a claim, not a fact. It lands as SourceExternal, which
// ranks below the file's own metadata and above a model's guess: a publisher
// who wrote the year into the EPUB knows better than a catalogue, and a
// catalogue knows better than a model reading a title page.
package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotConfigured reports a lookup attempted with no catalogue configured.
var ErrNotConfigured = errors.New("no metadata catalogue is configured")

// ErrNotFound reports a book the catalogue does not have.
var ErrNotFound = errors.New("the catalogue has no record of this book")

// Book is what a catalogue knows, reduced to what this library stores.
//
// Every field is optional. A partial answer is normal and useful: most of
// these books need a year and a publisher and have everything else already.
type Book struct {
	Title     string
	Subtitle  string
	Authors   []string
	Publisher string
	Year      int
	ISBN13    string
	ISBN10    string
	PageCount int
	// Source is the catalogue's own identifier for the record, so a lookup can
	// be traced back to what answered it.
	Source string
}

// Empty reports a result with nothing worth recording.
func (b Book) Empty() bool {
	return b.Title == "" && b.Publisher == "" && b.Year == 0 &&
		b.ISBN13 == "" && b.ISBN10 == "" && b.PageCount == 0 && len(b.Authors) == 0
}

// Client queries an OpenLibrary-compatible catalogue.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// UserAgent identifies this installation to the catalogue. OpenLibrary
	// asks for one and rate-limits anonymous traffic harder.
	UserAgent string
}

// New builds a client, or nil when no catalogue is configured.
//
// Nil rather than an error: a library with no catalogue is the default and is
// working correctly, exactly as it is with no model.
func New(baseURL, userAgent string, timeout time.Duration) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if userAgent == "" {
		userAgent = "Lexarii (+https://scm.poniatowski.dev/adam/lexarii)"
	}
	return &Client{
		BaseURL:   baseURL,
		HTTP:      &http.Client{Timeout: timeout},
		UserAgent: userAgent,
	}
}

// ByISBN looks a book up by its ISBN, which is the only exact question here.
//
// A title search guesses; an ISBN names one printing. When a book has one this
// is always the right call.
func (c *Client) ByISBN(ctx context.Context, isbn string) (Book, error) {
	if c == nil {
		return Book{}, ErrNotConfigured
	}
	isbn = normaliseISBN(isbn)
	if isbn == "" {
		return Book{}, fmt.Errorf("%q is not an ISBN", isbn)
	}

	// The read API answers with the edition record keyed by the query.
	q := url.Values{
		"bibkeys": {"ISBN:" + isbn},
		"format":  {"json"},
		"jscmd":   {"data"},
	}
	var out map[string]olRecord
	if err := c.get(ctx, "/api/books?"+q.Encode(), &out); err != nil {
		return Book{}, err
	}
	record, ok := out["ISBN:"+isbn]
	if !ok {
		return Book{}, ErrNotFound
	}

	book := record.book()
	// The ISBN asked for is known to be this book's, whatever the record says.
	if len(isbn) == 13 {
		book.ISBN13 = isbn
	} else {
		book.ISBN10 = isbn
	}
	return book, nil
}

// ByTitle searches by title and author, which is a guess and is treated as one.
//
// Only an unambiguous hit is returned: the search has to agree about the title
// and, when one is given, about an author. A catalogue's first result for a
// common title is a different book often enough that taking it would write
// wrong years into a library.
func (c *Client) ByTitle(ctx context.Context, title string, authors []string) (Book, error) {
	if c == nil {
		return Book{}, ErrNotConfigured
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return Book{}, fmt.Errorf("no title to search for")
	}

	q := url.Values{
		"title":  {title},
		"limit":  {"5"},
		"fields": {"title,subtitle,author_name,first_publish_year,publisher,isbn,number_of_pages_median,key"},
	}
	if len(authors) > 0 && strings.TrimSpace(authors[0]) != "" {
		q.Set("author", strings.TrimSpace(authors[0]))
	}

	var out olSearch
	if err := c.get(ctx, "/search.json?"+q.Encode(), &out); err != nil {
		return Book{}, err
	}

	for _, doc := range out.Docs {
		if !sameTitle(title, doc.Title) {
			continue
		}
		if len(authors) > 0 && !sharesAuthor(authors, doc.AuthorName) {
			continue
		}
		return doc.book(), nil
	}
	return Book{}, ErrNotFound
}

func (c *Client) get(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("building the catalogue request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("asking the catalogue: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("the catalogue is rate-limiting this installation")
	case resp.StatusCode >= 400:
		return fmt.Errorf("the catalogue answered %s", resp.Status)
	}

	// Bounded: this is a stranger's response being decoded into memory on a
	// machine in somebody's house.
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 2<<20)).Decode(into); err != nil {
		return fmt.Errorf("reading the catalogue's answer: %w", err)
	}
	return nil
}

// olRecord is the shape of an /api/books record.
type olRecord struct {
	Title         string `json:"title"`
	Subtitle      string `json:"subtitle"`
	URL           string `json:"url"`
	NumberOfPages int    `json:"number_of_pages"`
	PublishDate   string `json:"publish_date"`
	Authors       []struct {
		Name string `json:"name"`
	} `json:"authors"`
	Publishers []struct {
		Name string `json:"name"`
	} `json:"publishers"`
	Identifiers struct {
		ISBN10 []string `json:"isbn_10"`
		ISBN13 []string `json:"isbn_13"`
	} `json:"identifiers"`
}

func (r olRecord) book() Book {
	b := Book{
		Title:     strings.TrimSpace(r.Title),
		Subtitle:  strings.TrimSpace(r.Subtitle),
		PageCount: r.NumberOfPages,
		Year:      yearIn(r.PublishDate),
		Source:    strings.TrimSpace(r.URL),
	}
	for _, a := range r.Authors {
		if name := strings.TrimSpace(a.Name); name != "" {
			b.Authors = append(b.Authors, name)
		}
	}
	if len(r.Publishers) > 0 {
		b.Publisher = strings.TrimSpace(r.Publishers[0].Name)
	}
	if len(r.Identifiers.ISBN13) > 0 {
		b.ISBN13 = normaliseISBN(r.Identifiers.ISBN13[0])
	}
	if len(r.Identifiers.ISBN10) > 0 {
		b.ISBN10 = normaliseISBN(r.Identifiers.ISBN10[0])
	}
	return b
}

// olSearch is the shape of /search.json.
type olSearch struct {
	Docs []olDoc `json:"docs"`
}

type olDoc struct {
	Title      string   `json:"title"`
	Subtitle   string   `json:"subtitle"`
	AuthorName []string `json:"author_name"`
	FirstYear  int      `json:"first_publish_year"`
	Publisher  []string `json:"publisher"`
	ISBN       []string `json:"isbn"`
	Pages      int      `json:"number_of_pages_median"`
	Key        string   `json:"key"`
}

func (d olDoc) book() Book {
	b := Book{
		Title:     strings.TrimSpace(d.Title),
		Subtitle:  strings.TrimSpace(d.Subtitle),
		Authors:   d.AuthorName,
		Year:      d.FirstYear,
		PageCount: d.Pages,
		Source:    strings.TrimSpace(d.Key),
	}
	if len(d.Publisher) > 0 {
		b.Publisher = strings.TrimSpace(d.Publisher[0])
	}
	// A search result lists every ISBN of every printing. Taking one at random
	// would attach a paperback's number to a hardback, so only the length is
	// used to sort them and the first of each is kept.
	for _, raw := range d.ISBN {
		isbn := normaliseISBN(raw)
		if len(isbn) == 13 && b.ISBN13 == "" {
			b.ISBN13 = isbn
		}
		if len(isbn) == 10 && b.ISBN10 == "" {
			b.ISBN10 = isbn
		}
	}
	return b
}

// normaliseISBN strips the punctuation people write ISBNs with.
func normaliseISBN(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		if (r >= '0' && r <= '9') || r == 'X' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) != 10 && len(out) != 13 {
		return ""
	}
	return out
}

// yearIn finds a four-digit year in a publication date written any of the ways
// a catalogue writes them: "1968", "March 1968", "1968-03-01".
func yearIn(s string) int {
	for i := 0; i+4 <= len(s); i++ {
		chunk := s[i : i+4]
		if year, err := strconv.Atoi(chunk); err == nil && year >= 1400 && year <= 2200 {
			return year
		}
	}
	return 0
}

// sameTitle compares titles the way a person would: ignoring case,
// punctuation, a leading article, and a subtitle.
//
// The subtitle is the part that matters here. A catalogue records "Head First
// Agile: A Brain-Friendly Guide to Agile Principles, Ideas, and Real-World
// Practices" where a book's own metadata says "Head First Agile", and
// demanding exact equality rejected the right record for that book and for
// most of the others tried - 20 misses in the first 40 lookups against a real
// library, every one of them a book the catalogue plainly had.
//
// The main title is what comes before the first colon, which is where
// publishers put the break. Compared for EQUALITY and not as a prefix: a
// prefix rule matches "Linear Algebra" against "Linear Algebra Done Right",
// which is somebody else's book, and writing its year into this library would
// be worse than leaving the field empty.
func sameTitle(a, b string) bool {
	ka, kb := titleKey(a), titleKey(b)
	if ka == "" {
		return false
	}
	if ka == kb {
		return true
	}
	return titleKey(mainTitle(a)) == titleKey(mainTitle(b))
}

// mainTitle is a title without its subtitle.
func mainTitle(s string) string {
	if before, _, found := strings.Cut(s, ":"); found {
		return strings.TrimSpace(before)
	}
	return s
}

func titleKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))

	// The article goes first, while the words are still words. Stripping it
	// after folding takes the "a" off "Art of Computer Programming" and leaves
	// "rtofcomputerprogramming", which matches nothing.
	for _, article := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(s, article) {
			s = s[len(article):]
			break
		}
	}

	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 128:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sharesAuthor reports whether the two lists name anybody in common.
//
// Surnames, because a catalogue writes "Donald E. Knuth" where an EPUB wrote
// "Knuth, Donald E." and neither is wrong.
func sharesAuthor(ours, theirs []string) bool {
	if len(theirs) == 0 {
		return false
	}
	want := map[string]bool{}
	for _, name := range ours {
		if key := surnameKey(name); key != "" {
			want[key] = true
		}
	}
	for _, name := range theirs {
		if want[surnameKey(name)] {
			return true
		}
	}
	return false
}

func surnameKey(name string) string {
	name = strings.TrimSpace(name)
	if before, _, found := strings.Cut(name, ","); found {
		return titleKey(before)
	}
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return ""
	}
	return titleKey(fields[len(fields)-1])
}
