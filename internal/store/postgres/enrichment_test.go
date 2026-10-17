// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// untitledWork ingests a file carrying no metadata at all, which is what
// roughly two in five files in the real library look like.
func untitledWork(t *testing.T, p *Pool) uuid.UUID {
	t.Helper()
	cleanLibrary(t, p)

	in := IngestInput{
		ContentHash: hashOf("b"),
		Path:        "/library/scan0001.pdf",
		Format:      domain.FormatPDF,
		SizeBytes:   4096,
		Status:      domain.IngestReady,
		Result: &ingest.Result{
			Format: domain.FormatPDF,
			Spine:  []ingest.SpineItem{{Index: 0}},
			// No title, no authors, no provenance: nothing the file declared.
			Metadata: ingest.Metadata{},
		},
	}
	out, err := p.UpsertBookFile(t.Context(), in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	return out.WorkID
}

// publishedWork ingests a file whose own metadata says what it is.
func publishedWork(t *testing.T, p *Pool) uuid.UUID {
	t.Helper()
	cleanLibrary(t, p)

	out, err := p.UpsertBookFile(t.Context(),
		sampleInput(hashOf("c"), "/library/real.epub", "The Real Title", "A Real Author"))
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	return out.WorkID
}

func classification() EnrichmentInput {
	index := 2.0
	return EnrichmentInput{
		Title:       "An Introduction to Logic",
		Summary:     "A first course in formal logic.",
		Series:      "Introductions",
		SeriesIndex: &index,
		Subjects:    []string{"logic", "philosophy"},
		Audience:    "undergraduate",
		Model:       "qwen3-8b-instruct",
		PromptID:    "classify/v1",
		Confidence:  0.8,
	}
}

// An empty field is filled: an untitled book helps nobody, and the provenance
// records where the title came from.
func TestEnrichmentFillsEmptyFields(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	in := classification()
	in.WorkID = workID
	out, err := p.SaveEnrichment(t.Context(), in)
	if err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}

	if len(out.Applied) == 0 {
		t.Fatalf("nothing was applied to a work with no metadata: %+v", out)
	}
	if len(out.Suggested) != 0 {
		t.Errorf("fields were only suggested on an empty work: %v", out.Suggested)
	}

	var title, summary, series *string
	var index *float64
	if err := p.QueryRow(t.Context(),
		`SELECT title, summary, series, series_index FROM works WHERE id = $1`, workID,
	).Scan(&title, &summary, &series, &index); err != nil {
		t.Fatalf("reading work: %v", err)
	}
	if deref(title) != "An Introduction to Logic" {
		t.Errorf("title = %q, want it filled", deref(title))
	}
	if deref(summary) == "" {
		t.Error("the summary was not applied")
	}
	if index == nil || *index != 2 {
		t.Errorf("series index = %v, want 2", index)
	}
}

// The invariant the whole feature rests on: publisher metadata beats model
// output. A model must never silently replace what the file itself said.
func TestEnrichmentNeverOverwritesPublisherMetadata(t *testing.T) {
	p := testPool(t)
	workID := publishedWork(t, p)

	in := classification()
	in.WorkID = workID
	out, err := p.SaveEnrichment(t.Context(), in)
	if err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}

	if !contains(out.Suggested, "title") {
		t.Errorf("title was not suggested: applied=%v suggested=%v", out.Applied, out.Suggested)
	}

	var title string
	if err := p.QueryRow(t.Context(), `SELECT title FROM works WHERE id = $1`, workID).Scan(&title); err != nil {
		t.Fatalf("reading work: %v", err)
	}
	if title != "The Real Title" {
		t.Fatalf("title = %q: a model overwrote what the file said", title)
	}

	// The suggestion is kept, unaccepted, for a person to decide on.
	pending, err := p.PendingSuggestions(t.Context(), 50)
	if err != nil {
		t.Fatalf("PendingSuggestions: %v", err)
	}
	if !hasField(pending, "title") {
		t.Error("the rejected-for-now title was not recorded as a suggestion")
	}
}

