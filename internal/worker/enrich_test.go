// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// fakeEnrichStore records what the handler asked for and what it saved.
type fakeEnrichStore struct {
	subject postgres.EnrichmentSubject
	err     error
	saved   []postgres.EnrichmentInput
}

func (f *fakeEnrichStore) EnrichmentSubject(_ context.Context, workID uuid.UUID) (postgres.EnrichmentSubject, error) {
	if f.err != nil {
		return postgres.EnrichmentSubject{}, f.err
	}
	s := f.subject
	s.WorkID = workID
	return s, nil
}

func (f *fakeEnrichStore) SaveEnrichment(_ context.Context, in postgres.EnrichmentInput) (postgres.EnrichmentOutcome, error) {
	f.saved = append(f.saved, in)
	return postgres.EnrichmentOutcome{Applied: []string{"title"}}, nil
}

// modelSaying is a model server that answers with one fixed reply.
// recordedPrompts collects what was sent to the model.
//
// Guarded, because the handler runs on the server's goroutine and the test
// reads it from its own. It was a bare slice appended to from one and ranged
// over from the other - a data race that -race had simply not caught yet. The
// chat tests next door already do this with a mutex; this file did not.
type recordedPrompts struct {
	mu   sync.Mutex
	sent []string
}

func (r *recordedPrompts) add(content string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, content)
}

// joined is everything sent to the model, for a test that wants to assert a
// phrase reached it.
func (r *recordedPrompts) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.sent, "\n")
}

func modelSaying(t *testing.T, content string) (*llm.Client, *atomic.Int64, *recordedPrompts) {
	t.Helper()

	var calls atomic.Int64
	prompts := &recordedPrompts{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		body, _ := io.ReadAll(r.Body)
		var decoded struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &decoded)
		for _, m := range decoded.Messages {
			prompts.add(m.Content)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
		})
	}))
	t.Cleanup(server.Close)

	client := llm.New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "test-model",
		MaxConcurrency: 1, Timeout: 5 * time.Second,
	})
	return client, &calls, prompts
}

// writeEPUB builds a small but genuinely valid EPUB on disk.
func writeEPUB(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.epub")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	add := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip %s: %v", name, err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	add("mimetype", "application/epub+zip")
	add("META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)
	add("OEBPS/content.opf", `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="id">urn:uuid:test</dc:identifier>
    <dc:title></dc:title>
    <dc:language>en</dc:language>
  </metadata>
  <manifest><item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/></manifest>
  <spine><itemref idref="c1"/></spine>
</package>`)
	add("OEBPS/ch1.xhtml", `<?xml version="1.0"?>
<html xmlns="http://www.w3.org/1999/xhtml"><body>`+body+`</body></html>`)

	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing epub: %v", err)
	}
	return path
}

func enrichJob(t *testing.T, workID uuid.UUID) postgres.Job {
	t.Helper()
	payload, err := json.Marshal(EnrichPayload{WorkID: workID.String()})
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	return postgres.Job{ID: uuid.New(), Type: domain.JobEnrich, Payload: payload}
}

func noProgress(float32, string) {}

func TestEnrichDescribesABook(t *testing.T) {
	path := writeEPUB(t, `<h1>An Introduction to Logic</h1><p>This is a first course in formal logic.</p>`)
	client, calls, prompts := modelSaying(t, `{
		"subjects": ["logic", "philosophy"],
		"audience": "undergraduate",
		"summary": "A first course in formal logic.",
		"title": "An Introduction to Logic",
		"confidence": 0.9
	}`)

	store := &fakeEnrichStore{subject: postgres.EnrichmentSubject{
		Path: path, Format: domain.FormatEPUB, Filename: "book.epub", Language: "en",
	}}
	enricher := &Enrich{Store: store, Client: client}

	workID := uuid.New()
	if err := enricher.Run(context.Background(), enrichJob(t, workID), noProgress); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if calls.Load() != 1 {
		t.Errorf("made %d model calls, want 1", calls.Load())
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved %d enrichments, want 1", len(store.saved))
	}

	saved := store.saved[0]
	if saved.WorkID != workID {
		t.Errorf("saved against %s, want %s", saved.WorkID, workID)
	}
	if saved.Title != "An Introduction to Logic" {
		t.Errorf("title = %q", saved.Title)
	}
	if len(saved.Subjects) != 2 {
		t.Errorf("subjects = %v", saved.Subjects)
	}
	// Provenance travels with the claim.
	if saved.Model != "test-model" || saved.PromptID == "" {
		t.Errorf("provenance lost: model=%q prompt=%q", saved.Model, saved.PromptID)
	}

	// The book's actual text reached the model, not just its filename.
	joined := prompts.joined()
	if !strings.Contains(joined, "first course in formal logic") {
		t.Error("the book text did not reach the model")
	}
}

