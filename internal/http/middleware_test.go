// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
)

// The floor is on every response, including the application document and the
// error pages that previously carried nothing. The app is an authenticated,
// self-hosted library: without these, any site could frame the UI and a
// sniffed response could be run as a script.
func TestSecureHeadersAreOnEveryResponse(t *testing.T) {
	h := SecureHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	for header, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("no Content-Security-Policy on the app shell")
	} else {
		// The vectors that do not depend on where scripts load from.
		for _, directive := range []string{"frame-ancestors 'none'", "base-uri 'self'", "object-src 'none'", "form-action 'self'"} {
			if !strings.Contains(csp, directive) {
				t.Errorf("CSP %q is missing %q", csp, directive)
			}
		}
	}
}

// A byte-serving endpoint returns untrusted book content and sets a far
// stricter CSP of its own. That must win: the global floor is a floor, not a
// ceiling, and a handler's Set replaces the header the middleware wrote.
func TestAHandlersOwnCSPOverridesTheFloor(t *testing.T) {
	const strict = "default-src 'none'; sandbox"
	h := SecureHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", strict)
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/books/x/assets/y.png", nil))

	if got := rec.Header().Get("Content-Security-Policy"); got != strict {
		t.Errorf("CSP = %q, want the handler's stricter %q", got, strict)
	}
	// The framing and sniffing protections are not something a content handler
	// sets, so they must survive alongside its stricter CSP.
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want it kept alongside the handler's CSP", got)
	}
}

// A panicking handler costs one request a 500 in the standard shape, not the
// server.
func TestRecoverAnswersAPanicWithA500(t *testing.T) {
	h := Recover(LogRequests(SecureHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("a bug")
	}))))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mounted/elsewhere", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body dto.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not the standard error shape: %v: %s", err, rec.Body)
	}
	if body.Error.Code != string(CodeInternal) {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeInternal)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want the floor on an error too", got)
	}
}

// The request log sits in front of streaming handlers, so it must not hide
// Flush from them, and it records the status the handler actually wrote.
func TestLogRequestsKeepsFlushAndTheStatus(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	h := LogRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flushing through the request log: %v", err)
		}
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mounted/elsewhere", nil))

	if !rec.Flushed {
		t.Error("the response was never flushed")
	}
	var line struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("reading the log line: %v: %s", err, buf.String())
	}
	if line.Method != http.MethodPost || line.Path != "/mounted/elsewhere" || line.Status != http.StatusAccepted {
		t.Errorf("logged %+v, want POST /mounted/elsewhere 202", line)
	}
}
