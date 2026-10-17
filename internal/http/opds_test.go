// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

type fakeCatalogue struct {
	page     postgres.LibraryPage
	subjects []postgres.Subject
	files    map[uuid.UUID][]postgres.CatalogueFile
	lastQ    postgres.LibraryQuery
}

func (f *fakeCatalogue) Library(_ context.Context, q postgres.LibraryQuery) (postgres.LibraryPage, error) {
	f.lastQ = q
	return f.page, nil
}
func (f *fakeCatalogue) Subjects(context.Context, uuid.UUID) ([]postgres.Subject, error) {
	return f.subjects, nil
}
func (f *fakeCatalogue) Collections(context.Context, uuid.UUID) ([]postgres.CollectionSummary, error) {
	return nil, nil
}

func (f *fakeCatalogue) FilesForWorks(_ context.Context, _ uuid.UUID, ids []uuid.UUID) (map[uuid.UUID][]postgres.CatalogueFile, error) {
	out := map[uuid.UUID][]postgres.CatalogueFile{}
	for _, id := range ids {
		if files, ok := f.files[id]; ok {
			out[id] = files
		}
	}
	return out, nil
}

// signedIn puts a user on the request the way the middleware would.
func signedIn(r *http.Request, userID uuid.UUID) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userContextKey{}, userID))
}

// A reader app needs the acquisition link above all: without it the book is
// listed and cannot be opened.
func TestACatalogueEntryCanActuallyBeOpened(t *testing.T) {
	fileID, workID := uuid.New(), uuid.New()
	catalogue := &fakeCatalogue{page: postgres.LibraryPage{Items: []postgres.LibraryItem{{
		WorkID:     workID,
		Title:      "The Practice of Programming",
		Authors:    []string{"Brian Kernighan", "Rob Pike"},
		Formats:    []domain.Format{domain.FormatEPUB},
		BookFileID: fileID,
		CoverPath:  "/data/covers/x.jpg",
		AddedAt:    time.Now(),
		TagNames:   []string{"programming"},
	}}}}
	catalogue.files = map[uuid.UUID][]postgres.CatalogueFile{
		workID: {{ID: fileID, Format: "epub", Size: 1 << 20}},
	}
	h := &OPDSHandler{Store: catalogue}

	rec := httptest.NewRecorder()
	h.All(rec, signedIn(httptest.NewRequest(http.MethodGet, "/opds/all", nil), uuid.New()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "kind=acquisition") {
		t.Errorf("content type = %q, want an acquisition feed", ct)
	}

	var feed opdsFeed
	if err := xml.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("the feed is not valid XML: %v", err)
	}
	if len(feed.Entry) != 1 {
		t.Fatalf("entries = %d, want 1", len(feed.Entry))
	}

	entry := feed.Entry[0]
	if entry.ID != "urn:uuid:"+workID.String() {
		t.Errorf("id = %q, want a urn:uuid", entry.ID)
	}
	if len(entry.Author) != 2 {
		t.Errorf("authors = %+v, want both", entry.Author)
	}

	var acquisition, thumbnail string
	for _, link := range entry.Link {
		switch link.Rel {
		case relAcquisition:
			acquisition = link.Href
			if link.Type != "application/epub+zip" {
				t.Errorf("acquisition type = %q, want the EPUB media type", link.Type)
			}
		case relThumbnail:
			thumbnail = link.Href
		}
	}
	if acquisition != "/api/v1/books/"+fileID.String()+"/file" {
		t.Errorf("acquisition href = %q, want the book file", acquisition)
	}
	if thumbnail == "" {
		t.Error("no thumbnail, so a reader shows a shelf of blank rectangles")
	}
}

