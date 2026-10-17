// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// uploadPost sends a prepared multipart body to the upload handler as an
// administrator.
func uploadPost(t *testing.T, h *UploadHandler, ctx context.Context, body io.Reader, contentType string) dto.UploadResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", body).WithContext(ctx)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.Create(rec, asUser(req))
	var out dto.UploadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v; status %d body %s", err, rec.Code, rec.Body)
	}
	return out
}

// A form field that is not a file is skipped, not reported as a bad book.
func TestAnUploadSkipsFieldsThatAreNotFiles(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("note", "these are from the attic"); err != nil {
		t.Fatal(err)
	}
	part, err := w.CreateFormFile("files", "a.epub")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("a book"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	s := newFakeStorer()
	out := uploadPost(t, &UploadHandler{Store: s, Queue: &fakeUploadQueue{}, Accounts: anAdmin, Log: quietLogger()},
		t.Context(), &buf, w.FormDataContentType())
	if len(out.Files) != 1 || out.Files[0].Status != dto.UploadStored || out.Summary.Rejected != 0 {
		t.Errorf("result = %+v, want the one file stored and the field ignored", out)
	}
	if len(s.names) != 1 {
		t.Errorf("stored %v, want only the file", s.names)
	}
}

// A body that breaks between parts keeps what arrived whole and says the
// batch was interrupted.
func TestAnUploadBrokenBetweenPartsKeepsWhatArrived(t *testing.T) {
	body := "--x\r\nContent-Disposition: form-data; name=\"files\"; filename=\"a.epub\"\r\n\r\nbook one\r\n" +
		"--x\r\nContent-Dispos"
	out := uploadPost(t, &UploadHandler{Store: newFakeStorer(), Queue: &fakeUploadQueue{}, Accounts: anAdmin},
		t.Context(), strings.NewReader(body), "multipart/form-data; boundary=x")
	if !out.Interrupted || out.Message == "" {
		t.Errorf("result = %+v, want it reported as interrupted", out)
	}
	if len(out.Files) != 1 || out.Files[0].Status != dto.UploadStored {
		t.Errorf("files = %+v, want the first stored", out.Files)
	}
}

// A client that has gone stops the batch after the file in hand.
func TestAnUploadStopsWhenTheClientHasGone(t *testing.T) {
	body, contentType := multipartBody(t, [][2]string{{"a.epub", "one"}, {"b.epub", "two"}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s := newFakeStorer()
	out := uploadPost(t, &UploadHandler{Store: s, Queue: &fakeUploadQueue{}, Accounts: anAdmin, Log: quietLogger()},
		ctx, body, contentType)
	if !out.Interrupted || len(out.Files) != 1 {
		t.Errorf("result = %+v, want one file and the batch marked interrupted", out)
	}
}

// What a file was refused for decides whether the batch goes on: a bad name
// is that file's problem, a broken transfer is the batch's.
func TestARefusedFileAndABrokenTransferAreToldApart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		interrupted bool
	}{
		{"an unusable name", ingest.ErrBadFilename, false},
		{"too large", ingest.ErrUploadTooLarge, false},
		{"something else", errors.New("disk full"), false},
		{"the transfer was cancelled", context.Canceled, true},
		{"the transfer was cut short", io.ErrUnexpectedEOF, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeStorer()
			s.err = tc.err
			body, contentType := multipartBody(t, [][2]string{{"a.epub", "one"}, {"b.epub", "two"}})
			out := uploadPost(t, &UploadHandler{Store: s, Queue: &fakeUploadQueue{}, Accounts: anAdmin, Log: quietLogger()},
				t.Context(), body, contentType)
			if out.Interrupted != tc.interrupted {
				t.Errorf("interrupted = %v, want %v", out.Interrupted, tc.interrupted)
			}
			want := 2
			if tc.interrupted {
				want = 1
			}
			if len(out.Files) != want || out.Files[0].Status != dto.UploadRejected || out.Files[0].Error == "" {
				t.Errorf("files = %+v, want %d rejected with a reason", out.Files, want)
			}
		})
	}
}

// uploadDirStorer stores every upload "at" a path the test chose, so what
// deleting it does can be arranged.
type uploadDirStorer struct{ path string }

func (uploadDirStorer) Settle(ingest.StoredFile, bool) error { return nil }

func (s uploadDirStorer) Store(_ context.Context, name string, body io.Reader) (ingest.StoredFile, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return ingest.StoredFile{}, err
	}
	sum := sha256.Sum256(data)
	return ingest.StoredFile{Path: s.path, OriginalFilename: name, ContentHash: hex.EncodeToString(sum[:]),
		Format: domain.FormatEPUB, SizeBytes: int64(len(data))}, nil
}

// Blocked bytes whose copy cannot be deleted are still refused; the copy left
// behind is a log line, not a reason to accept the file.
func TestBlockedBytesAreRefusedEvenIfTheirCopyWillNotGo(t *testing.T) {
	// A directory with something in it cannot be removed as a file.
	dir := filepath.Join(t.TempDir(), "stuck")
	if err := os.MkdirAll(filepath.Join(dir, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("blocked bytes"))
	hash := hex.EncodeToString(sum[:])
	q := &fakeUploadQueue{blocked: map[string]bool{hash: true}}

	body, contentType := multipartBody(t, [][2]string{{"a.epub", "blocked bytes"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	(&UploadHandler{Store: uploadDirStorer{path: dir}, Queue: q, Accounts: anAdmin}).Create(rec, asUser(req))

	if rec.Code != http.StatusUnprocessableEntity || authErrorCode(t, rec) != string(codeContentBlocked) {
		t.Fatalf("status = %d body = %s, want 422 content_blocked", rec.Code, rec.Body)
	}
	if len(q.discarded) != 1 || len(q.enqueued) != 0 {
		t.Errorf("discarded %v enqueued %d", q.discarded, len(q.enqueued))
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the undeletable copy is gone after all: %v", err)
	}
}

// Removing a copy that is already gone counts as removed; one that exists is
// removed.
func TestRemovingAnUploadCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copy.epub")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeIfPresent(path); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy is still there: %v", err)
	}
	if err := removeIfPresent(path); err != nil {
		t.Errorf("removing it again = %v, want nil", err)
	}
}