// Tags are additive: a tag does not displace anything, it is one more way to
// find the book.
func TestEnrichmentAddsSubjectTags(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	in := classification()
	in.WorkID = workID
	out, err := p.SaveEnrichment(t.Context(), in)
	if err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}
	if out.TagsAdded != 2 {
		t.Errorf("added %d tags, want 2", out.TagsAdded)
	}

	rows, err := p.Query(t.Context(), `
		SELECT t.name, wt.source, wt.confidence
		FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
		WHERE wt.work_id = $1 ORDER BY t.name`, workID)
	if err != nil {
		t.Fatalf("reading tags: %v", err)
	}
	defer rows.Close()

	var found int
	for rows.Next() {
		var name, source string
		var confidence *float64
		if err := rows.Scan(&name, &source, &confidence); err != nil {
			t.Fatalf("scanning tag: %v", err)
		}
		found++
		// The source is what lets the UI show which tags a model chose.
		if source != string(domain.SourceLLM) {
			t.Errorf("tag %q has source %q, want llm", name, source)
		}
		if confidence == nil {
			t.Errorf("tag %q recorded no confidence", name)
		}
	}
	if found != 2 {
		t.Errorf("found %d tags, want 2", found)
	}
}

// A tag the file itself declared keeps its own source rather than being
// downgraded to a model's guess.
func TestEnrichmentDoesNotDowngradeEmbeddedTags(t *testing.T) {
	p := testPool(t)
	workID := publishedWork(t, p) // sampleInput declares subjects

	in := classification()
	in.WorkID = workID
	in.Subjects = []string{"computer science"} // one the file already declared
	if _, err := p.SaveEnrichment(t.Context(), in); err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}

	var source string
	if err := p.QueryRow(t.Context(), `
		SELECT wt.source FROM work_tags wt
		JOIN tags t ON t.id = wt.tag_id
		WHERE wt.work_id = $1 AND t.slug = $2`, workID, TagSlug("computer science"),
	).Scan(&source); err != nil {
		t.Fatalf("reading tag: %v", err)
	}
	if source != string(domain.SourceEmbedded) {
		t.Errorf("source = %q: an embedded tag was downgraded to a model's guess", source)
	}
}

// Re-running enrichment must not stack a second copy of the same answer.
func TestEnrichmentIsIdempotentPerPrompt(t *testing.T) {
	p := testPool(t)
	workID := publishedWork(t, p)

	in := classification()
	in.WorkID = workID
	for i := 0; i < 3; i++ {
		if _, err := p.SaveEnrichment(t.Context(), in); err != nil {
			t.Fatalf("SaveEnrichment %d: %v", i, err)
		}
	}

	var titleRecords int
	if err := p.QueryRow(t.Context(), `
		SELECT count(*) FROM enrichment_records
		WHERE work_id = $1 AND field = 'title' AND source = 'llm'`, workID,
	).Scan(&titleRecords); err != nil {
		t.Fatalf("counting records: %v", err)
	}
	if titleRecords != 1 {
		t.Errorf("three runs left %d title records, want 1", titleRecords)
	}
}

