// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// seedLibrary inserts n books with predictable titles and authors.
// owned attaches an owner to a hand-built input. Books belong to people, and
// one with no owner is visible to nobody.
func owned(in IngestInput, owner uuid.UUID) IngestInput {
	in.OwnerID = owner
	return in
}

// testOwner is the account that owns seeded books.
func testOwner(t *testing.T, p *Pool) uuid.UUID {
	t.Helper()
	owner, err := p.EnsureDefaultUser(t.Context())
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}
	return owner
}

// seedLibrary inserts books and returns their owner.
//
// Books belong to people, so a seeded library needs one: without an owner the
// rows are visible to nobody and every assertion below would pass for the wrong
// reason, finding nothing because nothing is readable rather than because the
// query is right.
func seedLibrary(t *testing.T, p *Pool, books []struct{ title, author string }) uuid.UUID {
	t.Helper()
	ctx := t.Context()

	owner := testOwner(t, p)

	for i, b := range books {
		in := sampleInput(hashOf(fmt.Sprintf("%x", i)), fmt.Sprintf("/library/book-%02d.epub", i), b.title, b.author)
		// hashOf repeats one character; make each hash genuinely distinct.
		in.ContentHash = fmt.Sprintf("%064x", i+1)
		in.OwnerID = owner
		if _, err := p.UpsertBookFile(ctx, in); err != nil {
			t.Fatalf("seeding %q: %v", b.title, err)
		}
	}
	return owner
}

var sampleBooks = []struct{ title, author string }{
	{"The Art of Computer Programming", "Donald Knuth"},
	{"Algorithmic Thinking", "Daniel Zingaro"},
	{"Bayesian Statistics the Fun Way", "Will Kurt"},
	{"The Dispossessed", "Ursula K. Le Guin"},
	{"Concrete Mathematics", "Donald Knuth"},
	{"Structure and Interpretation of Computer Programs", "Harold Abelson"},
	{"The Book of R", "Tilman Davies"},
	{"Eloquent JavaScript", "Marijn Haverbeke"},
	{"Grokking Algorithms", "Aditya Bhargava"},
	{"Head First Design Patterns", "Eric Freeman"},
}

func TestLibraryReturnsWorks(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := seedLibrary(t, p, sampleBooks)

	page, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != len(sampleBooks) {
		t.Fatalf("got %d items, want %d", len(page.Items), len(sampleBooks))
	}

	for _, it := range page.Items {
		if it.WorkID == uuid.Nil {
			t.Error("item has no work id")
		}
		if it.Title == "" {
			t.Error("item has no title")
		}
		if len(it.Authors) == 0 {
			t.Errorf("%q has no authors", it.Title)
		}
		if len(it.Formats) == 0 {
			t.Errorf("%q has no formats", it.Title)
		}
	}
}

// Default ordering is by sort title, which files "The Dispossessed" under D.
func TestLibrarySortsByTitleIgnoringArticles(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := seedLibrary(t, p, sampleBooks)

	page, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Sort: SortTitleAsc})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}

	var titles []string
	for _, it := range page.Items {
		titles = append(titles, it.Title)
	}
	if len(titles) < 3 {
		t.Fatalf("too few items: %v", titles)
	}
	// "Algorithmic Thinking" < "The Art of..." (Art) < "Bayesian..." by sort title.
	if titles[0] != "Algorithmic Thinking" {
		t.Errorf("first = %q, want Algorithmic Thinking", titles[0])
	}
	if titles[1] != "The Art of Computer Programming" {
		t.Errorf("second = %q; 'The Art of...' should sort under A", titles[1])
	}

	// Sort titles must be non-decreasing throughout.
	for i := 1; i < len(page.Items); i++ {
		if page.Items[i-1].SortTitle > page.Items[i].SortTitle {
			t.Fatalf("order broken at %d: %q then %q",
				i, page.Items[i-1].SortTitle, page.Items[i].SortTitle)
		}
	}
}

