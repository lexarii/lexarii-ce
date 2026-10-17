// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// rawRequest parses a request line exactly as the server would, so the URL
// keeps the escaping it arrived with. httptest.NewRequest would do the same,
// but spelling the wire form out makes the test say what it is testing.
func rawRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(
		method + " " + target + " HTTP/1.1\r\nHost: lexarii.test\r\n\r\n")))
	if err != nil {
		t.Fatalf("parsing %s %s: %v", method, target, err)
	}
	return r
}

// untouchedReader fails the test if the reader is asked for anything at all.
type untouchedReader struct{ t *testing.T }

func (u untouchedReader) ReadableBookFile(context.Context, uuid.UUID, uuid.UUID) (domain.BookFile, error) {
	u.t.Error("an anonymous request reached the reader's store")
	return domain.BookFile{}, nil
}

func (u untouchedReader) ReadableBookWithWork(context.Context, uuid.UUID, uuid.UUID) (postgres.ReadableBook, error) {
	u.t.Error("an anonymous request reached the reader's store")
	return postgres.ReadableBook{}, nil
}

func (u untouchedReader) CanDownload(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	u.t.Error("an anonymous request reached the reader's store")
	return false, nil
}

func (u untouchedReader) RecordAccess(context.Context, uuid.UUID, uuid.UUID, postgres.AccessKind, int) error {
	u.t.Error("an anonymous request reached the reader's store")
	return nil
}

// A path that only looks like an open one.
//
// The router matches the path as it was ESCAPED, unescaping one segment at a
// time, so "%2F" inside a segment is part of that segment's value and "%2E%2E"
// is a segment literally named "..". The decoded path the gate used to read
// has turned both into real slashes and dot segments. Those two readings can
// disagree about which route a request is for, and the gate must never be the
// one that is wrong in the generous direction.
var smuggledPaths = []struct{ method, target string }{
	// %2F decoded into a slash, so the decoded path ends in "/start" like a
	// federated sign-in, while the router dispatches to a session's id.
	{http.MethodDelete, "/api/v1/auth/sessions/x%2Fstart"},
	{http.MethodDelete, "/api/v1/auth/sessions/x%2Fcallback"},
	// Plain paths, no escaping at all: the sign-in routes are GET with one
	// provider segment, and a session whose id is "start" is not one of them.
	{http.MethodDelete, "/api/v1/auth/sessions/start"},
	{http.MethodDelete, "/api/v1/auth/sessions/callback"},
	// %2E%2E is not a dot segment to the router, so nothing redirects it.
	{http.MethodGet, "/api/v1/auth/%2E%2E/books/0b8a8f3e-0000-4000-8000-000000000000/assets/start"},
	{http.MethodGet, "/api/v1/auth/x%2F..%2Fme%2Fstart/start"},
	{http.MethodGet, "/api/v1/auth/me%2F/start"},
	// The shape that walks a reader asset back up to the sign-in routes after
	// a proxy or a later layer decodes it once more.
	{http.MethodGet, "/api/v1/books/0b8a8f3e-0000-4000-8000-000000000000/assets/" +
		"q%252Fq%252Fq%252Fq%2F..%2F..%2F..%2F..%2Fauth%2Fz%252F..%252F..%252FOEBPS%252Fimg.jpg"},
	{http.MethodGet, "/api/v1/books/0b8a8f3e-0000-4000-8000-000000000000/assets/" +
		"x%2F..%2F..%2F..%2F..%2Fauth%2Fy%2Fstart"},
	// Paths that leave /api only once decoded, or never, and must not be
	// mistaken for the web client.
	{http.MethodGet, "/x/%2E%2E/api/v1/library"},
	{http.MethodGet, "/%2E%2E/api/v1/library"},
	{http.MethodGet, "/x%2F..%2Fapi/v1/library"},
	{http.MethodGet, `/x\..\api/v1/library`},
	{http.MethodGet, "/opds/subjects/a%2F..%2F..%2F..%2Fx"},
	{http.MethodGet, "/opd%73"},
}

// Through the real router: an anonymous request with a smuggled path is
// refused at the door, and nothing behind it is ever consulted.
func TestASmuggledPathIsRefusedByTheRealRouter(t *testing.T) {
	auth_, _, _ := tokenHandler(t)
	rt := &Router{
		Auth:     auth_,
		Reader:   &ReaderHandler{Store: untouchedReader{t: t}},
		OPDS:     &OPDSHandler{Store: &fakeCatalogue{}},
		Sessions: auth_.Sessions,
	}
	h := rt.Handler()

	for _, c := range smuggledPaths {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, rawRequest(t, c.method, c.target))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", c.method, c.target, rec.Code)
		}
	}
}

