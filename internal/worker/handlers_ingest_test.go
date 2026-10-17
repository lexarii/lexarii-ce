// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// fakeLibraryStore is an in-memory LibraryStore whose every call can be made
// to fail, for the ingest paths a real database will not readily take.
type fakeLibraryStore struct {
	mu sync.Mutex

	blocked    map[string]bool
	blockedErr error

	statuses  map[string]domain.IngestStatus
	statusErr error

	upserts   []postgres.IngestInput
	upsertOut postgres.IngestOutcome
	upsertErr error

	touched  []string
	touchErr error

	enqueued   []postgres.EnqueueOptions
	enqueueErr error

	markMissingErr error

	paths map[string]string // content hash -> current path

	removeIfUnreferencedErr error
	removeIfUnreferenced    []string
	discardErr              error
	discarded               []string

	chunkQueueErr  error
	chunkQueued    []uuid.UUID
	enrichQueueErr error
	enrichQueued   []postgres.EnqueueOptions
}

func newFakeLibraryStore() *fakeLibraryStore {
	return &fakeLibraryStore{
		blocked:  map[string]bool{},
		statuses: map[string]domain.IngestStatus{},
		paths:    map[string]string{},
	}
}

func (s *fakeLibraryStore) UpsertBookFile(_ context.Context, in postgres.IngestInput) (postgres.IngestOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts = append(s.upserts, in)
	return s.upsertOut, s.upsertErr
}

func (s *fakeLibraryStore) Enqueue(_ context.Context, opts postgres.EnqueueOptions) (uuid.UUID, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enqueueErr != nil {
		return uuid.Nil, false, s.enqueueErr
	}
	s.enqueued = append(s.enqueued, opts)
	return uuid.New(), true, nil
}

func (s *fakeLibraryStore) MarkMissing(context.Context, time.Time) (int64, error) {
	return 0, s.markMissingErr
}

func (s *fakeLibraryStore) TouchSeen(_ context.Context, contentHash, _ string, _ uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched = append(s.touched, contentHash)
	return s.touchErr
}

func (s *fakeLibraryStore) BookFileStatus(_ context.Context, contentHash string) (domain.IngestStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusErr != nil {
		return "", s.statusErr
	}
	st, ok := s.statuses[contentHash]
	if !ok {
		return "", store.ErrNotFound
	}
	return st, nil
}

func (s *fakeLibraryStore) BookFilePath(_ context.Context, contentHash string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.paths[contentHash]
	if !ok {
		return "", store.ErrNotFound
	}
	return p, nil
}

func (s *fakeLibraryStore) RemoveIfUnreferenced(_ context.Context, _, path string, _ func(string) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeIfUnreferenced = append(s.removeIfUnreferenced, path)
	return s.removeIfUnreferencedErr
}

func (s *fakeLibraryStore) IsBlocked(_ context.Context, contentHash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked[contentHash], s.blockedErr
}

func (s *fakeLibraryStore) DiscardBlockedUpload(_ context.Context, contentHash string, _ func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discarded = append(s.discarded, contentHash)
	return s.discardErr
}

func (s *fakeLibraryStore) EnqueueIfNeedsChunks(_ context.Context, opts postgres.EnqueueOptions, _ int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chunkQueueErr != nil {
		return false, s.chunkQueueErr
	}
	s.chunkQueued = append(s.chunkQueued, *opts.TargetID)
	return true, nil
}

func (s *fakeLibraryStore) EnqueueIfNeedsEnrichment(_ context.Context, opts postgres.EnqueueOptions, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enrichQueueErr != nil {
		return false, s.enrichQueueErr
	}
	s.enrichQueued = append(s.enrichQueued, opts)
	return true, nil
}

// copyEPUBInto writes n copies of a small EPUB into dir, returning their paths.
func copyEPUBInto(t *testing.T, dir string, n int) []string {
	t.Helper()
	src, err := os.ReadFile(writeEPUB(t, "<p>Some readable text.</p>"))
	if err != nil {
		t.Fatalf("reading epub: %v", err)
	}
	paths := make([]string, n)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("copy-%02d.epub", i))
		if err := os.WriteFile(paths[i], src, 0o644); err != nil {
			t.Fatalf("writing copy: %v", err)
		}
	}
	return paths
}