// The property that matters: paging through the whole library must visit every
// work exactly once. Offset pagination fails this the moment a scan inserts a
// row mid-scroll, which is precisely what happens on first run.
func TestLibraryPaginationVisitsEveryWorkExactlyOnce(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := seedLibrary(t, p, sampleBooks)

	for _, sort := range []LibrarySort{SortTitleAsc, SortAddedDesc, SortAuthorAsc, SortSeriesAsc} {
		t.Run(string(sort), func(t *testing.T) {
			seen := make(map[uuid.UUID]int)
			cursor := ""
			pages := 0

			for {
				page, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Sort: sort, Limit: 3, Cursor: cursor})
				if err != nil {
					t.Fatalf("page %d: %v", pages, err)
				}
				for _, it := range page.Items {
					seen[it.WorkID]++
				}
				pages++
				if page.NextCursor == "" {
					break
				}
				cursor = page.NextCursor
				if pages > 20 {
					t.Fatal("pagination did not terminate")
				}
			}

			if len(seen) != len(sampleBooks) {
				t.Errorf("visited %d works, want %d", len(seen), len(sampleBooks))
			}
			for id, n := range seen {
				if n != 1 {
					t.Errorf("work %s visited %d times, want once", id, n)
				}
			}
			if pages < 3 {
				t.Errorf("paged in %d requests with limit 3 and %d books; pagination is not engaging",
					pages, len(sampleBooks))
			}
		})
	}
}

func TestLibrarySearchMatchesTitleAndAuthor(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := seedLibrary(t, p, sampleBooks)

	byTitle, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Search: "Bayesian"})
	if err != nil {
		t.Fatalf("search by title: %v", err)
	}
	if len(byTitle.Items) != 1 || byTitle.Items[0].Title != "Bayesian Statistics the Fun Way" {
		t.Errorf("title search returned %d items: %+v", len(byTitle.Items), titlesOf(byTitle))
	}

	byAuthor, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Search: "Knuth"})
	if err != nil {
		t.Fatalf("search by author: %v", err)
	}
	if len(byAuthor.Items) != 2 {
		t.Errorf("author search returned %d items, want 2: %v", len(byAuthor.Items), titlesOf(byAuthor))
	}

	// Case-insensitive.
	lower, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Search: "knuth"})
	if err != nil {
		t.Fatalf("lowercase search: %v", err)
	}
	if len(lower.Items) != len(byAuthor.Items) {
		t.Error("search is case-sensitive")
	}

	none, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Search: "zzzznotpresent"})
	if err != nil {
		t.Fatalf("empty search: %v", err)
	}
	if len(none.Items) != 0 {
		t.Errorf("got %d items for a term that matches nothing", len(none.Items))
	}
}

func TestLibraryFiltersByTag(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner := testOwner(t, p)

	in := sampleInput(fmt.Sprintf("%064x", 1), "/library/tagged.epub", "Tagged Book", "Author")
	in.Result.Metadata.Subjects = []string{"Machine Learning"}
	in.OwnerID = owner
	if _, err := p.UpsertBookFile(ctx, in); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	other := sampleInput(fmt.Sprintf("%064x", 2), "/library/other.epub", "Other Book", "Author")
	other.Result.Metadata.Subjects = []string{"Poetry"}
	other.OwnerID = owner
	if _, err := p.UpsertBookFile(ctx, other); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	page, err := p.Library(ctx, LibraryQuery{ViewerID: owner, Tag: "machine-learning"})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Title != "Tagged Book" {
		t.Fatalf("tag filter returned %v", titlesOf(page))
	}
}

