// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// fakeStorer records what it was asked to store and deduplicates by content,
// mirroring the real store's contract.
type fakeStorer struct {
	mu    sync.Mutex
	seen  map[string]bool
	names []string
	err   error
	// settled is, per stored file, whether it was settled as kept.
	settled map[string]bool
}

func newFakeStorer() *fakeStorer {
	return &fakeStorer{seen: map[string]bool{}, settled: map[string]bool{}}
}

func (f *fakeStorer) Settle(s ingest.StoredFile, keep bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settled[s.ContentHash] = keep
	return nil
}

func (f *fakeStorer) Store(_ context.Context, filename string, body io.Reader) (ingest.StoredFile, error) {
	if f.err != nil {
		return ingest.StoredFile{}, f.err
	}

	name, err := ingest.SafeFilename(filename)
	if err != nil {
		return ingest.StoredFile{}, err
	}
	format, ok := ingest.FormatForPath(name)
	if !ok {
		return ingest.StoredFile{}, ingest.ErrUnsupportedUpload
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return ingest.StoredFile{}, err
	}
	if len(data) == 0 {
		return ingest.StoredFile{}, ingest.ErrEmptyUpload
	}

	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, name)
	dup := f.seen[hash]
	f.seen[hash] = true

	return ingest.StoredFile{
		Path:             "/data/uploads/books/" + hash[:2] + "/" + hash + "." + string(format),
		OriginalFilename: name,
		ContentHash:      hash,
		Format:           format,
		SizeBytes:        int64(len(data)),
		Duplicate:        dup,
	}, nil
}

type fakeUploadQueue struct {
	mu       sync.Mutex
	enqueued []postgres.EnqueueOptions
	statuses map[string]domain.IngestStatus
	// held is who holds, or has claimed, each content hash - the store's
	// book_file_holders and holding_claims, without a database.
	held    map[string]map[uuid.UUID]bool
	holds   []postgres.HoldInput
	missing map[string]bool
	err     error
	holdErr error
	// blocked is the blocklist; discarded is every hash whose stored copy
	// the handler asked to have deleted.
	blocked   map[string]bool
	discarded []string
}

func (q *fakeUploadQueue) DiscardBlockedUpload(_ context.Context, hash string, remove func() error) error {
	q.mu.Lock()
	blocked := q.blocked[hash]
	if blocked {
		q.discarded = append(q.discarded, hash)
	}
	q.mu.Unlock()
	if !blocked {
		return nil
	}
	return remove()
}

func (q *fakeUploadQueue) Enqueue(_ context.Context, opts postgres.EnqueueOptions) (uuid.UUID, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return uuid.Nil, false, q.err
	}
	q.enqueued = append(q.enqueued, opts)
	return uuid.New(), true, nil
}

func (q *fakeUploadQueue) HoldUpload(_ context.Context, in postgres.HoldInput) (postgres.HoldOutcome, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.holdErr != nil {
		return postgres.HoldOutcome{}, q.holdErr
	}
	if q.blocked[in.ContentHash] {
		return postgres.HoldOutcome{}, fmt.Errorf("holding: %w", store.ErrBlocked)
	}
	q.holds = append(q.holds, in)
	if q.held == nil {
		q.held = map[string]map[uuid.UUID]bool{}
	}
	if q.held[in.ContentHash] == nil {
		q.held[in.ContentHash] = map[uuid.UUID]bool{}
	}
	out := postgres.HoldOutcome{
		AlreadyHeld: q.held[in.ContentHash][in.UserID],
		Status:      q.statuses[in.ContentHash],
		Missing:     q.missing[in.ContentHash],
	}
	q.held[in.ContentHash][in.UserID] = true
	// As the real one does: settled under the lock, once recorded.
	if in.Settle != nil {
		if err := in.Settle(); err != nil {
			return postgres.HoldOutcome{}, err
		}
	}
	return out, nil
}