// A person's decision is theirs. Re-running enrichment must not quietly undo a
// rejection by replacing it with a fresh suggestion.
func TestEnrichmentDoesNotReviveARejectedSuggestion(t *testing.T) {
	p := testPool(t)
	workID := publishedWork(t, p)

	in := classification()
	in.WorkID = workID
	if _, err := p.SaveEnrichment(t.Context(), in); err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}

	pending, err := p.PendingSuggestions(t.Context(), 50)
	if err != nil {
		t.Fatalf("PendingSuggestions: %v", err)
	}
	var titleID uuid.UUID
	for _, s := range pending {
		if s.Field == "title" {
			titleID = s.ID
		}
	}
	if titleID == uuid.Nil {
		t.Fatal("no title suggestion to reject")
	}
	if err := p.ResolveSuggestion(t.Context(), titleID, false); err != nil {
		t.Fatalf("ResolveSuggestion: %v", err)
	}

	// Enrichment runs again, as it would after a restart.
	if _, err := p.SaveEnrichment(t.Context(), in); err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}

	var rejected int
	if err := p.QueryRow(t.Context(), `
		SELECT count(*) FROM enrichment_records
		WHERE work_id = $1 AND field = 'title' AND rejected_at IS NOT NULL`, workID,
	).Scan(&rejected); err != nil {
		t.Fatalf("counting rejections: %v", err)
	}
	if rejected != 1 {
		t.Errorf("the rejection was lost: %d rejected title records", rejected)
	}
}

// Accepting is the only way a model value replaces an existing one, and only
// because a person said so.
func TestAcceptingASuggestionAppliesIt(t *testing.T) {
	p := testPool(t)
	workID := publishedWork(t, p)

	in := classification()
	in.WorkID = workID
	if _, err := p.SaveEnrichment(t.Context(), in); err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}

	pending, err := p.PendingSuggestions(t.Context(), 50)
	if err != nil {
		t.Fatalf("PendingSuggestions: %v", err)
	}
	var titleID uuid.UUID
	for _, s := range pending {
		if s.Field == "title" {
			titleID = s.ID
		}
	}
	if titleID == uuid.Nil {
		t.Fatal("no title suggestion")
	}

	if err := p.ResolveSuggestion(t.Context(), titleID, true); err != nil {
		t.Fatalf("ResolveSuggestion: %v", err)
	}

	var title string
	if err := p.QueryRow(t.Context(), `SELECT title FROM works WHERE id = $1`, workID).Scan(&title); err != nil {
		t.Fatalf("reading work: %v", err)
	}
	if title != "An Introduction to Logic" {
		t.Errorf("title = %q, want the accepted suggestion applied", title)
	}

	// And it leaves the pending list.
	pending, err = p.PendingSuggestions(t.Context(), 50)
	if err != nil {
		t.Fatalf("PendingSuggestions: %v", err)
	}
	for _, s := range pending {
		if s.ID == titleID {
			t.Error("an accepted suggestion is still pending")
		}
	}
}

func TestEnrichmentRecordsProvenance(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	in := classification()
	in.WorkID = workID
	if _, err := p.SaveEnrichment(t.Context(), in); err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}

	var model, prompt string
	var confidence *float64
	if err := p.QueryRow(t.Context(), `
		SELECT model_id, prompt_version, confidence FROM enrichment_records
		WHERE work_id = $1 AND field = 'title' AND source = 'llm'`, workID,
	).Scan(&model, &prompt, &confidence); err != nil {
		t.Fatalf("reading record: %v", err)
	}
	// Which model and which prompt: without both, "the AI said so" cannot be
	// checked or reproduced later.
	if model != "qwen3-8b-instruct" || prompt != "classify/v1" {
		t.Errorf("model = %q, prompt = %q", model, prompt)
	}
	// The column is `real`, so 0.8 does not survive as exactly 0.8.
	if confidence == nil || *confidence < 0.79 || *confidence > 0.81 {
		t.Errorf("confidence = %v, want about 0.8", confidence)
	}
}

