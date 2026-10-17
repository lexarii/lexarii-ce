// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// EnrichmentInput is one model classification, ready to store.
//
// Spelled out here rather than taking the llm package's type: the store must
// not know what a model is. It stores a claim about a work, who made it, and
// how sure they were - which would read the same if the claim came from a
// cataloguing service or a person with a spreadsheet.
type EnrichmentInput struct {
	WorkID uuid.UUID

	Title    string
	Subtitle string
	// Authors is the byline the model read off the title page.
	//
	// Applied ONLY to a work that has none. A model's reading of a byline is a
	// weaker claim than the file's own metadata and must never compete with
	// it - but against nothing it is far better than nothing, and 29 books in
	// a real library are on the shelf with no author at all: unattributed,
	// unfindable by author, exported as citations with nobody's name on them.
	Authors     []string
	Edition     string
	Summary     string
	Series      string
	SeriesIndex *float64
	Subjects    []string
	Audience    string

	// Model and PromptID say who made the claim and on what basis, and are
	// stored with every record so it can be told from the next one.
	Model      string
	PromptID   string
	Confidence float64

	// MinApplyConfidence is the bar for writing a value straight onto the work
	// rather than only proposing it. Zero uses DefaultMinApplyConfidence.
	MinApplyConfidence float64

	// Ungrounded names the fields whose value could not be found in the text
	// the model was shown, and those are recorded as suggestions rather than
	// applied. See llm.Grounded for why this exists and what it costs: the
	// model's own confidence came back at 1.00 for 1,560 of 1,576 claims, so
	// the bar above has never blocked anything, while "is this actually
	// printed in the book" separates one claim from another.
	//
	// Empty means nothing was checked, which is what a caller with no sample
	// to check against passes.
	Ungrounded map[string]bool
}

// DefaultMinApplyConfidence is the bar a claim must clear to be applied
// unreviewed to a field the book left empty.
//
// Empty fields are the ones with no protection: nothing is being overwritten,
// so nothing stops a weak guess from becoming what the library says the book
// is. Running the enrichment over a real library made that concrete - titles
// like "Copyrightc" and "Chattamvelli Et Al", read off a copyright page by a
// model with nothing better to go on, were applied with no review.
//
// Below the bar the claim is still recorded, as a suggestion. Nothing is
// thrown away; it just does not become a fact on its own.
//
// The number is a dial, not a discovery: how a particular model scores its own
// confidence is a property of that model, and this default is deliberately
// cautious rather than tuned.
const DefaultMinApplyConfidence = 0.6

// EnrichmentOutcome reports what was actually done with a classification.
type EnrichmentOutcome struct {
	// Applied lists fields written straight onto the work, because the work had
	// nothing there.
	Applied []string
	// Suggested lists fields recorded for a person to decide on, because the
	// work already had a value from the file itself.
	Suggested []string
	// Refused lists fields recorded and rejected outright, because the value
	// could not be right whoever reviewed it.
	Refused []string
	// TagsAdded counts subject tags linked to the work.
	TagsAdded int
}

// publisherSources are the sources an LLM must never overwrite.
//
// A value that came out of the file, or that a person typed, is a fact about
// the book. A model's opinion is a suggestion, however confident it sounds, and
// the difference has to survive in the database or the library slowly fills
// with plausible fiction nobody can distinguish from the real thing.
var publisherSources = []string{
	string(domain.SourceEmbedded),
	string(domain.SourceUser),
	string(domain.SourceExternal),
}

