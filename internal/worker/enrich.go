// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest/extract"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/llm/prompts"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// EnrichStore is the slice of the store the enrichment handler needs.
type EnrichStore interface {
	EnrichmentSubject(ctx context.Context, workID uuid.UUID) (postgres.EnrichmentSubject, error)
	SaveEnrichment(ctx context.Context, in postgres.EnrichmentInput) (postgres.EnrichmentOutcome, error)
}

// EnrichPromptID is the prompt version enrichment asks with. A work counts as
// described once a model has answered this prompt about it, so the sweep and
// enrichment on ingest both ask by it.
func EnrichPromptID() string { return "classify/" + prompts.ClassifyVersion }

// EnrichJob is the job that describes one work, for whoever it is queued for
// (zero for the installation's own sweep). Last in the queue, behind
// everything else (domain.JobType.Priority): a book has to be on the shelf and
// searchable before its description is worth the model's time. Keyed on the
// work, so the queue holds at most one per work however many ask.
func EnrichJob(workID, requestedBy uuid.UUID) postgres.EnqueueOptions {
	return postgres.EnqueueOptions{
		Type:        domain.JobEnrich,
		TargetID:    &workID,
		Payload:     EnrichPayload{WorkID: workID.String(), PromptID: EnrichPromptID()},
		RequestedBy: requestedBy,
	}
}

// EnrichPayload names the work to describe.
type EnrichPayload struct {
	WorkID string `json:"workId"`
	// PromptID is the prompt version the work was queued to be asked about,
	// so a work asked recently under it is not queued again for nothing
	// (needsEnrichment's cooldown).
	PromptID string `json:"promptId,omitempty"`
}

// Enrich describes books using the user's own model.
//
// The whole feature is optional by invariant: with no model configured, or with
// the machine it runs on switched off, the library and reader are unaffected
// and enrichment jobs wait rather than fail.
type Enrich struct {
	Store  EnrichStore
	Client *llm.Client
	Logger *slog.Logger

	// SampleChars bounds how much of a book is sent. Defaults to
	// DefaultSampleChars.
	SampleChars int

	// Deadline bounds one book, end to end. Defaults to DefaultDeadline.
	Deadline time.Duration

	// HeartbeatInterval is how often a job waiting on the model says it is
	// alive. Zero means heartbeatInterval, which is what production uses; a
	// test sets it short enough to watch.
	HeartbeatInterval time.Duration
}

// DefaultDeadline is the longest one book may take.
//
// A ceiling on the whole job, not on one HTTP request, because the two are not
// the same: a request can be retried, and a client timeout is a promise the
// transport makes rather than one the queue can rely on. Observed in practice -
// two enrichment jobs sat inside a single http.Client.Do for fifty-two minutes
// against a client configured with a three-minute timeout, on a model that
// answers the same book in twenty-three seconds. The cause of that stall is not
// yet understood, which is exactly why the job needs a limit of its own.
//
// The heartbeat makes this necessary rather than optional: before it, a stuck
// job went silent and the queue eventually reclaimed it. Now it is kept alive,
// so without a deadline it would be kept alive forever.
const DefaultDeadline = 10 * time.Minute

// DefaultSampleChars is how much book text one enrichment sends.
//
// Roughly 3,000 words: enough for a title page, a contents list, and the start
// of an introduction, which is where a description of a book comes from. Larger
// samples cost context and answer no better, because the middle of chapter nine
// does not say what the book is.
const DefaultSampleChars = 16000

// Handlers returns the job handlers this enricher provides.
func (e *Enrich) Handlers() map[domain.JobType]Handler {
	return map[domain.JobType]Handler{
		domain.JobEnrich: e.Run,
	}
}

// Run enriches one work.
//
// A job somebody may follow - one queued for whoever added the book - says
// what went wrong only in general terms; the detail goes to the server log.
// The file sampled is the work's largest, which need not be one they hold,
// and an error about it can carry its path, what the parser made of it, or a
// piece of the model's reply to its text (llm.ErrBadJSON, llm.HTTPError):
// all of it somebody else's book. The installation's own sweep, which only
// administrators follow, keeps the detail in the job. The error still wraps
// the cause, so a skip is still a skip and a server's Retry-After is still
// honoured.
func (e *Enrich) Run(ctx context.Context, job postgres.Job, report ProgressFunc) error {
	err := e.run(ctx, job, report)
	if err == nil || job.RequestedBy == uuid.Nil {
		return err
	}
	e.logger().Warn("enrich: a followed job did not finish",
		"job", job.ID, "requestedBy", job.RequestedBy, "error", err)
	message := "describing the book failed; the server log has the details"
	if errors.Is(err, ErrSkipped) {
		message = "waiting for the model; the server log has the details"
	}
	return &followedError{message: message, cause: err}
}

