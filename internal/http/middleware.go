// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"log/slog"
	"net/http"
	"time"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the wrapped writer to http.ResponseController.
//
// Without it this wrapper hides Flush from everything downstream, and SSE
// silently never flushes: the client receives nothing until the handler
// returns, which for a progress stream is exactly never.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// LogRequests logs one line per request through slog's default logger: the
// method, the decoded path, the status written and how long it took.
//
// Exported, like Recover and SecureHeaders, for code that mounts handlers of
// its own beside the Router and wants them to behave the same way. Router
// applies all three to everything it serves, in the order
// Recover(LogRequests(SecureHeaders(h))); a caller wrapping its own handler
// should use the same order, with Recover outermost so that a panic anywhere
// inside is caught.
//
// The writer it passes on still supports http.ResponseController, so a
// streaming handler behind it can flush.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"durationMs", time.Since(start).Milliseconds(),
		)
	})
}

// SecureHeaders sets response headers that harden the whole surface, on
// every response including the app shell and error pages: no framing, no
// content sniffing, a same-origin referrer, and a Content-Security-Policy
// floor. A handler that sets its own Content-Security-Policy replaces the
// floor with it.
//
// The byte-serving endpoints - covers, reader assets, PDF pages - set their
// own stricter Content-Security-Policy because they return untrusted book
// content, and a handler's own Set REPLACES what is put here. This is the
// floor under everything else, which previously had nothing: the application
// document, served at "/", carried no framing or content-type protection at
// all, so any site could frame the authenticated UI and a sniffed response
// could be treated as a script.
//
// What is deliberately NOT set here is a script-src / style-src policy. The
// client is a SvelteKit SPA whose hydration injects inline scripts, and an
// inline theme script runs in the document head before paint; a blind
// script-src would break both. The CSP set here therefore closes the vectors
// that do NOT depend on where scripts may load from - framing, <base>
// injection, plugin objects, and form hijacking - and a full script-src
// policy is left as a separate change that has to be built with SvelteKit's
// own csp hashing and verified in a browser rather than shipped blind.
func SecureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Clickjacking. frame-ancestors is the modern control and X-Frame-
		// Options is the one older readers and embedded webviews still obey;
		// nothing in this app frames anything, so both can be absolute.
		h.Set("X-Frame-Options", "DENY")
		// Do not let a response be reinterpreted as a type it did not declare.
		h.Set("X-Content-Type-Options", "nosniff")
		// A library says what somebody reads. The full path of a book or a
		// search should not travel to a third-party image or link target.
		h.Set("Referrer-Policy", "same-origin")
		// Belt-and-braces alongside frame-ancestors, plus base-uri so an
		// injected <base> cannot repoint every relative URL, object-src so no
		// plugin can run, and form-action so a form cannot post credentials
		// off-origin. No script-src or style-src: see the doc comment.
		h.Set("Content-Security-Policy",
			"frame-ancestors 'none'; base-uri 'self'; object-src 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// Recover keeps one panicking handler from taking down the server. The panic
// is logged and the client gets a 500 in the standard error shape. Panics are
// still bugs; this only stops them being fatal ones.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic in handler", "path", r.URL.Path, "panic", v)
				respondError(w, &httpError{
					status:  http.StatusInternalServerError,
					code:    CodeInternal,
					message: "an unexpected error occurred",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