// multipartBody builds a multipart request body from filename/content pairs.
func multipartBody(t *testing.T, files [][2]string) (io.Reader, string) {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range files {
		part, err := w.CreateFormFile("files", f[0])
		if err != nil {
			t.Fatalf("creating part %q: %v", f[0], err)
		}
		if _, err := part.Write([]byte(f[1])); err != nil {
			t.Fatalf("writing part %q: %v", f[0], err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing writer: %v", err)
	}
	return &buf, w.FormDataContentType()
}

func upload(t *testing.T, rt *Router, files [][2]string) (*httptest.ResponseRecorder, dto.UploadResult) {
	t.Helper()
	return uploadAs(t, rt, uploaderID, files)
}

// uploaderID is who upload() signs in as. One fixed person, so a second
// upload in the same test is the same person coming back.
var uploaderID = uuid.MustParse("5a1b2c3d-0000-4000-8000-000000000001")

func uploadAs(t *testing.T, rt *Router, user uuid.UUID, files [][2]string) (*httptest.ResponseRecorder, dto.UploadResult) {
	t.Helper()

	body, contentType := multipartBody(t, files)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", body)
	req.Header.Set("Content-Type", contentType)

	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, asUserID(req, user))

	var out dto.UploadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v; body = %s", err, rec.Body)
	}
	return rec, out
}

func uploadRouter(s *fakeStorer, q *fakeUploadQueue) *Router {
	return &Router{
		Health:  &HealthHandler{},
		Uploads: &UploadHandler{Store: s, Queue: q, Log: quietLogger(), Accounts: anAdmin},
	}
}

func TestBulkUploadStoresEveryFile(t *testing.T) {
	s, q := newFakeStorer(), &fakeUploadQueue{}
	rt := uploadRouter(s, q)

	rec, out := upload(t, rt, [][2]string{
		{"algebraessentials.epub", "epub bytes"},
		{"97thingseveryjavaprogrammershouldknow.pdf", "pdf bytes"},
		{"somebook.mobi", "mobi bytes"},
	})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body)
	}
	if len(out.Files) != 3 {
		t.Fatalf("got %d file results, want 3: %+v", len(out.Files), out.Files)
	}
	if out.Summary.Stored != 3 {
		t.Errorf("summary = %+v, want 3 stored", out.Summary)
	}

	for _, f := range out.Files {
		// Every stored file is settled once it is recorded, so a duplicate's
		// own copy can stand in for one erased meanwhile.
		if keep, settled := s.settled[f.ContentHash]; !settled || !keep {
			t.Errorf("%s: settled = %v (%v), want settled as kept", f.Filename, keep, settled)
		}
		if f.Status != dto.UploadStored {
			t.Errorf("%s: status = %q, want stored (%s)", f.Filename, f.Status, f.Error)
		}
		if f.ContentHash == "" {
			t.Errorf("%s: no content hash", f.Filename)
		}
		if f.JobID == "" {
			t.Errorf("%s: no extraction job was queued", f.Filename)
		}
	}

	// Every file must be queued for extraction on the server - that is the whole
	// point of uploading rather than copying files in by hand.
	if len(q.enqueued) != 3 {
		t.Fatalf("queued %d extraction jobs, want 3", len(q.enqueued))
	}
	for _, opts := range q.enqueued {
		if opts.Type != domain.JobExtract {
			t.Errorf("queued a %q job, want extract", opts.Type)
		}
		if opts.TargetID == nil {
			t.Error("queued without a target id, so duplicates cannot be deduplicated")
		}
	}
}

// Re-uploading a batch after a failure is the recovery path, so it has to be
// cheap and must not duplicate anything.
func TestReuploadingIsDeduplicated(t *testing.T) {
	s, q := newFakeStorer(), &fakeUploadQueue{statuses: map[string]domain.IngestStatus{}}
	rt := uploadRouter(s, q)

	files := [][2]string{
		{"one.epub", "first book"},
		{"two.pdf", "second book"},
	}

	_, first := upload(t, rt, files)
	if first.Summary.Stored != 2 {
		t.Fatalf("first upload = %+v, want 2 stored", first.Summary)
	}

	// Mark them ingested, as the worker would have done.
	for _, f := range first.Files {
		q.statuses[f.ContentHash] = domain.IngestReady
	}

	_, second := upload(t, rt, files)
	if second.Summary.Duplicate != 2 {
		t.Fatalf("re-upload = %+v, want 2 duplicates", second.Summary)
	}
	if second.Summary.Stored != 0 {
		t.Errorf("re-upload stored %d files, want 0", second.Summary.Stored)
	}

	// And no extra extraction work was queued.
	if len(q.enqueued) != 2 {
		t.Errorf("queued %d jobs across both uploads, want 2", len(q.enqueued))
	}
}

