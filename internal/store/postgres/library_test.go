// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// cleanLibrary empties the library tables so each test starts from a known
// state. Deleting works cascades to editions, files, tags links, and provenance.
func cleanLibrary(t *testing.T, p *Pool) {
	t.Helper()
	ctx := context.Background()
	// Collections too: they are not reached by deleting works - a collection
	// survives its books on purpose - so without this they accumulate across
	// runs and every count made about them is wrong.
	//
	// Tombstones too: nothing reaches them by cascade - a deletion has to
	// outlive the book it was in, which is the point of them - so a test that
	// deletes a mark would leave a record behind for the next one to trip on.
	// Claims too: they wait on a content hash rather than a row, so deleting
	// the works does not reach them.
	// Released holdings too: they remember bytes, not rows, so they outlive
	// the works a test deletes. And the blocklist, for the same reason.
	for _, table := range []string{"annotation_tombstones", "collections", "works", "authors", "tags",
		"holding_claims", "released_holdings", "blocked_content", "pending_discards"} {
		if _, err := p.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("cleaning %s: %v", table, err)
		}
	}
}

func hashOf(seed string) string {
	// A valid-looking SHA-256: 64 lowercase hex characters, as the CHECK
	// constraint requires.
	h := strings.Repeat(seed, 64)
	return h[:64]
}

func sampleInput(hash, path, title, author string) IngestInput {
	return IngestInput{
		ContentHash: hash,
		Path:        path,
		Format:      domain.FormatEPUB,
		SizeBytes:   4096,
		Status:      domain.IngestReady,
		Result: &ingest.Result{
			Format: domain.FormatEPUB,
			Spine:  []ingest.SpineItem{{Index: 0}, {Index: 1}, {Index: 2}},
			Metadata: ingest.Metadata{
				Title:     title,
				Authors:   []ingest.Contributor{{Name: author, Role: domain.RoleAuthor}},
				Language:  "en",
				Publisher: "A Publisher",
				Subjects:  []string{"Computer science", "Algorithms"},
				Provenance: map[string]domain.MetadataSource{
					"title":     domain.SourceEmbedded,
					"authors":   domain.SourceEmbedded,
					"language":  domain.SourceEmbedded,
					"publisher": domain.SourceEmbedded,
					"subjects":  domain.SourceEmbedded,
				},
			},
		},
	}
}