// SaveEnrichment stores a classification.
//
// A field the work has no value for is FILLED, because an empty title helps
// nobody and the provenance says where it came from. A field the work already
// has is only SUGGESTED. Nothing here ever silently replaces a publisher fact.
//
// Idempotent per (work, field, prompt): re-running enrichment with the same
// prompt replaces that prompt's earlier suggestions rather than stacking a
// second copy beside them.
func (p *Pool) SaveEnrichment(ctx context.Context, in EnrichmentInput) (EnrichmentOutcome, error) {
	var out EnrichmentOutcome

	tx, err := p.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("saving enrichment: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// What the work already holds, and from where. The decision to fill or
	// suggest is per FIELD, because a book routinely arrives with a real title
	// and nothing else.
	existing, err := existingFields(ctx, tx, in.WorkID)
	if err != nil {
		return out, err
	}

	confidence := in.Confidence
	bar := in.MinApplyConfidence
	if bar <= 0 {
		bar = DefaultMinApplyConfidence
	}

	// A value the model could not have read in the book is proposed, never
	// applied. Not discarded either: extraction from a PDF is lossy enough
	// that a real title can genuinely be missing from the sample, so the cost
	// of being wrong here is one row in the review screen rather than a book
	// left wrongly empty.
	ungrounded := in.Ungrounded

	// Whether this work already has a byline, read inside the transaction so
	// two enrichments cannot both decide it was empty.
	var hasAuthors bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM work_authors WHERE work_id = $1)`,
		in.WorkID).Scan(&hasAuthors); err != nil {
		return out, fmt.Errorf("checking for authors: %w", mapError(err))
	}

	fields := []struct {
		name  string
		value any
		empty bool
		// plausible is an extra bar for fields where a bad value is worse than
		// none. A title is the one a person reads first.
		plausible bool
		apply     func(context.Context, pgx.Tx) error
	}{
		{
			name:      "title",
			value:     in.Title,
			empty:     in.Title == "",
			plausible: ingest.PlausibleTitle(in.Title),
			apply: func(ctx context.Context, tx pgx.Tx) error {
				// sort_title with it. It is what the shelf orders and pages by,
				// and a title applied without it leaves the book displaying its
				// real name while still filed under "Untitled".
				_, err := tx.Exec(ctx, `
					UPDATE works SET title = $2, sort_title = $3, updated_at = now()
					WHERE id = $1`, in.WorkID, in.Title, SortTitle(in.Title))
				return err
			},
		},
		{
			// The subtitle exists so the title does not have to carry it. A
			// model given nowhere to put "A Brain-Friendly Guide" put it in
			// the title, every time, because dropping it would have been
			// unfaithful to the page.
			name:  "subtitle",
			value: in.Subtitle,
			empty: in.Subtitle == "",
			apply: func(ctx context.Context, tx pgx.Tx) error {
				// Not when the title already says it. A publisher's own title
				// is often "Python Playground: Geeky Projects for the Curious
				// Programmer", and the model correctly reports the half after
				// the colon as the subtitle - which then renders under a title
				// that already ends with those words. 39 of the first 116
				// books enriched this way came out reading themselves twice.
				//
				// The title wins, because it came from the file and a
				// publisher's title outranks a model's reading of a title
				// page. The subtitle is simply not worth saying twice.
				_, err := tx.Exec(ctx, `
					UPDATE works SET subtitle = $2, updated_at = now()
					WHERE id = $1 AND position(lower($2) in lower(title)) = 0`,
					in.WorkID, in.Subtitle)
				return err
			},
		},
		{
			// Recorded, not applied, on the same footing as audience: there is
			// no edition column to write it to yet. Getting it OUT of the
			// title is most of the value, and the record is where it waits for
			// a column to exist.
			name:  "edition",
			value: in.Edition,
			empty: in.Edition == "",
			apply: nil,
		},
		{
			name:  "summary",
			value: in.Summary,
			empty: in.Summary == "",
			apply: func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE works SET summary = $2, updated_at = now() WHERE id = $1`,
					in.WorkID, in.Summary)
				return err
			},
		},
		{
			name:  "series",
			value: in.Series,
			empty: in.Series == "",
			apply: func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					UPDATE works SET series = $2, series_index = $3, updated_at = now()
					WHERE id = $1`, in.WorkID, in.Series, in.SeriesIndex)
				return err
			},
		},
		{
			// Only ever ADDED, never replacing: the guard below is "the work
			// has no authors", not "the ledger has no record". A book whose
			// EPUB named one author while the title page names two is not a
			// book this should quietly re-credit.
			name:  "authors",
			value: in.Authors,
			empty: len(in.Authors) == 0 || hasAuthors,
			apply: func(ctx context.Context, tx pgx.Tx) error {
				contributors := make([]ingest.Contributor, 0, len(in.Authors))
				for _, name := range in.Authors {
					if strings.TrimSpace(name) == "" {
						continue
					}
					contributors = append(contributors, ingest.Contributor{
						Name: strings.TrimSpace(name),
						Role: domain.RoleAuthor,
					})
				}
				return linkAuthors(ctx, tx, in.WorkID, contributors)
			},
		},
		{
			name:  "subjects",
			value: in.Subjects,
			empty: len(in.Subjects) == 0,
			apply: nil, // tags are additive, handled below
		},
		{
			name:  "audience",
			value: in.Audience,
			empty: in.Audience == "",
			apply: nil, // no column of its own yet; the record is the value
		},
	}

	for _, field := range fields {
		if field.empty {
			continue
		}

		// Applied only when there is nothing to overwrite AND the claim is
		// strong enough to stand without review. Everything else is recorded
		// as a suggestion: kept, attributed, and waiting for a person.
		accepted := !existing[field.name].fromPublisher && confidence >= bar
		// A title that does not look like one is never applied, however
		// confident the model was. This is the same check that stops a filename
		// being recorded as a title during ingest.
		//
		// It is not offered for review either. A suggestion is a claim somebody
		// could reasonably accept, and "Numerical Methods in Engineering and
		// Science: C, C++, and MATLAB(R).(R).(R).(R)." is not one - it is the
		// model having lost its place. Recorded and rejected in the same breath:
		// the ledger still says what the model answered, which is what makes a
		// bad prompt visible later, and the review queue stays a list of real
		// decisions rather than a list of things to dismiss.
		refused := field.name == "title" && !field.plausible
		if refused {
			accepted = false
		}

		// A value that is not in the book is proposed rather than applied.
		//
		// Unlike the refusal above this is not a rejection: the claim may well
		// be right and merely missing from a bad extraction, so it goes to the
		// person as a suggestion. It is the only thing here that ever actually
		// stops a value being written, because the model's confidence never
		// does - 1,560 of 1,576 claims came back at exactly 1.00.
		if ungrounded[field.name] {
			accepted = false
		}

		// Replace this prompt's own earlier answer for this field, so re-running
		// enrichment does not stack duplicates. A person's accepted or rejected
		// decision is never touched.
		if _, err := tx.Exec(ctx, `
			DELETE FROM enrichment_records
			WHERE work_id = $1 AND field = $2 AND source = 'llm'
			  AND prompt_version IS NOT DISTINCT FROM $3
			  AND accepted_at IS NULL AND rejected_at IS NULL`,
			in.WorkID, field.name, in.PromptID,
		); err != nil {
			return out, fmt.Errorf("clearing previous suggestion for %q: %w", field.name, mapError(err))
		}

		now := time.Now()
		var acceptedAt, rejectedAt *time.Time
		switch {
		case accepted:
			acceptedAt = &now
		case refused:
			rejectedAt = &now
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO enrichment_records
				(work_id, field, value, source, model_id, prompt_version, confidence,
				 accepted_at, rejected_at)
			VALUES ($1, $2, $3, 'llm', $4, $5, $6, $7, $8)`,
			in.WorkID, field.name, jsonValue(field.value), in.Model, in.PromptID,
			nullableConfidence(confidence), acceptedAt, rejectedAt,
		); err != nil {
			return out, fmt.Errorf("recording enrichment for %q: %w", field.name, mapError(err))
		}

		if accepted {
			if field.apply != nil {
				if err := field.apply(ctx, tx); err != nil {
					return out, fmt.Errorf("applying %q: %w", field.name, mapError(err))
				}
			}
			out.Applied = append(out.Applied, field.name)
		} else if refused {
			out.Refused = append(out.Refused, field.name)
		} else {
			out.Suggested = append(out.Suggested, field.name)
		}
	}

	// Subject tags are additive whatever the title says, because a tag does not
	// displace anything: it is one more way to find the book. They carry their
	// source, so the UI can show which came from a model.
	if len(in.Subjects) > 0 {
		added, err := linkModelTags(ctx, tx, in.WorkID, in.Subjects, confidence)
		if err != nil {
			return out, err
		}
		out.TagsAdded = added
	}

	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("saving enrichment: %w", mapError(err))
	}
	return out, nil
}

