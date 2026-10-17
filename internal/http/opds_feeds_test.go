// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

var errCatalogueDown = errors.New("catalogue store unavailable")

// opdsShelfStub is a catalogue store whose every answer, and every failure,
// a test chooses. It records the shelf queries it was asked.
type opdsShelfStub struct {
	pages       []postgres.LibraryPage
	subjects    []postgres.Subject
	collections []postgres.CollectionSummary
	files       map[uuid.UUID][]postgres.CatalogueFile

	libraryErr     error
	subjectsErr    error
	collectionsErr error
	filesErr       error

	queries []postgres.LibraryQuery
	viewers []uuid.UUID
}

func (s *opdsShelfStub) Library(_ context.Context, q postgres.LibraryQuery) (postgres.LibraryPage, error) {
	s.queries = append(s.queries, q)
	if s.libraryErr != nil {
		return postgres.LibraryPage{}, s.libraryErr
	}
	if len(s.pages) == 0 {
		return postgres.LibraryPage{}, nil
	}
	return s.pages[0], nil
}

func (s *opdsShelfStub) Subjects(_ context.Context, viewer uuid.UUID) ([]postgres.Subject, error) {
	s.viewers = append(s.viewers, viewer)
	return s.subjects, s.subjectsErr
}

func (s *opdsShelfStub) Collections(_ context.Context, viewer uuid.UUID) ([]postgres.CollectionSummary, error) {
	s.viewers = append(s.viewers, viewer)
	return s.collections, s.collectionsErr
}

func (s *opdsShelfStub) FilesForWorks(_ context.Context, _ uuid.UUID, ids []uuid.UUID) (map[uuid.UUID][]postgres.CatalogueFile, error) {
	if s.filesErr != nil {
		return nil, s.filesErr
	}
	out := map[uuid.UUID][]postgres.CatalogueFile{}
	for _, id := range ids {
		out[id] = s.files[id]
	}
	return out, nil
}

// opdsGet drives the real router, signed in, so path values are what the
// router extracts.
func opdsGet(t *testing.T, h *OPDSHandler, user uuid.UUID, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if user != uuid.Nil {
		req = signedIn(req, user)
	}
	rec := httptest.NewRecorder()
	(&Router{Health: &HealthHandler{}, OPDS: h}).Handler().ServeHTTP(rec, req)
	return rec
}

func opdsDecode(t *testing.T, rec *httptest.ResponseRecorder) opdsFeed {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var feed opdsFeed
	if err := xml.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("not a feed: %v; body = %s", err, rec.Body)
	}
	return feed
}

func opdsLinkTo(links []opdsLink, rel string) (opdsLink, bool) {
	for _, l := range links {
		if l.Rel == rel {
			return l, true
		}
	}
	return opdsLink{}, false
}