// The shelf should show what is actually readable. A file on an unmounted drive
// is still in the database but is not on the shelf by default.
func TestLibraryHidesMissingFilesByDefault(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner := testOwner(t, p)

	out, err := p.UpsertBookFile(ctx, owned(
		sampleInput(fmt.Sprintf("%064x", 1), "/library/gone.epub", "Gone", "Author"), owner))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE book_files SET missing_since = now() WHERE id = $1`, out.BookFileID); err != nil {
		t.Fatalf("marking missing: %v", err)
	}

	hidden, err := p.Library(ctx, LibraryQuery{ViewerID: owner})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(hidden.Items) != 0 {
		t.Errorf("a missing file appeared on the shelf: %v", titlesOf(hidden))
	}

	shown, err := p.Library(ctx, LibraryQuery{ViewerID: owner, IncludeMissing: true})
	if err != nil {
		t.Fatalf("Library with IncludeMissing: %v", err)
	}
	if len(shown.Items) != 1 {
		t.Fatalf("got %d items with IncludeMissing, want 1", len(shown.Items))
	}
	if !shown.Items[0].Missing {
		t.Error("the item is not flagged as missing")
	}
}

// Two files of one work are one card, not two.
func TestLibraryCollapsesMultipleFilesIntoOneWork(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner := testOwner(t, p)

	withISBN := func(hash, path string, format domain.Format) IngestInput {
		in := sampleInput(hash, path, "Two Formats", "Author")
		in.Format = format
		in.Result.Metadata.ISBN13 = "9780262510875"
		in.Result.Metadata.Provenance["isbn13"] = domain.SourceEmbedded
		return in
	}
	if _, err := p.UpsertBookFile(ctx, owned(
		withISBN(fmt.Sprintf("%064x", 1), "/library/b.epub", domain.FormatEPUB), owner)); err != nil {
		t.Fatalf("epub: %v", err)
	}
	if _, err := p.UpsertBookFile(ctx, owned(
		withISBN(fmt.Sprintf("%064x", 2), "/library/b.pdf", domain.FormatPDF), owner)); err != nil {
		t.Fatalf("pdf: %v", err)
	}

	page, err := p.Library(ctx, LibraryQuery{ViewerID: owner})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("got %d cards for one work with two files: %v", len(page.Items), titlesOf(page))
	}
	if len(page.Items[0].Formats) != 2 {
		t.Errorf("formats = %v, want both epub and pdf", page.Items[0].Formats)
	}
}

func TestLibraryRejectsBadInput(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)

	if _, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Sort: "nonsense"}); err == nil {
		t.Error("an unknown sort was accepted")
	}
	if _, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Cursor: "!!!not-base64!!!"}); err == nil {
		t.Error("a malformed cursor was accepted")
	}
	if _, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Cursor: "dGhpcy1oYXMtbm8tbnVsbA"}); err == nil {
		t.Error("a cursor with no id was accepted")
	}
}

// A page size beyond the cap must be clamped, not honoured: an unbounded query
// is how a library of 50,000 books takes the server down.
func TestLibraryClampsPageSize(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := seedLibrary(t, p, sampleBooks)

	page, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Limit: 100000})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) > maxPageLen {
		t.Errorf("returned %d items, over the %d cap", len(page.Items), maxPageLen)
	}
}

func TestStats(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := seedLibrary(t, p, sampleBooks)

	s, err := p.Stats(t.Context(), owner)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Works != len(sampleBooks) {
		t.Errorf("Works = %d, want %d", s.Works, len(sampleBooks))
	}
	if s.Files != len(sampleBooks) {
		t.Errorf("Files = %d, want %d", s.Files, len(sampleBooks))
	}
	// Knuth wrote two of them, so authors are fewer than books.
	if s.Authors != len(sampleBooks)-1 {
		t.Errorf("Authors = %d, want %d (Knuth appears twice)", s.Authors, len(sampleBooks)-1)
	}
	if s.Pages == 0 {
		t.Error("Pages = 0; the sample has three units per book")
	}
	if s.ByFormat[domain.FormatEPUB] != len(sampleBooks) {
		t.Errorf("epub count = %d, want %d", s.ByFormat[domain.FormatEPUB], len(sampleBooks))
	}
}

func TestWorkDetail(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner := testOwner(t, p)

	out, err := p.UpsertBookFile(ctx, owned(sampleInput(fmt.Sprintf("%064x", 1),
		"/library/detail.epub", "The Art of Computer Programming", "Donald Knuth"), owner))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	d, err := p.Work(ctx, out.WorkID, owner)
	if err != nil {
		t.Fatalf("Work: %v", err)
	}

	if d.Work.Title != "The Art of Computer Programming" {
		t.Errorf("Title = %q", d.Work.Title)
	}
	if len(d.Work.Authors) != 1 || d.Work.Authors[0].Author.Name != "Donald Knuth" {
		t.Errorf("Authors = %+v", d.Work.Authors)
	}
	if len(d.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(d.Files))
	}
	if d.Files[0].Format != domain.FormatEPUB {
		t.Errorf("format = %q", d.Files[0].Format)
	}
	if len(d.Tags) != 2 {
		t.Errorf("got %d tags, want 2", len(d.Tags))
	}

	// The UI must be able to say where each value came from.
	for _, field := range []string{"title", "authors", "publisher"} {
		if d.Provenance[field] != domain.SourceEmbedded {
			t.Errorf("Provenance[%q] = %q, want embedded", field, d.Provenance[field])
		}
	}

	if _, err := p.Work(ctx, uuid.New(), owner); !isNotFound(err) {
		t.Errorf("Work on an unknown id = %v, want ErrNotFound", err)
	}
}

func titlesOf(p LibraryPage) []string {
	out := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		out = append(out, it.Title)
	}
	return out
}

// A book with no embedded title is unfindable by title search, and that is a
// large minority of a real library. The filename fallback is what makes those
// books reachable at all.
func TestLibrarySearchFindsUntitledBooksByFilename(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner := testOwner(t, p)

	// No title at all, exactly as a metadata-less PDF arrives.
	in := IngestInput{
		ContentHash: fmt.Sprintf("%064x", 1),
		Path:        "/library/appliedlinearalgebraandoptimizationusingmatlab.pdf",
		Format:      domain.FormatPDF,
		SizeBytes:   4096,
		Status:      domain.IngestReady,
		Result:      &ingest.Result{Format: domain.FormatPDF, Spine: []ingest.SpineItem{{Index: 0}}},
	}
	in.OwnerID = owner
	if _, err := p.UpsertBookFile(ctx, in); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	for _, query := range []string{"linear algebra", "Linear Algebra", "matlab", "optimization"} {
		t.Run(query, func(t *testing.T) {
			page, err := p.Library(ctx, LibraryQuery{ViewerID: owner, Search: query})
			if err != nil {
				t.Fatalf("Library: %v", err)
			}
			if len(page.Items) != 1 {
				t.Fatalf("search %q returned %d results, want 1; an untitled book must still be findable", query, len(page.Items))
			}
		})
	}

	// It must not match everything: the fallback is a search, not a wildcard.
	page, err := p.Library(ctx, LibraryQuery{ViewerID: owner, Search: "bayesian statistics"})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("an unrelated query matched %d books", len(page.Items))
	}
}

// A book held in a readable format and an unreadable one is a readable book.
// The card must represent it at its best, not by whichever file happened to be
// ingested first.
func TestLibraryCardRepresentsTheBestFile(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner := testOwner(t, p)

	// The unsupported MOBI lands first, as it did in a real upload.
	mobi := IngestInput{
		ContentHash:      fmt.Sprintf("%064x", 1),
		Path:             "/uploads/books/ab/abc.mobi",
		Format:           domain.FormatMOBI,
		SizeBytes:        87761394,
		Status:           domain.IngestUnsupported,
		OriginalFilename: "97thingseveryjavaprogrammershouldknow.mobi",
		Uploaded:         true,
	}
	mobi.OwnerID = owner
	if _, err := p.UpsertBookFile(ctx, mobi); err != nil {
		t.Fatalf("mobi: %v", err)
	}

	epub := sampleInput(fmt.Sprintf("%064x", 2), "/uploads/books/cd/cde.epub",
		"97 Things Every Java Programmer Should Know", "Kevlin Henney")
	epub.OriginalFilename = "97thingseveryjavaprogrammershouldknow.epub"
	epub.Uploaded = true
	epub.OwnerID = owner
	if _, err := p.UpsertBookFile(ctx, epub); err != nil {
		t.Fatalf("epub: %v", err)
	}

	page, err := p.Library(ctx, LibraryQuery{ViewerID: owner})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("got %d cards, want 1 - the two formats are one book", len(page.Items))
	}

	item := page.Items[0]
	if item.Status != domain.IngestReady {
		t.Errorf("status = %q, want ready: the book has a readable EPUB", item.Status)
	}
	if item.PageCount == nil {
		t.Error("no unit count; the readable file has one and should have been chosen")
	}
	if len(item.Formats) != 2 {
		t.Errorf("formats = %v, want both", item.Formats)
	}
}

// A subject list describes YOUR library, not the installation's.
//
// Counting every tag on every book would tell somebody who was lent two books
// how many books there are on Kubernetes, which both misreports their library
// and describes somebody else's.
func TestSubjectsAreCountedOverWhatYouCanRead(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	borrower := secondUser(t, p)
	ctx := t.Context()

	shared := owned(sampleInput(fmt.Sprintf("%064x", 71), "/library/sharedsubject.epub",
		"A Shared Book", "An Author"), owner)
	sharedOut, err := p.UpsertBookFile(ctx, shared)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	private := owned(sampleInput(fmt.Sprintf("%064x", 72), "/library/privatesubject.epub",
		"A Private Book", "An Author"), owner)
	privateOut, err := p.UpsertBookFile(ctx, private)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Both books carry the same subject; only one is lent.
	for _, work := range []uuid.UUID{sharedOut.WorkID, privateOut.WorkID} {
		if _, err := p.Exec(ctx, `
			WITH t AS (
				INSERT INTO tags (name, slug, is_subject) VALUES ('kubernetes', 'kubernetes', true)
				ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
				RETURNING id
			)
			INSERT INTO work_tags (work_id, tag_id, source)
			SELECT $1, t.id, 'llm' FROM t
			ON CONFLICT DO NOTHING`, work); err != nil {
			t.Fatalf("tagging: %v", err)
		}
	}
	if err := p.ShareBook(ctx, sharedOut.BookFileID, owner, borrower, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	// Both books carry the seeded subjects too, so this looks for the one that
	// was added here rather than assuming the whole list.
	count := func(subjects []Subject, slug string) int {
		for _, s := range subjects {
			if s.Slug == slug {
				return s.Books
			}
		}
		return 0
	}

	mine, err := p.Subjects(ctx, owner)
	if err != nil {
		t.Fatalf("Subjects: %v", err)
	}
	if got := count(mine, "kubernetes"); got != 2 {
		t.Fatalf("the owner sees kubernetes on %d books, want 2: %+v", got, mine)
	}

	theirs, err := p.Subjects(ctx, borrower)
	if err != nil {
		t.Fatalf("Subjects: %v", err)
	}
	if got := count(theirs, "kubernetes"); got != 1 {
		t.Fatalf("the borrower sees kubernetes on %d books, want 1: %+v", got, theirs)
	}

	// Commonest first, so the shape of the library reads off the top.
	for i := 1; i < len(mine); i++ {
		if mine[i].Books > mine[i-1].Books {
			t.Fatalf("subjects are not ordered by book count: %+v", mine)
		}
	}

	// Somebody with nothing has no subjects, rather than the library's.
	stranger := secondUser(t, p)
	none, err := p.Subjects(ctx, stranger)
	if err != nil {
		t.Fatalf("Subjects: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("a stranger was shown %d subjects", len(none))
	}
}

// A name is resolved to a book by title match, best first, before the limit:
// sorted by title, the one book called "It" was the 21st of 21 titles
// containing the letters and fell off a page of twenty.
func TestALibraryByTitleMatchPutsTheExactTitleFirst(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	books := []struct{ title, author string }{{"It", "Stephen King"}, {"Bring It On", "A Writer"}}
	for i := 1; i <= 19; i++ {
		books = append(books, struct{ title, author string }{fmt.Sprintf("A Habit of Mind %02d", i), "Someone"})
	}
	owner := seedLibrary(t, p, books)

	plain, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Search: "It", Limit: 20})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	// The probe: in title order the exact title is not on the page at all.
	for _, item := range plain.Items {
		if item.Title == "It" {
			t.Fatal("title order kept It on the page; the probe no longer probes anything")
		}
	}

	ranked, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Search: "It", Limit: 20, ByTitleMatch: true})
	if err != nil {
		t.Fatalf("Library by title match: %v", err)
	}
	if len(ranked.Items) != 20 {
		t.Fatalf("got %d items, want a full page of 20", len(ranked.Items))
	}
	if ranked.Items[0].Title != "It" || ranked.Items[1].Title != "Bring It On" {
		t.Errorf("first two = %q, %q; want the exact title, then the whole word",
			ranked.Items[0].Title, ranked.Items[1].Title)
	}
	if ranked.NextCursor != "" {
		t.Error("a listing by title match offered a next page")
	}
	if _, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Search: "It", ByTitleMatch: true,
		Cursor: plain.NextCursor}); !errors.Is(err, store.ErrInvalidQuery) {
		t.Errorf("a cursor on a title-match listing: err = %v", err)
	}
}

// A search is for the text typed: % and _ are not patterns.
func TestALibrarySearchMatchesPercentAndUnderscoreLiterally(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := seedLibrary(t, p, []struct{ title, author string }{
		{"100% Recall", "A Writer"}, {"1000 Recipes", "A Cook"}, {"snake_case Style", "A Pythonist"}, {"snakescase", "Nobody"},
	})
	for search, want := range map[string]string{"100%": "100% Recall", "snake_case": "snake_case Style"} {
		page, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Search: search})
		if err != nil {
			t.Fatalf("Library %q: %v", search, err)
		}
		if len(page.Items) != 1 || page.Items[0].Title != want {
			var got []string
			for _, i := range page.Items {
				got = append(got, i.Title)
			}
			t.Errorf("search %q found %q, want only %q", search, got, want)
		}
	}
}

func TestTitleKey(t *testing.T) {
	for in, want := range map[string]string{
		"The Adventures of Sherlock Holmes!": "adventures of sherlock holmes",
		"  Dracula's   Guest ":               "dracula's guest",
		"It":                                 "it",
		"The":                                "the",
		"A Habit of Mind 01":                 "habit of mind 01",
	} {
		if got := TitleKey(in); got != want {
			t.Errorf("TitleKey(%q) = %q, want %q", in, got, want)
		}
	}
}
