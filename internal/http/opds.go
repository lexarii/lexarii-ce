// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// OPDS serves the library as a catalogue any ebook reader can open.
//
// OPDS is the one thing every reader app already speaks: KOReader on a Kobo,
// Librera and Moon+ on Android, Thorium and Foliate on a desktop. Without it a
// self-hosted library is readable only through its own web page and its own
// client, which is the most common complaint made about every program in this
// category - and it means somebody can read their library on a phone today,
// before any Android client of ours exists.
//
// Version 1.2, which is Atom XML, rather than the newer JSON 2.0: 1.2 is what
// the readers people actually own can open, and a catalogue nothing can read
// is not a catalogue.
type OPDSHandler struct {
	Store OPDSStore
	// Title names this library in a reader's catalogue list, where it sits
	// beside half a dozen others.
	Title string
}

// OPDSStore is the reading a catalogue needs. The same queries the web shelf
// uses, because a catalogue is a shelf with a different skin.
type OPDSStore interface {
	Library(ctx context.Context, q postgres.LibraryQuery) (postgres.LibraryPage, error)
	Subjects(ctx context.Context, viewer uuid.UUID) ([]postgres.Subject, error)
	Collections(ctx context.Context, userID uuid.UUID) ([]postgres.CollectionSummary, error)
	FilesForWorks(ctx context.Context, viewer uuid.UUID, workIDs []uuid.UUID) (map[uuid.UUID][]postgres.CatalogueFile, error)
}

// opdsPageSize is how many books a feed carries.
//
// Readers page with the `next` link, and a phone parsing XML over a home
// connection would rather do it fifty at a time than wait for a library.
const opdsPageSize = 50

const (
	navigationType  = "application/atom+xml;profile=opds-catalog;kind=navigation"
	acquisitionType = "application/atom+xml;profile=opds-catalog;kind=acquisition"
	relAcquisition  = "http://opds-spec.org/acquisition"
	relImage        = "http://opds-spec.org/image"
	relThumbnail    = "http://opds-spec.org/image/thumbnail"
	relSearch       = "search"
	relSelf         = "self"
	relStart        = "start"
	relNext         = "next"
)

// Root serves GET /opds: the catalogue's front page.
func (h *OPDSHandler) Root(w http.ResponseWriter, r *http.Request) {
	if _, ok := opdsUser(w, r); !ok {
		return
	}

	feed := h.feed("urn:lexarii:opds:root", h.title(), "/opds", navigationType)
	feed.Link = append(feed.Link, searchLink())

	// Newest first is the front page on purpose: the book somebody wants is
	// usually the one they added this week.
	feed.Entry = []opdsEntry{
		h.navEntry("new", "Recently added", "The books that arrived most recently", "/opds/new", acquisitionType),
		h.navEntry("all", "All books", "Everything on the shelf, by title", "/opds/all", acquisitionType),
		h.navEntry("subjects", "Subjects", "What the library is about", "/opds/subjects", navigationType),
		h.navEntry("collections", "Collections", "The shelves you made yourself", "/opds/collections", navigationType),
	}
	writeFeed(w, navigationType, feed)
}

// New serves GET /opds/new.
func (h *OPDSHandler) New(w http.ResponseWriter, r *http.Request) {
	h.books(w, r, "new", "Recently added", postgres.LibraryQuery{Sort: postgres.SortAddedDesc})
}

// All serves GET /opds/all.
func (h *OPDSHandler) All(w http.ResponseWriter, r *http.Request) {
	h.books(w, r, "all", "All books", postgres.LibraryQuery{Sort: postgres.SortTitleAsc})
}

// Search serves GET /opds/search?q=, and the OpenSearch document that tells a
// reader the shape of that URL.
func (h *OPDSHandler) Search(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	h.books(w, r, "search:"+query, "Search: "+query, postgres.LibraryQuery{
		Search: query,
		Sort:   postgres.SortTitleAsc,
	})
}