// Changing what a prompt asks for means every book is un-enriched with respect
// to the new question.
func TestWorksNeedingEnrichmentIsKeyedOnThePrompt(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	needing, err := p.WorksNeedingEnrichment(t.Context(), "classify/v1", 10)
	if err != nil {
		t.Fatalf("WorksNeedingEnrichment: %v", err)
	}
	if !containsID(needing, workID) {
		t.Fatal("a work with no enrichment was not listed")
	}

	in := classification()
	in.WorkID = workID
	if _, err := p.SaveEnrichment(t.Context(), in); err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}

	needing, err = p.WorksNeedingEnrichment(t.Context(), "classify/v1", 10)
	if err != nil {
		t.Fatalf("WorksNeedingEnrichment: %v", err)
	}
	if containsID(needing, workID) {
		t.Error("an enriched work is still listed as needing enrichment")
	}

	// A new prompt asks a different question, so the answer is not yet known.
	needing, err = p.WorksNeedingEnrichment(t.Context(), "classify/v2", 10)
	if err != nil {
		t.Fatalf("WorksNeedingEnrichment: %v", err)
	}
	if !containsID(needing, workID) {
		t.Error("a new prompt version did not make the work eligible again")
	}
}

// Asking a model about a book whose file is gone would be asking it to invent
// one.
func TestWorksNeedingEnrichmentSkipsMissingFiles(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	if _, err := p.Exec(t.Context(), `
		UPDATE book_files SET missing_since = now()
		WHERE edition_id IN (SELECT id FROM editions WHERE work_id = $1)`, workID); err != nil {
		t.Fatalf("marking missing: %v", err)
	}

	needing, err := p.WorksNeedingEnrichment(t.Context(), "classify/v1", 10)
	if err != nil {
		t.Fatalf("WorksNeedingEnrichment: %v", err)
	}
	if containsID(needing, workID) {
		t.Error("a work whose file is gone was listed for enrichment")
	}
}