func TestUpsertCreatesFullBook(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	in := sampleInput(hashOf("a"), "/library/taocp.epub", "The Art of Computer Programming", "Donald Knuth")

	out, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if !out.Created {
		t.Error("Created = false on a first insert")
	}
	if out.BookFileID == uuid.Nil || out.WorkID == uuid.Nil || out.EditionID == uuid.Nil {
		t.Fatalf("missing ids: %+v", out)
	}

	var title, sortTitle string
	if err := p.QueryRow(ctx,
		`SELECT title, sort_title FROM works WHERE id = $1`, out.WorkID,
	).Scan(&title, &sortTitle); err != nil {
		t.Fatalf("reading work: %v", err)
	}
	if title != in.Result.Metadata.Title {
		t.Errorf("title = %q", title)
	}
	if sortTitle != SortTitle(title) {
		t.Errorf("sort_title = %q, want %q", sortTitle, SortTitle(title))
	}

	var authorName, sortName string
	if err := p.QueryRow(ctx, `
		SELECT a.name, a.sort_name FROM authors a
		JOIN work_authors wa ON wa.author_id = a.id
		WHERE wa.work_id = $1`, out.WorkID,
	).Scan(&authorName, &sortName); err != nil {
		t.Fatalf("reading author: %v", err)
	}
	if authorName != "Donald Knuth" || sortName != "Knuth, Donald" {
		t.Errorf("author = %q / %q", authorName, sortName)
	}

	var unitCount int
	var status string
	if err := p.QueryRow(ctx,
		`SELECT unit_count, status FROM book_files WHERE id = $1`, out.BookFileID,
	).Scan(&unitCount, &status); err != nil {
		t.Fatalf("reading book file: %v", err)
	}
	if unitCount != 3 {
		t.Errorf("unit_count = %d, want 3", unitCount)
	}
	if status != string(domain.IngestReady) {
		t.Errorf("status = %q", status)
	}

	var tagCount int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM work_tags WHERE work_id = $1`, out.WorkID,
	).Scan(&tagCount); err != nil {
		t.Fatalf("counting tags: %v", err)
	}
	if tagCount != 2 {
		t.Errorf("tag count = %d, want 2", tagCount)
	}
}

// Re-running a scan over an unchanged library must not create anything.
func TestUpsertIsIdempotent(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	in := sampleInput(hashOf("b"), "/library/book.epub", "Idempotent", "An Author")

	first, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	for i := 0; i < 4; i++ {
		next, err := p.UpsertBookFile(ctx, in)
		if err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
		if next.Created {
			t.Fatalf("upsert %d reported Created on an unchanged file", i)
		}
		if next.BookFileID != first.BookFileID {
			t.Fatalf("upsert %d returned a different book file id", i)
		}
		if next.WorkID != first.WorkID {
			t.Fatalf("upsert %d returned a different work id", i)
		}
	}

	counts := map[string]int{"works": 0, "editions": 0, "book_files": 0, "authors": 0}
	for table := range counts {
		var n int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("%s has %d rows after 5 identical upserts, want 1", table, n)
		}
	}
}

// Identity is the content hash, not the path. This is what stops a tidy-up of
// the library directory looking like a library full of new books.
func TestUpsertRecognisesMovedFile(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	hash := hashOf("c")
	first, err := p.UpsertBookFile(ctx, sampleInput(hash, "/library/Inbox/book.epub", "Moved", "Author"))
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	moved, err := p.UpsertBookFile(ctx, sampleInput(hash, "/library/Sorted/Author/renamed.epub", "Moved", "Author"))
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	if moved.Created {
		t.Error("a moved file was reported as newly created")
	}
	if !moved.Moved {
		t.Error("Moved = false for a file at a new path")
	}
	if moved.PreviousPath != "/library/Inbox/book.epub" {
		t.Errorf("PreviousPath = %q", moved.PreviousPath)
	}
	if moved.BookFileID != first.BookFileID {
		t.Error("a moved file got a new book file id")
	}

	var path string
	var count int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM book_files`).Scan(&count); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != 1 {
		t.Fatalf("book_files has %d rows after a move, want 1", count)
	}
	if err := p.QueryRow(ctx, `SELECT path FROM book_files WHERE id = $1`, first.BookFileID).Scan(&path); err != nil {
		t.Fatalf("reading path: %v", err)
	}
	if path != "/library/Sorted/Author/renamed.epub" {
		t.Errorf("path = %q, want the new location", path)
	}
}

// A user may have corrected a title. Re-scanning must not silently undo that.
func TestRescanDoesNotOverwriteUserEdits(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	in := sampleInput(hashOf("d"), "/library/book.epub", "Original Title From File", "Author")
	out, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	const corrected = "Title The User Fixed By Hand"
	if _, err := p.Exec(ctx, `UPDATE works SET title = $2 WHERE id = $1`, out.WorkID, corrected); err != nil {
		t.Fatalf("simulating a user edit: %v", err)
	}

	if _, err := p.UpsertBookFile(ctx, in); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	var title string
	if err := p.QueryRow(ctx, `SELECT title FROM works WHERE id = $1`, out.WorkID).Scan(&title); err != nil {
		t.Fatalf("reading title: %v", err)
	}
	if title != corrected {
		t.Fatalf("title = %q, want the user's correction %q preserved", title, corrected)
	}
}

