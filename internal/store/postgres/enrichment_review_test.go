// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// pendingByFieldForReview indexes the review queue by field, for one work.
func pendingByFieldForReview(t *testing.T, p *Pool, workID uuid.UUID) map[string]Suggestion {
	t.Helper()
	pending, err := p.PendingSuggestions(t.Context(), 0)
	if err != nil {
		t.Fatalf("PendingSuggestions: %v", err)
	}
	out := map[string]Suggestion{}
	for _, s := range pending {
		if s.WorkID == workID {
			out[s.Field] = s
		}
	}
	return out
}

// workColumnsForReview reads the columns a suggestion can change.
func workColumnsForReview(t *testing.T, p *Pool, workID uuid.UUID) (subtitle, summary, series string) {
	t.Helper()
	var sub, sum, ser *string
	if err := p.QueryRow(t.Context(), `SELECT subtitle, summary, series FROM works WHERE id = $1`, workID).
		Scan(&sub, &sum, &ser); err != nil {
		t.Fatalf("reading work: %v", err)
	}
	return deref(sub), deref(sum), deref(ser)
}

// A weak claim waits for a person. Accepting writes it onto the work; rejecting
// leaves the work alone; and a decision, once made, cannot be made again.
func TestResolvingSuggestionsForEachColumn(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)
	ctx := t.Context()

	in := classification()
	in.WorkID = workID
	in.Subtitle = "A Gentle Start"
	in.Confidence = 0.2
	out, err := p.SaveEnrichment(ctx, in)
	if err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}
	if len(out.Applied) != 0 {
		t.Fatalf("a weak claim was applied: %+v", out)
	}

	pending := pendingByFieldForReview(t, p, workID)
	for _, field := range []string{"subtitle", "summary", "series", "audience"} {
		if _, ok := pending[field]; !ok {
			t.Fatalf("no %s suggestion in %v", field, pending)
		}
	}

	for _, field := range []string{"subtitle", "summary", "audience"} {
		if err := p.ResolveSuggestion(ctx, pending[field].ID, true); err != nil {
			t.Fatalf("accepting %s: %v", field, err)
		}
	}
	if err := p.ResolveSuggestion(ctx, pending["series"].ID, false); err != nil {
		t.Fatalf("rejecting series: %v", err)
	}

	subtitle, summary, series := workColumnsForReview(t, p, workID)
	if subtitle != "A Gentle Start" || summary != "A first course in formal logic." {
		t.Errorf("accepted values not applied: subtitle %q, summary %q", subtitle, summary)
	}
	if series != "" {
		t.Errorf("a rejected series was applied: %q", series)
	}

	// The audience has no column; accepting it is the record of a person's
	// agreement.
	if n := rowCount(t, p, `SELECT count(*) FROM enrichment_records WHERE id = $1 AND accepted_at IS NOT NULL`,
		pending["audience"].ID); n != 1 {
		t.Error("the accepted audience was not stamped accepted")
	}
	if n := rowCount(t, p, `SELECT count(*) FROM enrichment_records WHERE id = $1 AND rejected_at IS NOT NULL`,
		pending["series"].ID); n != 1 {
		t.Error("the rejected series was not stamped rejected")
	}

	remaining := pendingByFieldForReview(t, p, workID)
	for _, field := range []string{"subtitle", "summary", "series", "audience"} {
		if _, ok := remaining[field]; ok {
			t.Errorf("%s is still pending after being decided", field)
		}
	}

	t.Run("a decided suggestion cannot be decided again", func(t *testing.T) {
		if err := p.ResolveSuggestion(ctx, pending["series"].ID, true); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ResolveSuggestion: %v, want store.ErrNotFound", err)
		}
		if _, _, series := workColumnsForReview(t, p, workID); series != "" {
			t.Errorf("a rejected suggestion was applied on a second try: %q", series)
		}
	})

	t.Run("an unknown suggestion is not found", func(t *testing.T) {
		if err := p.ResolveSuggestion(ctx, uuid.New(), true); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ResolveSuggestion: %v, want store.ErrNotFound", err)
		}
	})
}

// An accepted byline credits only the names that are names.
func TestAcceptingABylineSkipsBlankNames(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	var id uuid.UUID
	if err := p.QueryRow(t.Context(), `
		INSERT INTO enrichment_records (work_id, field, value, source, model_id)
		VALUES ($1, 'authors', '["  ", "Grace Hopper"]'::jsonb, 'llm', 'test')
		RETURNING id`, workID).Scan(&id); err != nil {
		t.Fatalf("recording a suggestion: %v", err)
	}
	if err := p.ResolveSuggestion(t.Context(), id, true); err != nil {
		t.Fatalf("ResolveSuggestion: %v", err)
	}
	if got := authorsOf(t, p, workID); len(got) != 1 || got[0] != "Grace Hopper" {
		t.Errorf("authors = %v, want only Grace Hopper", got)
	}
}

