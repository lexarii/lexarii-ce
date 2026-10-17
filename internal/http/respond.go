// SPDX-License-Identifier: AGPL-3.0-or-later

// Package http holds handlers, middleware, DTOs, and routing. It knows HTTP.
// It must not know SQL, pgx, or file formats.
package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
)

// ErrorCode is a stable, machine-readable error identifier the client may
// branch on. Messages are for humans and may change freely; codes may not.
type ErrorCode string

const (
	CodeNotFound   ErrorCode = "not_found"
	CodeInvalid    ErrorCode = "invalid_request"
	CodeConflict   ErrorCode = "conflict"
	CodeInternal   ErrorCode = "internal_error"
	CodeLLMOffline ErrorCode = "llm_unavailable"
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
	status, code := http.StatusInternalServerError, CodeInternal
	message := "an unexpected error occurred"

	var he *httpError
	var pe *PolicyError
	if errors.As(err, &he) {
		status, code, message = he.status, he.code, he.message
	} else if errors.As(err, &pe) {
		he = pe.asHTTPError()
		status, code, message = he.status, he.code, he.message
	} else {
		// Unexpected errors are logged in full and reported vaguely, so internal
		// detail never leaks to the client.
		slog.Error("unhandled error", "error", err)
	}

	respondJSON(w, status, dto.Error{Error: dto.ErrorDetail{
		Code:    string(code),
		Message: message,
	}})
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
