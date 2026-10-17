// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// routerDocumentedRoutes is every route the router registers, with sample
// values in place of its path parameters ({id} is replaced by a UUID).
var routerDocumentedRoutes = []string{
	"GET /api/v1/library", "GET /api/v1/stats", "GET /api/v1/subjects",
	"GET /api/v1/works/{id}", "GET /api/v1/works/{id}/mentions", "PATCH /api/v1/works/{id}",
	"GET /api/v1/duplicates", "POST /api/v1/works/{id}/merge",
	"POST /api/v1/scan", "GET /api/v1/jobs/{id}", "DELETE /api/v1/jobs/{id}", "GET /api/v1/jobs/{id}/events",
	"GET /api/v1/books/{id}/info", "GET /api/v1/books/{id}/file", "GET /api/v1/books/{id}/manifest",
	"GET /api/v1/books/{id}/spine/1", "GET /api/v1/books/{id}/assets/OEBPS/style.css", "GET /api/v1/books/{id}/page/1",
	"GET /api/v1/auth/providers", "POST /api/v1/auth/login", "POST /api/v1/auth/token", "POST /api/v1/auth/logout",
	"POST /api/v1/auth/setup", "GET /api/v1/auth/me", "GET /api/v1/auth/sessions", "DELETE /api/v1/auth/sessions/{id}",
	"POST /api/v1/auth/sessions/revoke-all", "GET /api/v1/auth/oidc/start", "GET /api/v1/auth/oidc/callback",
	"GET /api/v1/books/{id}/annotations", "POST /api/v1/books/{id}/annotations",
	"POST /api/v1/books/{id}/annotations/orphaned", "POST /api/v1/books/{id}/annotations/import",
	"PATCH /api/v1/annotations/{id}", "DELETE /api/v1/annotations/{id}", "GET /api/v1/annotations",
	"GET /opds", "GET /opds/new", "GET /opds/all", "GET /opds/search", "GET /opds/search.xml", "GET /opds/subjects",
	"GET /opds/subjects/maths", "GET /opds/collections", "GET /opds/collections/{id}",
	"GET /api/v1/sync", "POST /api/v1/sync",
	"GET /api/v1/books/{id}/progress", "PUT /api/v1/books/{id}/progress", "GET /api/v1/continue",
	"POST /api/v1/ask",
	"GET /api/v1/conversations", "POST /api/v1/conversations", "GET /api/v1/conversations/{id}",
	"DELETE /api/v1/conversations/{id}", "POST /api/v1/conversations/{id}/messages",
	"POST /api/v1/conversations/stream", "POST /api/v1/conversations/{id}/messages/stream",
	"POST /api/v1/enrich", "POST /api/v1/index", "GET /api/v1/suggestions",
	"POST /api/v1/suggestions/{id}/accept", "POST /api/v1/suggestions/{id}/reject",
	"GET /api/v1/covers/pending", "GET /api/v1/books/{id}/cover", "PUT /api/v1/books/{id}/cover",
	"GET /api/v1/collections", "POST /api/v1/collections", "GET /api/v1/collections/{id}",
	"PATCH /api/v1/collections/{id}", "DELETE /api/v1/collections/{id}",
	"PUT /api/v1/collections/{id}/works/{id}", "DELETE /api/v1/collections/{id}/works/{id}",
	"GET /api/v1/works/{id}/citation", "GET /api/v1/citations",
	"GET /api/v1/people", "GET /api/v1/shares/activity", "DELETE /api/v1/users/{id}", "GET /api/v1/archive",
	"POST /api/v1/archive/{id}/claim", "DELETE /api/v1/books/{id}/holding",
	"GET /api/v1/books/{id}/annotations/shares", "PUT /api/v1/books/{id}/annotations/shares/{id}",
	"DELETE /api/v1/books/{id}/annotations/shares/{id}", "GET /api/v1/books/{id}/shares",
	"PUT /api/v1/books/{id}/shares/{id}", "DELETE /api/v1/books/{id}/shares/{id}", "GET /api/v1/books/{id}/access",
	"POST /api/v1/uploads",
	"POST /api/v1/admin/books/{id}/remove", "GET /api/v1/admin/blocked",
	"DELETE /api/v1/admin/blocked/" + strings.Repeat("ab", 32),
	"POST /api/v1/admin/blocked/" + strings.Repeat("ab", 32) + "/discard",
	"GET /branding/branding.css",
}