// The invariant: with no model configured, enrichment waits rather than fails.
// A library of 5,000 books must not become 5,000 permanently failed jobs
// because the GPU machine is off.
func TestEnrichSkipsWhenNoModelIsConfigured(t *testing.T) {
	enricher := &Enrich{Store: &fakeEnrichStore{}, Client: nil}

	err := enricher.Run(context.Background(), enrichJob(t, uuid.New()), noProgress)
	if !errors.Is(err, ErrSkipped) {
		t.Fatalf("error = %v, want ErrSkipped", err)
	}
}

// A model server that is switched off is a machine to wait for, not a book to
// give up on.
func TestEnrichSkipsWhenTheModelIsUnreachable(t *testing.T) {
	path := writeEPUB(t, `<p>Some readable text about logic and reasoning.</p>`)

	client := llm.New(config.ModelConfig{
		// Reserved for documentation: nothing can be listening.
		BaseURL: "http://192.0.2.1:9/v1", Model: "m",
		MaxConcurrency: 1, Timeout: 200 * time.Millisecond,
	})
	enricher := &Enrich{
		Store:  &fakeEnrichStore{subject: postgres.EnrichmentSubject{Path: path, Format: domain.FormatEPUB}},
		Client: client,
	}

	err := enricher.Run(context.Background(), enrichJob(t, uuid.New()), noProgress)
	if !errors.Is(err, ErrSkipped) {
		t.Fatalf("error = %v, want ErrSkipped so the job waits", err)
	}
}

// A model that answers badly is not a reason to retry forever: the answer will
// be just as bad next time, and the job should fail visibly.
func TestEnrichFailsOnAnUnusableReply(t *testing.T) {
	path := writeEPUB(t, `<p>Some readable text about logic.</p>`)
	client, _, _ := modelSaying(t, "I would rather not answer that.")

	enricher := &Enrich{
		Store:  &fakeEnrichStore{subject: postgres.EnrichmentSubject{Path: path, Format: domain.FormatEPUB}},
		Client: client,
	}

	err := enricher.Run(context.Background(), enrichJob(t, uuid.New()), noProgress)
	if err == nil {
		t.Fatal("an unusable reply was reported as success")
	}
	if errors.Is(err, ErrSkipped) {
		t.Error("a bad reply was treated as a machine being off; it will be just as bad next time")
	}
}

// A scan with no OCR has nothing to read. Asking a model anyway invites it to
// describe a book from its filename.
func TestEnrichRecordsNothingForABookWithNoText(t *testing.T) {
	path := writeEPUB(t, `<p>   </p>`)
	client, calls, _ := modelSaying(t, `{"subjects":["invented"],"summary":"x","confidence":0.9}`)

	store := &fakeEnrichStore{subject: postgres.EnrichmentSubject{Path: path, Format: domain.FormatEPUB}}
	enricher := &Enrich{Store: store, Client: client}

	if err := enricher.Run(context.Background(), enrichJob(t, uuid.New()), noProgress); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("made %d model calls for a book with no text", calls.Load())
	}
	if len(store.saved) != 0 {
		t.Errorf("saved %d enrichments for a book with no text", len(store.saved))
	}
}

// "I could not tell" is a legitimate answer the prompt asks for, and storing
// nothing is the honest outcome.
func TestEnrichStoresNothingWhenTheModelFoundNothing(t *testing.T) {
	path := writeEPUB(t, `<p>Some readable text.</p>`)
	client, _, _ := modelSaying(t, `{"subjects":[],"summary":"","confidence":0.1}`)

	store := &fakeEnrichStore{subject: postgres.EnrichmentSubject{Path: path, Format: domain.FormatEPUB}}
	enricher := &Enrich{Store: store, Client: client}

	if err := enricher.Run(context.Background(), enrichJob(t, uuid.New()), noProgress); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.saved) != 0 {
		t.Errorf("saved %d enrichments for an empty answer", len(store.saved))
	}
}

// A work deleted between queueing and running is not a failure.
func TestEnrichIgnoresAWorkThatIsGone(t *testing.T) {
	client, calls, _ := modelSaying(t, `{"subjects":["x"],"summary":"y","confidence":0.5}`)
	enricher := &Enrich{
		Store:  &fakeEnrichStore{err: store.ErrNotFound},
		Client: client,
	}

	if err := enricher.Run(context.Background(), enrichJob(t, uuid.New()), noProgress); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("asked the model about a work that no longer exists")
	}
}