// A file that was stored but never ingested - an upload that landed just before
// a crash - must be re-queued rather than dismissed as a duplicate.
func TestStoredButUningestedFileIsRequeued(t *testing.T) {
	s, q := newFakeStorer(), &fakeUploadQueue{statuses: map[string]domain.IngestStatus{}}
	rt := uploadRouter(s, q)

	files := [][2]string{{"book.epub", "content"}}
	_, first := upload(t, rt, files)

	// The bytes are on disk but nothing ever extracted them.
	q.statuses[first.Files[0].ContentHash] = domain.IngestPending

	_, second := upload(t, rt, files)
	if second.Files[0].Status != dto.UploadStored {
		t.Fatalf("status = %q, want stored: an un-ingested file must be re-queued",
			second.Files[0].Status)
	}
	if len(q.enqueued) != 2 {
		t.Errorf("queued %d jobs, want 2 (the second re-queues)", len(q.enqueued))
	}
}

// One bad file in a batch of two hundred must cost that one file.
func TestBadFilesDoNotAbandonTheBatch(t *testing.T) {
	s, q := newFakeStorer(), &fakeUploadQueue{}
	rt := uploadRouter(s, q)

	rec, out := upload(t, rt, [][2]string{
		{"good.epub", "real content"},
		{"notes.txt", "not a book"},
		{"empty.pdf", ""},
		{"alsogood.pdf", "more content"},
	})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if len(out.Files) != 4 {
		t.Fatalf("got %d results, want 4 - every file must be reported", len(out.Files))
	}
	if out.Summary.Stored != 2 || out.Summary.Rejected != 2 {
		t.Errorf("summary = %+v, want 2 stored and 2 rejected", out.Summary)
	}

	byName := map[string]dto.UploadedFile{}
	for _, f := range out.Files {
		byName[f.Filename] = f
	}
	if byName["notes.txt"].Status != dto.UploadRejected {
		t.Error("a .txt was accepted as a book")
	}
	if byName["notes.txt"].Error == "" {
		t.Error("a rejected file carries no reason")
	}
	if byName["good.epub"].Status != dto.UploadStored {
		t.Error("a good file was lost because a bad one was in the same batch")
	}
}

// A bulk upload over a flaky connection will lose files partway through. The
// response has to say which ones landed, because the recovery is to re-upload.
func TestInterruptedUploadReportsWhatLanded(t *testing.T) {
	s, q := newFakeStorer(), &fakeUploadQueue{}
	rt := uploadRouter(s, q)

	// Build a valid body, then truncate it mid-stream.
	body, contentType := multipartBody(t, [][2]string{
		{"one.epub", strings.Repeat("a", 500)},
		{"two.epub", strings.Repeat("b", 500)},
		{"three.epub", strings.Repeat("c", 500)},
	})
	full, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", bytes.NewReader(full[:len(full)*2/3]))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, asUser(req))

	var out dto.UploadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v; body = %s", err, rec.Body)
	}

	if !out.Interrupted {
		t.Error("a truncated upload was not reported as interrupted")
	}
	if out.Message == "" {
		t.Error("an interrupted upload carries no explanation for the user")
	}
	// Whatever arrived intact before the break must still be reported, so the
	// user knows it does not need re-sending.
	if len(out.Files) == 0 {
		t.Error("no files were reported from a partially-received batch")
	}
}