// routerFallback reports whether a response is what the router gives a
// request no route claimed: the API's own 404, the mux's 405, or the web
// client's shell.
func routerFallback(rec *httptest.ResponseRecorder) bool {
	if rec.Code == http.StatusMethodNotAllowed {
		return true
	}
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		return true
	}
	return rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "endpoint not found")
}

// Every route is reachable when its handler is configured, and absent when it
// is not: a handler group left out of the wiring must not leave its routes
// answering, and one put in must not be shadowed by the web client.
func TestEveryRouteIsWiredToItsHandler(t *testing.T) {
	wired := (&Router{
		Health: &HealthHandler{}, Library: &LibraryHandler{}, Jobs: &JobHandler{}, Uploads: &UploadHandler{},
		Reader: &ReaderHandler{}, Progress: &ProgressHandler{}, Sync: &SyncHandler{}, OPDS: &OPDSHandler{},
		Auth: &AuthHandler{Log: quietLogger()}, Annotations: &AnnotationHandler{}, Enrichment: &EnrichmentHandler{},
		Ask: &AskHandler{}, Chat: &ChatHandler{}, Sharing: &SharingHandler{}, Covers: &CoverHandler{},
		Accounts: &AccountHandler{}, Citations: &CitationHandler{}, Collections: &CollectionHandler{},
		Moderation: &ModerationHandler{},
		Branding:   http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	}).Handler()
	bare := (&Router{Health: &HealthHandler{}}).Handler()

	id := "0b8a8f3e-0000-4000-8000-000000000000"
	for _, route := range routerDocumentedRoutes {
		method, target, _ := strings.Cut(strings.ReplaceAll(route, "{id}", id), " ")
		t.Run(route, func(t *testing.T) {
			// Anonymous, so nearly every handler refuses before it needs a
			// store it was not given; one that does reach for one is caught by
			// Recover, which is still not the fallback.
			rec := httptest.NewRecorder()
			wired.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader("{}")))
			if routerFallback(rec) {
				t.Errorf("wired: %d %q, want the route's own handler", rec.Code, rec.Body)
			}

			rec = httptest.NewRecorder()
			bare.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader("{}")))
			if !routerFallback(rec) {
				t.Errorf("unwired: %d %q, want no handler", rec.Code, rec.Body)
			}
		})
	}
}

// The error the handlers build carries its cause, so a caller can still ask
// what went wrong underneath, and the client is told only the message.
func TestAnInvalidRequestErrorKeepsItsCause(t *testing.T) {
	cause := errors.New("strconv: not a number")
	err := fmt.Errorf("handling: %w", Invalid("limit must be an integer", cause))
	if !errors.Is(err, cause) {
		t.Error("the cause is lost")
	}
	var he *httpError
	if !errors.As(err, &he) || he.status != http.StatusBadRequest || he.Error() != he.message {
		t.Errorf("httpError = %+v", he)
	}

	rec := httptest.NewRecorder()
	respondError(rec, err)
	if rec.Code != http.StatusBadRequest || authErrorCode(t, rec) != string(CodeInvalid) {
		t.Errorf("a wrapped invalid request = %d %s, want 400", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "strconv") {
		t.Error("the cause leaked to the client")
	}

	var nf *httpError
	if !errors.As(NotFound("thing"), &nf) || nf.Unwrap() != nil {
		t.Error("a not-found has a cause it was never given")
	}
}