// writeEPUBWithCover builds an EPUB 3 whose manifest declares a PNG cover.
func writeEPUBWithCover(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{
		{"mimetype", "application/epub+zip"},
		{"META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`},
		{"OEBPS/content.opf", `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="id">urn:uuid:cover-test</dc:identifier>
    <dc:title>With a Cover</dc:title>
    <dc:language>en</dc:language>
  </metadata>
  <manifest>
    <item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="img" href="cover.png" media-type="image/png" properties="cover-image"/>
  </manifest>
  <spine><itemref idref="c1"/></spine>
</package>`},
		{"OEBPS/ch1.xhtml", `<?xml version="1.0"?>
<html xmlns="http://www.w3.org/1999/xhtml"><body><p>Text beside a cover.</p></body></html>`},
		{"OEBPS/cover.png", "\x89PNG\r\n\x1a\npretend png data"},
	} {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatalf("zip %s: %v", f.name, err)
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			t.Fatalf("writing %s: %v", f.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	path := filepath.Join(t.TempDir(), "covered.epub")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing epub: %v", err)
	}
	return path
}

func scanJob(t *testing.T, payload ScanPayload) postgres.Job {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	return postgres.Job{ID: uuid.New(), Type: domain.JobScan, Payload: raw}
}

func extractJobFor(t *testing.T, payload ExtractPayload) postgres.Job {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	return postgres.Job{ID: uuid.New(), Type: domain.JobExtract, Payload: raw}
}

func TestScanRootComesFromThePayloadOrTheConfiguration(t *testing.T) {
	t.Run("no directory at all is an error", func(t *testing.T) {
		ing := &Ingest{Store: newFakeLibraryStore(), Logger: quietLogger()}
		if err := ing.Scan(t.Context(), postgres.Job{ID: uuid.New()}, noProgress); err == nil {
			t.Fatal("a scan with no library directory succeeded")
		}
	})

	t.Run("the payload's root and owner are used", func(t *testing.T) {
		dir := t.TempDir()
		copyEPUBInto(t, dir, 1)
		owner := uuid.New()
		s := newFakeLibraryStore()
		ing := &Ingest{Store: s, Logger: quietLogger()}

		if err := ing.Scan(t.Context(), scanJob(t, ScanPayload{Root: dir, OwnerID: owner}), noProgress); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(s.enqueued) != 1 {
			t.Fatalf("queued %d extractions, want 1", len(s.enqueued))
		}
		if got := s.enqueued[0]; got.Type != domain.JobExtract || got.RequestedBy != owner {
			t.Errorf("queued %+v, want an extraction for the scan's owner", got)
		}
		if p, ok := s.enqueued[0].Payload.(ExtractPayload); !ok || p.OwnerID != owner || !strings.HasPrefix(p.Path, dir) {
			t.Errorf("payload = %+v, want the owner and a path under the scanned root", s.enqueued[0].Payload)
		}
	})
}

// The total is not known until the walk ends, so progress is a running count.
func TestScanReportsProgressAsItGoes(t *testing.T) {
	dir := t.TempDir()
	copyEPUBInto(t, dir, 25)
	s := newFakeLibraryStore()
	ing := &Ingest{Store: s, LibraryDir: dir, Logger: quietLogger()}

	var notes []string
	report := func(_ float32, note string) { notes = append(notes, note) }
	if err := ing.Scan(t.Context(), postgres.Job{ID: uuid.New()}, report); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(notes) < 2 || notes[0] != "scanned 25 files" {
		t.Errorf("progress notes = %q, want a running count then a summary", notes)
	}
}

func TestScanFailures(t *testing.T) {
	storeDown := errors.New("database unavailable")
	tests := []struct {
		name   string
		break_ func(s *fakeLibraryStore)
	}{
		{name: "checking for a block", break_: func(s *fakeLibraryStore) { s.blockedErr = storeDown }},
		{name: "checking the file's status", break_: func(s *fakeLibraryStore) { s.statusErr = storeDown }},
		{name: "queueing the extraction", break_: func(s *fakeLibraryStore) { s.enqueueErr = storeDown }},
		{name: "marking missing files", break_: func(s *fakeLibraryStore) { s.markMissingErr = storeDown }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			copyEPUBInto(t, dir, 1)
			s := newFakeLibraryStore()
			tc.break_(s)
			ing := &Ingest{Store: s, LibraryDir: dir, Logger: quietLogger()}

			if err := ing.Scan(t.Context(), postgres.Job{ID: uuid.New()}, noProgress); !errors.Is(err, storeDown) {
				t.Fatalf("Scan = %v, want the store's error", err)
			}
		})
	}

	t.Run("the library directory does not exist", func(t *testing.T) {
		ing := &Ingest{Store: newFakeLibraryStore(), LibraryDir: filepath.Join(t.TempDir(), "absent"), Logger: quietLogger()}
		if err := ing.Scan(t.Context(), postgres.Job{ID: uuid.New()}, noProgress); err == nil {
			t.Fatal("scanning a directory that does not exist succeeded")
		}
	})
}

// Two kinds of trouble a scan logs and moves past: an unreadable file it
// could not record, and an unchanged file it could not mark as seen. Neither
// stops the scan of everything else.
func TestScanCarriesOnPastFilesItCannotRecord(t *testing.T) {
	t.Run("an unreadable file is recorded as failed", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "empty.epub"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		copyEPUBInto(t, dir, 1)
		s := newFakeLibraryStore()
		s.upsertErr = errors.New("database unavailable")
		ing := &Ingest{Store: s, LibraryDir: dir, Logger: quietLogger()}

		if err := ing.Scan(t.Context(), postgres.Job{ID: uuid.New()}, noProgress); err != nil {
			t.Fatalf("Scan = %v, want it to carry on", err)
		}
		if len(s.upserts) != 1 || s.upserts[0].Status != domain.IngestFailed || s.upserts[0].IngestError == "" {
			t.Errorf("upserts = %+v, want the empty file recorded as failed with its reason", s.upserts)
		}
		if len(s.enqueued) != 1 {
			t.Errorf("queued %d extractions, want the readable file still queued", len(s.enqueued))
		}
	})

	t.Run("an unchanged file is touched, not re-extracted", func(t *testing.T) {
		dir := t.TempDir()
		paths := copyEPUBInto(t, dir, 1)
		s := newFakeLibraryStore()
		s.touchErr = errors.New("database unavailable")
		ing := &Ingest{Store: s, LibraryDir: dir, Logger: quietLogger()}
		// Every hash is settled: the first scan's status for this file.
		hash := hashOfFile(t, paths[0])
		s.statuses[hash] = domain.IngestReady

		if err := ing.Scan(t.Context(), postgres.Job{ID: uuid.New()}, noProgress); err != nil {
			t.Fatalf("Scan = %v, want a failed touch to be survived", err)
		}
		if len(s.touched) != 1 || s.touched[0] != hash {
			t.Errorf("touched %v, want the unchanged file", s.touched)
		}
		if len(s.enqueued) != 0 {
			t.Errorf("queued %d extractions for an unchanged file", len(s.enqueued))
		}
	})
}

func hashOfFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256HexForTest(raw)
}

func sha256HexForTest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestExtractRejectsAnUnusablePayload(t *testing.T) {
	for name, payload := range map[string][]byte{
		"not JSON":      []byte("{"),
		"no path":       []byte(`{"contentHash":"abc"}`),
		"no hash":       []byte(`{"path":"/x.epub"}`),
		"entirely bare": []byte(`{}`),
	} {
		t.Run(name, func(t *testing.T) {
			s := newFakeLibraryStore()
			ing := &Ingest{Store: s, Logger: quietLogger()}
			job := postgres.Job{ID: uuid.New(), Type: domain.JobExtract, Payload: payload}
			if err := ing.Extract(t.Context(), job, noProgress); err == nil {
				t.Fatal("an unusable payload was accepted")
			}
			if len(s.upserts) != 0 {
				t.Error("something was stored from an unusable payload")
			}
		})
	}
}

// The bytes moved after the job was queued - an archive took them - and the
// row knows where they went: the extraction follows them there.
func TestExtractFollowsAFileThatMoved(t *testing.T) {
	current := writeEPUB(t, "<p>The file in its new home.</p>")
	s := newFakeLibraryStore()
	s.paths["hash-1"] = current
	ing := &Ingest{Store: s, Logger: quietLogger()}

	gone := filepath.Join(t.TempDir(), "where-it-was.epub")
	job := extractJobFor(t, ExtractPayload{Path: gone, ContentHash: "hash-1", Format: domain.FormatEPUB})
	if err := ing.Extract(t.Context(), job, noProgress); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(s.upserts) != 1 || s.upserts[0].Path != current || s.upserts[0].Status != domain.IngestReady {
		t.Errorf("upserts = %+v, want the file read from where it is now", s.upserts)
	}
}