// SearchDescription serves GET /opds/search.xml.
//
// A reader fetches this to learn how to search, then builds the URL itself -
// which is why the search box in a reader app works at all.
func (h *OPDSHandler) SearchDescription(w http.ResponseWriter, r *http.Request) {
	if _, ok := opdsUser(w, r); !ok {
		return
	}

	doc := openSearch{
		XMLNS:       "http://a9.com/-/spec/opensearch/1.1/",
		ShortName:   h.title(),
		Description: "Search " + h.title(),
		URL: []openSearchURL{{
			Type:     acquisitionType,
			Template: "/opds/search?q={searchTerms}",
		}},
	}
	w.Header().Set("Content-Type", "application/opensearchdescription+xml;charset=utf-8")
	writeXML(w, doc)
}

// Subjects serves GET /opds/subjects, a navigation feed of what the library is
// about.
func (h *OPDSHandler) Subjects(w http.ResponseWriter, r *http.Request) {
	userID, ok := opdsUser(w, r)
	if !ok {
		return
	}

	subjects, err := h.Store.Subjects(r.Context(), userID)
	if err != nil {
		respondError(w, err)
		return
	}

	feed := h.feed("urn:lexarii:opds:subjects", "Subjects", "/opds/subjects", navigationType)
	for _, s := range subjects {
		feed.Entry = append(feed.Entry, h.navEntry(
			"subject:"+s.Slug, s.Name,
			fmt.Sprintf("%d book%s", s.Books, plural(s.Books)),
			"/opds/subjects/"+url.PathEscape(s.Slug), acquisitionType))
	}
	writeFeed(w, navigationType, feed)
}

// Subject serves GET /opds/subjects/{slug}.
func (h *OPDSHandler) Subject(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	h.books(w, r, "subject:"+slug, slug, postgres.LibraryQuery{
		Tag:  slug,
		Sort: postgres.SortTitleAsc,
	})
}

// Collections serves GET /opds/collections.
func (h *OPDSHandler) Collections(w http.ResponseWriter, r *http.Request) {
	userID, ok := opdsUser(w, r)
	if !ok {
		return
	}

	collections, err := h.Store.Collections(r.Context(), userID)
	if err != nil {
		respondError(w, err)
		return
	}

	feed := h.feed("urn:lexarii:opds:collections", "Collections", "/opds/collections", navigationType)
	for _, c := range collections {
		feed.Entry = append(feed.Entry, h.navEntry(
			"collection:"+c.Collection.ID.String(), c.Collection.Name,
			fmt.Sprintf("%d book%s", c.Books, plural(c.Books)),
			"/opds/collections/"+c.Collection.ID.String(), acquisitionType))
	}
	writeFeed(w, navigationType, feed)
}

// Collection serves GET /opds/collections/{id}.
func (h *OPDSHandler) Collection(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "no such collection", http.StatusNotFound)
		return
	}
	h.books(w, r, "collection:"+id.String(), "Collection", postgres.LibraryQuery{
		CollectionID: &id,
		Sort:         postgres.SortTitleAsc,
	})
}

// books renders one page of an acquisition feed.
func (h *OPDSHandler) books(w http.ResponseWriter, r *http.Request, id, title string, query postgres.LibraryQuery) {
	userID, ok := opdsUser(w, r)
	if !ok {
		return
	}

	query.ViewerID = userID
	query.Limit = opdsPageSize
	query.Cursor = strings.TrimSpace(r.URL.Query().Get("cursor"))

	page, err := h.Store.Library(r.Context(), query)
	if err != nil {
		respondError(w, err)
		return
	}

	self := r.URL.RequestURI()
	feed := h.feed("urn:lexarii:opds:"+id, title, self, acquisitionType)
	feed.Link = append(feed.Link, searchLink())

	// Paging, so a reader can walk a library of any size rather than being
	// handed the first fifty and told that is all there is.
	if page.NextCursor != "" {
		feed.Link = append(feed.Link, opdsLink{
			Rel:  relNext,
			Type: acquisitionType,
			Href: withCursor(r.URL.Path, r.URL.Query().Get("q"), page.NextCursor),
		})
	}

	// Every format each book is held in, in ONE query for the whole page. A
	// reader app should be offered the choice: the format this server prefers
	// to render is not the one somebody wants on their e-reader.
	workIDs := make([]uuid.UUID, 0, len(page.Items))
	for _, item := range page.Items {
		workIDs = append(workIDs, item.WorkID)
	}
	files, err := h.Store.FilesForWorks(r.Context(), userID, workIDs)
	if err != nil {
		respondError(w, err)
		return
	}

	for _, item := range page.Items {
		feed.Entry = append(feed.Entry, h.bookEntry(item, files[item.WorkID]))
	}
	writeFeed(w, acquisitionType, feed)
}