// The subtitle is not written when the title it would sit under already says
// it; a subtitle that adds something is.
func TestEnrichmentSubtitleIsNotSaidTwice(t *testing.T) {
	for _, tc := range []struct {
		name, subtitle, want string
	}{
		{"repeats the title", "Geeky Projects", ""},
		{"adds to the title", "For the Curious Programmer", "For the Curious Programmer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPool(t)
			workID := untitledWork(t, p)

			in := classification()
			in.WorkID = workID
			in.Title = "Python Playground: Geeky Projects"
			in.Subtitle = tc.subtitle
			out, err := p.SaveEnrichment(t.Context(), in)
			if err != nil {
				t.Fatalf("SaveEnrichment: %v", err)
			}
			if !contains(out.Applied, "subtitle") || !contains(out.Applied, "title") {
				t.Fatalf("outcome = %+v, want title and subtitle applied", out)
			}
			if subtitle, _, _ := workColumnsForReview(t, p, workID); subtitle != tc.want {
				t.Errorf("subtitle = %q, want %q", subtitle, tc.want)
			}
		})
	}
}

// A byline read off the title page fills a work with none, skipping blanks;
// subject tags skip what slugs to nothing and count each tag once.
func TestEnrichmentCleansBylinesAndSubjects(t *testing.T) {
	p := testPool(t)
	workID := untitledWork(t, p)

	in := classification()
	in.WorkID = workID
	in.Authors = []string{"", "Barbara Liskov"}
	in.Subjects = []string{"!!!", "Logic", "logic", "Set theory"}
	out, err := p.SaveEnrichment(t.Context(), in)
	if err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}
	if got := authorsOf(t, p, workID); len(got) != 1 || got[0] != "Barbara Liskov" {
		t.Errorf("authors = %v, want only Barbara Liskov", got)
	}
	if out.TagsAdded != 2 {
		t.Errorf("TagsAdded = %d, want 2 (logic, set theory)", out.TagsAdded)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM work_tags WHERE work_id = $1 AND source = 'llm'`, workID); n != 2 {
		t.Errorf("%d model tags linked, want 2", n)
	}
}

// Confidence is a fraction: nothing reported is stored as nothing, and an
// overconfident model is capped at certainty.
func TestNullableConfidence(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want *float64
	}{
		{0, nil},
		{-0.5, nil},
		{0.4, ptrFloatForReview(0.4)},
		{1.7, ptrFloatForReview(1)},
	} {
		got := nullableConfidence(tc.in)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("nullableConfidence(%v) = %v, want nil", tc.in, *got)
		case tc.want != nil && (got == nil || *got != *tc.want):
			t.Errorf("nullableConfidence(%v) = %v, want %v", tc.in, got, *tc.want)
		}
	}
}

func ptrFloatForReview(f float64) *float64 { return &f }

// There is nothing to describe for a work that does not exist, or whose only
// file has no text a model could read.
func TestEnrichmentSubjectWithNothingToRead(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	if _, err := p.EnrichmentSubject(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("EnrichmentSubject(unknown) = %v, want store.ErrNotFound", err)
	}

	comic := IngestInput{
		ContentHash: hashOf("e"),
		Path:        "/library/comic.cbz",
		Format:      domain.FormatCBZ,
		SizeBytes:   4096,
		Status:      domain.IngestReady,
		Result:      &ingest.Result{Format: domain.FormatCBZ, Metadata: ingest.Metadata{Title: "Pictures Only"}},
	}
	out, err := p.UpsertBookFile(ctx, comic)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if _, err := p.EnrichmentSubject(ctx, out.WorkID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("EnrichmentSubject(comic) = %v, want store.ErrNotFound", err)
	}

	// Nor is it offered to the sweep.
	ids, err := p.WorksNeedingEnrichment(ctx, "classify/v1", 0)
	if err != nil {
		t.Fatalf("WorksNeedingEnrichment: %v", err)
	}
	if containsID(ids, out.WorkID) {
		t.Error("a work with nothing to read was offered for enrichment")
	}
}

func TestEnqueueIfNeedsEnrichmentNeedsATarget(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	added, err := p.EnqueueIfNeedsEnrichment(t.Context(), EnqueueOptions{Type: domain.JobEnrich}, "classify/v1")
	if err == nil || added {
		t.Fatalf("EnqueueIfNeedsEnrichment with no target = %v, %v; want an error", added, err)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM jobs`); n != 0 {
		t.Errorf("%d jobs queued for no work", n)
	}
}