// Two files of the same book are one shelf item. An ISBN is the only match
// trustworthy enough to merge on automatically.
func TestSameISBNSharesWorkAndEdition(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	withISBN := func(hash, path string, format domain.Format) IngestInput {
		in := sampleInput(hash, path, "Shared Book", "Author")
		in.Format = format
		in.Result.Metadata.ISBN13 = "9780262510875"
		in.Result.Metadata.Provenance["isbn13"] = domain.SourceEmbedded
		return in
	}

	a, err := p.UpsertBookFile(ctx, withISBN(hashOf("e"), "/library/book.epub", domain.FormatEPUB))
	if err != nil {
		t.Fatalf("epub upsert: %v", err)
	}
	b, err := p.UpsertBookFile(ctx, withISBN(hashOf("f"), "/library/book.pdf", domain.FormatPDF))
	if err != nil {
		t.Fatalf("pdf upsert: %v", err)
	}

	if a.WorkID != b.WorkID {
		t.Error("same ISBN produced two works; they should be one shelf item")
	}
	if a.EditionID != b.EditionID {
		t.Error("same ISBN produced two editions")
	}
	if a.BookFileID == b.BookFileID {
		t.Error("two distinct files collapsed into one book file row")
	}

	var files int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM book_files WHERE edition_id = $1`, a.EditionID).Scan(&files); err != nil {
		t.Fatalf("counting files: %v", err)
	}
	if files != 2 {
		t.Errorf("edition has %d files, want 2", files)
	}
}

// Matching on anything looser than an ISBN is a guess, and wrongly merging two
// books is much harder for a user to undo than leaving them separate.
func TestSameTitleWithoutISBNStaysSeparate(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	a, err := p.UpsertBookFile(ctx, sampleInput(hashOf("1"), "/library/a.epub", "Selected Poems", "Various"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := p.UpsertBookFile(ctx, sampleInput(hashOf("2"), "/library/b.epub", "Selected Poems", "Various"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if a.WorkID == b.WorkID {
		t.Fatal("two different files with the same title were merged without an ISBN to justify it")
	}
}

// Authors are shared rows, not duplicated per book.
func TestAuthorsAreDeduplicated(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	for i, spelling := range []string{"Donald Knuth", "donald knuth", "DONALD KNUTH"} {
		in := sampleInput(hashOf(string(rune('a'+i))), "/library/book"+string(rune('0'+i))+".epub", "Book", spelling)
		if _, err := p.UpsertBookFile(ctx, in); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	var authors int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM authors`).Scan(&authors); err != nil {
		t.Fatalf("counting authors: %v", err)
	}
	if authors != 1 {
		t.Errorf("authors = %d, want 1 - case variants are the same person", authors)
	}
}