// bookEntry is one book as a reader sees it.
func (h *OPDSHandler) bookEntry(item postgres.LibraryItem, files []postgres.CatalogueFile) opdsEntry {
	entry := opdsEntry{
		ID:      "urn:uuid:" + item.WorkID.String(),
		Title:   item.Title,
		Updated: item.AddedAt.UTC().Format(time.RFC3339),
	}
	if item.Subtitle != "" {
		entry.Title = item.Title + ": " + item.Subtitle
	}
	for _, name := range item.Authors {
		entry.Author = append(entry.Author, opdsAuthor{Name: name})
	}
	for _, tag := range item.TagNames {
		entry.Category = append(entry.Category, opdsCategory{Term: tag, Label: tag})
	}

	// The acquisition links: the one thing an entry is FOR. Without them a
	// reader shows the book and cannot open it.
	//
	// One per format the viewer may actually have. A reader that can only open
	// EPUB picks the EPUB; somebody who wants the PDF for its figures gets the
	// PDF. Offering only this server's preferred format would make that choice
	// for them, and it is not this server's to make.
	for _, file := range files {
		entry.Link = append(entry.Link, opdsLink{
			Rel:  relAcquisition,
			Type: mediaTypeForFormat(file.Format),
			Href: "/api/v1/books/" + file.ID.String() + "/file",
		})
	}
	// A book whose only file cannot be downloaded - lent to be read, not
	// handed over - is listed with no acquisition link rather than with one
	// that resolves to a refusal.
	if len(files) == 0 && item.BookFileID != uuid.Nil && !item.Missing {
		entry.Content = &opdsContent{
			Type: "text",
			Body: "Readable in Lexarii; this copy cannot be downloaded.",
		}
	}
	if item.CoverPath != "" && item.BookFileID != uuid.Nil {
		cover := "/api/v1/books/" + item.BookFileID.String() + "/cover"
		entry.Link = append(entry.Link,
			opdsLink{Rel: relImage, Type: "image/jpeg", Href: cover},
			opdsLink{Rel: relThumbnail, Type: "image/jpeg", Href: cover})
	}

	if series := seriesNote(item); series != "" {
		entry.Content = &opdsContent{Type: "text", Body: series}
	}
	return entry
}

// seriesNote is the one thing a shelf card says that a title does not.
func seriesNote(item postgres.LibraryItem) string {
	if item.Series == "" {
		return ""
	}
	if item.SeriesIndex == nil {
		return item.Series
	}
	return fmt.Sprintf("%s, book %g", item.Series, *item.SeriesIndex)
}

// mediaTypeForFormat names a format so a reader knows what it is fetching,
// and so it can decline what it cannot open.
func mediaTypeForFormat(format string) string {
	switch domain.Format(format) {
	case domain.FormatEPUB:
		return "application/epub+zip"
	case domain.FormatPDF:
		return "application/pdf"
	case domain.FormatCBZ:
		return "application/vnd.comicbook+zip"
	case domain.FormatCBR:
		return "application/vnd.comicbook-rar"
	case domain.FormatMOBI:
		return "application/x-mobipocket-ebook"
	case domain.FormatAZW3:
		return "application/vnd.amazon.ebook"
	default:
		return "application/octet-stream"
	}
}

func (h *OPDSHandler) title() string {
	if strings.TrimSpace(h.Title) == "" {
		return "Lexarii"
	}
	return h.Title
}