func TestExtractStopsOnShutdown(t *testing.T) {
	s := newFakeLibraryStore()
	ing := &Ingest{Store: s, Logger: quietLogger()}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	job := extractJobFor(t, ExtractPayload{Path: writeEPUB(t, "<p>x</p>"), ContentHash: "h", Format: domain.FormatEPUB})
	if err := ing.Extract(ctx, job, noProgress); !errors.Is(err, context.Canceled) {
		t.Fatalf("Extract = %v, want context.Canceled", err)
	}
	if len(s.upserts) != 0 {
		t.Error("an interrupted extraction stored something")
	}
}

func TestExtractStoresTheCoverUnderTheDataDirectory(t *testing.T) {
	coverDir := filepath.Join(t.TempDir(), "covers")
	s := newFakeLibraryStore()
	ing := &Ingest{Store: s, CoverDir: coverDir, Logger: quietLogger()}

	job := extractJobFor(t, ExtractPayload{Path: writeEPUBWithCover(t), ContentHash: "h", Format: domain.FormatEPUB})
	if err := ing.Extract(t.Context(), job, noProgress); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(s.upserts) != 1 || s.upserts[0].CoverPath == "" {
		t.Fatalf("upserts = %+v, want a cover recorded", s.upserts)
	}
	if _, err := os.Stat(filepath.Join(coverDir, s.upserts[0].CoverPath)); err != nil {
		t.Errorf("the recorded cover is not on disk: %v", err)
	}
}

// A cover that cannot be written costs the cover, never the book.
func TestExtractKeepsTheFileWhenTheCoverCannotBeWritten(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newFakeLibraryStore()
	ing := &Ingest{Store: s, CoverDir: filepath.Join(notADir, "covers"), Logger: quietLogger()}

	job := extractJobFor(t, ExtractPayload{Path: writeEPUBWithCover(t), ContentHash: "h", Format: domain.FormatEPUB})
	if err := ing.Extract(t.Context(), job, noProgress); err != nil {
		t.Fatalf("Extract = %v, want the file stored without its cover", err)
	}
	if len(s.upserts) != 1 || s.upserts[0].CoverPath != "" || s.upserts[0].Status != domain.IngestReady {
		t.Errorf("upserts = %+v, want a ready file with no cover", s.upserts)
	}
}

func TestExtractFailsWhenNothingCanBeStored(t *testing.T) {
	storeDown := errors.New("database unavailable")
	s := newFakeLibraryStore()
	s.upsertErr = storeDown
	ing := &Ingest{Store: s, Logger: quietLogger()}

	job := extractJobFor(t, ExtractPayload{Path: writeEPUB(t, "<p>x</p>"), ContentHash: "h", Format: domain.FormatEPUB})
	if err := ing.Extract(t.Context(), job, noProgress); !errors.Is(err, storeDown) {
		t.Fatalf("Extract = %v, want the store's error", err)
	}
}