func TestTagsAreDeduplicatedAcrossBooks(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	variants := [][]string{
		{"Machine Learning", "Statistics"},
		{"machine learning", "statistics"},
		{"MACHINE-LEARNING", "Statistics"},
	}
	for i, subjects := range variants {
		in := sampleInput(hashOf(string(rune('a'+i))), "/library/b"+string(rune('0'+i))+".epub", "Book", "Author")
		in.Result.Metadata.Subjects = subjects
		if _, err := p.UpsertBookFile(ctx, in); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	var tags int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM tags`).Scan(&tags); err != nil {
		t.Fatalf("counting tags: %v", err)
	}
	if tags != 2 {
		t.Errorf("tags = %d, want 2 - a library must not grow near-duplicate tags", tags)
	}
}

// Enrichment must be able to tell which fields are publisher facts it may not
// overwrite, and the UI must be able to answer "where did this come from?".
func TestProvenanceIsRecorded(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	out, err := p.UpsertBookFile(ctx, sampleInput(hashOf("9"), "/library/book.epub", "Provenanced", "Author"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	rows, err := p.Query(ctx,
		`SELECT field, source, accepted_at IS NOT NULL FROM enrichment_records WHERE work_id = $1`, out.WorkID)
	if err != nil {
		t.Fatalf("reading provenance: %v", err)
	}
	defer rows.Close()

	got := make(map[string]string)
	for rows.Next() {
		var field, source string
		var accepted bool
		if err := rows.Scan(&field, &source, &accepted); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		if !accepted {
			t.Errorf("%s: embedded metadata should be accepted, not pending", field)
		}
		got[field] = source
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}

	for _, field := range []string{"title", "authors", "language", "publisher", "subjects"} {
		if got[field] != string(domain.SourceEmbedded) {
			t.Errorf("provenance for %q = %q, want embedded", field, got[field])
		}
	}
}

// A file that failed to parse is still recorded, so the UI can show it with its
// error rather than silently omitting it.
func TestFailedParseIsStillRecorded(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	in := IngestInput{
		ContentHash: hashOf("0"),
		Path:        "/library/broken.epub",
		Format:      domain.FormatEPUB,
		SizeBytes:   512,
		Status:      domain.IngestFailed,
		IngestError: "not a valid epub: missing META-INF/container.xml",
		Result:      nil,
	}

	out, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var status, ingestErr, title string
	if err := p.QueryRow(ctx, `
		SELECT bf.status, bf.ingest_error, w.title
		FROM book_files bf
		JOIN editions e ON e.id = bf.edition_id
		JOIN works w ON w.id = e.work_id
		WHERE bf.id = $1`, out.BookFileID,
	).Scan(&status, &ingestErr, &title); err != nil {
		t.Fatalf("reading: %v", err)
	}

	if status != string(domain.IngestFailed) {
		t.Errorf("status = %q", status)
	}
	if ingestErr == "" {
		t.Error("the error was not recorded")
	}
	if title != "Untitled" {
		t.Errorf("title = %q, want a visible placeholder rather than an invented title", title)
	}
}

func TestScannedPDFIsStoredAsNeedsOCR(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	in := sampleInput(hashOf("7"), "/library/scan.pdf", "A Scan", "Author")
	in.Format = domain.FormatPDF
	in.Status = domain.IngestNeedsOCR
	in.Result.NeedsOCR = true
	in.Result.Warnings = []string{"only 12 characters extracted across 300 pages"}

	out, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var status string
	var warnings []string
	var extractedAt *time.Time
	if err := p.QueryRow(ctx,
		`SELECT status, ingest_warnings, extracted_at FROM book_files WHERE id = $1`, out.BookFileID,
	).Scan(&status, &warnings, &extractedAt); err != nil {
		t.Fatalf("reading: %v", err)
	}

	if status != string(domain.IngestNeedsOCR) {
		t.Errorf("status = %q, want needs_ocr", status)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want the parser's explanation preserved", warnings)
	}
	if extractedAt != nil {
		t.Error("a scan should not be marked as successfully extracted")
	}
}

// An unmounted drive must not destroy reading positions and annotations.
func TestMarkMissingFlagsRatherThanDeletes(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	gone, err := p.UpsertBookFile(ctx, sampleInput(hashOf("3"), "/library/gone.epub", "Gone", "Author"))
	if err != nil {
		t.Fatalf("upsert gone: %v", err)
	}
	// Age it past the cutoff.
	if _, err := p.Exec(ctx,
		`UPDATE book_files SET last_seen_at = now() - interval '1 hour' WHERE id = $1`, gone.BookFileID,
	); err != nil {
		t.Fatalf("ageing: %v", err)
	}

	present, err := p.UpsertBookFile(ctx, sampleInput(hashOf("4"), "/library/here.epub", "Here", "Author"))
	if err != nil {
		t.Fatalf("upsert present: %v", err)
	}

	n, err := p.MarkMissing(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("MarkMissing: %v", err)
	}
	if n != 1 {
		t.Fatalf("MarkMissing marked %d files, want 1", n)
	}

	var missing *time.Time
	if err := p.QueryRow(ctx, `SELECT missing_since FROM book_files WHERE id = $1`, gone.BookFileID).Scan(&missing); err != nil {
		t.Fatalf("reading missing file: %v", err)
	}
	if missing == nil {
		t.Error("the absent file was not marked missing")
	}

	if err := p.QueryRow(ctx, `SELECT missing_since FROM book_files WHERE id = $1`, present.BookFileID).Scan(&missing); err != nil {
		t.Fatalf("reading present file: %v", err)
	}
	if missing != nil {
		t.Error("a present file was marked missing")
	}

	// Crucially: the row still exists, so annotations and reading positions survive.
	var count int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM book_files`).Scan(&count); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != 2 {
		t.Errorf("book_files has %d rows, want 2 - missing files are flagged, never deleted", count)
	}
}

// A file that reappears must be un-marked.
func TestReappearingFileClearsMissing(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	in := sampleInput(hashOf("5"), "/library/flaky.epub", "Flaky", "Author")
	out, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := p.Exec(ctx,
		`UPDATE book_files SET missing_since = now(), last_seen_at = now() - interval '1 hour' WHERE id = $1`,
		out.BookFileID); err != nil {
		t.Fatalf("marking missing: %v", err)
	}

	if _, err := p.UpsertBookFile(ctx, in); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	var missing *time.Time
	if err := p.QueryRow(ctx, `SELECT missing_since FROM book_files WHERE id = $1`, out.BookFileID).Scan(&missing); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if missing != nil {
		t.Error("a file that reappeared is still marked missing")
	}
}

func TestTouchSeen(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	hash := hashOf("6")
	if _, err := p.UpsertBookFile(ctx, sampleInput(hash, "/library/book.epub", "Touched", "Author")); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := p.TouchSeen(ctx, hash, "/library/book.epub", uuid.Nil); err != nil {
		t.Fatalf("TouchSeen: %v", err)
	}
	if err := p.TouchSeen(ctx, hashOf("z"), "/library/nope.epub", uuid.Nil); !isNotFound(err) {
		t.Fatalf("TouchSeen on an unknown hash = %v, want ErrNotFound", err)
	}
}

