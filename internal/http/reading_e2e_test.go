// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// readingRig is one book against a real database, as three people see it:
// alice holds it, bob was lent it to read but not to download, and carol has
// nothing to do with it.
type readingRig struct {
	pool              *postgres.Pool
	rt                *Router
	alice, bob, carol uuid.UUID
	fileID, workID    uuid.UUID
}

func newReadingRig(t *testing.T) *readingRig {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	p := e2ePool(t, dbURL)
	r := &readingRig{pool: p, alice: e2eUser(t, p, false), bob: e2eUser(t, p, false), carol: e2eUser(t, p, false)}

	uploads := &Router{Uploads: &UploadHandler{
		Store: &ingest.UploadStore{Root: t.TempDir()},
		Queue: p, Accounts: p, Log: quietLogger(), AnyoneMayUpload: true,
	}}
	epub, err := os.ReadFile(writeReaderEPUB(t))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	if rec, _ := uploadAs(t, uploads, r.alice, [][2]string{{"untrusted.epub", string(epub)}}); rec.Code >= 300 {
		t.Fatalf("upload: status %d, body %s", rec.Code, rec.Body)
	}
	drainQueue(t, p, &worker.Ingest{Store: p, CoverDir: t.TempDir(), Logger: quietLogger()})

	page, err := p.Library(t.Context(), postgres.LibraryQuery{ViewerID: r.alice, Owned: postgres.OwnedMine})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("alice's shelf = %+v, %v; want the one book", page.Items, err)
	}
	r.fileID, r.workID = page.Items[0].BookFileID, page.Items[0].WorkID

	if err := p.ShareBook(t.Context(), r.fileID, r.alice, r.bob, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	r.rt = &Router{
		Reader:      &ReaderHandler{Store: p},
		Annotations: &AnnotationHandler{Store: p},
		Progress:    &ProgressHandler{Store: p},
		Sync:        &SyncHandler{Store: p},
		Collections: &CollectionHandler{Store: p},
	}
	return r
}

func (r *readingRig) do(t *testing.T, who uuid.UUID, method, target, body string) (int, string) {
	t.Helper()
	rec := serveAsViewer(t, r.rt, who, method, target, body)
	return rec.Code, rec.Body.String()
}

func (r *readingRig) fileURL(rest string) string { return bookFileURL(r.fileID, rest) }

// The reader against the real access views: a holder downloads, a read-only
// grantee reads sanitised chapters and is refused the file, and somebody with
// no access is told the book does not exist - never that it is forbidden.
func TestReadingAgainstTheRealAccessModel(t *testing.T) {
	r := newReadingRig(t)

	for _, tc := range []struct {
		who    string
		viewer uuid.UUID
		want   map[string]int
	}{
		{"the holder", r.alice, map[string]int{
			"/info": 200, "/file": 200, "/manifest": 200, "/spine/0": 200, "/assets/OEBPS/images/plate%201.png": 200,
		}},
		{"a read-only grantee", r.bob, map[string]int{
			"/info": 200, "/file": 403, "/manifest": 200, "/spine/0": 200, "/assets/OEBPS/images/plate%201.png": 200,
		}},
		{"a stranger", r.carol, map[string]int{
			"/info": 404, "/file": 404, "/manifest": 404, "/spine/0": 404, "/assets/OEBPS/images/plate%201.png": 404,
			"/page/1": 404,
		}},
	} {
		for endpoint, status := range tc.want {
			t.Run(tc.who+endpoint, func(t *testing.T) {
				code, body := r.do(t, tc.viewer, http.MethodGet, r.fileURL(endpoint), "")
				if code != status {
					t.Fatalf("status = %d, want %d; body = %s", code, status, body)
				}
			})
		}
	}

	t.Run("the download permission is reported to each reader", func(t *testing.T) {
		for who, want := range map[uuid.UUID]bool{r.alice: true, r.bob: false} {
			code, body := r.do(t, who, http.MethodGet, r.fileURL("/info"), "")
			var info dto.BookInfo
			if err := json.Unmarshal([]byte(body), &info); err != nil || code != 200 {
				t.Fatalf("info: %d %s", code, body)
			}
			if info.CanDownload != want || info.WorkID != r.workID.String() || !info.Reflowable {
				t.Errorf("info for %s = %+v, want canDownload %v", who, info, want)
			}
		}
	})

	t.Run("the grantee's chapter is sanitised", func(t *testing.T) {
		_, body := r.do(t, r.bob, http.MethodGet, r.fileURL("/spine/0"), "")
		if strings.Contains(body, "<script") || strings.Contains(body, "onclick") || strings.Contains(body, "javascript:") {
			t.Fatalf("a hostile chapter reached a reader: %s", body)
		}
	})

	t.Run("reading somebody else's book is recorded; reading your own is not", func(t *testing.T) {
		var bobRows, aliceRows int
		if err := r.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM access_log WHERE user_id = $1 AND kind = 'chapter'`, r.bob).Scan(&bobRows); err != nil {
			t.Fatalf("reading the log: %v", err)
		}
		if err := r.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM access_log WHERE user_id = $1`, r.alice).Scan(&aliceRows); err != nil {
			t.Fatalf("reading the log: %v", err)
		}
		if bobRows == 0 {
			t.Error("the grantee's reading left no record")
		}
		if aliceRows != 0 {
			t.Errorf("the holder's own reading was logged (%d rows)", aliceRows)
		}
	})
}