// followedError is an error whose text is safe to show whoever follows the
// job, around a cause that may not be.
type followedError struct {
	message string
	cause   error
}

func (e *followedError) Error() string { return e.message }
func (e *followedError) Unwrap() error { return e.cause }

// run enriches one work, with errors in full.
func (e *Enrich) run(ctx context.Context, job postgres.Job, report ProgressFunc) error {
	// The model being switched off is the expected state of a box in someone's
	// house, not a failure. Skipping reschedules without consuming an attempt,
	// so a library does not end up with thousands of permanently failed jobs
	// because the GPU machine was off overnight.
	if e.Client == nil {
		return fmt.Errorf("%w: no model is configured", ErrSkipped)
	}

	var payload EnrichPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("enrich payload: %w", err)
	}
	workID, err := uuid.Parse(payload.WorkID)
	if err != nil {
		return fmt.Errorf("enrich payload: work id is not a UUID: %w", err)
	}

	report(0.1, "reading the book")

	subject, err := e.Store.EnrichmentSubject(ctx, workID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The work was deleted between the job being queued and run. Not a
			// failure: there is simply nothing to describe.
			e.logger().Info("enrich: the work is gone", "workId", workID)
			return nil
		}
		return fmt.Errorf("loading work %s: %w", workID, err)
	}

	sample, err := extract.Sample(ctx, subject.Path, subject.Format, e.sampleChars())
	if err != nil {
		if errors.Is(err, extract.ErrUnsupportedFormat) {
			// A comic has no text to describe, and asking a model about one
			// would produce a confident answer about nothing.
			e.logger().Info("enrich: nothing to read",
				"workId", workID, "format", subject.Format)
			return nil
		}
		return fmt.Errorf("sampling %s: %w", subject.Path, err)
	}
	if sample == "" {
		// A scan with no OCR. Recording nothing is correct; the alternative is
		// asking a model to describe a book from its filename.
		e.logger().Info("enrich: no text could be read",
			"workId", workID, "path", subject.Path)
		return nil
	}

	report(0.4, "asking the model")

	// Bounded, so a job that stops making progress ends instead of running
	// until the process does.
	askCtx, cancelAsk := context.WithTimeout(ctx, e.deadline())
	defer cancelAsk()

	// A 12B model reading several thousand words takes minutes, and a second
	// job may be waiting on the client's concurrency gate behind it. Without a
	// heartbeat the queue sees a job that has not reported for longer than its
	// stale threshold, reclaims it as abandoned, and runs it again - spending
	// the same minutes a second time while the first is still going.
	stop := e.heartbeat(askCtx, report)
	result, err := llm.Classify(askCtx, e.Client, prompts.ClassifyInput{
		Title:    subject.Title,
		Authors:  subject.Authors,
		Filename: subject.Filename,
		Language: subject.Language,
		Sample:   sample,
	})
	stop()
	if err != nil {
		// The job's own deadline, not the caller's shutdown. Worth retrying
		// later - the model may simply have been busy - so it is skipped rather
		// than failed, but it is said plainly so a pattern of them is visible.
		if errors.Is(askCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			e.logger().Warn("enrich: gave up waiting for the model",
				"workId", workID, "after", e.deadline())
			return fmt.Errorf("%w: the model did not answer within %s", ErrSkipped, e.deadline())
		}
		// A machine that is off is a reason to wait. Anything else is a reason
		// to fail this book and move on.
		if transient(err) {
			// %w for both, so the pool can still find a Retry-After inside.
			return fmt.Errorf("%w: %w", ErrSkipped, err)
		}
		return fmt.Errorf("classifying %s: %w", workID, err)
	}

	if result.Empty() {
		// The model read the sample and found nothing it could support. That is
		// a legitimate answer and the prompt asks for it, so it is not retried.
		e.logger().Info("enrich: the model found nothing to say", "workId", workID)
		return nil
	}

	report(0.8, "recording what it found")

	outcome, err := e.Store.SaveEnrichment(ctx, postgres.EnrichmentInput{
		WorkID:   workID,
		Title:    result.Title,
		Subtitle: result.Subtitle,
		// The byline. The prompt has asked for it since v1 and the model has
		// been answering; nothing carried it any further than this call.
		Authors:     result.Authors,
		Edition:     result.Edition,
		Summary:     result.Summary,
		Series:      result.Series,
		SeriesIndex: result.SeriesIndex,
		Subjects:    result.Subjects,
		Audience:    result.Audience,
		Model:       result.Model,
		PromptID:    result.PromptID,
		Confidence:  result.Confidence,
		// What the model could not have read in the book. The one signal here
		// that actually separates one claim from another: its own confidence
		// comes back at 1.00 for almost everything, so the bar it is measured
		// against has never blocked a value.
		Ungrounded: ungroundedFields(sample, result),
	})
	if err != nil {
		return fmt.Errorf("saving enrichment for %s: %w", workID, err)
	}

	e.logger().Info("enriched",
		"workId", workID,
		"applied", outcome.Applied,
		"suggested", outcome.Suggested,
		"refused", outcome.Refused,
		"tags", outcome.TagsAdded,
		"confidence", result.Confidence,
		"prompt", result.PromptID)

	report(1, "done")
	return nil
}