func TestEnrichmentSubjectCarriesWhatIsKnown(t *testing.T) {
	p := testPool(t)
	workID := publishedWork(t, p)

	subject, err := p.EnrichmentSubject(t.Context(), workID)
	if err != nil {
		t.Fatalf("EnrichmentSubject: %v", err)
	}
	if subject.Title != "The Real Title" {
		t.Errorf("title = %q", subject.Title)
	}
	if len(subject.Authors) != 1 || subject.Authors[0] != "A Real Author" {
		t.Errorf("authors = %v", subject.Authors)
	}
	if subject.Path == "" || subject.Format != domain.FormatEPUB {
		t.Errorf("file = %q (%s)", subject.Path, subject.Format)
	}
	// The filename is a real signal for books with no metadata, so it has to
	// reach the prompt even when nothing recorded it separately.
	if subject.Filename == "" {
		t.Error("no filename was carried")
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func containsID(values []uuid.UUID, want uuid.UUID) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func hasField(suggestions []Suggestion, field string) bool {
	for _, s := range suggestions {
		if s.Field == field {
			return true
		}
	}
	return false
}

// Empty fields are the ones with no protection: nothing is being overwritten,
// so a weak guess would otherwise become what the library says the book is.
func TestEnrichmentOnlySuggestsALowConfidenceClaim(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	in := classification()
	in.WorkID = workID
	in.Confidence = 0.2 // the model could not tell

	out, err := p.SaveEnrichment(t.Context(), in)
	if err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}
	if len(out.Applied) != 0 {
		t.Errorf("applied %v unreviewed on a weak claim", out.Applied)
	}
	if !contains(out.Suggested, "title") {
		t.Errorf("the weak title was not kept as a suggestion: %+v", out)
	}

	// The work keeps its placeholder rather than gaining a guess.
	var title string
	if err := p.QueryRow(t.Context(), `SELECT title FROM works WHERE id = $1`, workID).Scan(&title); err != nil {
		t.Fatalf("reading work: %v", err)
	}
	if title != PlaceholderTitle {
		t.Errorf("title = %q, want the placeholder kept", title)
	}

	// Nothing is thrown away: it is recorded, attributed, and waiting.
	pending, err := p.PendingSuggestions(t.Context(), 50)
	if err != nil {
		t.Fatalf("PendingSuggestions: %v", err)
	}
	if !hasField(pending, "title") {
		t.Error("a low-confidence claim was discarded rather than suggested")
	}
}

// A title that does not look like a title is never applied, however confident
// the model was. This is the check that stops a copyright page becoming a book.
func TestEnrichmentNeverAppliesAnImplausibleTitle(t *testing.T) {
	p := testPool(t)

	for _, bad := range []string{
		"Microsoft Word - chapter1.doc",
		"untitled document",
		"book.indb",
		// A model that lost its place mid-title. Verbatim from gemma4-12b.
		"Numerical Methods in Engineering and Science: C, C++, and MATLAB" +
			strings.Repeat("®.", 30),
	} {
		t.Run(bad, func(t *testing.T) {
			workID := untitledWork(t, p)

			in := classification()
			in.WorkID = workID
			in.Title = bad
			in.Confidence = 0.99 // as sure as it can be, and still wrong

			out, err := p.SaveEnrichment(t.Context(), in)
			if err != nil {
				t.Fatalf("SaveEnrichment: %v", err)
			}
			if contains(out.Applied, "title") {
				t.Errorf("%q was applied as a title", bad)
			}
			// Nor offered for review. A suggestion is a claim somebody could
			// reasonably accept; these are not, and a review queue full of
			// things to dismiss is a review queue nobody opens.
			if contains(out.Suggested, "title") {
				t.Errorf("%q was queued for review as a title", bad)
			}
			if !contains(out.Refused, "title") {
				t.Errorf("%q was not recorded as refused: %v", bad, out.Refused)
			}

			// Still in the ledger, so a prompt that starts producing rubbish is
			// visible rather than silently dropped.
			var rejected bool
			if err := p.QueryRow(t.Context(), `
				SELECT rejected_at IS NOT NULL FROM enrichment_records
				WHERE work_id = $1 AND field = 'title' AND source = 'llm'`,
				workID).Scan(&rejected); err != nil {
				t.Fatalf("reading the ledger: %v", err)
			}
			if !rejected {
				t.Error("the refused title was not recorded as rejected")
			}

			var title string
			if err := p.QueryRow(t.Context(), `SELECT title FROM works WHERE id = $1`, workID).Scan(&title); err != nil {
				t.Fatalf("reading work: %v", err)
			}
			if title != PlaceholderTitle {
				t.Errorf("title = %q, want the placeholder kept", title)
			}
		})
	}
}

// The bar is a dial, not a constant of nature: a model whose confidence is
// scored differently needs a different one.
func TestEnrichmentConfidenceBarIsConfigurable(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	in := classification()
	in.WorkID = workID
	in.Confidence = 0.3
	in.MinApplyConfidence = 0.2 // this deployment trusts its model more

	out, err := p.SaveEnrichment(t.Context(), in)
	if err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}
	if !contains(out.Applied, "title") {
		t.Errorf("a claim above the configured bar was not applied: %+v", out)
	}
}

// A title the model could not have read in the book is proposed, not written.
//
// The one signal here that ever actually stops a value being applied: the
// model's own confidence comes back at 1.00 for almost every claim it makes,
// so the bar it is measured against has never blocked anything.
func TestAnUngroundedValueIsSuggestedRatherThanApplied(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	in := classification()
	in.WorkID = workID
	// Confidence well over the bar, exactly as the real model reports it.
	in.Confidence = 1.0
	in.Ungrounded = map[string]bool{"title": true}

	out, err := p.SaveEnrichment(t.Context(), in)
	if err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}

	for _, field := range out.Applied {
		if field == "title" {
			t.Fatal("a title that is nowhere in the book was written onto the work")
		}
	}
	var suggested bool
	for _, field := range out.Suggested {
		if field == "title" {
			suggested = true
		}
	}
	if !suggested {
		t.Errorf("the title was neither applied nor suggested: %+v", out)
	}

	// Still "Untitled", and the claim is waiting in the review screen rather
	// than thrown away - it may well be right and merely missing from a bad
	// extraction.
	var title string
	if err := p.QueryRow(t.Context(),
		`SELECT title FROM works WHERE id = $1`, workID).Scan(&title); err != nil {
		t.Fatalf("reading work: %v", err)
	}
	if title != PlaceholderTitle {
		t.Errorf("title = %q, want it left alone", title)
	}

	var waiting int
	if err := p.QueryRow(t.Context(), `
		SELECT count(*) FROM enrichment_records
		WHERE work_id = $1 AND field = 'title'
		  AND accepted_at IS NULL AND rejected_at IS NULL`, workID).Scan(&waiting); err != nil {
		t.Fatalf("reading the ledger: %v", err)
	}
	if waiting != 1 {
		t.Errorf("%d title suggestions waiting for review, want 1", waiting)
	}
}