// Notes, positions and collections against the real store: each is
// somebody's own, and a book they cannot open is "not found".
func TestPersonalMarksAgainstTheRealStore(t *testing.T) {
	r := newReadingRig(t)
	mark := `{"type":"highlight","start":"epub:0/body[1]/p[1]:0","end":"epub:0/body[1]/p[1]:7","selectedText":"Opening"}`

	t.Run("a stranger cannot annotate a book they cannot open", func(t *testing.T) {
		code, body := r.do(t, r.carol, http.MethodPost, r.fileURL("/annotations"), mark)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, body = %s; want 404", code, body)
		}
		var n int
		if err := r.pool.QueryRow(t.Context(), `SELECT count(*) FROM annotations`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("annotations stored = %d, %v; want none", n, err)
		}
	})

	code, body := r.do(t, r.bob, http.MethodPost, r.fileURL("/annotations"), mark)
	if code != http.StatusCreated {
		t.Fatalf("grantee annotating: %d %s", code, body)
	}
	var bobs dto.Annotation
	if err := json.Unmarshal([]byte(body), &bobs); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	t.Run("a mark is its author's alone", func(t *testing.T) {
		_, list := r.do(t, r.alice, http.MethodGet, r.fileURL("/annotations"), "")
		if strings.Contains(list, bobs.ID) {
			t.Errorf("the holder sees the grantee's unshared mark: %s", list)
		}
		for _, method := range []string{http.MethodPatch, http.MethodDelete} {
			if code, body := r.do(t, r.carol, method, "/api/v1/annotations/"+bobs.ID, `{"note":"mine now"}`); code != http.StatusNotFound {
				t.Errorf("%s by a stranger: %d %s; want 404", method, code, body)
			}
		}
		if code, _ := r.do(t, r.bob, http.MethodPatch, "/api/v1/annotations/"+bobs.ID, `{"note":"mine"}`); code != http.StatusOK {
			t.Errorf("the author's own edit: status %d", code)
		}
	})

	t.Run("an import into a book nobody lent you is not found", func(t *testing.T) {
		code, _ := r.do(t, r.carol, http.MethodPost, r.fileURL("/annotations/import"), `{"marks":[{"page":1,"text":"x"}]}`)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
		}
	})

	t.Run("a stranger cannot save a position in a book they cannot open", func(t *testing.T) {
		code, _ := r.do(t, r.carol, http.MethodPut, r.fileURL("/progress"), `{"locator":"epub:1/body[1]/p[1]:3"}`)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
		}
		// And a file that does not exist answers the same, so the two cannot
		// be told apart.
		code, _ = r.do(t, r.carol, http.MethodPut, "/api/v1/books/"+uuid.NewString()+"/progress", `{"locator":"epub:1/body[1]/p[1]:3"}`)
		if code != http.StatusNotFound {
			t.Fatalf("an absent file: status = %d, want 404", code)
		}
	})

	t.Run("a position is saved and read back", func(t *testing.T) {
		if code, body := r.do(t, r.bob, http.MethodPut, r.fileURL("/progress"), `{"locator":"epub:1/body[1]/p[1]:3","percent":0.6}`); code != http.StatusOK {
			t.Fatalf("saving: %d %s", code, body)
		}
		_, body := r.do(t, r.bob, http.MethodGet, r.fileURL("/progress"), "")
		var got dto.ReadingProgress
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if !got.Started || got.Locator != "epub:1/body[1]/p[1]:3" {
			t.Errorf("progress = %+v", got)
		}
		// Going back a chapter moves the position and keeps how far they got.
		if code, _ := r.do(t, r.bob, http.MethodPut, r.fileURL("/progress"), `{"locator":"epub:0/body[1]/p[1]:0"}`); code != http.StatusOK {
			t.Fatalf("saving backwards: %d", code)
		}
		_, body = r.do(t, r.bob, http.MethodGet, r.fileURL("/progress"), "")
		got = dto.ReadingProgress{}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if got.Locator != "epub:0/body[1]/p[1]:0" || got.FurthestLocator != "epub:1/body[1]/p[1]:3" {
			t.Errorf("after going back: %+v", got)
		}
		// Somebody else's position on the same book is not theirs to read.
		_, body = r.do(t, r.alice, http.MethodGet, r.fileURL("/progress"), "")
		if strings.Contains(body, `"started":true`) {
			t.Errorf("the holder reads the grantee's position: %s", body)
		}
	})

	t.Run("collections", func(t *testing.T) {
		code, body := r.do(t, r.alice, http.MethodPost, "/api/v1/collections", `{"name":"To read"}`)
		if code != http.StatusCreated {
			t.Fatalf("creating: %d %s", code, body)
		}
		var c dto.Collection
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		members := "/api/v1/collections/" + c.ID + "/works/" + r.workID.String()
		if code, body := r.do(t, r.alice, http.MethodPut, members, ""); code != http.StatusNoContent {
			t.Fatalf("adding: %d %s", code, body)
		}
		_, body = r.do(t, r.alice, http.MethodGet, "/api/v1/collections/"+c.ID, "")
		var detail dto.CollectionDetail
		if err := json.Unmarshal([]byte(body), &detail); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if len(detail.Items) != 1 || detail.Items[0].WorkID != r.workID.String() {
			t.Errorf("the collection holds %+v", detail.Items)
		}

		// Somebody else's collection does not exist for them, and a book they
		// cannot read cannot go in their own.
		if code, _ := r.do(t, r.carol, http.MethodGet, "/api/v1/collections/"+c.ID, ""); code != http.StatusNotFound {
			t.Errorf("a stranger reading the collection: %d, want 404", code)
		}
		if code, _ := r.do(t, r.carol, http.MethodPut, members, ""); code != http.StatusNotFound {
			t.Errorf("a stranger adding to it: %d, want 404", code)
		}
		code, body = r.do(t, r.carol, http.MethodPost, "/api/v1/collections", `{"name":"Mine"}`)
		var own dto.Collection
		if err := json.Unmarshal([]byte(body), &own); err != nil || code != http.StatusCreated {
			t.Fatalf("creating: %d %s", code, body)
		}
		if code, _ := r.do(t, r.carol, http.MethodPut, "/api/v1/collections/"+own.ID+"/works/"+r.workID.String(), ""); code != http.StatusNotFound {
			t.Errorf("a stranger shelving a book they cannot read: %d, want 404", code)
		}
		if code, _ := r.do(t, r.alice, http.MethodDelete, members, ""); code != http.StatusNoContent {
			t.Errorf("removing: %d", code)
		}
		if code, _ := r.do(t, r.alice, http.MethodDelete, "/api/v1/collections/"+c.ID, ""); code != http.StatusNoContent {
			t.Errorf("deleting: %d", code)
		}
	})
}