// fieldState is what is already known about one field.
type fieldState struct {
	fromPublisher bool
}

// existingFields reports which fields already carry a value that a model must
// not overwrite.
//
// Two sources of truth, deliberately: the provenance ledger says where a value
// came from, and the work row says whether there is a value at all. A row whose
// title was written before the ledger existed still counts as a real title.
func existingFields(ctx context.Context, tx pgx.Tx, workID uuid.UUID) (map[string]fieldState, error) {
	out := make(map[string]fieldState)

	rows, err := tx.Query(ctx, `
		SELECT field FROM enrichment_records
		WHERE work_id = $1 AND source = ANY($2) AND rejected_at IS NULL`,
		workID, publisherSources)
	if err != nil {
		return nil, fmt.Errorf("reading provenance: %w", mapError(err))
	}
	for rows.Next() {
		var field string
		if err := rows.Scan(&field); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning provenance: %w", err)
		}
		out[field] = fieldState{fromPublisher: true}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading provenance: %w", mapError(err))
	}

	var title, summary, series *string
	if err := tx.QueryRow(ctx, `
		-- The placeholder is not a title. Treating it as one would leave every
		-- book that arrived without metadata permanently called "Untitled".
		SELECT NULLIF(NULLIF(title, ''), $2), summary, series
		FROM works WHERE id = $1`, workID, PlaceholderTitle,
	).Scan(&title, &summary, &series); err != nil {
		return nil, fmt.Errorf("reading work: %w", mapError(err))
	}

	// A value present on the work but absent from the ledger is still a value
	// somebody put there.
	for field, value := range map[string]*string{"title": title, "summary": summary, "series": series} {
		if value != nil && *value != "" {
			out[field] = fieldState{fromPublisher: true}
		}
	}
	return out, nil
}

