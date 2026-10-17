// SPDX-License-Identifier: AGPL-3.0-or-later

// Package http holds handlers, middleware, DTOs, and routing. It knows HTTP.
// It must not know SQL, pgx, or file formats.
package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
)

// ErrorCode is a stable, machine-readable error identifier the client may
// branch on. Messages are for humans and may change freely; codes may not.
type ErrorCode string

const (
	CodeNotFound ErrorCode = "not_found"
	CodeInvalid  ErrorCode = "invalid_request"
	CodeConflict ErrorCode = "conflict"
	CodeInternal ErrorCode = "internal_error"
	// CodeLLMOffline is a 503: the chat model is busy, slow or switched off,
	// and asking again later may work. Only answers are affected - the
	// library and the reader never depend on the model.
	CodeLLMOffline ErrorCode = "llm_unavailable"
	// CodeModelError is a 502: the chat model server refused the request
	// itself, which is a configuration problem for whoever runs it.
	CodeModelError ErrorCode = "model_error"
)

// The wire shape lives in dto.Error, so the generated API spec and the
// TypeScript client describe the error every endpoint can return.
// respondJSON writes v as JSON with the given status.
func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent; all we can do is record it.
		slog.Error("encoding response", "error", err)
	}
}

// respondError writes the canonical error shape. Sentinel errors are mapped to
// status codes here and nowhere else.
func respondError(w http.ResponseWriter, err error) {
	f := describe(err)
	if f.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(f.retryAfter)))
	}
	respondJSON(w, f.status, dto.Error{Error: f.detail()})
}

// failure is an error as the client is told it.
type failure struct {
	status     int
	code       ErrorCode
	message    string
	retryAfter time.Duration
}

func (f failure) detail() dto.ErrorDetail {
	return dto.ErrorDetail{Code: string(f.code), Message: f.message}
}

// describe decides what the client is told about err. It is the one mapping,
// shared by respondError and by a stream that fails after its headers are
// sent and can only say so in an event.
func describe(err error) failure {
	var he *httpError
	var pe *PolicyError
	switch {
	case errors.As(err, &he):
		return failure{status: he.status, code: he.code, message: he.message}
	case errors.As(err, &pe):
		he = pe.asHTTPError()
		return failure{status: he.status, code: he.code, message: he.message}
	}
	if f, ok := describeModelFailure(err); ok {
		return f
	}
	// Unexpected errors are logged in full and reported vaguely, so internal
	// detail never leaks to the client.
	slog.Error("unhandled error", "error", err)
	return failure{status: http.StatusInternalServerError, code: CodeInternal,
		message: "an unexpected error occurred"}
}

type httpError struct {
	status  int
	code    ErrorCode
	message string
	cause   error
}

func (e *httpError) Error() string { return e.message }
func (e *httpError) Unwrap() error { return e.cause }

// NotFound reports a missing resource.
func NotFound(what string) error {
	return &httpError{status: http.StatusNotFound, code: CodeNotFound, message: what + " not found"}
}

// Invalid reports a malformed or unacceptable request.
func Invalid(message string, cause error) error {
	return &httpError{status: http.StatusBadRequest, code: CodeInvalid, message: message, cause: cause}
}
