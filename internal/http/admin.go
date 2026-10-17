// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// The library is shared and reading is private: works, files, tags and chunks
// carry no user, while annotations, reading position and conversations do. That
// much the schema settled long ago.
//
// What it did not settle is who may CHANGE the shared half. `users.is_admin`
// has existed since the second migration and was populated correctly, and
// nothing ever checked it - so on a four-account installation, every reader
// could upload files, trigger a rescan, spend the model's time on enrichment,
// and accept metadata suggestions that rewrite what everyone else sees. The
// flag was decoration.
//
// Admin is required for changing the shared library. It is NOT required for
// reading, annotating, tracking position, or asking questions: those are the
// point of having an account, and they only ever affect the person doing them.

// AdminStore is the account lookup the guard needs.
type AdminStore interface {
	User(ctx context.Context, id uuid.UUID) (postgres.UserRecord, error)
}

// adminOrForbidden returns the signed-in user's id, having checked they may
// change the shared library.
//
// The lookup is a database read per call, which is affordable because every
// endpoint behind this guard is rare and already expensive: uploading files,
// rescanning a library, or running a model over a book. Caching the flag in the
// session would make revoking someone's admin rights take effect whenever they
// next signed in, rather than immediately, which is the wrong way round for a
// permission.
func adminOrForbidden(w http.ResponseWriter, r *http.Request, store AdminStore) (uuid.UUID, bool) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return uuid.Nil, false
	}
	if store == nil {
		// No account store wired up means this installation has no accounts to
		// check against, and refusing everything would be worse than the
		// alternative only if authentication were also off - which it is not,
		// because userOrUnauthorised has already passed.
		return userID, true
	}

	user, err := store.User(r.Context(), userID)
	if err != nil {
		respondError(w, err)
		return uuid.Nil, false
	}
	if !user.IsAdmin {
		respondError(w, &httpError{
			status: http.StatusForbidden,
			code:   "not_an_admin",
			// Named so the person knows what to ask for rather than assuming
			// something is broken.
			message: "only an administrator can change the shared library",
		})
		return uuid.Nil, false
	}
	return userID, true
}