// The cover a re-extract displaced is deleted - but only a bare name inside
// the cover directory, never a path that could point anywhere else.
func TestExtractRemovesOnlyTheCoverItReplaced(t *testing.T) {
	coverDir := t.TempDir()
	old := filepath.Join(coverDir, "old.png")
	outside := filepath.Join(t.TempDir(), "outside.png")
	for _, p := range []string{old, outside} {
		if err := os.WriteFile(p, []byte("png"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		replaced string
		gone     string
		kept     string
	}{
		{replaced: "old.png", gone: old},
		{replaced: outside, kept: outside},
		{replaced: "../" + filepath.Base(outside), kept: outside},
	} {
		s := newFakeLibraryStore()
		s.upsertOut = postgres.IngestOutcome{BookFileID: uuid.New(), ReplacedCover: tc.replaced}
		ing := &Ingest{Store: s, CoverDir: coverDir, Logger: quietLogger()}
		job := extractJobFor(t, ExtractPayload{Path: writeEPUB(t, "<p>x</p>"), ContentHash: "h", Format: domain.FormatEPUB})
		if err := ing.Extract(t.Context(), job, noProgress); err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if tc.gone != "" {
			if _, err := os.Stat(tc.gone); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("replaced cover %q was left behind", tc.replaced)
			}
		}
		if tc.kept != "" {
			if _, err := os.Stat(tc.kept); err != nil {
				t.Errorf("replaced cover %q removed a file outside the cover directory", tc.replaced)
			}
		}
	}
}

// After a stored extraction, the follow-on work it asks for - indexing,
// enrichment, removing an archive copy an upload replaced - is never a reason
// to fail it.
func TestExtractFollowOnFailuresDoNotFailTheFile(t *testing.T) {
	archiveDir := t.TempDir()
	previous := filepath.Join(archiveDir, "user", "old.epub")
	queueDown := errors.New("queue unavailable")

	s := newFakeLibraryStore()
	s.upsertOut = postgres.IngestOutcome{
		BookFileID: uuid.New(), WorkID: uuid.New(), Moved: true, PreviousPath: previous,
	}
	s.removeIfUnreferencedErr = queueDown
	s.chunkQueueErr = queueDown
	s.enrichQueueErr = queueDown
	ing := &Ingest{Store: s, ArchiveDir: archiveDir, Indexing: s, Enriching: s, Logger: quietLogger()}

	job := extractJobFor(t, ExtractPayload{
		Path: writeEPUB(t, "<p>x</p>"), ContentHash: "h", Format: domain.FormatEPUB, Uploaded: true,
	})
	if err := ing.Extract(t.Context(), job, noProgress); err != nil {
		t.Fatalf("Extract = %v, want success", err)
	}
	if len(s.removeIfUnreferenced) != 1 || s.removeIfUnreferenced[0] != previous {
		t.Errorf("asked to remove %v, want the archive copy the upload replaced", s.removeIfUnreferenced)
	}
}

// Blocked bytes leave nothing behind: the cover just written is removed, and
// so is the upload's own copy - though failing to remove it is not an error.
func TestExtractOfBlockedBytesCleansUp(t *testing.T) {
	uploadDir := t.TempDir()
	upload := filepath.Join(uploadDir, "ab", "upload.epub")
	if err := os.MkdirAll(filepath.Dir(upload), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(writeEPUBWithCover(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(upload, src, 0o644); err != nil {
		t.Fatal(err)
	}
	coverDir := t.TempDir()

	s := newFakeLibraryStore()
	s.upsertErr = fmt.Errorf("upserting: %w", store.ErrBlocked)
	s.discardErr = errors.New("database unavailable")
	ing := &Ingest{Store: s, CoverDir: coverDir, UploadDir: uploadDir, Logger: quietLogger()}

	job := extractJobFor(t, ExtractPayload{Path: upload, ContentHash: "blocked", Format: domain.FormatEPUB, Uploaded: true})
	if err := ing.Extract(t.Context(), job, noProgress); err != nil {
		t.Fatalf("Extract = %v, want the job done", err)
	}
	entries, err := os.ReadDir(coverDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("cover directory holds %d files, want the blocked file's cover removed", len(entries))
	}
	if len(s.discarded) != 1 || s.discarded[0] != "blocked" {
		t.Errorf("discarded %v, want the upload's copy asked for", s.discarded)
	}
}

func TestRemoveFileTreatsGoneAsRemoved(t *testing.T) {
	dir := t.TempDir()
	if err := removeFile(filepath.Join(dir, "never-there")); err != nil {
		t.Errorf("removing a missing file = %v, want nil", err)
	}
	// A directory with something in it cannot be removed, and says so.
	full := filepath.Join(dir, "full")
	if err := os.MkdirAll(filepath.Join(full, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeFile(full); err == nil {
		t.Error("removing a non-empty directory reported success")
	}
}