func TestEnrichRejectsABadPayload(t *testing.T) {
	enricher := &Enrich{Store: &fakeEnrichStore{}, Client: mustClient(t)}

	job := postgres.Job{ID: uuid.New(), Type: domain.JobEnrich, Payload: []byte(`{"workId":"not-a-uuid"}`)}
	if err := enricher.Run(context.Background(), job, noProgress); err == nil {
		t.Error("a malformed work id was accepted")
	}
	if errors.Is(enricher.Run(context.Background(), job, noProgress), ErrSkipped) {
		t.Error("a malformed payload was treated as a temporary problem; it will never improve")
	}
}

// The sample is bounded: a model must never be sent a whole book.
func TestEnrichBoundsTheSample(t *testing.T) {
	long := strings.Repeat("<p>This sentence is repeated many times over. </p>", 5000)
	path := writeEPUB(t, long)

	client, _, prompts := modelSaying(t, `{"subjects":["x"],"summary":"y","confidence":0.5}`)
	enricher := &Enrich{
		Store:       &fakeEnrichStore{subject: postgres.EnrichmentSubject{Path: path, Format: domain.FormatEPUB}},
		Client:      client,
		SampleChars: 2000,
	}

	if err := enricher.Run(context.Background(), enrichJob(t, uuid.New()), noProgress); err != nil {
		t.Fatalf("Run: %v", err)
	}

	joined := prompts.joined()
	// The prompt is the sample plus the instructions around it, so the bound is
	// on the sample rather than the whole message.
	if len(joined) > 2000+4000 {
		t.Errorf("the prompt was %d bytes with a 2,000 character sample limit", len(joined))
	}
}

func mustClient(t *testing.T) *llm.Client {
	t.Helper()
	client, _, _ := modelSaying(t, `{"subjects":["x"],"summary":"y","confidence":0.5}`)
	return client
}

// A 12B model reading several thousand words takes minutes. Without a
// heartbeat the queue decides the worker has died, reclaims the job, and runs
// it again - spending the same minutes twice while the first is still going.
func TestEnrichReportsProgressWhileWaitingOnTheModel(t *testing.T) {
	path := writeEPUB(t, `<p>Some readable text about logic and reasoning.</p>`)

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": `{"subjects":["logic"],"summary":"x","confidence":0.9}`},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	client := llm.New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "slow", MaxConcurrency: 1, Timeout: 30 * time.Second,
	})

	enricher := &Enrich{
		Store:             &fakeEnrichStore{subject: postgres.EnrichmentSubject{Path: path, Format: domain.FormatEPUB}},
		Client:            client,
		HeartbeatInterval: time.Millisecond,
	}

	var beats atomic.Int64
	report := func(_ float32, note string) {
		if note == "waiting for the model" {
			beats.Add(1)
		}
	}

	// The heartbeat interval is a constant, so rather than waiting 30 seconds
	// this drives the same mechanism directly and checks it ticks and stops.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := enricher.heartbeat(ctx, report)
	time.Sleep(20 * time.Millisecond)
	stop()

	// Read, because the whole point is that it ticks. This test declared
	// `beats`, incremented it in the callback, and then never looked at it -
	// so the heartbeat could have emitted nothing at all and the test still
	// passed, which is worse than not having it: the queue reclaims a job that
	// stops reporting and runs it a second time.
	if beats.Load() == 0 {
		t.Error("the heartbeat never reported; a long job would be reclaimed and run twice")
	}

	// And it stops when told, or a finished job keeps touching the row.
	settled := beats.Load()
	time.Sleep(20 * time.Millisecond)
	if beats.Load() != settled {
		t.Errorf("the heartbeat kept going after stop(): %d then %d", settled, beats.Load())
	}

	// And the real path completes with the heartbeat wrapped around it.
	go func() { close(release) }()
	if err := enricher.Run(context.Background(), enrichJob(t, uuid.New()), func(float32, string) {}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// Stopping the heartbeat must actually stop it: a goroutine left ticking would
// report progress for a job that has already finished.
func TestHeartbeatStops(t *testing.T) {
	enricher := &Enrich{}

	var beats atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := enricher.heartbeat(ctx, func(float32, string) { beats.Add(1) })
	stop()

	settled := beats.Load()
	time.Sleep(50 * time.Millisecond)
	if beats.Load() != settled {
		t.Error("the heartbeat kept reporting after it was stopped")
	}
}

// Shutdown must not be held up by a heartbeat.
func TestHeartbeatStopsWithContext(t *testing.T) {
	enricher := &Enrich{}
	ctx, cancel := context.WithCancel(context.Background())

	stop := enricher.heartbeat(ctx, func(float32, string) {})
	cancel()

	done := make(chan struct{})
	go func() { stop(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stopping the heartbeat hung after its context was cancelled")
	}
}

// A job that stops making progress must end, not run until the process does.
//
// The heartbeat is what makes this necessary: it keeps a working job alive, so
// without a ceiling it would keep a stuck one alive too. Seen in practice - two
// jobs sat inside one HTTP request for fifty-two minutes against a client
// configured with a three-minute timeout.
func TestEnrichGivesUpOnAStuckModel(t *testing.T) {
	path := writeEPUB(t, `<p>Some readable text about logic and reasoning.</p>`)

	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never answers, and never closes: exactly the observed failure.
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(block)

	client := llm.New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "stuck",
		MaxConcurrency: 1,
		// Deliberately longer than the job deadline, so the job's own limit is
		// what ends this rather than the transport's.
		Timeout: time.Hour,
	})

	enricher := &Enrich{
		Store:    &fakeEnrichStore{subject: postgres.EnrichmentSubject{Path: path, Format: domain.FormatEPUB}},
		Client:   client,
		Deadline: 300 * time.Millisecond,
	}

	start := time.Now()
	err := enricher.Run(context.Background(), enrichJob(t, uuid.New()), noProgress)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a stuck model was reported as success")
	}
	// Skipped, not failed: the model may simply have been busy, and the book
	// deserves another try later.
	if !errors.Is(err, ErrSkipped) {
		t.Errorf("error = %v, want ErrSkipped", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %v to give up on a 300ms deadline", elapsed)
	}
}

