// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

const localUserPassword = "a long enough password"

// freshUsername returns a username no other test uses, and removes whatever
// account ends up with it when the test is over.
func freshUsername(t *testing.T, p *Pool) string {
	t.Helper()
	name := "tx-" + uuid.NewString()[:8]
	t.Cleanup(func() {
		if _, err := p.Exec(context.Background(), `DELETE FROM users WHERE username = $1`, name); err != nil {
			t.Logf("removing %s: %v", name, err)
		}
	})
	return name
}

func userCount(t *testing.T, p *Pool, username string) int {
	t.Helper()
	var n int
	if err := p.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE username = $1`, username).Scan(&n); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	return n
}

// Committed with the caller's own row: both are there, and the account can
// sign in with the password it was given.
func TestCreateLocalUserTxCommitsWithTheCallersRows(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	name := freshUsername(t, p)

	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	userID, err := CreateLocalUserTx(ctx, tx, name, "", localUserPassword, false)
	if err != nil {
		t.Fatalf("CreateLocalUserTx: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO collections (user_id, name) VALUES ($1, 'To read')`, userID); err != nil {
		t.Fatalf("the caller's own row: %v", err)
	}

	// Not visible outside the transaction until it commits.
	if n := userCount(t, p, name); n != 0 {
		t.Fatalf("the account is visible before the caller committed (%d rows)", n)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	identity, hash, err := p.LocalIdentity(ctx, name)
	if err != nil {
		t.Fatalf("LocalIdentity: %v", err)
	}
	if identity.Subject != name || !auth.VerifyPassword(localUserPassword, hash) {
		t.Errorf("the account cannot sign in: subject %q", identity.Subject)
	}
	var collections int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM collections WHERE user_id = $1`, userID).Scan(&collections); err != nil {
		t.Fatalf("counting collections: %v", err)
	}
	if collections != 1 {
		t.Errorf("collections = %d, want the caller's one", collections)
	}
}

// Rolled back by the caller: no account, no identity, nothing half made.
func TestCreateLocalUserTxRollsBackWithTheCaller(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	name := freshUsername(t, p)

	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := CreateLocalUserTx(ctx, tx, name, "", localUserPassword, true); err != nil {
		t.Fatalf("CreateLocalUserTx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if n := userCount(t, p, name); n != 0 {
		t.Errorf("%d accounts survived the rollback", n)
	}
	if _, _, err := p.LocalIdentity(ctx, name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LocalIdentity after rollback: %v, want store.ErrNotFound", err)
	}
}

// A refused password is refused before the transaction is touched, so the
// caller's transaction is still usable afterwards.
func TestCreateLocalUserTxRefusesAWeakPasswordWithoutSpoilingTheTransaction(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	name := freshUsername(t, p)

	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // the test never commits

	if _, err := CreateLocalUserTx(ctx, tx, name, "", "short", false); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("a short password: %v, want auth.ErrWeakPassword", err)
	}
	if _, err := CreateLocalUserTx(ctx, tx, name, "", localUserPassword, false); err != nil {
		t.Fatalf("the transaction was spoiled by the refusal: %v", err)
	}
}

// A taken username is a conflict, through either door.
func TestCreateLocalUserReportsATakenUsername(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	name := freshUsername(t, p)

	if _, err := p.CreateLocalUser(ctx, name, "", localUserPassword, false); err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	if _, err := p.CreateLocalUser(ctx, name, "", localUserPassword, false); !errors.Is(err, store.ErrConflict) {
		t.Errorf("CreateLocalUser with a taken name: %v, want store.ErrConflict", err)
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // the test never commits
	if _, err := CreateLocalUserTx(ctx, tx, name, "", localUserPassword, false); !errors.Is(err, store.ErrConflict) {
		t.Errorf("CreateLocalUserTx with a taken name: %v, want store.ErrConflict", err)
	}
}
