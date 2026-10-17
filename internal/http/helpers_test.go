// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// quietLogger keeps handler logs out of test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeAccounts is the admin check, without a database.
type fakeAccounts struct{ admin bool }

func (f fakeAccounts) User(context.Context, uuid.UUID) (postgres.UserRecord, error) {
	return postgres.UserRecord{ID: uuid.New(), Username: "tester", IsAdmin: f.admin}, nil
}

var (
	anAdmin = fakeAccounts{admin: true}
	aReader = fakeAccounts{admin: false}
)

// asUser puts a signed-in user on the request, which every guarded endpoint
// needs before it can get as far as checking whether they are an admin.
func asUser(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userContextKey{}, uuid.New()))
}

// asUserID puts a particular signed-in user on the request, for tests about
// what one person may see of another's.
func asUserID(r *http.Request, id uuid.UUID) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userContextKey{}, id))
}
