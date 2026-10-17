// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// Removing a file for everybody and the blocklist are an administrator's,
// like everything else that changes the installation rather than one shelf -
// refused to anybody else before anything is looked up.
func TestReadersCannotRemoveOrUnblock(t *testing.T) {
	rt := &Router{Health: &HealthHandler{}, Moderation: &ModerationHandler{Accounts: aReader}}
	hash := strings.Repeat("ab", 32)
	for _, e := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/admin/books/" + uuid.NewString() + "/remove"},
		{http.MethodGet, "/api/v1/admin/blocked"},
		{http.MethodDelete, "/api/v1/admin/blocked/" + hash},
	} {
		rec := serve(t, rt, e.method, e.path)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not_an_admin") {
			t.Errorf("%s %s as a reader = %d %s, want 403 not_an_admin", e.method, e.path, rec.Code, rec.Body)
		}
	}
}

// Uploading bytes an administrator blocked: one file alone is refused with a
// 422 that says nothing but that; in a batch it is one rejected file among
// the others. Either way the copy just stored is deleted again.
func TestUploadingBlockedBytes(t *testing.T) {
	blockedBody := "bytes nobody here may keep"
	sum := sha256.Sum256([]byte(blockedBody))
	blocked := hex.EncodeToString(sum[:])

	t.Run("alone", func(t *testing.T) {
		s, q := newFakeStorer(), &fakeUploadQueue{blocked: map[string]bool{blocked: true}}
		rec, _ := uploadRaw(t, uploadRouter(s, q), [][2]string{{"a.epub", blockedBody}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body)
		}
		var e dto.Error
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error.Code != "content_blocked" {
			t.Fatalf("body = %s, want the standard error shape with content_blocked", rec.Body)
		}
		if e.Error.Message != blockedMessage {
			t.Errorf("message = %q", e.Error.Message)
		}
		if len(q.discarded) != 1 || len(q.enqueued) != 0 || len(q.holds) != 0 {
			t.Errorf("discarded %v, enqueued %d, held %d; want the copy discarded and nothing else",
				q.discarded, len(q.enqueued), len(q.holds))
		}
		// And the upload's own copy is not put back in place of anything.
		if keep, settled := s.settled[blocked]; !settled || keep {
			t.Errorf("settled = %v (%v), want settled as refused", keep, settled)
		}
	})

	t.Run("in a batch", func(t *testing.T) {
		s, q := newFakeStorer(), &fakeUploadQueue{blocked: map[string]bool{blocked: true}}
		rec, out := upload(t, uploadRouter(s, q), [][2]string{{"a.epub", blockedBody}, {"b.epub", "a book"}})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
		if f := out.Files[0]; f.Status != dto.UploadRejected || f.Code != "content_blocked" || f.Error != blockedMessage {
			t.Errorf("blocked file = %+v", f)
		}
		if f := out.Files[1]; f.Status != dto.UploadStored {
			t.Errorf("the other file = %+v, want stored", f)
		}
		if out.Summary.Rejected != 1 || out.Summary.Stored != 1 {
			t.Errorf("summary = %+v", out.Summary)
		}
	})
}

// uploadRaw is upload without decoding the reply as a batch report.
func uploadRaw(t *testing.T, rt *Router, files [][2]string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	body, contentType := multipartBody(t, files)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, asUserID(req, uploaderID))
	return rec, rec.Body.String()
}