// A reader decides how to render a feed from what its own self link claims to
// be. A navigation feed calling itself an acquisition feed is shown as a shelf
// of entries that cannot be opened.
func TestAFeedSaysWhichKindItIs(t *testing.T) {
	h := &OPDSHandler{Store: &fakeCatalogue{}}

	rec := httptest.NewRecorder()
	h.Root(rec, signedIn(httptest.NewRequest(http.MethodGet, "/opds", nil), uuid.New()))

	var feed opdsFeed
	if err := xml.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, link := range feed.Link {
		if link.Rel == relSelf && !strings.Contains(link.Type, "kind=navigation") {
			t.Errorf("the root feed calls itself %q, want a navigation feed", link.Type)
		}
	}

	// And each entry says what it points AT, which is how a reader knows
	// whether tapping it opens a shelf or another menu.
	want := map[string]string{
		"urn:lexarii:opds:new":         "kind=acquisition",
		"urn:lexarii:opds:all":         "kind=acquisition",
		"urn:lexarii:opds:subjects":    "kind=navigation",
		"urn:lexarii:opds:collections": "kind=navigation",
	}
	for _, entry := range feed.Entry {
		kind, known := want[entry.ID]
		if !known {
			continue
		}
		if !strings.Contains(entry.Link[0].Type, kind) {
			t.Errorf("%s points at a %q, want %s", entry.ID, entry.Link[0].Type, kind)
		}
	}
}

// A book whose drive is unmounted must not be offered for download: a reader
// that follows the link gets an error instead of a book.
func TestAMissingBookIsNotOfferedForDownload(t *testing.T) {
	catalogue := &fakeCatalogue{page: postgres.LibraryPage{Items: []postgres.LibraryItem{{
		WorkID: uuid.New(), Title: "Gone", BookFileID: uuid.New(), Missing: true,
		Formats: []domain.Format{domain.FormatEPUB}, AddedAt: time.Now(),
	}}}}
	h := &OPDSHandler{Store: catalogue}

	rec := httptest.NewRecorder()
	h.All(rec, signedIn(httptest.NewRequest(http.MethodGet, "/opds/all", nil), uuid.New()))

	var feed opdsFeed
	if err := xml.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, link := range feed.Entry[0].Link {
		if link.Rel == relAcquisition {
			t.Error("a book that is not on disk was offered for download")
		}
	}
}

// A book held in three formats is three links. The format this server prefers
// to render is not the one somebody wants on their e-reader, and that choice
// is not this server's to make.
func TestEveryFormatIsOfferedSeparately(t *testing.T) {
	workID := uuid.New()
	epub, pdf, mobi := uuid.New(), uuid.New(), uuid.New()
	catalogue := &fakeCatalogue{
		page: postgres.LibraryPage{Items: []postgres.LibraryItem{{
			WorkID: workID, Title: "One Book, Three Files", AddedAt: time.Now(),
			BookFileID: epub,
		}}},
		files: map[uuid.UUID][]postgres.CatalogueFile{workID: {
			{ID: epub, Format: "epub"},
			{ID: mobi, Format: "mobi"},
			{ID: pdf, Format: "pdf"},
		}},
	}
	h := &OPDSHandler{Store: catalogue}

	rec := httptest.NewRecorder()
	h.All(rec, signedIn(httptest.NewRequest(http.MethodGet, "/opds/all", nil), uuid.New()))

	var feed opdsFeed
	if err := xml.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	types := map[string]string{}
	for _, link := range feed.Entry[0].Link {
		if link.Rel == relAcquisition {
			types[link.Type] = link.Href
		}
	}
	for _, want := range []string{
		"application/epub+zip",
		"application/pdf",
		"application/x-mobipocket-ebook",
	} {
		if types[want] == "" {
			t.Errorf("no acquisition link of type %q; a reader that can only open one format has nothing", want)
		}
	}
}

// A book lent to be read rather than handed over has no acquisition link, and
// says so instead of offering one that refuses.
func TestABookLentToReadIsNotOfferedForDownload(t *testing.T) {
	workID := uuid.New()
	catalogue := &fakeCatalogue{page: postgres.LibraryPage{Items: []postgres.LibraryItem{{
		WorkID: workID, Title: "Borrowed", BookFileID: uuid.New(), AddedAt: time.Now(),
	}}}}
	h := &OPDSHandler{Store: catalogue}

	rec := httptest.NewRecorder()
	h.All(rec, signedIn(httptest.NewRequest(http.MethodGet, "/opds/all", nil), uuid.New()))

	var feed opdsFeed
	if err := xml.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, link := range feed.Entry[0].Link {
		if link.Rel == relAcquisition {
			t.Error("a book that cannot be downloaded was offered for download")
		}
	}
}