// And a grounded one is applied as before, so this cannot quietly become a
// rule that blocks everything.
func TestAGroundedValueIsStillApplied(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	in := classification()
	in.WorkID = workID
	in.Ungrounded = map[string]bool{"series": true}

	out, err := p.SaveEnrichment(t.Context(), in)
	if err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}
	var appliedTitle bool
	for _, field := range out.Applied {
		if field == "title" {
			appliedTitle = true
		}
	}
	if !appliedTitle {
		t.Errorf("a grounded title was not applied: %+v", out)
	}
}

// authorsOf reads a work's byline straight from the table: the fixture here
// has no owner, so Work() correctly cannot see it.
func authorsOf(t *testing.T, p *Pool, workID uuid.UUID) []string {
	t.Helper()
	rows, err := p.Query(t.Context(), `
		SELECT a.name FROM work_authors wa
		JOIN authors a ON a.id = wa.author_id
		WHERE wa.work_id = $1 ORDER BY wa.position`, workID)
	if err != nil {
		t.Fatalf("reading authors: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning author: %v", err)
		}
		out = append(out, name)
	}
	return out
}

// The byline. The prompt has asked for it since v1 and the model has been
// answering; until now nothing carried it past the worker, so 29 books in a
// real library sat on the shelf with no author while the model had read one
// off the title page of every single one.
func TestAByLineIsAppliedToABookWithNoAuthor(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	if got := authorsOf(t, p, workID); len(got) != 0 {
		t.Fatalf("the fixture already has authors %v; this tests the empty case", got)
	}

	in := classification()
	in.WorkID = workID
	in.Authors = []string{"Ada Lovelace", "Charles Babbage"}

	out, err := p.SaveEnrichment(t.Context(), in)
	if err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}
	var applied bool
	for _, field := range out.Applied {
		if field == "authors" {
			applied = true
		}
	}
	if !applied {
		t.Fatalf("the byline was not applied: %+v", out)
	}

	got := authorsOf(t, p, workID)
	if len(got) != 2 || got[0] != "Ada Lovelace" || got[1] != "Charles Babbage" {
		t.Errorf("authors = %v, want both in the order the model gave", got)
	}
}

// A model's reading of a byline is a weaker claim than the file's own, so it
// must never be added to a book that already has one. A book whose EPUB named
// one author while the title page names two is not one to quietly re-credit.
func TestAByLineNeverReplacesOneTheFileGave(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)

	// A file that declared its author, which is the case being protected.
	in := sampleInput(hashOf("c"), "/library/described.epub", "A Described Book", "The Real Author")
	book, err := p.UpsertBookFile(t.Context(), in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	before := authorsOf(t, p, book.WorkID)
	if len(before) == 0 {
		t.Fatal("the fixture has no author to protect")
	}

	enrich := classification()
	enrich.WorkID = book.WorkID
	enrich.Authors = []string{"Somebody Else Entirely"}

	if _, err := p.SaveEnrichment(t.Context(), enrich); err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}

	after := authorsOf(t, p, book.WorkID)
	for _, name := range after {
		if name == "Somebody Else Entirely" {
			t.Error("a model's byline was added to a book that already had one")
		}
	}
	if len(after) != len(before) {
		t.Errorf("authors went from %v to %v", before, after)
	}
}