// The whole of it, with a real database and real directories: two people
// hold an upload, it is lent and covered and has an archive copy, and an
// administrator removes it. Every copy Lexarii stored is deleted; a scanned
// file in the library directory is not. Uploading the bytes again is refused
// until the block is lifted.
func TestRemovingAFileForEveryoneOnDisk(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	p := e2ePool(t, url)
	ctx := t.Context()
	uploads, archive, covers, library := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	files := &ingest.UploadStore{Root: uploads}
	ing := &worker.Ingest{Store: p, CoverDir: covers, ArchiveDir: archive, UploadDir: uploads, Logger: quietLogger()}
	admin, alice, bob := e2eUser(t, p, true), e2eUser(t, p, false), e2eUser(t, p, false)
	adminRouter := &Router{Health: &HealthHandler{}, Moderation: &ModerationHandler{
		Store: p, Accounts: p, Logger: quietLogger(),
		Discard: ingest.Discard{Uploads: uploads, Archive: archive, Covers: covers},
	}}
	uploadRouter := &Router{Health: &HealthHandler{}, Uploads: &UploadHandler{
		Store: files, Queue: p, Accounts: p, Log: quietLogger(), AnyoneMayUpload: true,
	}}
	book := minimalEPUB(t, "Not To Be Kept")
	sum := sha256.Sum256([]byte(book))
	hash := hex.EncodeToString(sum[:])

	for _, who := range []uuid.UUID{alice, bob} {
		if _, out := uploadAs(t, uploadRouter, who, [][2]string{{"book.epub", book}}); out.Files[0].Status != dto.UploadStored {
			t.Fatalf("upload = %+v", out.Files[0])
		}
	}
	drainQueue(t, p, ing)
	var id uuid.UUID
	var stored string
	if err := p.QueryRow(ctx, `SELECT id, path FROM book_files`).Scan(&id, &stored); err != nil {
		t.Fatalf("reading: %v", err)
	}
	// A cover, and an archive copy left from an earlier departure.
	archived := filepath.Join(archive, "old.epub")
	for _, f := range []string{archived, filepath.Join(covers, "c.jpg")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing: %v", err)
		}
	}
	if _, err := p.Exec(ctx, `UPDATE book_files SET cover_path = 'c.jpg', archived_path = $2 WHERE id = $1`,
		id, archived); err != nil {
		t.Fatalf("setting: %v", err)
	}
	// And a scanned file of other bytes, in the library directory.
	scannedPath := filepath.Join(library, "scanned.epub")
	scannedBook := minimalEPUB(t, "Somebody's Own File")
	if err := os.WriteFile(scannedPath, []byte(scannedBook), 0o444); err != nil {
		t.Fatalf("writing: %v", err)
	}
	scanSum := sha256.Sum256([]byte(scannedBook))
	scanned, err := p.UpsertBookFile(ctx, postgres.IngestInput{
		ContentHash: hex.EncodeToString(scanSum[:]), Path: scannedPath, Format: "epub",
		SizeBytes: int64(len(scannedBook)), Status: "ready", OwnerID: admin,
	})
	if err != nil {
		t.Fatalf("scanning: %v", err)
	}

	remove := func(id uuid.UUID, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/books/"+id.String()+"/remove",
			strings.NewReader(body))
		rec := httptest.NewRecorder()
		adminRouter.Handler().ServeHTTP(rec, asUserID(req, admin))
		return rec
	}
	if rec := remove(id, `{"reason":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no reason: %d, want 400", rec.Code)
	}
	if rec := remove(uuid.New(), `{"reason":"r"}`); rec.Code != http.StatusNotFound {
		t.Errorf("no such file: %d, want 404", rec.Code)
	}
	rec := remove(id, `{"reason":"not ours to keep"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body)
	}
	var removed dto.RemovedForEveryone
	if err := json.Unmarshal(rec.Body.Bytes(), &removed); err != nil || removed.RemovedFor != 2 {
		t.Errorf("reply = %s, want removedFor 2", rec.Body)
	}
	for _, f := range []string{stored, archived, filepath.Join(covers, "c.jpg")} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s survived the removal: %v", f, err)
		}
	}

	// A copy left behind - an archive copy under a collision name, here -
	// is finished off even when whoever asked has gone: the clean-up is
	// detached from the request.
	leftover := filepath.Join(archive, hash+"-2.epub")
	if err := os.WriteFile(leftover, []byte(book), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE blocked_content SET copies_discarded_at = NULL`); err != nil {
		t.Fatalf("marking: %v", err)
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := adminRouter.Moderation.discard(gone, hash); err != nil {
		t.Fatalf("discard after the request went away: %v", err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("the leftover copy survived: %v", err)
	}
	finish := func(hash string) (int, dto.DiscardedCopies) {
		rec := httptest.NewRecorder()
		adminRouter.Handler().ServeHTTP(rec, asUserID(
			httptest.NewRequest(http.MethodPost, "/api/v1/admin/blocked/"+hash+"/discard", nil), admin))
		var out dto.DiscardedCopies
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, out := finish(hash); code != http.StatusOK || out.CopiesRemain {
		t.Errorf("finishing = %d %+v, want 200 and nothing remaining", code, out)
	}
	if code, _ := finish(strings.Repeat("0d", 32)); code != http.StatusNotFound {
		t.Errorf("finishing bytes that are not blocked = %d, want 404", code)
	}

	// The library directory is never touched, removal or not.
	if rec := remove(scanned.BookFileID, `{"reason":"also not to be kept"}`); rec.Code != http.StatusOK {
		t.Fatalf("remove scanned = %d %s", rec.Code, rec.Body)
	}
	if data, err := os.ReadFile(scannedPath); err != nil || string(data) != scannedBook {
		t.Errorf("the library file was touched: %v", err)
	}

	// Alice uploads the same bytes again: refused, neutrally, and the copy
	// that upload stored is gone again.
	rec, body := uploadRaw(t, &Router{Health: &HealthHandler{}, Uploads: &UploadHandler{
		Store: files, Queue: p, Accounts: p, Log: quietLogger(), AnyoneMayUpload: true,
	}}, [][2]string{{"again.epub", book}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "content_blocked") {
		t.Fatalf("re-upload = %d %s, want 422 content_blocked", rec.Code, body)
	}
	if strings.Contains(body, "not ours") || strings.Contains(body, "removed") {
		t.Errorf("the refusal says more than that the file is refused: %s", body)
	}
	if _, err := os.Stat(files.PathFor(hash, "epub")); !os.IsNotExist(err) {
		t.Errorf("the refused upload's copy is still stored: %v", err)
	}

	// The blocklist, and lifting it.
	list := httptest.NewRecorder()
	adminRouter.Handler().ServeHTTP(list, asUserID(httptest.NewRequest(http.MethodGet, "/api/v1/admin/blocked", nil), admin))
	var blocked dto.BlockedFiles
	if err := json.Unmarshal(list.Body.Bytes(), &blocked); err != nil || len(blocked.Items) != 2 {
		t.Fatalf("blocklist = %s", list.Body)
	}
	for _, item := range blocked.Items {
		if item.CopiesRemain {
			t.Errorf("%s: copies reported as remaining after they were deleted", item.ContentHash)
		}
	}
	unblock := func(hash string) int {
		rec := httptest.NewRecorder()
		adminRouter.Handler().ServeHTTP(rec, asUserID(
			httptest.NewRequest(http.MethodDelete, "/api/v1/admin/blocked/"+hash, nil), admin))
		return rec.Code
	}
	if code := unblock("not-a-hash"); code != http.StatusBadRequest {
		t.Errorf("unblock of a malformed hash = %d, want 400", code)
	}
	if code := unblock(hash); code != http.StatusNoContent {
		t.Fatalf("unblock = %d", code)
	}
	if code := unblock(hash); code != http.StatusNotFound {
		t.Errorf("unblock again = %d, want 404", code)
	}
	_, out := uploadAs(t, uploadRouter, alice, [][2]string{{"again.epub", book}})
	if out.Files[0].Status != dto.UploadStored {
		t.Fatalf("upload after unblocking = %+v", out.Files[0])
	}
	drainQueue(t, p, ing)
	var n int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM readable_book_files WHERE book_file_id = $1 AND user_id = $2`,
		id, alice).Scan(&n); err != nil || n != 1 {
		t.Errorf("after unblocking and uploading again, Alice can read it: %d, %v", n, err)
	}
}

// A removal that kept meeting concurrent changes and stopped is a 503 the
// client can act on, not a 500.
func TestABusyRemovalSaysTryAgain(t *testing.T) {
	rec := httptest.NewRecorder()
	respondModerationError(rec, fmt.Errorf("removing: %w", store.ErrBusy))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"try_again"`) {
		t.Errorf("= %d %s, want 503 try_again", rec.Code, rec.Body)
	}
}
