// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// fakeEnrichment is enough of a store to reach the permission check.
type fakeEnrichment struct{}

func (fakeEnrichment) WorksNeedingEnrichment(context.Context, string, int) ([]uuid.UUID, error) {
	return nil, nil
}
func (fakeEnrichment) PendingSuggestions(context.Context, int) ([]postgres.Suggestion, error) {
	return nil, nil
}
func (fakeEnrichment) ResolveSuggestion(context.Context, uuid.UUID, bool) error { return nil }
func (fakeEnrichment) BookFilesNeedingChunks(context.Context, int, int) ([]postgres.ChunkTarget, error) {
	return nil, nil
}
func (fakeEnrichment) RetrievalStats(context.Context, string) (postgres.RetrievalStats, error) {
	return postgres.RetrievalStats{}, nil
}

// Every endpoint that changes the SHARED library must refuse a reader.
//
// `users.is_admin` existed from the second migration, was populated correctly,
// and was checked by nothing: on a four-account installation every reader could
// upload files, trigger a rescan, spend the model on enrichment, and accept
// suggestions that rewrite a book's metadata for everyone. This test is what
// stops that returning.
func TestReadersCannotChangeTheSharedLibrary(t *testing.T) {
	routerFor := func(accounts AdminStore) *Router {
		return &Router{
			Health: &HealthHandler{},
			Jobs:   &JobHandler{Queue: &fakeQueue{}, Accounts: accounts},
			Enrichment: &EnrichmentHandler{
				Store: &fakeEnrichment{}, Queue: &fakeQueue{},
				ModelConfigured: true, Accounts: accounts,
			},
		}
	}

	endpoints := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/scan"},
		{http.MethodPost, "/api/v1/enrich"},
		{http.MethodPost, "/api/v1/index"},
		{http.MethodPost, "/api/v1/suggestions/8b1c5f16-0b4b-4a4e-9c3f-9b6a1f0d5e21/accept"},
		{http.MethodPost, "/api/v1/suggestions/8b1c5f16-0b4b-4a4e-9c3f-9b6a1f0d5e21/reject"},
		{http.MethodDelete, "/api/v1/jobs/8b1c5f16-0b4b-4a4e-9c3f-9b6a1f0d5e21"},
	}

	for _, e := range endpoints {
		t.Run("reader refused "+e.path, func(t *testing.T) {
			rec := serve(t, routerFor(aReader), e.method, e.path)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s as a reader: status = %d, want 403; body = %s",
					e.method, e.path, rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), "not_an_admin") {
				t.Fatalf("error code should say why: %s", rec.Body)
			}
		})

		t.Run("admin allowed "+e.path, func(t *testing.T) {
			rec := serve(t, routerFor(anAdmin), e.method, e.path)
			if rec.Code == http.StatusForbidden {
				t.Fatalf("%s %s as an admin was refused: %s", e.method, e.path, rec.Body)
			}
		})
	}
}

func TestReadersCannotUpload(t *testing.T) {
	var body bytes.Buffer
	body.WriteString("--x\r\nContent-Disposition: form-data; name=\"files\"; filename=\"a.epub\"\r\n\r\nx\r\n--x--\r\n")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", &body)
	req.Header.Set("Content-Type", `multipart/form-data; boundary=x`)

	h := &UploadHandler{Log: slog.New(slog.DiscardHandler), Accounts: aReader}
	rec := httptest.NewRecorder()
	h.Create(rec, asUser(req))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body)
	}
}

// Reading, annotating and asking are not admin actions: they are the point of
// having an account and they affect nobody else.
func TestReadersCanStillRead(t *testing.T) {
	rt := &Router{
		Health:  &HealthHandler{},
		Library: &LibraryHandler{Store: &fakeLibrary{}},
	}
	for _, path := range []string{"/api/v1/library", "/api/v1/stats"} {
		rec := serve(t, rt, http.MethodGet, path)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("GET %s was refused to a reader: %s", path, rec.Body)
		}
	}
}
