// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// eraseFakeStore erases without a database. What an erasure owes the disk is
// kept in owed, and DiscardPending pays it through the discard it is given,
// as the real one does under each hash's lock.
type eraseFakeStore struct {
	erased    int
	kept      int
	withdrawn int
	editions  []uuid.UUID
	err       error
	// owed is what the disk is owed, by hash; waiting are hashes something
	// still wants.
	owed    map[string]postgres.StoredCopies
	waiting map[string]bool
	// newly are the hashes the next erasure owes.
	newly []string

	deletedAccount [2]uuid.UUID // actor, target
	erasedFor      [2]uuid.UUID // admin, target
	paidFor        [][]string
	emptied        []uuid.UUID
	// afterPass is told when a DiscardPending pass ends.
	afterPass func()
}

func (s *eraseFakeStore) run() (postgres.Erasure, error) {
	if s.err != nil {
		return postgres.Erasure{}, s.err
	}
	out := postgres.Erasure{Kept: s.kept, Withdrawn: s.withdrawn, Owed: s.newly, Editions: s.editions}
	for range s.erased {
		out.Erased = append(out.Erased, uuid.New())
	}
	return out, nil
}

func (s *eraseFakeStore) DeleteAccountErasing(_ context.Context, actor, target uuid.UUID) (postgres.Erasure, error) {
	s.deletedAccount = [2]uuid.UUID{actor, target}
	return s.run()
}

func (s *eraseFakeStore) EraseSoleHoldings(_ context.Context, admin, target uuid.UUID) (postgres.Erasure, error) {
	s.erasedFor = [2]uuid.UUID{admin, target}
	return s.run()
}

func (s *eraseFakeStore) DiscardPending(_ context.Context, hashes []string,
	discard func(postgres.StoredCopies) error) (postgres.DiscardResult, error) {
	s.paidFor = append(s.paidFor, hashes)
	if hashes == nil {
		for hash := range s.owed {
			hashes = append(hashes, hash)
		}
	}
	var out postgres.DiscardResult
	for _, hash := range hashes {
		c, ok := s.owed[hash]
		switch {
		case !ok:
		case s.waiting[hash]:
			out.Waiting++
		default:
			if err := discard(c); err != nil {
				out.Failed++
				continue
			}
			delete(s.owed, hash)
			out.Paid++
		}
	}
	if s.afterPass != nil {
		s.afterPass()
	}
	return out, nil
}

func (s *eraseFakeStore) StillOwed(_ context.Context, hashes []string) (int, error) {
	n := 0
	for _, h := range hashes {
		if _, ok := s.owed[h]; ok {
			n++
		}
	}
	return n, nil
}

func (s *eraseFakeStore) DeleteEmptyEditions(_ context.Context, editions []uuid.UUID) error {
	s.emptied = append(s.emptied, editions...)
	return nil
}

// eraseDiscarder deletes "copies" Lexarii stored - anything under /data - and
// never anything in the library directory, as ingest.Discard does. It lists
// its archive once per pass, as ingest.Discard can.
type eraseDiscarder struct {
	removed []string
	covers  []string
	fail    error
	indexed int
}

func (d *eraseDiscarder) Copies(hash string) []string {
	return []string{"/data/uploads/books/" + hash + ".epub"}
}

func (d *eraseDiscarder) ArchiveIndex() map[string][]string {
	d.indexed++
	return map[string][]string{}
}

func (d *eraseDiscarder) CopiesIn(hash string, _ map[string][]string) []string {
	return d.Copies(hash)
}

func (d *eraseDiscarder) Remove(path string) (bool, error) {
	if d.fail != nil {
		return false, d.fail
	}
	if len(path) < 6 || path[:6] != "/data/" {
		return false, nil
	}
	d.removed = append(d.removed, path)
	return true, nil
}

func (d *eraseDiscarder) RemoveCover(name string) error {
	if name != "" {
		d.covers = append(d.covers, name)
	}
	return nil
}

