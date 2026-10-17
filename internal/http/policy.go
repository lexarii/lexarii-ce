// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// AuthorizeFunc applies an installation's own access policy to a request whose
// user has already been identified.
//
// Authentication answers "who is this"; this answers "may they, right now".
// It is the place for rules Lexarii has no opinion on - a read-only
// maintenance window, an account suspended without being deleted, a policy
// that depends on something outside Lexarii - without re-implementing sign-in
// to get at the user.
//
// It is called exactly once per protected request, after the session, device
// token, Basic credentials or trusted proxy have identified userID, and never
// on an open route. Return nil to let the request through. Return a
// *PolicyError to refuse it with that status and code. Any other error refuses
// it with a 500: a policy that could not be evaluated is not a policy that
// allowed the request.
type AuthorizeFunc func(ctx context.Context, r *http.Request, userID uuid.UUID) error

// PolicyError is a refusal from an AuthorizeFunc, written to the client in the
// standard error shape: {"error":{"code":Code,"message":Message}}.
//
// Status should be a 4xx or 5xx code. Anything else is answered 403, so a
// mistake in a policy still refuses rather than appearing to succeed. An empty
// Code becomes "forbidden" and an empty Message a generic refusal.
//
// A nil *PolicyError returned as a non-nil error - the typed-nil mistake, as
// in `var pe *PolicyError; return pe` - is a refusal with a 500, not
// permission: reading it as "no error" would let a request through on a
// policy bug.
type PolicyError struct {
	Status  int
	Code    string
	Message string
}

func (e *PolicyError) Error() string {
	if e == nil {
		return "nil policy error"
	}
	if e.Message != "" {
		return e.Message
	}
	return "refused by policy"
}

// asHTTPError turns a policy refusal into the error respondError writes.
func (e *PolicyError) asHTTPError() *httpError {
	if e == nil {
		// See the type's comment: a typed nil refuses, it never allows.
		return &httpError{status: http.StatusInternalServerError, code: CodeInternal,
			message: "an unexpected error occurred"}
	}
	status := e.Status
	if status < 400 || status > 599 {
		status = http.StatusForbidden
	}
	code := e.Code
	if code == "" {
		code = "forbidden"
	}
	message := e.Message
	if message == "" {
		message = "this request is not allowed"
	}
	return &httpError{status: status, code: ErrorCode(code), message: message, cause: e}
}

// authorize runs the policy, if there is one, and answers the request itself
// when it refuses. It reports whether the caller should carry on.
func authorize(w http.ResponseWriter, r *http.Request, policy AuthorizeFunc, userID uuid.UUID) bool {
	if policy == nil {
		return true
	}
	err := policy(r.Context(), r, userID)
	if err == nil {
		return true
	}
	// A *PolicyError is written with its own status and code; anything else is
	// logged in full by respondError and answered 500.
	var pe *PolicyError
	if errors.As(err, &pe) && pe == nil {
		slog.Error("access policy returned a nil *PolicyError as an error; refusing",
			"user", userID, "path", r.URL.Path)
	}
	respondError(w, err)
	return false
}
