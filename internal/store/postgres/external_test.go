// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// externalWork ingests one book for owner and returns its work id. Its
// edition has a publisher (sampleInput sets one) and nothing else a citation
// needs.
func externalWork(t *testing.T, p *Pool, owner uuid.UUID, seed int, title, author string) IngestOutcome {
	t.Helper()
	out, err := p.UpsertBookFile(t.Context(), owned(sampleInput(fmt.Sprintf("%064x", seed),
		fmt.Sprintf("/library/external-%d.epub", seed), title, author), owner))
	if err != nil {
		t.Fatalf("seeding %q: %v", title, err)
	}
	return out
}

func externalComplete(t *testing.T, p *Pool, workID uuid.UUID, isbn13 string) {
	t.Helper()
	if _, err := p.Exec(t.Context(), `
		UPDATE editions SET publisher = 'Addison-Wesley', published_on = '1999-10-20', isbn13 = $2
		WHERE work_id = $1`, workID, isbn13); err != nil {
		t.Fatalf("completing an edition: %v", err)
	}
}

// externalRecords reads the catalogue's provenance rows for a work.
func externalRecords(t *testing.T, p *Pool, workID uuid.UUID) map[string]string {
	t.Helper()
	rows, err := p.Query(t.Context(), `
		SELECT field, value #>> '{}', COALESCE(model_id, ''), accepted_at IS NOT NULL
		FROM enrichment_records WHERE work_id = $1 AND source = $2`, workID, domain.SourceExternal)
	if err != nil {
		t.Fatalf("reading records: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var field, value, model string
		var accepted bool
		if err := rows.Scan(&field, &value, &model, &accepted); err != nil {
			t.Fatalf("scanning record: %v", err)
		}
		if !accepted {
			t.Errorf("record for %s was written as a suggestion, but it was applied", field)
		}
		out[field] = value + " @ " + model
	}
	return out
}

func TestNeedsExternalMetadataListsTheViewersIncompleteWorks(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner := testOwner(t, p)
	stranger := secondUser(t, p)

	incomplete := externalWork(t, p, owner, 401, "Incomplete Reference", "Ann Author")
	if _, err := p.Exec(ctx, `UPDATE editions SET isbn10 = '0201633612' WHERE work_id = $1`,
		incomplete.WorkID); err != nil {
		t.Fatalf("setting an ISBN-10: %v", err)
	}

	complete := externalWork(t, p, owner, 402, "Complete Reference", "Bea Author")
	externalComplete(t, p, complete.WorkID, "9780201633610")

	// Every edition field present, but nobody's name on it.
	anonymous := externalWork(t, p, owner, 403, "Anonymous Reference", "Cy Author")
	externalComplete(t, p, anonymous.WorkID, "9780201633627")
	if _, err := p.Exec(ctx, `DELETE FROM work_authors WHERE work_id = $1`, anonymous.WorkID); err != nil {
		t.Fatalf("removing authors: %v", err)
	}

	// A placeholder title is not something to search a catalogue by.
	externalWork(t, p, owner, 404, "", "Di Author")

	missing := externalWork(t, p, owner, 405, "Unmounted Reference", "Ed Author")
	if _, err := p.Exec(ctx, `UPDATE book_files SET missing_since = now() WHERE id = $1`,
		missing.BookFileID); err != nil {
		t.Fatalf("marking missing: %v", err)
	}

	theirs := externalWork(t, p, stranger, 406, "Somebody Else's Reference", "Flo Author")

	got, err := p.NeedsExternalMetadata(ctx, owner, 0)
	if err != nil {
		t.Fatalf("NeedsExternalMetadata: %v", err)
	}
	byWork := map[uuid.UUID]WorkLookup{}
	for _, w := range got {
		byWork[w.WorkID] = w
	}
	if len(got) != 2 || len(byWork) != 2 {
		t.Fatalf("listed %+v, want exactly the incomplete and the anonymous work", got)
	}

	inc, ok := byWork[incomplete.WorkID]
	if !ok {
		t.Fatalf("the incomplete work is not listed: %+v", got)
	}
	if inc.Title != "Incomplete Reference" || inc.ISBN10 != "0201633612" || inc.ISBN13 != "" ||
		!slices.Equal(inc.Authors, []string{"Ann Author"}) {
		t.Errorf("incomplete work = %+v", inc)
	}

	anon, ok := byWork[anonymous.WorkID]
	if !ok {
		t.Fatalf("the unattributed work is not listed: %+v", got)
	}
	if anon.ISBN13 != "9780201633627" || len(anon.Authors) != 0 {
		t.Errorf("anonymous work = %+v", anon)
	}

	// The list is in a stable order, so the limit takes a stable prefix.
	if !slices.IsSortedFunc(got, func(a, b WorkLookup) int { return slices.Compare(a.WorkID[:], b.WorkID[:]) }) {
		t.Errorf("listing is not ordered by work id")
	}
	first, err := p.NeedsExternalMetadata(ctx, owner, 1)
	if err != nil {
		t.Fatalf("NeedsExternalMetadata (limit 1): %v", err)
	}
	if len(first) != 1 || first[0].WorkID != got[0].WorkID {
		t.Errorf("limit 1 returned %+v, want the first of %+v", first, got)
	}

	// The stranger is offered their own book and nothing of the owner's.
	other, err := p.NeedsExternalMetadata(ctx, stranger, 50)
	if err != nil {
		t.Fatalf("NeedsExternalMetadata (stranger): %v", err)
	}
	if len(other) != 1 || other[0].WorkID != theirs.WorkID {
		t.Errorf("stranger was offered %+v", other)
	}
}

// Publisher metadata beats a catalogue: only empty fields are filled, and
// every value written says where it came from.
func TestApplyExternalMetadataFillsOnlyWhatIsEmpty(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	work := externalWork(t, p, testOwner(t, p), 411, "Refactoring Workbook", "William Wake").WorkID

	offer := ExternalMetadata{
		WorkID:    work,
		Publisher: "A Different House",
		Year:      2003,
		ISBN13:    "9780321109293",
		PageCount: 236,
		Authors:   []string{"Somebody Else"},
		Source:    "openlibrary:/works/OL1W",
	}
	out, err := p.ApplyExternalMetadata(ctx, offer)
	if err != nil {
		t.Fatalf("ApplyExternalMetadata: %v", err)
	}
	if want := []string{"isbn13", "publishedOn", "pageCount"}; !slices.Equal(out.Filled, want) {
		t.Errorf("filled = %v, want %v", out.Filled, want)
	}
	// Offered and already known: the file's own publisher and byline stay.
	if want := []string{"publisher", "authors"}; !slices.Equal(out.Kept, want) {
		t.Errorf("kept = %v, want %v", out.Kept, want)
	}

	var publisher, isbn13 string
	var isbn10 *string
	var published time.Time
	var pages int
	if err := p.QueryRow(ctx, `
		SELECT publisher, isbn13, isbn10, published_on, page_count FROM editions WHERE work_id = $1`,
		work).Scan(&publisher, &isbn13, &isbn10, &published, &pages); err != nil {
		t.Fatalf("reading the edition: %v", err)
	}
	if publisher != "A Publisher" {
		t.Errorf("publisher = %q, the file's own value was overwritten", publisher)
	}
	if isbn13 != "9780321109293" || isbn10 != nil || pages != 236 ||
		!published.Equal(time.Date(2003, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("edition = isbn13 %q isbn10 %v pages %d published %v", isbn13, isbn10, pages, published)
	}
	if got := authorsOf(t, p, work); !slices.Equal(got, []string{"William Wake"}) {
		t.Errorf("authors = %v, a catalogue replaced the file's byline", got)
	}

	want := map[string]string{
		"isbn13":      "9780321109293 @ openlibrary:/works/OL1W",
		"publishedOn": "2003 @ openlibrary:/works/OL1W",
		"pageCount":   "236 @ openlibrary:/works/OL1W",
	}
	if got := externalRecords(t, p, work); !maps.Equal(got, want) {
		t.Errorf("provenance = %v, want %v", got, want)
	}

	// The same answer again changes nothing and records nothing new.
	out, err = p.ApplyExternalMetadata(ctx, offer)
	if err != nil {
		t.Fatalf("ApplyExternalMetadata (again): %v", err)
	}
	if len(out.Filled) != 0 {
		t.Errorf("a second identical lookup filled %v", out.Filled)
	}
	if want := []string{"publisher", "isbn13", "publishedOn", "pageCount", "authors"}; !slices.Equal(out.Kept, want) {
		t.Errorf("kept = %v, want %v", out.Kept, want)
	}
	if got := externalRecords(t, p, work); len(got) != 3 {
		t.Errorf("%d provenance rows after a no-op lookup, want 3", len(got))
	}
}

// For a book with nobody's name on it, a catalogue's byline beats silence.
func TestApplyExternalMetadataNamesAnUnattributedWork(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner := testOwner(t, p)

	work := externalWork(t, p, owner, 421, "Notes on Programming", "Placeholder Name").WorkID
	if _, err := p.Exec(ctx, `DELETE FROM work_authors WHERE work_id = $1`, work); err != nil {
		t.Fatalf("removing authors: %v", err)
	}

	out, err := p.ApplyExternalMetadata(ctx, ExternalMetadata{
		WorkID:  work,
		Authors: []string{"  ", " Grace Hopper ", "Alan Turing"},
	})
	if err != nil {
		t.Fatalf("ApplyExternalMetadata: %v", err)
	}
	if !slices.Equal(out.Filled, []string{"authors"}) || len(out.Kept) != 0 {
		t.Errorf("outcome = %+v, want only authors filled", out)
	}
	if got := authorsOf(t, p, work); !slices.Equal(got, []string{"Grace Hopper", "Alan Turing"}) {
		t.Errorf("authors = %v", got)
	}
	// No catalogue key was given, so none is recorded.
	recs := externalRecords(t, p, work)
	if v, ok := recs["authors"]; !ok || !strings.HasSuffix(v, " @ ") {
		t.Errorf("authors provenance = %q, want a record with no source", v)
	}

	// Blank names only: nothing to fill, and nothing claimed.
	bare := externalWork(t, p, owner, 422, "More Notes", "Placeholder Name").WorkID
	if _, err := p.Exec(ctx, `DELETE FROM work_authors WHERE work_id = $1`, bare); err != nil {
		t.Fatalf("removing authors: %v", err)
	}
	out, err = p.ApplyExternalMetadata(ctx, ExternalMetadata{WorkID: bare, Authors: []string{" ", ""}})
	if err != nil {
		t.Fatalf("ApplyExternalMetadata (blank names): %v", err)
	}
	if len(out.Filled) != 0 || len(out.Kept) != 0 {
		t.Errorf("blank names produced %+v", out)
	}
	if got := authorsOf(t, p, bare); len(got) != 0 {
		t.Errorf("blank names became authors %v", got)
	}
}

func TestApplyExternalMetadataToAWorkThatIsNotThere(t *testing.T) {
	p := testPool(t)
	_, err := p.ApplyExternalMetadata(t.Context(), ExternalMetadata{WorkID: uuid.New(), Publisher: "X"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A malformed catalogue value fails the whole lookup rather than half of it.
func TestApplyExternalMetadataIsAllOrNothing(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	work := externalWork(t, p, testOwner(t, p), 431, "Partial Truths", "Some Writer").WorkID

	_, err := p.ApplyExternalMetadata(ctx, ExternalMetadata{
		WorkID: work, Year: 1990, ISBN13: "978-not-an-isbn",
	})
	if err == nil {
		t.Fatal("a malformed ISBN was accepted")
	}
	var published *time.Time
	if err := p.QueryRow(ctx, `SELECT published_on FROM editions WHERE work_id = $1`, work).Scan(&published); err != nil {
		t.Fatalf("reading the edition: %v", err)
	}
	if published != nil {
		t.Errorf("the year was written by a lookup that failed: %v", published)
	}
	if got := externalRecords(t, p, work); len(got) != 0 {
		t.Errorf("a failed lookup left provenance %v", got)
	}
}

func TestExternalValueRendering(t *testing.T) {
	for _, tc := range []struct {
		n           int
		year, pages string
	}{{0, "", ""}, {-1, "", ""}, {1999, "1999", "1999"}} {
		if got := year(tc.n); got != tc.year {
			t.Errorf("year(%d) = %q", tc.n, got)
		}
		if got := count(tc.n); got != tc.pages {
			t.Errorf("count(%d) = %q", tc.n, got)
		}
	}
}