// Shutdown is not the job's deadline, and must not be reported as one.
func TestEnrichHonoursCallerCancellation(t *testing.T) {
	path := writeEPUB(t, `<p>Some readable text.</p>`)

	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(block)

	client := llm.New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "stuck", MaxConcurrency: 1, Timeout: time.Hour,
	})
	enricher := &Enrich{
		Store:    &fakeEnrichStore{subject: postgres.EnrichmentSubject{Path: path, Format: domain.FormatEPUB}},
		Client:   client,
		Deadline: time.Hour,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if err := enricher.Run(ctx, enrichJob(t, uuid.New()), noProgress); err == nil {
		t.Fatal("a cancelled run reported success")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v to honour cancellation", elapsed)
	}
}

// The classification that decides whether a book waits or fails.
//
// A skipped job is rescheduled without consuming an attempt, so anything
// misclassified as transient retries forever. That happened: a reply that was
// deterministically too long was treated as a transport problem and retried
// silently every few seconds, holding a worker each time.
func TestTransientOnlyCoversWhatWaitingCouldFix(t *testing.T) {
	waits := map[string]error{
		"no model configured": llm.ErrNotConfigured,
		"server busy":         &llm.HTTPError{Status: http.StatusTooManyRequests},
		"server starting up":  &llm.HTTPError{Status: http.StatusServiceUnavailable},
		"bad gateway":         &llm.HTTPError{Status: http.StatusBadGateway},
		"connection refused":  errors.New("model unreachable at http://x: connect: connection refused"),
		// Operator-fixable and identical for every queued job: wait, do not fail.
		"bad api key":                &llm.HTTPError{Status: http.StatusUnauthorized},
		"out of credit":              &llm.HTTPError{Status: http.StatusPaymentRequired},
		"wrapped out of credit":      fmt.Errorf("classify: %w", &llm.HTTPError{Status: http.StatusPaymentRequired}),
		"out of credit in a 200":     &llm.ProviderError{Code: "402", Message: "no credit"},
		"provider 502 in a 200":      &llm.ProviderError{Code: "502", Message: "upstream"},
		"stream server_error string": &llm.ProviderError{Code: "server_error", Message: "disconnected"},
	}
	for name, err := range waits {
		if !transient(err) {
			t.Errorf("%s: transient = false, want true", name)
		}
	}

	fails := map[string]error{
		// The one that caused the loop.
		"reply too long":   llm.ErrTokenBudget,
		"wrapped too long": fmt.Errorf("classify: %w", llm.ErrTokenBudget),
		"unusable reply":   llm.ErrBadJSON,
		"empty reply":      llm.ErrEmptyReply,
		"request rejected": &llm.HTTPError{Status: http.StatusBadRequest},
		"model not found":  &llm.HTTPError{Status: http.StatusNotFound},
		// Hosted providers report failures inside a 200 with their own code. A
		// client-error code is as final as the status it stands in for, and
		// used to be misread as a "transport failure" and skipped for ~9h.
		"provider 400 in a 200": &llm.ProviderError{Code: "400", Message: "bad"},
		"provider 404 in a 200": &llm.ProviderError{Code: "404", Message: "no endpoints"},
		"403 moderation":        &llm.HTTPError{Status: http.StatusForbidden},
	}
	for name, err := range fails {
		if transient(err) {
			t.Errorf("%s: transient = true, want false - this is what retries forever", name)
		}
	}
}

