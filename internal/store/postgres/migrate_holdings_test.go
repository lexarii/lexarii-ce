// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"scm.poniatowski.dev/adam/lexarii/migrations"
)

// Migrations 33 to 35 against a library as version 32 left it, up, down and up
// again. Run on a database of its own, created and dropped here, because the
// shared test database is always at the latest version.
//
// What it pins: an owned file gains its owner as holder; a file nobody owned
// is archived under a marker rather than left on nobody's shelf, and its grant
// - made by nobody who could answer for it - is dropped; Down restores the
// owner and leaves that file archived; and two editions sharing an ISBN, which
// migration 35 allows, do not stop Down from restoring its uniqueness.
func TestHoldingsMigrationsRoundTrip(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := t.Context()

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := "lexarii_migration_" + uuid.NewString()[:8]
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parsing the URL: %v", err)
	}
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("opening the scratch database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatalf("goose: %v", err)
	}
	if _, err := provider.UpTo(ctx, 32); err != nil {
		t.Fatalf("up to 32: %v", err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	one := func(q string, args ...any) string {
		t.Helper()
		var out sql.NullString
		if err := db.QueryRowContext(ctx, q, args...).Scan(&out); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return out.String
	}

	alice, bob := uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, username, is_admin) VALUES ($1, 'alice', true), ($2, 'bob', false)`, alice, bob)
	file := func(n int, owner *uuid.UUID) uuid.UUID {
		work, edition, id := uuid.New(), uuid.New(), uuid.New()
		exec(`INSERT INTO works (id, title, sort_title) VALUES ($1, 'W', 'W')`, work)
		exec(`INSERT INTO editions (id, work_id) VALUES ($1, $2)`, edition, work)
		exec(`INSERT INTO book_files (id, edition_id, path, content_hash, format, size_bytes, owner_id)
		      VALUES ($1, $2, $3, $4, 'epub', 10, $5)`,
			id, edition, fmt.Sprintf("/l/%d.epub", n), fmt.Sprintf("%064x", n), owner)
		return id
	}
	owned := file(1, &alice)
	ownerless := file(2, nil)
	exec(`INSERT INTO book_access (book_file_id, user_id, granted_by, can_download) VALUES ($1, $2, NULL, true)`,
		ownerless, bob)

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	if got := one(`SELECT user_id::text FROM book_file_holders WHERE book_file_id = $1`, owned); got != alice.String() {
		t.Errorf("owned file holder = %q, want alice", got)
	}
	if got := one(`SELECT archived_owner FROM book_files WHERE id = $1 AND archived_at IS NOT NULL`, ownerless); got == "" {
		t.Error("the ownerless file was not archived under a marker")
	}
	if got := one(`SELECT count(*)::text FROM book_access WHERE book_file_id = $1`, ownerless); got != "0" {
		t.Errorf("the ownerless file kept %s grants", got)
	}
	if got := one(`SELECT count(*)::text FROM readable_book_files WHERE book_file_id = $1`, ownerless); got != "0" {
		t.Error("an archived file is readable")
	}
	// Two editions with one ISBN, as two people's copies of one book may be.
	exec(`UPDATE editions SET isbn13 = '9780131103627'`)

	if _, err := provider.DownTo(ctx, 32); err != nil {
		t.Fatalf("down to 32: %v", err)
	}
	if got := one(`SELECT owner_id::text FROM book_files WHERE id = $1`, owned); got != alice.String() {
		t.Errorf("owner after down = %q, want alice", got)
	}
	if got := one(`SELECT (archived_at IS NOT NULL)::text FROM book_files WHERE id = $1`, ownerless); got != "true" {
		t.Error("the ownerless file came out of the archive on the way down")
	}
	if got := one(`SELECT count(*)::text FROM editions WHERE isbn13 IS NOT NULL`); got != "1" {
		t.Errorf("%s editions keep the ISBN after down, want 1", got)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