// A book owned as both PDF and EPUB is one shelf item. ISBN cannot do this:
// PDFs essentially never carry one, so the filename stem is the signal.
func TestSameFilenameDifferentFormatSharesWork(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	// The PDF arrives first, with no usable metadata at all - exactly the real case.
	pdf := sampleInput(fmt.Sprintf("%064x", 1),
		"/library/done/97thingseveryjavaprogrammershouldknow.pdf", "Untitled", "")
	pdf.Format = domain.FormatPDF
	pdf.Result.Metadata.Authors = nil
	a, err := p.UpsertBookFile(ctx, pdf)
	if err != nil {
		t.Fatalf("pdf: %v", err)
	}

	// The EPUB arrives later, from a different directory, with real metadata.
	epub := sampleInput(fmt.Sprintf("%064x", 2),
		"/library/97thingseveryjavaprogrammershouldknow.epub",
		"97 Things Every Java Programmer Should Know", "Kevlin Henney")
	epub.Result.Metadata.ISBN13 = "9781491952696"
	b, err := p.UpsertBookFile(ctx, epub)
	if err != nil {
		t.Fatalf("epub: %v", err)
	}

	if a.WorkID != b.WorkID {
		t.Error("the same book in two formats produced two works")
	}
	if a.EditionID != b.EditionID {
		t.Error("the same book in two formats produced two editions")
	}
	if a.BookFileID == b.BookFileID {
		t.Error("two distinct files collapsed into one row")
	}

	var works, files int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM works`).Scan(&works); err != nil {
		t.Fatalf("counting works: %v", err)
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM book_files`).Scan(&files); err != nil {
		t.Fatalf("counting files: %v", err)
	}
	if works != 1 || files != 2 {
		t.Errorf("works = %d, files = %d; want 1 work with 2 files", works, files)
	}
}

// Same name AND same format is two different things - a duplicate download, two
// printings, two unrelated papers - and must not be merged.
func TestSameFilenameSameFormatStaysSeparate(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	one := sampleInput(fmt.Sprintf("%064x", 1), "/library/a/introductiontoalgorithms.pdf", "Book A", "Author")
	one.Format = domain.FormatPDF
	two := sampleInput(fmt.Sprintf("%064x", 2), "/library/b/introductiontoalgorithms.pdf", "Book B", "Author")
	two.Format = domain.FormatPDF

	a, err := p.UpsertBookFile(ctx, one)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := p.UpsertBookFile(ctx, two)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a.WorkID == b.WorkID {
		t.Fatal("two same-format files with the same name were merged")
	}
}

// Short stems collide by accident. Two folders each holding "notes" files are
// almost certainly unrelated documents.
func TestShortStemsAreNotMerged(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	pdf := sampleInput(fmt.Sprintf("%064x", 1), "/library/maths/notes.pdf", "Maths Notes", "Author")
	pdf.Format = domain.FormatPDF
	epub := sampleInput(fmt.Sprintf("%064x", 2), "/library/physics/notes.epub", "Physics Notes", "Author")

	a, err := p.UpsertBookFile(ctx, pdf)
	if err != nil {
		t.Fatalf("pdf: %v", err)
	}
	b, err := p.UpsertBookFile(ctx, epub)
	if err != nil {
		t.Fatalf("epub: %v", err)
	}
	if a.WorkID == b.WorkID {
		t.Fatalf("stem %q is only %d characters and must not be trusted for a merge",
			StemKey("notes.pdf"), len(StemKey("notes.pdf")))
	}
}

// A scan must not declare uploaded books missing.
//
// An uploaded book lives in the data directory, which the scanner is forbidden
// from walking, so a scan never sees it and its last_seen_at never moves. Every
// scan therefore marked every upload missing - and missing means gone from the
// shelf, from Continue Reading, from search, from retrieval and from
// enrichment, with nothing left that would ever clear the flag.
func TestAScanDoesNotLoseUploadedBooks(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)

	scanned := sampleInput(hashOf("a"), "/library/scanned.epub", "A Scanned Book", "An Author")
	if _, err := p.UpsertBookFile(t.Context(), scanned); err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	uploaded := sampleInput(hashOf("b"), "/data/uploads/ab/cd/uploaded.epub", "An Uploaded Book", "An Author")
	uploaded.Uploaded = true
	up, err := p.UpsertBookFile(t.Context(), uploaded)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	// A scan that started after both were added and walked neither.
	if _, err := p.MarkMissing(t.Context(), time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("MarkMissing: %v", err)
	}

	var missing bool
	if err := p.QueryRow(t.Context(),
		`SELECT missing_since IS NOT NULL FROM book_files WHERE id = $1`,
		up.BookFileID).Scan(&missing); err != nil {
		t.Fatalf("reading the uploaded file: %v", err)
	}
	if missing {
		t.Error("a scan marked an uploaded book missing; it would vanish from the shelf")
	}

	// The scanned one still is, which is what the sweep is for.
	var scannedMissing bool
	if err := p.QueryRow(t.Context(), `
		SELECT missing_since IS NOT NULL FROM book_files
		WHERE origin = 'scanned' AND path = '/library/scanned.epub'`).Scan(&scannedMissing); err != nil {
		t.Fatalf("reading the scanned file: %v", err)
	}
	if !scannedMissing {
		t.Error("a scanned file the walk never saw was not marked missing")
	}
}