// End to end: a book the model cannot answer within its budget fails once,
// visibly, rather than being rescheduled for ever.
func TestEnrichFailsABookThatOverrunsTheBudget(t *testing.T) {
	path := writeEPUB(t, `<p>Some readable text about logic.</p>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": `{"subjects":["lo`},
				"finish_reason": "length",
			}},
		})
	}))
	defer server.Close()

	client := llm.New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "verbose", MaxConcurrency: 1, Timeout: 5 * time.Second,
	})
	enricher := &Enrich{
		Store:  &fakeEnrichStore{subject: postgres.EnrichmentSubject{Path: path, Format: domain.FormatEPUB}},
		Client: client,
	}

	err := enricher.Run(context.Background(), enrichJob(t, uuid.New()), noProgress)
	if err == nil {
		t.Fatal("a book the model could not answer was reported as success")
	}
	if errors.Is(err, ErrSkipped) {
		t.Fatal("the book was rescheduled without consuming an attempt; this retries forever")
	}
}

// A job somebody may follow never carries the path of the file sampled - the
// work's largest, which need not be one they hold - in its error, nor
// anything else about it. The installation's own sweep still does, for the
// administrator reading it.
func TestASampledPathStaysOutOfAFollowedJob(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "somebody-elses-file.epub")
	store := &fakeEnrichStore{subject: postgres.EnrichmentSubject{Path: missing, Format: domain.FormatEPUB}}
	enricher := &Enrich{Store: store, Client: mustClient(t)}

	job := enrichJob(t, uuid.New())
	job.RequestedBy = uuid.New()
	err := enricher.Run(context.Background(), job, noProgress)
	if err == nil || strings.Contains(err.Error(), missing) || strings.Contains(err.Error(), "somebody-elses") {
		t.Errorf("followed job's error = %v; want an error without the path", err)
	}

	job.RequestedBy = uuid.Nil
	if err := enricher.Run(context.Background(), job, noProgress); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("the sweep's error = %v; want the path, for the administrator", err)
	}
}

// Nor what the model said about somebody else's book, nor what its server
// answered: a reply that is not the JSON asked for is quoted in the error,
// and a followed job's error leaves it out. A skip stays a skip.
func TestAModelReplyStaysOutOfAFollowedJob(t *testing.T) {
	path := writeEPUB(t, "<p>Somebody else's sentence about lighthouses.</p>")
	client, _, _ := modelSaying(t, `not json at all, but a SECRET-REPLY about lighthouses`)
	enricher := &Enrich{Store: &fakeEnrichStore{subject: postgres.EnrichmentSubject{Path: path, Format: domain.FormatEPUB}},
		Client: client}

	job := enrichJob(t, uuid.New())
	job.RequestedBy = uuid.New()
	err := enricher.Run(context.Background(), job, noProgress)
	if err == nil || strings.Contains(err.Error(), "SECRET-REPLY") || strings.Contains(err.Error(), "lighthouses") {
		t.Errorf("followed job's error = %v; want nothing of the reply", err)
	}

	job.RequestedBy = uuid.Nil
	if err := enricher.Run(context.Background(), job, noProgress); err == nil || !strings.Contains(err.Error(), "SECRET-REPLY") {
		t.Errorf("the sweep's error = %v; want the reply, for the administrator", err)
	}
}

// A followed job the model cannot answer yet is still a skip, so it waits
// rather than failing.
func TestAFollowedSkipIsStillASkip(t *testing.T) {
	enricher := &Enrich{Store: &fakeEnrichStore{}, Client: nil}
	job := enrichJob(t, uuid.New())
	job.RequestedBy = uuid.New()
	err := enricher.Run(context.Background(), job, noProgress)
	if !errors.Is(err, ErrSkipped) || strings.Contains(err.Error(), "configured") {
		t.Errorf("err = %v; want a skip, in general terms", err)
	}
}