// transient reports whether waiting could plausibly change the outcome.
//
// An ALLOW-LIST, not a block-list, and that distinction is the bug this fixes.
// It was written the other way round - everything was transient unless it was
// one of two known model failures - and `ErrTokenBudget` fell through the gap.
// A skipped job is rescheduled WITHOUT consuming an attempt, by design, so that
// a model server which is off overnight does not fail a whole library. The
// result was a book whose reply is deterministically too long being retried
// forever, silently, a few seconds apart, holding a worker each time.
//
// So: a condition that a different moment could fix is transient. A property of
// this book and this prompt is not, and must fail where someone can see it.
func transient(err error) bool {
	switch {
	case errors.Is(err, llm.ErrNotConfigured):
		// No model at all. It may be configured later.
		return true
	case errors.Is(err, llm.ErrTokenBudget):
		// Deterministic: the same book and prompt will overrun the same budget
		// at the same point, every time.
		return false
	case errors.Is(err, llm.ErrBadJSON), errors.Is(err, llm.ErrEmptyReply):
		// The model's problem, and not one that waiting fixes.
		return false
	case errors.Is(err, llm.ErrUnauthorized), errors.Is(err, llm.ErrInsufficientCredits):
		// A rejected key or an empty balance is not a property of this book: it
		// is the same for every queued job, and the operator fixes it in one
		// place. Failing them would turn one top-up into a library's worth of
		// permanently failed jobs to re-run by hand; skipping them leaves the
		// queue waiting (with backoff, and bounded by maxSkips) for the fix.
		return true
	}

	var httpErr *llm.HTTPError
	if errors.As(err, &httpErr) {
		// 429 and 5xx are a busy or still-loading server. A 400 is a request
		// that will be just as wrong in an hour.
		return httpErr.Retryable()
	}

	// A failure the provider reported inside a 200 (a choice's error, a stream
	// event). It carries the provider's own code in place of a status, so it is
	// classified the same way: a 400-404 is final, not a "transport failure".
	var providerErr *llm.ProviderError
	if errors.As(err, &providerErr) {
		return providerErr.Retryable()
	}

	// What is left is a transport failure: a machine that is off, a network
	// that is down, a request that timed out. All worth another try later.
	return true
}

// heartbeatInterval is how often a job waiting on the model says it is alive.
//
// Comfortably inside the pool's stale threshold, and cheap: one small update to
// a row the worker already holds.
const heartbeatInterval = 30 * time.Second

// heartbeat reports progress until the returned function is called.
//
// The progress figure does not move, deliberately: nothing is known about how
// far through the model is, and inventing a rising number would be a lie told
// to a progress bar. What it reports is that the job is still alive, which is
// the only thing the queue needs to know.
func (e *Enrich) heartbeat(ctx context.Context, report ProgressFunc) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})

	// Overridable so a test can watch it beat. Thirty seconds is right in
	// production and impossible in a test, which is why the test that was
	// supposed to check this never actually did: it waited twenty
	// milliseconds, counted zero beats and asserted nothing.
	interval := e.HeartbeatInterval
	if interval <= 0 {
		interval = heartbeatInterval
	}

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				report(0.4, "waiting for the model")
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

func (e *Enrich) deadline() time.Duration {
	if e.Deadline > 0 {
		return e.Deadline
	}
	return DefaultDeadline
}

func (e *Enrich) sampleChars() int {
	if e.SampleChars > 0 {
		return e.SampleChars
	}
	return DefaultSampleChars
}

func (e *Enrich) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// ungroundedFields names the values that are nowhere in the text the model was
// shown.
//
// Only the fields that are PRINTED in a book. A summary is the model's own
// prose and is supposed to be new; an audience is a judgement, not a quotation.
// Checking those would flag every one of them.
func ungroundedFields(sample string, result llm.Classification) map[string]bool {
	out := map[string]bool{}
	for name, value := range map[string]string{
		"title":    result.Title,
		"subtitle": result.Subtitle,
		// A byline is printed on the title page like everything else here, so
		// the same question applies: is this name anywhere in what the model
		// was shown, or did it come from the filename?
		"authors": strings.Join(result.Authors, " "),
		"edition": result.Edition,
		"series":  result.Series,
	} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if !llm.Grounded(sample, value) {
			out[name] = true
		}
	}
	return out
}