func eraseRouter(accounts *accountsFakeStore, eraser *eraseFakeStore, d CopyDiscarder, onDelete bool) *Router {
	return &Router{
		Health:  &HealthHandler{},
		Sharing: &SharingHandler{Store: &fakeSharing{}},
		Accounts: &AccountHandler{Store: accounts, Archive: &accountsKeeper{}, Logger: quietLogger(),
			Eraser: eraser, Discard: d, EraseOnDelete: onDelete},
	}
}

// With erasing on, deleting an account erases instead of archiving, then
// pays what the erasure owes the disk at once: the copies Lexarii stored -
// never one in the library directory - and the covers. What cannot be paid
// yet is counted as pending. The editions left empty are tidied.
func TestDeletingAnAccountErasesWhenTheInstallationSaysSo(t *testing.T) {
	edition := uuid.New()
	accounts := &accountsFakeStore{}
	eraser := &eraseFakeStore{
		erased: 2,
		kept:   1,
		owed: map[string]postgres.StoredCopies{
			"aa": {ContentHash: "aa", Paths: []string{"/data/uploads/books/aa.epub"}, Covers: []string{"aa.jpg"}},
			"bb": {ContentHash: "bb", Paths: []string{"/library/mine/bb.pdf"}},
			"cc": {ContentHash: "cc"},
		},
		waiting:  map[string]bool{"cc": true},
		newly:    []string{"aa", "bb", "cc"},
		editions: []uuid.UUID{edition},
	}
	d := &eraseDiscarder{}
	admin, target := uuid.New(), uuid.New()

	rec := accountsServe(t, eraseRouter(accounts, eraser, d, true), admin, http.MethodDelete,
		"/api/v1/users/"+target.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if len(accounts.deletes) != 0 {
		t.Error("the archiving deletion ran too")
	}
	if eraser.deletedAccount != [2]uuid.UUID{admin, target} {
		t.Errorf("erasing deletion asked for %v", eraser.deletedAccount)
	}
	got := decodeInto[dto.AccountDeleted](t, rec)
	if got.FilesErased != 2 || got.BooksArchived != 1 || got.CopiesPending != 1 {
		t.Errorf("reply = %+v, want 2 erased, 1 kept, 1 still owed", got)
	}
	if len(eraser.paidFor) != 1 || len(eraser.paidFor[0]) != 3 {
		t.Errorf("paid for %v, want exactly what the erasure owed", eraser.paidFor)
	}
	want := map[string]bool{"/data/uploads/books/aa.epub": true, "/data/uploads/books/bb.epub": true}
	if len(d.removed) != len(want) {
		t.Errorf("removed %v, want %v", d.removed, want)
	}
	for _, path := range d.removed {
		if !want[path] {
			t.Errorf("removed %s, which was not to go", path)
		}
	}
	if len(d.covers) != 1 || d.covers[0] != "aa.jpg" {
		t.Errorf("covers removed = %v", d.covers)
	}
	if d.indexed != 1 {
		t.Errorf("the archive was listed %d times for one pass, want once", d.indexed)
	}
	if len(eraser.emptied) != 1 || eraser.emptied[0] != edition {
		t.Errorf("editions tidied = %v", eraser.emptied)
	}
	if _, still := eraser.owed["cc"]; !still {
		t.Error("bytes something still wants were not left owed")
	}
}

// Off - the default - deleting an account archives exactly as it always did.
func TestDeletingAnAccountArchivesByDefault(t *testing.T) {
	accounts := &accountsFakeStore{}
	eraser := &eraseFakeStore{erased: 1}
	rec := accountsServe(t, eraseRouter(accounts, eraser, &eraseDiscarder{}, false), uuid.New(),
		http.MethodDelete, "/api/v1/users/"+uuid.New().String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if len(accounts.deletes) != 1 || eraser.deletedAccount != [2]uuid.UUID{} {
		t.Error("the default did not archive")
	}
}

// A disk that refuses does not undo a deletion: the records are gone, the
// copies stay owed and the reply says so. Refusals map as an archiving
// deletion's do.
func TestErasingDeletionFailures(t *testing.T) {
	t.Run("the disk refused", func(t *testing.T) {
		eraser := &eraseFakeStore{erased: 1, newly: []string{"aa"},
			owed: map[string]postgres.StoredCopies{"aa": {ContentHash: "aa"}}}
		rec := accountsServe(t, eraseRouter(&accountsFakeStore{}, eraser, &eraseDiscarder{fail: errors.New("read-only")}, true),
			uuid.New(), http.MethodDelete, "/api/v1/users/"+uuid.New().String(), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body)
		}
		if got := decodeInto[dto.AccountDeleted](t, rec); got.FilesErased != 1 || got.CopiesPending != 1 {
			t.Errorf("reply = %+v, want the copy still owed", got)
		}
	})
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not an administrator", store.ErrForbidden, http.StatusForbidden, "not_an_admin"},
		{"the last administrator", fmt.Errorf("%w: only one", store.ErrInvalidQuery), http.StatusBadRequest, string(CodeInvalid)},
		{"nobody", store.ErrNotFound, http.StatusNotFound, string(CodeNotFound)},
		{"the store is down", errAccountsDown, http.StatusInternalServerError, string(CodeInternal)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eraser := &eraseFakeStore{err: tc.err}
			rec := accountsServe(t, eraseRouter(&accountsFakeStore{}, eraser, &eraseDiscarder{}, true),
				uuid.New(), http.MethodDelete, "/api/v1/users/"+uuid.New().String(), "")
			if rec.Code != tc.status || authErrorCode(t, rec) != tc.code {
				t.Errorf("status = %d body = %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
		})
	}
}

// An administrator erasing somebody's books now: whatever the setting, and
// only an administrator.
func TestErasingSomebodysBooksOnDemand(t *testing.T) {
	eraser := &eraseFakeStore{
		erased: 1, kept: 2, withdrawn: 3, newly: []string{"aa"},
		owed: map[string]postgres.StoredCopies{"aa": {ContentHash: "aa", Paths: []string{"/data/uploads/books/aa.epub"}}},
	}
	d := &eraseDiscarder{}
	admin, target := uuid.New(), uuid.New()
	rec := accountsServe(t, eraseRouter(&accountsFakeStore{}, eraser, d, false), admin, http.MethodPost,
		"/api/v1/admin/users/"+target.String()+"/erase-files", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if eraser.erasedFor != [2]uuid.UUID{admin, target} {
		t.Errorf("erased for %v", eraser.erasedFor)
	}
	got := decodeInto[dto.FilesErased](t, rec)
	if got.FilesErased != 1 || got.CopiesDeleted != 1 || got.CopiesPending != 0 || got.Kept != 2 ||
		got.Withdrawn != 3 {
		t.Errorf("reply = %+v", got)
	}

	for _, tc := range []struct {
		name   string
		err    error
		target string
		status int
		code   string
	}{
		{"not an administrator", store.ErrForbidden, target.String(), http.StatusForbidden, "not_an_admin"},
		{"nobody", store.ErrNotFound, target.String(), http.StatusNotFound, string(CodeNotFound)},
		{"not an id", nil, "someone", http.StatusBadRequest, string(CodeInvalid)},
		{"the store is down", errAccountsDown, target.String(), http.StatusInternalServerError, string(CodeInternal)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := accountsServe(t, eraseRouter(&accountsFakeStore{}, &eraseFakeStore{err: tc.err}, &eraseDiscarder{}, false),
				admin, http.MethodPost, "/api/v1/admin/users/"+tc.target+"/erase-files", "")
			if rec.Code != tc.status || authErrorCode(t, rec) != tc.code {
				t.Errorf("status = %d body = %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
		})
	}

	// Without a way to delete the copies they stay owed, and the reply says
	// so: never records gone and bytes silently kept.
	t.Run("no discarder", func(t *testing.T) {
		eraser := &eraseFakeStore{erased: 1, newly: []string{"aa"},
			owed: map[string]postgres.StoredCopies{"aa": {ContentHash: "aa"}}}
		rt := &Router{Health: &HealthHandler{}, Sharing: &SharingHandler{Store: &fakeSharing{}},
			Accounts: &AccountHandler{Store: &accountsFakeStore{}, Logger: quietLogger(), Eraser: eraser}}
		rec := accountsServe(t, rt, admin, http.MethodPost, "/api/v1/admin/users/"+target.String()+"/erase-files", "")
		if got := decodeInto[dto.FilesErased](t, rec); rec.Code != http.StatusOK || got.CopiesPending != 1 {
			t.Errorf("status = %d, reply = %+v; want the copy still owed", rec.Code, got)
		}
	})

	t.Run("signed out", func(t *testing.T) {
		rec := accountsServe(t, eraseRouter(&accountsFakeStore{}, &eraseFakeStore{}, &eraseDiscarder{}, false),
			uuid.Nil, http.MethodPost, "/api/v1/admin/users/"+target.String()+"/erase-files", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

// What stays owed is paid later without anybody asking: at once, and again
// every interval until the server stops.
func TestFinishingErasuresLater(t *testing.T) {
	eraser := &eraseFakeStore{owed: map[string]postgres.StoredCopies{
		"aa": {ContentHash: "aa", Paths: []string{"/data/uploads/books/aa.epub"}},
	}}
	d := &eraseDiscarder{}
	h := &AccountHandler{Store: &accountsFakeStore{}, Logger: quietLogger(), Eraser: eraser, Discard: d}

	ctx, cancel := context.WithCancel(t.Context())
	passed := make(chan struct{})
	eraser.afterPass = func() {
		eraser.afterPass = nil
		close(passed)
	}
	done := make(chan struct{})
	go func() {
		h.FinishErasures(ctx, time.Hour)
		close(done)
	}()
	<-passed
	cancel()
	<-done
	if len(eraser.owed) != 0 || len(eraser.paidFor) == 0 || eraser.paidFor[0] != nil {
		t.Errorf("owed = %v, passes = %v; want everything owed paid in one pass over all of it", eraser.owed, eraser.paidFor)
	}
}

// Bytes an upload is on its way to read: nothing in the upload directory is
// deleted, only the archive's copies and the covers - and with a discarder
// that cannot tell where its uploads are, no copy at all.
func TestDeletingCopiesSparesAnUploadsBytes(t *testing.T) {
	d := ingestLikeDiscarder{eraseDiscarder: &eraseDiscarder{}}
	n, err := deleteCopiesIndexed(d, postgres.StoredCopies{
		ContentHash: "aa",
		Paths:       []string{"/data/archive/aa.epub"},
		Covers:      []string{"aa.jpg"},
		SkipUploads: true,
	}, d.ArchiveIndex())
	if err != nil || n != 1 || len(d.removed) != 1 || d.removed[0] != "/data/archive/aa.epub" ||
		len(d.covers) != 1 {
		t.Errorf("removed %v covers %v (%d, %v), want only the archive copy and the cover", d.removed, d.covers, n, err)
	}

	blind := &eraseDiscarder{}
	if _, err := deleteCopiesIndexed(blind, postgres.StoredCopies{
		ContentHash: "aa", Paths: []string{"/data/archive/aa.epub"}, SkipUploads: true,
	}, nil); err != nil || len(blind.removed) != 0 {
		t.Errorf("a discarder that cannot tell removed %v (%v)", blind.removed, err)
	}
}

// ingestLikeDiscarder knows its upload directory, as ingest.Discard does.
type ingestLikeDiscarder struct{ *eraseDiscarder }

func (ingestLikeDiscarder) IsUpload(path string) bool {
	return len(path) >= 14 && path[:14] == "/data/uploads/"
}