func TestUploadRejectsNonMultipart(t *testing.T) {
	rt := uploadRouter(newFakeStorer(), &fakeUploadQueue{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", strings.NewReader(`{"not":"multipart"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, asUser(req))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestEmptyUploadIsRejected(t *testing.T) {
	rt := uploadRouter(newFakeStorer(), &fakeUploadQueue{})
	rec, out := upload(t, rt, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if out.Message == "" {
		t.Error("an empty request carries no explanation")
	}
}

// A file whose bytes are safe but whose job could not be queued is NOT lost.
// Saying so precisely matters: a rescan will pick it up.
func TestQueueFailureStillReportsTheFileAsStored(t *testing.T) {
	s := newFakeStorer()
	q := &fakeUploadQueue{err: errors.New("database unavailable")}
	rt := uploadRouter(s, q)

	_, out := upload(t, rt, [][2]string{{"book.epub", "content"}})

	if len(out.Files) != 1 {
		t.Fatalf("got %d results, want 1", len(out.Files))
	}
	f := out.Files[0]
	if f.Status != dto.UploadStored {
		t.Errorf("status = %q, want stored: the bytes are safe", f.Status)
	}
	if f.Error == "" {
		t.Error("the queueing failure was not reported")
	}
	if f.JobID != "" {
		t.Error("a job id was reported for a job that was never queued")
	}
}

// Storage is content-addressed, so a hostile filename never becomes a path -
// but the handler must still not pass one through untouched.
func TestHostileFilenamesAreSanitisedOrRejected(t *testing.T) {
	s, q := newFakeStorer(), &fakeUploadQueue{}
	rt := uploadRouter(s, q)

	_, out := upload(t, rt, [][2]string{
		{"../../../etc/passwd.epub", "content"},
		{`..\..\windows\evil.pdf`, "content"},
		{"/absolute/path/book.epub", "content"},
	})

	for _, f := range out.Files {
		if f.Status == dto.UploadRejected {
			continue // rejection is a safe outcome
		}
		if strings.ContainsAny(f.Filename, `/\`) {
			t.Errorf("stored filename %q still contains a path separator", f.Filename)
		}
		if strings.Contains(f.Filename, "..") {
			t.Errorf("stored filename %q still contains a traversal", f.Filename)
		}
	}
}

// Who may upload is a setting. "admins" is what every installation did before
// it existed, so it must still refuse a reader; "users" lets everybody add to
// their own shelf. The per-file cap is the storer's and applies either way.
func TestUploadPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		anyone   bool
		accounts AdminStore
		want     int
	}{
		{"admins only, an administrator", false, anAdmin, http.StatusAccepted},
		{"admins only, a reader", false, aReader, http.StatusForbidden},
		{"everybody, an administrator", true, anAdmin, http.StatusAccepted},
		{"everybody, a reader", true, aReader, http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, q := newFakeStorer(), &fakeUploadQueue{}
			rt := &Router{
				Health: &HealthHandler{},
				Uploads: &UploadHandler{
					Store: s, Queue: q, Log: quietLogger(),
					Accounts: tc.accounts, AnyoneMayUpload: tc.anyone,
				},
			}

			body, contentType := multipartBody(t, [][2]string{{"book.epub", "content"}})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", body)
			req.Header.Set("Content-Type", contentType)
			rec := httptest.NewRecorder()
			rt.Handler().ServeHTTP(rec, asUserID(req, uploaderID))

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.want, rec.Body)
			}
			if tc.want == http.StatusForbidden {
				if len(s.names) != 0 || len(q.enqueued) != 0 {
					t.Error("a refused upload still stored or queued something")
				}
				return
			}
			// The book is the uploader's: held by them, queued for them.
			if len(q.holds) != 1 || q.holds[0].UserID != uploaderID {
				t.Errorf("holdings = %+v, want one for the uploader", q.holds)
			}
			if len(q.enqueued) != 1 || q.enqueued[0].RequestedBy != uploaderID {
				t.Errorf("queued = %+v, want one job for the uploader", q.enqueued)
			}
		})
	}
}

// The per-file cap applies to everybody once anybody may upload.
func TestTheSizeCapAppliesToEveryUploader(t *testing.T) {
	s, q := newFakeStorer(), &fakeUploadQueue{}
	s.err = ingest.ErrUploadTooLarge
	rt := &Router{
		Health: &HealthHandler{},
		Uploads: &UploadHandler{
			Store: s, Queue: q, Log: quietLogger(),
			Accounts: aReader, AnyoneMayUpload: true,
		},
	}
	_, out := upload(t, rt, [][2]string{{"huge.pdf", "pretend this is large"}})
	if len(out.Files) != 1 || out.Files[0].Status != dto.UploadRejected {
		t.Fatalf("an oversized file was not refused: %+v", out.Files)
	}
	if len(q.enqueued) != 0 || len(q.holds) != 0 {
		t.Error("an oversized file was still held or queued")
	}
}

// "duplicate" is only ever about the person asking.
//
// A second person adding bytes somebody else already added - finished or
// still being extracted - gets exactly the reply a new file gets: stored, a job
// of their own, no note. Anything else tells them what another account here
// owns. The same person adding a file again is told it is a duplicate, which
// tells them only about their own shelf.
func TestDuplicateMeansOnlyYourOwn(t *testing.T) {
	alice := uuid.MustParse("a11ce000-0000-4000-8000-000000000001")
	bob := uuid.MustParse("b0b00000-0000-4000-8000-000000000002")
	book := [][2]string{{"shared-book.epub", "the same bytes"}}

	for _, tc := range []struct {
		name string
		// settled is whether the first upload has been extracted before the
		// second arrives.
		settled bool
		second  uuid.UUID
		want    dto.UploadStatus
	}{
		{"same person, already extracted", true, alice, dto.UploadDuplicate},
		{"same person, still extracting", false, alice, dto.UploadStored},
		{"another person, already extracted", true, bob, dto.UploadStored},
		{"another person, still extracting", false, bob, dto.UploadStored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeStorer()
			q := &fakeUploadQueue{statuses: map[string]domain.IngestStatus{}}
			rt := &Router{
				Health: &HealthHandler{},
				Uploads: &UploadHandler{
					Store: s, Queue: q, Log: quietLogger(),
					Accounts: aReader, AnyoneMayUpload: true,
				},
			}

			_, first := uploadAs(t, rt, alice, book)
			if tc.settled {
				q.statuses[first.Files[0].ContentHash] = domain.IngestReady
			}
			_, second := uploadAs(t, rt, tc.second, book)

			got := second.Files[0]
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q (%+v)", got.Status, tc.want, got)
			}
			if tc.second == alice {
				return
			}

			// Indistinguishable from a first upload, field by field, apart
			// from the job id - which is a different job, and Bob's own.
			want := first.Files[0]
			if got.JobID == "" || got.JobID == want.JobID {
				t.Errorf("job id = %q, want a job of Bob's own (Alice's is %q)", got.JobID, want.JobID)
			}
			got.JobID, want.JobID = "", ""
			if got != want {
				t.Errorf("Bob's reply differs from a fresh upload:\n got  %+v\n want %+v", got, want)
			}
			if second.Summary != first.Summary || second.Message != first.Message {
				t.Errorf("summary %+v / %q, want %+v / %q",
					second.Summary, second.Message, first.Summary, first.Message)
			}

			// Bob holds it, and his job is his: a different target, so the
			// queue cannot fold it into Alice's and hand him her job.
			if len(q.holds) != 2 || q.holds[1].UserID != bob {
				t.Errorf("holdings = %+v, want Bob's recorded", q.holds)
			}
			if len(q.enqueued) != 2 {
				t.Fatalf("queued %d jobs, want 2", len(q.enqueued))
			}
			a, b := q.enqueued[0], q.enqueued[1]
			if *a.TargetID == *b.TargetID {
				t.Error("Bob's job shares Alice's target, so it would be deduplicated into hers")
			}
			if b.RequestedBy != bob {
				t.Errorf("Bob's job was queued for %s", b.RequestedBy)
			}
		})
	}
}

// When the file cannot be put on the uploader's shelf, the request fails with
// the standard error shape, and nothing is queued. Answering "stored" would be
// untrue: the extraction respects a release the person made earlier, and only
// the upload itself forgets one, so nothing else would ever put it there.
func TestAHoldingFailureFailsTheUpload(t *testing.T) {
	s := newFakeStorer()
	q := &fakeUploadQueue{holdErr: errors.New("database hiccup")}
	rt := uploadRouter(s, q)

	body, contentType := multipartBody(t, [][2]string{{"book.epub", "content"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, asUserID(req, uploaderID))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body)
	}
	var got dto.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Error.Code != string(CodeInternal) {
		t.Errorf("body = %s, want the standard error shape", rec.Body)
	}
	if len(q.enqueued) != 0 {
		t.Errorf("queued %d jobs for a file that is on nobody's shelf", len(q.enqueued))
	}
}

// A book whose recorded copy has gone missing - an administrator tidied the
// library directory - comes back when somebody who holds it uploads it again.
// Answering "duplicate" would leave it gone for good: nothing else would ever
// point the file at the copy that exists.
func TestReuploadingAMissingBookRequeuesIt(t *testing.T) {
	s := newFakeStorer()
	q := &fakeUploadQueue{statuses: map[string]domain.IngestStatus{}, missing: map[string]bool{}}
	rt := uploadRouter(s, q)
	files := [][2]string{{"book.epub", "content"}}

	_, first := upload(t, rt, files)
	q.statuses[first.Files[0].ContentHash] = domain.IngestReady
	q.missing[first.Files[0].ContentHash] = true

	_, second := upload(t, rt, files)
	if second.Files[0].Status != dto.UploadStored || second.Files[0].JobID == "" {
		t.Fatalf("re-upload of a missing book = %+v, want stored and queued", second.Files[0])
	}
}