// Every feed refuses a stranger with the challenge that makes a reader app
// ask for a password.
func TestEveryCatalogueFeedChallengesAStranger(t *testing.T) {
	h := &OPDSHandler{Store: &opdsShelfStub{}}
	for _, target := range []string{
		"/opds", "/opds/new", "/opds/all", "/opds/search?q=x", "/opds/search.xml",
		"/opds/subjects", "/opds/subjects/maths", "/opds/collections", "/opds/collections/" + uuid.NewString(),
	} {
		t.Run(target, func(t *testing.T) {
			rec := opdsGet(t, h, uuid.Nil, target)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="Lexarii"` {
				t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
			}
		})
	}
}

// The newest-first and title-ordered feeds ask the shelf for what they say.
func TestTheNewAndAllFeedsOrderTheShelf(t *testing.T) {
	for _, tc := range []struct {
		target string
		sort   postgres.LibrarySort
	}{
		{"/opds/new", postgres.SortAddedDesc},
		{"/opds/all", postgres.SortTitleAsc},
	} {
		t.Run(tc.target, func(t *testing.T) {
			s := &opdsShelfStub{}
			user := uuid.New()
			feed := opdsDecode(t, opdsGet(t, &OPDSHandler{Store: s}, user, tc.target))
			if len(s.queries) != 1 || s.queries[0].Sort != tc.sort || s.queries[0].ViewerID != user ||
				s.queries[0].Limit != opdsPageSize {
				t.Errorf("queries = %+v, want one %s page for this viewer", s.queries, tc.sort)
			}
			if self, _ := opdsLinkTo(feed.Link, relSelf); self.Href != tc.target || self.Type != acquisitionType {
				t.Errorf("self = %+v", self)
			}
		})
	}
}

// A reader learns how to search from this document, then builds the URL
// itself.
func TestTheOpenSearchDescription(t *testing.T) {
	rec := opdsGet(t, &OPDSHandler{Store: &opdsShelfStub{}, Title: "Adam's books"}, uuid.New(), "/opds/search.xml")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/opensearchdescription+xml") {
		t.Errorf("content type = %q", ct)
	}
	var doc openSearch
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if doc.ShortName != "Adam's books" || len(doc.URL) != 1 ||
		doc.URL[0].Template != "/opds/search?q={searchTerms}" || doc.URL[0].Type != acquisitionType {
		t.Errorf("description = %+v", doc)
	}
}

func TestTheSubjectsFeed(t *testing.T) {
	s := &opdsShelfStub{subjects: []postgres.Subject{
		{Name: "Mathematics", Slug: "mathematics", Books: 48},
		{Name: "Type theory", Slug: "type theory", Books: 1},
	}}
	user := uuid.New()
	feed := opdsDecode(t, opdsGet(t, &OPDSHandler{Store: s}, user, "/opds/subjects"))

	if s.viewers[0] != user {
		t.Errorf("subjects read for %v, want the signed-in reader", s.viewers[0])
	}
	if len(feed.Entry) != 2 {
		t.Fatalf("entries = %+v", feed.Entry)
	}
	if feed.Entry[0].Content.Body != "48 books" || feed.Entry[1].Content.Body != "1 book" {
		t.Errorf("summaries = %q, %q; want counted, singular for one",
			feed.Entry[0].Content.Body, feed.Entry[1].Content.Body)
	}
	link := feed.Entry[1].Link[0]
	if link.Href != "/opds/subjects/"+url.PathEscape("type theory") || link.Type != acquisitionType {
		t.Errorf("link = %+v, want an escaped acquisition link", link)
	}
}

// Following a subject from that feed reaches a shelf filtered by its tag.
func TestASubjectIsAFilteredShelf(t *testing.T) {
	s := &opdsShelfStub{}
	feed := opdsDecode(t, opdsGet(t, &OPDSHandler{Store: s}, uuid.New(), "/opds/subjects/mathematics"))
	if len(s.queries) != 1 || s.queries[0].Tag != "mathematics" {
		t.Errorf("queries = %+v, want the tag", s.queries)
	}
	if feed.Title != "mathematics" {
		t.Errorf("title = %q", feed.Title)
	}
}

func TestTheCollectionsFeed(t *testing.T) {
	maths := domain.Collection{ID: uuid.New(), Name: "Maths"}
	s := &opdsShelfStub{collections: []postgres.CollectionSummary{{Collection: maths, Books: 2}}}
	feed := opdsDecode(t, opdsGet(t, &OPDSHandler{Store: s}, uuid.New(), "/opds/collections"))
	if len(feed.Entry) != 1 || feed.Entry[0].Title != "Maths" || feed.Entry[0].Content.Body != "2 books" ||
		feed.Entry[0].Link[0].Href != "/opds/collections/"+maths.ID.String() {
		t.Errorf("entries = %+v", feed.Entry)
	}

	// Following one reaches the shelf for that collection.
	s2 := &opdsShelfStub{}
	opdsDecode(t, opdsGet(t, &OPDSHandler{Store: s2}, uuid.New(), "/opds/collections/"+maths.ID.String()))
	if len(s2.queries) != 1 || s2.queries[0].CollectionID == nil || *s2.queries[0].CollectionID != maths.ID {
		t.Errorf("queries = %+v, want this collection", s2.queries)
	}

	// A collection id that is not one is simply not there.
	rec := opdsGet(t, &OPDSHandler{Store: &opdsShelfStub{}}, uuid.New(), "/opds/collections/nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("a bad collection id = %d, want 404", rec.Code)
	}
}

func TestACatalogueStoreFailureIsAnError(t *testing.T) {
	for _, tc := range []struct {
		target string
		stub   *opdsShelfStub
	}{
		{"/opds/subjects", &opdsShelfStub{subjectsErr: errCatalogueDown}},
		{"/opds/collections", &opdsShelfStub{collectionsErr: errCatalogueDown}},
		{"/opds/all", &opdsShelfStub{libraryErr: errCatalogueDown}},
		{"/opds/all", &opdsShelfStub{filesErr: errCatalogueDown,
			pages: []postgres.LibraryPage{{Items: []postgres.LibraryItem{{WorkID: uuid.New()}}}}}},
	} {
		t.Run(tc.target, func(t *testing.T) {
			rec := opdsGet(t, &OPDSHandler{Store: tc.stub}, uuid.New(), tc.target)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "unavailable") {
				t.Error("the cause leaked")
			}
		})
	}
}

// The next page of a search keeps the search terms, and the cursor it hands
// out is the one the next request asks the shelf for.
func TestASearchsNextPageKeepsTheTerms(t *testing.T) {
	s := &opdsShelfStub{pages: []postgres.LibraryPage{{
		Items:      []postgres.LibraryItem{{WorkID: uuid.New(), Title: "x"}},
		NextCursor: "next&page",
	}}}
	feed := opdsDecode(t, opdsGet(t, &OPDSHandler{Store: s}, uuid.New(), "/opds/search?q=type+theory"))
	next, ok := opdsLinkTo(feed.Link, relNext)
	if !ok {
		t.Fatal("no next link")
	}
	u, err := url.Parse(next.Href)
	if err != nil || u.Path != "/opds/search" || u.Query().Get("q") != "type theory" ||
		u.Query().Get("cursor") != "next&page" {
		t.Fatalf("next = %q, want the search terms and the cursor, escaped", next.Href)
	}

	opdsDecode(t, opdsGet(t, &OPDSHandler{Store: s}, uuid.New(), next.Href))
	if last := s.queries[len(s.queries)-1]; last.Cursor != "next&page" || last.Search != "type theory" {
		t.Errorf("the next page asked for cursor %q search %q", last.Cursor, last.Search)
	}
}

// An entry says what a shelf card says: the subtitle in the title, the
// series, and the media type of every format so a reader can pick.
func TestACatalogueEntryDescribesTheWork(t *testing.T) {
	two := 2.0
	work := uuid.New()
	files := []postgres.CatalogueFile{}
	formats := map[domain.Format]string{
		domain.FormatEPUB: "application/epub+zip",
		domain.FormatPDF:  "application/pdf",
		domain.FormatCBZ:  "application/vnd.comicbook+zip",
		domain.FormatCBR:  "application/vnd.comicbook-rar",
		domain.FormatMOBI: "application/x-mobipocket-ebook",
		domain.FormatAZW3: "application/vnd.amazon.ebook",
		"djvu":            "application/octet-stream",
	}
	want := map[string]string{}
	for f, mt := range formats {
		id := uuid.New()
		files = append(files, postgres.CatalogueFile{ID: id, Format: string(f)})
		want["/api/v1/books/"+id.String()+"/file"] = mt
	}
	s := &opdsShelfStub{
		pages: []postgres.LibraryPage{{Items: []postgres.LibraryItem{
			{WorkID: work, Title: "Dune", Subtitle: "Deluxe Edition", Series: "Dune Chronicles", SeriesIndex: &two,
				AddedAt: time.Now()},
			{WorkID: uuid.New(), Title: "Standalone", Series: "Loose", AddedAt: time.Now()},
		}}},
		files: map[uuid.UUID][]postgres.CatalogueFile{work: files},
	}
	feed := opdsDecode(t, opdsGet(t, &OPDSHandler{Store: s}, uuid.New(), "/opds/all"))

	entry := feed.Entry[0]
	if entry.Title != "Dune: Deluxe Edition" {
		t.Errorf("title = %q", entry.Title)
	}
	if entry.Content == nil || entry.Content.Body != "Dune Chronicles, book 2" {
		t.Errorf("content = %+v, want the series and number", entry.Content)
	}
	got := map[string]string{}
	for _, l := range entry.Link {
		if l.Rel == relAcquisition {
			got[l.Href] = l.Type
		}
	}
	if len(got) != len(want) {
		t.Fatalf("acquisition links = %v, want one per format", got)
	}
	for href, mt := range want {
		if got[href] != mt {
			t.Errorf("%s: type = %q, want %q", href, got[href], mt)
		}
	}
	if c := feed.Entry[1].Content; c == nil || c.Body != "Loose" {
		t.Errorf("an unnumbered series = %+v, want just its name", c)
	}
}

// A catalogue with no name configured calls itself Lexarii.
func TestTheCatalogueNamesItself(t *testing.T) {
	for _, tc := range []struct{ title, want string }{{"", "Lexarii"}, {"   ", "Lexarii"}, {"Home", "Home"}} {
		feed := opdsDecode(t, opdsGet(t, &OPDSHandler{Store: &opdsShelfStub{}, Title: tc.title}, uuid.New(), "/opds"))
		if feed.Title != tc.want || feed.Author == nil || feed.Author.Name != tc.want {
			t.Errorf("Title %q: feed title %q author %+v, want %q", tc.title, feed.Title, feed.Author, tc.want)
		}
	}
}
