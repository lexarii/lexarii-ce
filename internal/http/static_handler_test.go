// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The embedded client is mounted under everything else, and must never be
// mistaken for the API.
func TestTheClientShellAndTheAPIDoNotOverlap(t *testing.T) {
	rt := &Router{}

	t.Run("an unknown API path is the API's 404, not the app shell", func(t *testing.T) {
		rec := serveAsViewer(t, rt, uuid.Nil, http.MethodGet, "/api/v1/no-such-endpoint", "")
		if rec.Code != http.StatusNotFound || wireCodeOf(t, rec) != string(CodeNotFound) {
			t.Fatalf("status = %d, body = %s; want a JSON 404", rec.Code, rec.Body)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("content type = %q; a typo'd endpoint must not look like it worked", ct)
		}
	})

	for _, path := range []string{"/", "/works/0b8a8f3e-0000-4000-8000-000000000000", "/read/abc?spine=3"} {
		t.Run("the shell at "+path, func(t *testing.T) {
			rec := serveAsViewer(t, rt, uuid.Nil, http.MethodGet, path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if !strings.HasPrefix(strings.ToLower(rec.Body.String()), "<!doctype html") {
				t.Errorf("body is not the app shell: %.80q", rec.Body.String())
			}
			if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
				t.Errorf("the shell was marked immutable: %q", cc)
			}
		})
	}
}
