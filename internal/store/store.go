// SPDX-License-Identifier: AGPL-3.0-or-later

// Package store holds repository interfaces and their Postgres implementations.
// It knows SQL. It must not know HTTP status codes or business rules.
package store

import "errors"

// Sentinel errors live in the package that owns the concept. Callers assert with
// errors.Is, never on error strings.
var (
	// ErrNotFound reports that a requested row does not exist.
	ErrNotFound = errors.New("not found")

	// ErrConflict reports a uniqueness violation: a duplicate content hash, a
	// username already taken, a job already queued for the same target.
	ErrConflict = errors.New("conflict")

	// ErrInvalidQuery reports a malformed request the store rejects before
	// touching the database: an unknown sort key, a corrupt pagination cursor.
	// It is the caller's mistake, so handlers map it to 400 rather than 500.
	ErrInvalidQuery = errors.New("invalid query")

	// ErrForbidden reports a caller who exists, is known, and may not do this.
	//
	// Deliberately rare. Where an object is somebody else's - a book, a share, a
	// note - the store answers ErrNotFound instead, because "you may not touch
	// this" confirms that it is there. This is for the cases where the thing
	// being refused is not a secret: administering the installation, and
	// deleting somebody else's account. Both are about the ACTOR's standing
	// rather than about hiding an object.
	ErrForbidden = errors.New("forbidden")

	// ErrBlocked reports bytes an administrator has removed from the
	// installation and blocked from coming back. It is about the bytes, not
	// about who else has or had them, and nothing derived from it may say
	// more.
	ErrBlocked = errors.New("content blocked")

	// ErrBusy reports work that kept meeting concurrent changes and stopped
	// rather than retry for ever. Nothing was changed; asking again later is
	// the remedy.
	ErrBusy = errors.New("busy; try again")
)