// Accepting a suggestion has to actually apply it.
//
// Authors and subjects have no column on works, and the resolver mapped only
// the four that do - so accepting an author stamped the record accepted and
// wrote nothing. The administrator saw a 2xx, the book still had no author,
// and the record never came back to the queue because it was now resolved.
// Authors became offerable the moment the model's byline started being kept.
func TestAcceptingASuggestionWithNoColumnAppliesIt(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	if _, err := p.Exec(t.Context(),
		`DELETE FROM work_authors WHERE work_id = $1`, workID); err != nil {
		t.Fatalf("clearing authors: %v", err)
	}

	// A suggestion of each kind, as SaveEnrichment would record one.
	for _, s := range []struct {
		field string
		value string
	}{
		{"authors", `["Ada Lovelace"]`},
		{"subjects", `["mathematics"]`},
		{"title", `"A Real Title"`},
	} {
		var id uuid.UUID
		if err := p.QueryRow(t.Context(), `
			INSERT INTO enrichment_records (work_id, field, value, source, model_id)
			VALUES ($1, $2, $3::jsonb, 'llm', 'test')
			RETURNING id`, workID, s.field, s.value).Scan(&id); err != nil {
			t.Fatalf("recording a %s suggestion: %v", s.field, err)
		}
		if err := p.ResolveSuggestion(t.Context(), id, true); err != nil {
			t.Fatalf("accepting %s: %v", s.field, err)
		}
	}

	if got := authorsOf(t, p, workID); len(got) != 1 || got[0] != "Ada Lovelace" {
		t.Errorf("authors = %v, want the accepted one", got)
	}

	var tags int
	if err := p.QueryRow(t.Context(),
		`SELECT count(*) FROM work_tags WHERE work_id = $1`, workID).Scan(&tags); err != nil {
		t.Fatalf("counting tags: %v", err)
	}
	if tags == 0 {
		t.Error("an accepted subject added no tag")
	}

	// And the title carries sort_title with it, which is what the shelf orders
	// and pages by.
	var title, sortTitle string
	if err := p.QueryRow(t.Context(),
		`SELECT title, sort_title FROM works WHERE id = $1`, workID).Scan(&title, &sortTitle); err != nil {
		t.Fatalf("reading the work: %v", err)
	}
	if title != "A Real Title" {
		t.Errorf("title = %q", title)
	}
	if sortTitle == PlaceholderTitle || sortTitle == "" {
		t.Errorf("sort_title = %q, want it to follow the title: the shelf sorts by this", sortTitle)
	}
}