// feed builds an empty feed. The kind is not decoration: a reader reads the
// self link's type to decide whether it is looking at a list of books or a
// list of places to go, and a navigation feed that calls itself an
// acquisition feed is rendered as a shelf of unopenable entries.
func (h *OPDSHandler) feed(id, title, self, kind string) *opdsFeed {
	return &opdsFeed{
		XMLNS:     "http://www.w3.org/2005/Atom",
		XMLNSOPDS: "http://opds-spec.org/2010/catalog",
		ID:        id,
		Title:     title,
		// The feed's own timestamp, not the library's. A reader uses it to
		// decide whether to re-fetch, and a stale one is worse than an honest
		// "just now" for a catalogue assembled per request.
		Updated: time.Now().UTC().Format(time.RFC3339),
		Author:  &opdsAuthor{Name: h.title()},
		Link: []opdsLink{
			{Rel: relSelf, Type: kind, Href: self},
			{Rel: relStart, Type: navigationType, Href: "/opds"},
		},
	}
}

func (h *OPDSHandler) navEntry(id, title, summary, href, kind string) opdsEntry {
	return opdsEntry{
		ID:      "urn:lexarii:opds:" + id,
		Title:   title,
		Updated: time.Now().UTC().Format(time.RFC3339),
		Content: &opdsContent{Type: "text", Body: summary},
		Link:    []opdsLink{{Rel: "subsection", Type: kind, Href: href}},
	}
}

func searchLink() opdsLink {
	return opdsLink{
		Rel:  relSearch,
		Type: "application/opensearchdescription+xml",
		Href: "/opds/search.xml",
	}
}

// withCursor builds the next page's address, keeping the search terms.
func withCursor(path, q, cursor string) string {
	out := path + "?cursor=" + url.QueryEscape(cursor)
	if q != "" {
		out += "&q=" + url.QueryEscape(q)
	}
	return out
}

// opdsUser is the authenticated reader, or a 401 that makes a reader app ask
// for credentials.
func opdsUser(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, ok := UserFrom(r.Context())
	if !ok {
		// The header is what turns a refusal into a password prompt in a
		// reader app rather than an error message it cannot act on.
		w.Header().Set("WWW-Authenticate", `Basic realm="Lexarii"`)
		http.Error(w, "sign in to read this catalogue", http.StatusUnauthorized)
		return uuid.Nil, false
	}
	return id, true
}

func writeFeed(w http.ResponseWriter, contentType string, feed *opdsFeed) {
	w.Header().Set("Content-Type", contentType+";charset=utf-8")
	writeXML(w, feed)
}

func writeXML(w http.ResponseWriter, doc any) {
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return
	}
	encoder := xml.NewEncoder(w)
	encoder.Indent("", "  ")
	_ = encoder.Encode(doc)
	_, _ = w.Write([]byte("\n"))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// --- the wire shapes -------------------------------------------------------
//
// Written out rather than generated, because OPDS is Atom and Atom is not a
// thing the DTO generator describes: the contract here is somebody else's
// specification and it does not move.

type opdsFeed struct {
	XMLName   xml.Name    `xml:"feed"`
	XMLNS     string      `xml:"xmlns,attr"`
	XMLNSOPDS string      `xml:"xmlns:opds,attr"`
	ID        string      `xml:"id"`
	Title     string      `xml:"title"`
	Updated   string      `xml:"updated"`
	Author    *opdsAuthor `xml:"author,omitempty"`
	Link      []opdsLink  `xml:"link"`
	Entry     []opdsEntry `xml:"entry"`
}

type opdsEntry struct {
	ID       string         `xml:"id"`
	Title    string         `xml:"title"`
	Updated  string         `xml:"updated"`
	Author   []opdsAuthor   `xml:"author,omitempty"`
	Category []opdsCategory `xml:"category,omitempty"`
	Content  *opdsContent   `xml:"content,omitempty"`
	Link     []opdsLink     `xml:"link"`
}

type opdsAuthor struct {
	Name string `xml:"name"`
}

type opdsCategory struct {
	Term  string `xml:"term,attr"`
	Label string `xml:"label,attr,omitempty"`
}

type opdsContent struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
}

type opdsLink struct {
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr,omitempty"`
	Href string `xml:"href,attr"`
}

type openSearch struct {
	XMLName     xml.Name        `xml:"OpenSearchDescription"`
	XMLNS       string          `xml:"xmlns,attr"`
	ShortName   string          `xml:"ShortName"`
	Description string          `xml:"Description"`
	URL         []openSearchURL `xml:"Url"`
}

type openSearchURL struct {
	Type     string `xml:"type,attr"`
	Template string `xml:"template,attr"`
}