// The catalogue is a read of somebody's whole library. RequireAuth treats
// everything outside /api as a static asset of the web client, which would
// have served this to anybody who asked.
func TestTheCatalogueIsNotServedToStrangers(t *testing.T) {
	guarded := RequireAuth(&auth.Sessions{Store: &sessionRecorder{
		hashes: map[string]auth.Session{}, devices: map[string]string{},
	}}, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an unauthenticated request reached the catalogue")
	}))

	for _, path := range []string{"/opds", "/opds/all", "/opds/subjects/go", "/opds/search?q=x"} {
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, rec.Code)
		}
	}
}

// And the web client itself must still load, or there is no page to sign in on.
func TestTheWebClientStillLoadsWithoutASession(t *testing.T) {
	var reached bool
	guarded := RequireAuth(&auth.Sessions{Store: &sessionRecorder{
		hashes: map[string]auth.Session{}, devices: map[string]string{},
	}}, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/signin", nil))
	if !reached {
		t.Fatalf("the sign-in page was refused: status %d", rec.Code)
	}
}

// basicChecker stands in for the real password check.
type basicChecker struct {
	userID uuid.UUID
	user   string
	pass   string
}

func (b basicChecker) AuthenticateBasic(_ context.Context, _ *http.Request, username, password string) (uuid.UUID, error) {
	if username != b.user || password != b.pass {
		return uuid.Nil, auth.ErrInvalidCredentials
	}
	return b.userID, nil
}

// The credentials an ebook reader knows how to send are the only ones it can
// send, so they have to work.
func TestAReaderAppSignsInWithAUsernameAndPassword(t *testing.T) {
	userID := uuid.New()
	var seen uuid.UUID
	guarded := RequireAuth(&auth.Sessions{Store: &sessionRecorder{
		hashes: map[string]auth.Session{}, devices: map[string]string{},
	}}, basicChecker{userID: userID, user: "adam", pass: "a-device-token"},
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen, _ = UserFrom(r.Context())
		}))

	r := httptest.NewRequest(http.MethodGet, "/opds", nil)
	r.SetBasicAuth("adam", "a-device-token")
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, r)

	if seen != userID {
		t.Fatalf("the request was not authenticated as %s: status %d", userID, rec.Code)
	}

	// And a wrong password is still a refusal.
	wrong := httptest.NewRequest(http.MethodGet, "/opds", nil)
	wrong.SetBasicAuth("adam", "not-the-token")
	rec = httptest.NewRecorder()
	seen = uuid.Nil
	guarded.ServeHTTP(rec, wrong)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a wrong password got status %d, want 401", rec.Code)
	}
}

// A reader asked to sign in needs to be TOLD to, in the one way it
// understands, or it shows an error instead of a password box.
func TestARefusedCatalogueAsksForCredentials(t *testing.T) {
	h := &OPDSHandler{Store: &fakeCatalogue{}}

	rec := httptest.NewRecorder()
	h.Root(rec, httptest.NewRequest(http.MethodGet, "/opds", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
	}
}

// Paging, so a reader can walk a library rather than being handed the first
// fifty and told that is all there is.
func TestALongShelfOffersTheNextPage(t *testing.T) {
	catalogue := &fakeCatalogue{page: postgres.LibraryPage{
		Items:      []postgres.LibraryItem{{WorkID: uuid.New(), Title: "One", AddedAt: time.Now()}},
		NextCursor: "the-next-page",
	}}
	h := &OPDSHandler{Store: catalogue}

	rec := httptest.NewRecorder()
	h.All(rec, signedIn(httptest.NewRequest(http.MethodGet, "/opds/all", nil), uuid.New()))

	var feed opdsFeed
	if err := xml.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	var next string
	for _, link := range feed.Link {
		if link.Rel == relNext {
			next = link.Href
		}
	}
	if !strings.Contains(next, "cursor=the-next-page") {
		t.Errorf("next = %q, want the cursor", next)
	}
}

// A search from a reader's own search box reaches the same query the shelf
// uses.
func TestSearchingFromAReaderReachesTheLibraryQuery(t *testing.T) {
	catalogue := &fakeCatalogue{}
	h := &OPDSHandler{Store: catalogue}

	rec := httptest.NewRecorder()
	h.Search(rec, signedIn(
		httptest.NewRequest(http.MethodGet, "/opds/search?q=kernighan", nil), uuid.New()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if catalogue.lastQ.Search != "kernighan" {
		t.Errorf("search = %q, want the reader's terms", catalogue.lastQ.Search)
	}
}