// The single-work enqueue asks the sweep's question in the same statement as
// its insert: a work with a readable file and no answer to this prompt is
// queued once, for whoever asked; one already described, one already queued,
// and one whose only file is missing are not.
func TestEnqueueIfNeedsEnrichment(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	cleanJobs(t, p)
	ctx := t.Context()
	alice := secondUser(t, p)
	out, err := p.UpsertBookFile(ctx, uploadInput(fmt.Sprintf("%064x", 0xe1), "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	const prompt = "classify/test"
	enqueue := func(requester uuid.UUID) bool {
		t.Helper()
		work := out.WorkID
		added, err := p.EnqueueIfNeedsEnrichment(ctx, EnqueueOptions{
			Type: domain.JobEnrich, TargetID: &work, RequestedBy: requester,
		}, prompt)
		if err != nil {
			t.Fatalf("EnqueueIfNeedsEnrichment: %v", err)
		}
		return added
	}

	if !enqueue(alice) {
		t.Fatal("a work waiting to be described was not queued")
	}
	var requester *uuid.UUID
	if err := p.QueryRow(ctx, `SELECT requested_by FROM jobs WHERE target_id = $1`, out.WorkID).Scan(&requester); err != nil ||
		requester == nil || *requester != alice {
		t.Errorf("requested_by = %v, %v; want the uploader", requester, err)
	}
	if enqueue(uuid.New()) {
		t.Error("a second job was queued for a work with one live")
	}
	if n := rowCountJobs(t, p, out.WorkID); n != 1 {
		t.Errorf("%d jobs for the work, want 1", n)
	}

	// Described under this prompt: nothing more to ask.
	if _, err := p.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO enrichment_records (work_id, field, value, source, model_id, prompt_version)
		VALUES ($1, 'summary', '"x"', 'llm', 'm', $2)`, out.WorkID, prompt); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if enqueue(alice) {
		t.Error("a work already described under this prompt was queued")
	}

	// Asked recently under this prompt, with nothing to show for it: not
	// asked again for a week. Under another prompt, or longer ago, it is.
	if _, err := p.Exec(ctx, `DELETE FROM enrichment_records`); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO jobs (type, target_id, status, payload, finished_at)
		VALUES ('enrich', $1, 'succeeded', jsonb_build_object('promptId', $2::text), now())`,
		out.WorkID, prompt); err != nil {
		t.Fatalf("recording an attempt: %v", err)
	}
	if enqueue(alice) {
		t.Error("a work asked about under this prompt an hour ago was queued again")
	}
	// An administrator asking now is not held back by it.
	if works, err := p.WorksNeedingEnrichment(ctx, prompt, 10); err != nil || len(works) != 1 {
		t.Errorf("the sweep = %v, %v; want the work, cooldown or not", works, err)
	}
	// A job that never reached the model - skipped until it gave up, every
	// attempt given back - did not ask anything, and does not count.
	if _, err := p.Exec(ctx, `UPDATE jobs SET status = 'failed', attempts = 0, max_attempts = 3, skips = 40`); err != nil {
		t.Fatalf("giving up: %v", err)
	}
	if !enqueue(alice) {
		t.Error("a work whose only job never reached the model was held back")
	}
	if _, err := p.Exec(ctx, `DELETE FROM jobs WHERE status = 'pending'`); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	// One that failed by using up its attempts did ask, and does.
	if _, err := p.Exec(ctx, `UPDATE jobs SET attempts = 3, skips = 0`); err != nil {
		t.Fatalf("failing: %v", err)
	}
	if enqueue(alice) {
		t.Error("a work whose job failed after asking was queued again within the week")
	}
	if _, err := p.Exec(ctx, `UPDATE jobs SET finished_at = now() - interval '8 days'`); err != nil {
		t.Fatalf("ageing: %v", err)
	}
	if !enqueue(alice) {
		t.Error("a work asked about over a week ago was not queued")
	}
	if _, err := p.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatalf("clearing: %v", err)
	}

	// Only formats a sample can be read from count: a comic has nothing.
	if _, err := p.Exec(ctx, `UPDATE book_files SET format = 'cbz'`); err != nil {
		t.Fatalf("format: %v", err)
	}
	if enqueue(alice) {
		t.Error("a work with only a comic was queued")
	}
	if _, err := p.Exec(ctx, `UPDATE book_files SET format = 'epub'`); err != nil {
		t.Fatalf("format: %v", err)
	}

	// A work whose only file is missing - or removed for everyone - has
	// nothing to sample.
	if _, err := p.Exec(ctx, `DELETE FROM enrichment_records`); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE book_files SET missing_since = now()`); err != nil {
		t.Fatalf("missing: %v", err)
	}
	if enqueue(alice) {
		t.Error("a work with no readable file was queued")
	}

	// The sweep agrees with it.
	if works, err := p.WorksNeedingEnrichment(ctx, prompt, 10); err != nil || len(works) != 0 {
		t.Errorf("WorksNeedingEnrichment = %v, %v; want the same answer", works, err)
	}
}

func rowCountJobs(t *testing.T, p *Pool, target uuid.UUID) int {
	t.Helper()
	var n int
	if err := p.QueryRow(t.Context(), `SELECT count(*) FROM jobs WHERE target_id = $1`, target).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	return n
}