// linkModelTags attaches subject tags, recording that a model chose them.
func linkModelTags(ctx context.Context, tx pgx.Tx, workID uuid.UUID, subjects []string, confidence float64) (int, error) {
	added := 0
	seen := make(map[string]struct{}, len(subjects))

	for _, name := range subjects {
		slug := TagSlug(name)
		if slug == "" {
			continue
		}
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}

		var tagID uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO tags (name, slug, is_subject)
			VALUES ($1, $2, true)
			ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug
			RETURNING id`, name, slug,
		).Scan(&tagID); err != nil {
			return added, fmt.Errorf("upserting tag %q: %w", name, mapError(err))
		}

		// DO NOTHING on conflict: a tag the file itself declared keeps its
		// 'embedded' source rather than being downgraded to a model's guess.
		tag, err := tx.Exec(ctx, `
			INSERT INTO work_tags (work_id, tag_id, source, confidence)
			VALUES ($1, $2, 'llm', $3)
			ON CONFLICT (work_id, tag_id) DO NOTHING`,
			workID, tagID, nullableConfidence(confidence))
		if err != nil {
			return added, fmt.Errorf("linking tag %q: %w", name, mapError(err))
		}
		added += int(tag.RowsAffected())
	}
	return added, nil
}

// nullableConfidence keeps a zero out of the column, since the CHECK allows 0
// but "no confidence reported" and "confidence of zero" are different claims.
func nullableConfidence(c float64) *float64 {
	if c <= 0 {
		return nil
	}
	if c > 1 {
		one := 1.0
		return &one
	}
	return &c
}

// EnrichmentSubject is everything the enricher needs about one work: what is
// already known, and which file to read.
type EnrichmentSubject struct {
	WorkID   uuid.UUID
	Title    string
	Authors  []string
	Language string

	// Path, Format, and Filename identify the file to sample.
	Path     string
	Format   domain.Format
	Filename string
}

// EnrichmentSubject loads a work and the best file to read it from.
//
// "Best" is the largest readable text format, because a 900-page PDF of the
// same work carries more of a title page than a 3-item EPUB stub, and a comic
// carries no text at all. A work whose only file is missing from disk is
// ErrNotFound: there is nothing to describe.
func (p *Pool) EnrichmentSubject(ctx context.Context, workID uuid.UUID) (EnrichmentSubject, error) {
	var out EnrichmentSubject
	out.WorkID = workID

	var title, language *string
	if err := p.QueryRow(ctx, `
		SELECT NULLIF(w.title, ''), NULLIF(w.language, '')
		FROM works w WHERE w.id = $1`, workID,
	).Scan(&title, &language); err != nil {
		return EnrichmentSubject{}, mapError(err)
	}
	out.Title = deref(title)
	out.Language = deref(language)

	rows, err := p.Query(ctx, `
		SELECT a.name
		FROM work_authors wa
		JOIN authors a ON a.id = wa.author_id
		WHERE wa.work_id = $1 AND wa.role = 'author'
		ORDER BY wa.position`, workID)
	if err != nil {
		return EnrichmentSubject{}, fmt.Errorf("reading authors: %w", mapError(err))
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return EnrichmentSubject{}, fmt.Errorf("scanning author: %w", err)
		}
		out.Authors = append(out.Authors, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return EnrichmentSubject{}, fmt.Errorf("reading authors: %w", mapError(err))
	}

	// Text formats only, and the biggest of them: a model cannot read a comic,
	// and the fuller file carries more of the front matter.
	if err := p.QueryRow(ctx, `
		SELECT bf.path, bf.format, COALESCE(bf.original_filename, '')
		FROM book_files bf
		JOIN editions ed ON ed.id = bf.edition_id
		WHERE ed.work_id = $1
		  AND bf.missing_since IS NULL
		  AND bf.format IN ('epub', 'pdf')
		ORDER BY bf.size_bytes DESC NULLS LAST
		LIMIT 1`, workID,
	).Scan(&out.Path, &out.Format, &out.Filename); err != nil {
		return EnrichmentSubject{}, mapError(err)
	}

	if out.Filename == "" {
		out.Filename = filepath.Base(out.Path)
	}
	return out, nil
}

// Suggestion is a model's answer awaiting a person's decision.
type Suggestion struct {
	ID        uuid.UUID
	WorkID    uuid.UUID
	WorkTitle string
	Field     string
	Value     []byte
	// Current is what the work holds now, so a person can see what accepting
	// would replace. A proposal shown on its own is not a decision anyone can
	// make: "Essentials" only looks wrong next to "Algebra Essentials".
	Current     string
	Model       string
	PromptID    string
	Confidence  *float64
	GeneratedAt time.Time
}

// PendingSuggestions lists enrichment awaiting review, oldest first.
func (p *Pool) PendingSuggestions(ctx context.Context, limit int) ([]Suggestion, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	rows, err := p.Query(ctx, `
		SELECT e.id, e.work_id, w.title, e.field, e.value,
		       COALESCE(
		           CASE e.field
		               WHEN 'title'   THEN w.title
		               WHEN 'summary' THEN w.summary
		               WHEN 'series'  THEN w.series
		           END, ''),
		       COALESCE(e.model_id, ''), COALESCE(e.prompt_version, ''),
		       e.confidence, e.generated_at
		FROM enrichment_records e
		JOIN works w ON w.id = e.work_id
		WHERE e.accepted_at IS NULL AND e.rejected_at IS NULL
		ORDER BY e.generated_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("reading suggestions: %w", mapError(err))
	}
	defer rows.Close()

	var out []Suggestion
	for rows.Next() {
		var s Suggestion
		if err := rows.Scan(&s.ID, &s.WorkID, &s.WorkTitle, &s.Field, &s.Value,
			&s.Current, &s.Model, &s.PromptID, &s.Confidence, &s.GeneratedAt); err != nil {
			return nil, fmt.Errorf("scanning suggestion: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// WorksNeedingEnrichment lists works no model has described yet.
//
// Keyed on the prompt version: changing what the prompt asks for means every
// book is un-enriched again with respect to the new question, which is the
// honest reading of "this record came from classify/v1".
//
// No cooldown: this is an administrator asking now, so a work tried recently
// with nothing to show for it is asked about again. Enrichment on ingest,
// which nobody presses, waits a week (needsEnrichment).
func (p *Pool) WorksNeedingEnrichment(ctx context.Context, promptID string, limit int) ([]uuid.UUID, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	rows, err := p.Query(ctx, `
		SELECT w.id
		FROM works w
		WHERE `+needsEnrichment("$1", false)+`
		ORDER BY w.created_at
		LIMIT $2`, promptID, limit)
	if err != nil {
		return nil, fmt.Errorf("finding works to enrich: %w", mapError(err))
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning work id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// needsEnrichment is the one definition of a work waiting to be described,
// over works as w, with the prompt version in the placeholder given. Shared by
// the sweep and the single-work enqueue, so the two cannot drift apart. The
// sweep is an administrator asking now, and passes cooldown false to ask
// again about works tried recently; enrichment on ingest keeps it.
func needsEnrichment(promptID string, cooldown bool) string {
	if !cooldown {
		return describable(promptID)
	}
	return describable(promptID) + `
		-- Asked recently under this prompt and nothing came of it - no text
		-- to read, an empty answer, a failure: not asked again for a week.
		-- Otherwise every re-extraction would spend the model on the same
		-- book for the same nothing. A cooldown rather than a permanent
		-- marker, because the reasons pass: a model that answers better, a
		-- file that is re-extracted with text. A job from before the prompt
		-- id was recorded does not count.
		--
		-- Only a job that got as far as trying counts: one that succeeded,
		-- or failed by using up its attempts (Fail, or the reclaimer). A job
		-- that gave up because the model was never there - skipped until
		-- RescheduleSkipped stopped it - did not ask anything, and its
		-- attempts are always below the maximum, since every skip gives its
		-- attempt back. Served by jobs_target_idx; a work has few jobs.
		AND NOT EXISTS (
			SELECT 1 FROM jobs j
			WHERE j.target_id = w.id AND j.type = 'enrich'
			  AND (j.status = 'succeeded'
			       OR (j.status = 'failed' AND j.attempts >= j.max_attempts))
			  AND j.finished_at > now() - interval '7 days'
			  AND j.payload ->> 'promptId' = ` + promptID + `
		)`
}

// describable is a work no model has described under this prompt, with a
// file a sample can be read from.
func describable(promptID string) string {
	return `NOT EXISTS (
			SELECT 1 FROM enrichment_records e
			WHERE e.work_id = w.id AND e.source = 'llm' AND e.prompt_version = ` + promptID + `
		)
		-- A work with no readable file cannot be sampled, so asking a model
		-- about it would be asking it to invent one. Only the formats a
		-- sample can be read from count (EnrichmentSubject): a comic would
		-- queue a job that finds nothing to read, every time. A file removed
		-- for everyone is missing, so blocked bytes are never sampled either.
		AND EXISTS (
			SELECT 1 FROM editions ed
			JOIN book_files bf ON bf.edition_id = ed.id
			WHERE ed.work_id = w.id AND bf.missing_since IS NULL
			  AND bf.format IN ('epub', 'pdf')
		)`
}

// EnqueueIfNeedsEnrichment queues opts - the enrich job for the work named
// by opts.TargetID - only if that work is waiting to be described under
// promptID, by the same rule WorksNeedingEnrichment sweeps with. It reports
// whether a job was added.
//
// One statement, as EnqueueIfNeedsChunks: the check and the insert see the
// same state. A live job for the same work already in the queue is left
// alone, with its own requester.
func (p *Pool) EnqueueIfNeedsEnrichment(ctx context.Context, opts EnqueueOptions, promptID string) (bool, error) {
	if opts.TargetID == nil {
		return false, fmt.Errorf("enqueuing %s: the target work is required", opts.Type)
	}
	payload, maxAttempts, runAfter, err := jobValues(opts)
	if err != nil {
		return false, err
	}
	tag, err := p.Exec(ctx, `
		INSERT INTO jobs (type, target_id, payload, priority, max_attempts, run_after, requested_by)
		SELECT $1, w.id, $3, $4, $5, $6, (SELECT id FROM users WHERE id = $8)
		FROM works w
		WHERE w.id = $2 AND `+needsEnrichment("$7", true)+`
		ON CONFLICT (type, target_id)
			WHERE status IN ('pending', 'running') AND target_id IS NOT NULL
			DO NOTHING`,
		opts.Type, *opts.TargetID, payload, priorityOf(opts), maxAttempts, runAfter, promptID,
		nullableUUID(opts.RequestedBy))
	if err != nil {
		return false, fmt.Errorf("enqueuing %s for %s: %w", opts.Type, *opts.TargetID, mapError(err))
	}
	return tag.RowsAffected() == 1, nil
}

// ResolveSuggestion accepts or rejects one suggestion.
//
// Accepting applies it to the work, which is the only place a model value is
// allowed to overwrite an existing one - and only because a person said so.
func (p *Pool) ResolveSuggestion(ctx context.Context, id uuid.UUID, accept bool) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("resolving suggestion: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var workID uuid.UUID
	var field string
	var value []byte
	if err := tx.QueryRow(ctx, `
		SELECT work_id, field, value FROM enrichment_records
		WHERE id = $1 AND accepted_at IS NULL AND rejected_at IS NULL`, id,
	).Scan(&workID, &field, &value); err != nil {
		return mapError(err)
	}

	column := ""
	switch field {
	case "title":
		column = "title"
	case "subtitle":
		column = "subtitle"
	case "summary":
		column = "summary"
	case "series":
		column = "series"
	}

	if accept && column != "" {
		// value is JSON; a string field arrives quoted.
		// $2::jsonb because the value is stored as JSON: a string field arrives
		// quoted, and #>> '{}' is what unwraps it back to text.
		//
		if column == "title" {
			// sort_title follows the title, and is computed HERE rather than in
			// SQL because the rule for it - drop a leading article - lives in
			// Go and has to give the same answer everywhere.
			//
			// Without it the shelf keeps ordering and paging by the old key:
			// forty books accepted out of "Untitled" display their real names
			// and stay filed under U.
			var title string
			if err := json.Unmarshal(value, &title); err != nil {
				return fmt.Errorf("reading the suggested title: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE works SET title = $2, sort_title = $3, updated_at = now()
				WHERE id = $1`, workID, title, SortTitle(title),
			); err != nil {
				return fmt.Errorf("applying the title: %w", mapError(err))
			}
		} else if _, err := tx.Exec(ctx, fmt.Sprintf(
			`UPDATE works SET %s = $2::jsonb #>> '{}', updated_at = now() WHERE id = $1`, column),
			workID, value,
		); err != nil {
			return fmt.Errorf("applying %q: %w", field, mapError(err))
		}
	}

	// Fields with no column of their own.
	//
	// Accepting one of these used to stamp it accepted and write nothing at
	// all: an administrator clicked accept on an author, got a 2xx, and the
	// book still had none - and the record was now resolved, so it never came
	// back to be decided again. Authors became offerable the moment the model's
	// byline started being kept, which is what made a silent no-op into a
	// silent loss.
	if accept && column == "" {
		switch field {
		case "authors":
			var names []string
			if err := json.Unmarshal(value, &names); err != nil {
				return fmt.Errorf("reading the suggested authors: %w", err)
			}
			contributors := make([]ingest.Contributor, 0, len(names))
			for _, name := range names {
				if strings.TrimSpace(name) == "" {
					continue
				}
				contributors = append(contributors, ingest.Contributor{
					Name: strings.TrimSpace(name),
					Role: domain.RoleAuthor,
				})
			}
			if err := linkAuthors(ctx, tx, workID, contributors); err != nil {
				return err
			}
		case "subjects":
			var subjects []string
			if err := json.Unmarshal(value, &subjects); err != nil {
				return fmt.Errorf("reading the suggested subjects: %w", err)
			}
			if _, err := linkModelTags(ctx, tx, workID, subjects, 0); err != nil {
				return err
			}
		default:
			// audience and edition have nowhere to go yet, and the record IS
			// the value for them. Accepting one is meaningful: it says a person
			// read it and agreed.
		}
	}

	stamp := "accepted_at"
	if !accept {
		stamp = "rejected_at"
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`UPDATE enrichment_records SET %s = now() WHERE id = $1`, stamp), id,
	); err != nil {
		return fmt.Errorf("stamping suggestion: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("resolving suggestion: %w", mapError(err))
	}
	return nil
}