// Sync against the real store: a push is answered per mark, and a pull pages
// with the cursor it hands out, not by resending `since`.
func TestSyncRoundTripAgainstTheRealStore(t *testing.T) {
	r := newReadingRig(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)

	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	push := dto.SyncPush{Positions: []dto.PushedPosition{{BookFileID: r.fileID.String(), Position: "epub:1/body[1]/p[1]:0", ReadAt: at}}}
	for i, id := range ids {
		push.Annotations = append(push.Annotations, dto.PushedMark{
			ID: id.String(), BookFileID: r.fileID.String(), Start: "epub:0/body[1]/p[1]:0", End: "epub:0/body[1]/p[1]:5",
			SelectedText: "Openi", Note: "offline", UpdatedAt: at.Add(time.Duration(i) * time.Second),
		})
	}
	pushBody, err := json.Marshal(push)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	pushAs := func(t *testing.T, who uuid.UUID, body string) dto.SyncPushResult {
		t.Helper()
		code, out := r.do(t, who, http.MethodPost, "/api/v1/sync", body)
		if code != http.StatusOK {
			t.Fatalf("push: %d %s", code, out)
		}
		var res dto.SyncPushResult
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		return res
	}

	t.Run("a stranger's marks are refused one by one", func(t *testing.T) {
		res := pushAs(t, r.carol, string(pushBody))
		if len(res.Marks) != len(ids) {
			t.Fatalf("results = %+v", res.Marks)
		}
		for _, m := range res.Marks {
			if m.Status != postgres.MarkRefused {
				t.Errorf("a stranger's mark on a book they cannot open: %+v", m)
			}
		}
	})

	t.Run("the reader's own marks are applied, and an older copy is stale", func(t *testing.T) {
		res := pushAs(t, r.bob, string(pushBody))
		for _, m := range res.Marks {
			if m.Status != postgres.MarkApplied {
				t.Errorf("mark = %+v, want applied", m)
			}
		}
		older := dto.SyncPush{Annotations: []dto.PushedMark{push.Annotations[2]}}
		older.Annotations[0].UpdatedAt = at.Add(-time.Hour)
		older.Annotations[0].Note = "an edit made before the one already stored"
		body, err := json.Marshal(older)
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		if res := pushAs(t, r.bob, string(body)); len(res.Marks) != 1 || res.Marks[0].Status != postgres.MarkStale {
			t.Errorf("older copy = %+v, want stale", res.Marks)
		}
	})

	pull := func(t *testing.T, query url.Values) (int, dto.SyncPull, string) {
		t.Helper()
		code, body := r.do(t, r.bob, http.MethodGet, "/api/v1/sync?"+query.Encode(), "")
		var out dto.SyncPull
		if code == http.StatusOK {
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				t.Fatalf("decoding: %v", err)
			}
		}
		return code, out, body
	}

	t.Run("a pull pages with its cursor until the round is done", func(t *testing.T) {
		seen := map[string]int{}
		positions := 0
		var until time.Time
		query := url.Values{"limit": {"1"}}
		for page := 0; ; page++ {
			if page > 10 {
				t.Fatal("the cursor never finished the round")
			}
			code, out, body := pull(t, query)
			if code != http.StatusOK {
				t.Fatalf("page %d: %d %s", page, code, body)
			}
			if len(out.Annotations) > 1 || len(out.Positions) > 1 {
				t.Fatalf("page %d ignored the limit: %+v", page, out)
			}
			if page == 0 {
				until = out.Until
			} else if !out.Until.Equal(until) {
				t.Errorf("page %d moved until from %v to %v inside one round", page, until, out.Until)
			}
			for _, a := range out.Annotations {
				seen[a.ID]++
			}
			positions += len(out.Positions)
			if !out.More {
				if out.Cursor != "" {
					t.Errorf("a finished round still carries a cursor %q", out.Cursor)
				}
				break
			}
			if out.Cursor == "" {
				t.Fatalf("page %d says more but gives no cursor", page)
			}
			query = url.Values{"limit": {"1"}, "cursor": {out.Cursor}}
		}
		for _, id := range ids {
			if seen[id.String()] != 1 {
				t.Errorf("mark %s seen %d times across the round, want once", id, seen[id.String()])
			}
		}
		if len(seen) != len(ids) || positions != 1 {
			t.Errorf("round carried %d marks and %d positions, want %d and 1", len(seen), positions, len(ids))
		}

		// The next round starts at `until`: nothing written before it, beyond
		// the deliberate overlap, comes back.
		_, next, _ := pull(t, url.Values{"since": {until.Add(time.Minute).Format(time.RFC3339Nano)}})
		if len(next.Annotations) != 0 || len(next.Positions) != 0 || next.More {
			t.Errorf("a round from after every change returned %+v", next)
		}
	})

	t.Run("somebody else's pull carries none of it", func(t *testing.T) {
		code, body := r.do(t, r.alice, http.MethodGet, "/api/v1/sync", "")
		var out dto.SyncPull
		if err := json.Unmarshal([]byte(body), &out); err != nil || code != http.StatusOK {
			t.Fatalf("pull: %d %s", code, body)
		}
		if len(out.Annotations) != 0 || len(out.Positions) != 0 {
			t.Errorf("the holder pulled the grantee's reading: %+v", out)
		}
	})

	t.Run("a tampered cursor is the client's mistake", func(t *testing.T) {
		for _, cursor := range []string{"not base64 at all!", "bm90IGpzb24"} {
			code, _, body := pull(t, url.Values{"cursor": {cursor}})
			if code != http.StatusBadRequest {
				t.Errorf("cursor %q: %d %s; want 400", cursor, code, body)
			}
		}
	})
}