// The day a format starts working, the books already shelved have to get
// their metadata.
//
// A Kindle book ingested before MOBI could be read is stored unsupported, with
// no title and no author. Making it re-extractable was only half the fix: the
// re-ingest path updated the file's own facts and threw the newly-parsed
// metadata away, so the book became readable and stayed called "Untitled"
// forever.
func TestReExtractingFillsMetadataThatWasMissing(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)

	// Shelved by a version that could not read the format.
	blind := IngestInput{
		ContentHash: hashOf("f"),
		Path:        "/library/kindle.mobi",
		Format:      domain.FormatMOBI,
		SizeBytes:   4096,
		Status:      domain.IngestUnsupported,
		Result:      &ingest.Result{Format: domain.FormatMOBI},
	}
	first, err := p.UpsertBookFile(t.Context(), blind)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	var title string
	if err := p.QueryRow(t.Context(),
		`SELECT title FROM works WHERE id = $1`, first.WorkID).Scan(&title); err != nil {
		t.Fatalf("reading the work: %v", err)
	}
	if title != PlaceholderTitle {
		t.Fatalf("title = %q, want the placeholder: this fixture models a book nothing could read", title)
	}

	// The same file, re-extracted by a version that can read it.
	seeing := blind
	seeing.Status = domain.IngestReady
	seeing.Result = &ingest.Result{
		Format: domain.FormatMOBI,
		Metadata: ingest.Metadata{
			Title:   "Algebra Essentials",
			Authors: []ingest.Contributor{{Name: "David A. Santos", Role: domain.RoleAuthor}},
			Provenance: map[string]domain.MetadataSource{
				"title": domain.SourceEmbedded, "authors": domain.SourceEmbedded,
			},
		},
	}
	if _, err := p.UpsertBookFile(t.Context(), seeing); err != nil {
		t.Fatalf("re-ingesting: %v", err)
	}

	if err := p.QueryRow(t.Context(),
		`SELECT title FROM works WHERE id = $1`, first.WorkID).Scan(&title); err != nil {
		t.Fatalf("reading the work: %v", err)
	}
	if title != "Algebra Essentials" {
		t.Errorf("title = %q, want the one the file finally gave up", title)
	}

	var authors int
	if err := p.QueryRow(t.Context(),
		`SELECT count(*) FROM work_authors WHERE work_id = $1`, first.WorkID).Scan(&authors); err != nil {
		t.Fatalf("counting authors: %v", err)
	}
	if authors == 0 {
		t.Error("the byline the file carried was discarded")
	}
}

// And a correction made by hand still survives a re-extract, which is what the
// old comment was protecting.
func TestReExtractingDoesNotUndoACorrection(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)

	userID, err := p.EnsureDefaultUser(t.Context())
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}
	in := sampleInput(hashOf("a"), "/library/book.epub", "What The File Says", "An Author")
	in.OwnerID = userID
	book, err := p.UpsertBookFile(t.Context(), in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	if _, err := p.UpdateWork(t.Context(), book.WorkID, userID,
		WorkEdit{Title: str("What The Reader Says")}); err != nil {
		t.Fatalf("UpdateWork: %v", err)
	}

	if _, err := p.UpsertBookFile(t.Context(), in); err != nil {
		t.Fatalf("re-ingesting: %v", err)
	}

	var title string
	if err := p.QueryRow(t.Context(),
		`SELECT title FROM works WHERE id = $1`, book.WorkID).Scan(&title); err != nil {
		t.Fatalf("reading the work: %v", err)
	}
	if title != "What The Reader Says" {
		t.Errorf("title = %q, want the correction to have survived", title)
	}
}