// At the gate itself, with every protected route the smuggled paths aim at
// standing behind it: none of them may be reached.
func TestASmuggledPathNeverReachesAProtectedHandler(t *testing.T) {
	mux := http.NewServeMux()
	for _, pattern := range []string{
		"GET /api/v1/books/{id}/assets/{path...}",
		"DELETE /api/v1/auth/sessions/{id}",
		"GET /api/v1/auth/me",
		"GET /api/v1/library",
		"GET /opds",
		"GET /opds/subjects/{slug}",
	} {
		mux.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {
			t.Errorf("an anonymous request reached %q", pattern)
		})
	}
	// The open routes and the web client answer, so a request the gate lets
	// through is visible as something other than a 401.
	for _, pattern := range []string{
		"GET /api/v1/auth/{provider}/start",
		"GET /api/v1/auth/{provider}/callback",
		"GET /",
	} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
	}

	guarded := RequireAuth(&auth.Sessions{Store: &sessionRecorder{
		hashes: map[string]auth.Session{}, devices: map[string]string{},
	}}, nil, mux)

	for _, c := range smuggledPaths {
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, rawRequest(t, c.method, c.target))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", c.method, c.target, rec.Code)
		}
	}
}

// The fix must not close the doors it exists to keep open: plain requests for
// the sign-in routes and the web client still arrive without a session.
func TestPlainOpenPathsStayOpen(t *testing.T) {
	var reached int
	guarded := RequireAuth(&auth.Sessions{Store: &sessionRecorder{
		hashes: map[string]auth.Session{}, devices: map[string]string{},
	}}, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))

	for _, c := range []struct{ method, target string }{
		{http.MethodGet, "/"},
		{http.MethodGet, "/signin"},
		{http.MethodGet, "/signin?error=that%20sign-in%20could%20not%20be%20verified"},
		{http.MethodGet, "/works/0b8a8f3e-0000-4000-8000-000000000000"},
		{http.MethodGet, "/collections/"},
		{http.MethodGet, "/_app/immutable/entry/start.abc123.js"},
		{http.MethodGet, "/api/v1/health"},
		{http.MethodGet, "/api/v1/auth/providers"},
		{http.MethodPost, "/api/v1/auth/login"},
		{http.MethodPost, "/api/v1/auth/token"},
		{http.MethodGet, "/api/v1/auth/oidc/start"},
		{http.MethodGet, "/api/v1/auth/oidc/callback?code=a%2Fb&state=x"},
	} {
		before := reached
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, rawRequest(t, c.method, c.target))
		if reached != before+1 {
			t.Errorf("%s %s was refused: status %d", c.method, c.target, rec.Code)
		}
	}
}

// An open route is open for the method it serves and no other, so a route
// added later on the same path under another method is protected by default.
func TestOpenRoutesAreOpenOnlyForTheirMethod(t *testing.T) {
	guarded := RequireAuth(&auth.Sessions{Store: &sessionRecorder{
		hashes: map[string]auth.Session{}, devices: map[string]string{},
	}}, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an anonymous request with the wrong method was let through")
	}))

	for _, c := range []struct{ method, target string }{
		{http.MethodPost, "/api/v1/health"},
		{http.MethodDelete, "/api/v1/auth/providers"},
		{http.MethodGet, "/api/v1/auth/login"},
		{http.MethodDelete, "/api/v1/auth/token"},
		{http.MethodPost, "/api/v1/auth/oidc/start"},
		{http.MethodPut, "/api/v1/auth/oidc/callback"},
		{http.MethodGet, "/api/v1/auth/start"},
		{http.MethodGet, "/api/v1/auth/a/b/start"},
	} {
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, rawRequest(t, c.method, c.target))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", c.method, c.target, rec.Code)
		}
	}

	// HEAD is GET's, as the router treats it.
	var reached bool
	head := RequireAuth(&auth.Sessions{Store: &sessionRecorder{
		hashes: map[string]auth.Session{}, devices: map[string]string{},
	}}, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	head.ServeHTTP(httptest.NewRecorder(), rawRequest(t, http.MethodHead, "/api/v1/health"))
	if !reached {
		t.Error("HEAD /api/v1/health was refused")
	}
}
