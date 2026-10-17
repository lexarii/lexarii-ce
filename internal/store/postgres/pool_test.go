// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// testPool opens a pool against TEST_DATABASE_URL, skipping when it is unset so
// `go test ./...` stays runnable without a database.
func testPool(t *testing.T) *Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	pool, err := Open(t.Context(), url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	lockTestDB(t, pool)
	return pool
}

// Test packages that touch the database run concurrently by default, and each
// one truncates shared tables in setup. A session-level advisory lock serialises
// them without relying on anyone remembering `go test -p 1`.
const testDBLockKey = 0x5348454c464d4b01

// lockTestDB holds the shared test lock for the duration of one test.
//
// The lock is session-scoped, so it must be taken on a single dedicated
// connection rather than through the pool, which would hand the unlock to a
// different connection than the lock.
func lockTestDB(t *testing.T, pool *Pool) {
	t.Helper()

	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquiring a connection for the test lock: %v", err)
	}
	if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_lock($1)", int64(testDBLockKey)); err != nil {
		conn.Release()
		t.Fatalf("taking the test lock: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(testDBLockKey)); err != nil {
			t.Logf("releasing the test lock: %v", err)
		}
		conn.Release()
	})
}

func TestOpenRejectsBadURL(t *testing.T) {
	_, err := Open(t.Context(), "not-a-url")
	if err == nil {
		t.Fatal("Open accepted a malformed URL")
	}
}

func TestPing(t *testing.T) {
	pool := testPool(t)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// The schema must be present and complete: a missing table here means a
// migration did not run, which is a far more useful failure than a confusing
// error on the first real query.
func TestSchemaIsMigrated(t *testing.T) {
	pool := testPool(t)

	want := []string{
		"authors", "works", "editions", "book_files", "tags", "collections",
		"chunks", "embeddings", "work_embeddings", "annotations",
		"reading_progress", "jobs", "conversations", "messages", "citations",
		"enrichment_records", "users", "sessions",
	}
	for _, table := range want {
		var exists bool
		err := pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = $1)`,
			table,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("checking %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %q missing - migrations not applied?", table)
		}
	}
}

func TestExtensionsInstalled(t *testing.T) {
	pool := testPool(t)

	for _, ext := range []string{"vector", "pg_trgm"} {
		var exists bool
		err := pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)`, ext,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("checking extension %s: %v", ext, err)
		}
		if !exists {
			t.Errorf("extension %q missing", ext)
		}
	}
}

// Re-ingesting a file must be a no-op enforced by the database rather than by a
// racy read-then-write in application code.
func TestDuplicateContentHashMapsToConflict(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback-always; the test never commits

	var workID, editionID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO works (title, sort_title) VALUES ('Dup Test', 'Dup Test') RETURNING id`,
	).Scan(&workID); err != nil {
		t.Fatalf("inserting work: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO editions (work_id) VALUES ($1) RETURNING id`, workID,
	).Scan(&editionID); err != nil {
		t.Fatalf("inserting edition: %v", err)
	}

	hash := strings.Repeat("b", 64)
	insert := `INSERT INTO book_files (edition_id, path, content_hash, format, size_bytes)
	           VALUES ($1, $2, $3, 'epub', 1024)`

	if _, err := tx.Exec(ctx, insert, editionID, "/library/one.epub", hash); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err = tx.Exec(ctx, insert, editionID, "/library/moved-and-renamed.epub", hash)
	if mapped := mapError(err); !errors.Is(mapped, store.ErrConflict) {
		t.Fatalf("duplicate hash mapped to %v, want store.ErrConflict", mapped)
	}
}

func TestNoRowsMapsToNotFound(t *testing.T) {
	pool := testPool(t)

	var id string
	err := pool.QueryRow(t.Context(),
		`SELECT id FROM works WHERE id = '00000000-0000-0000-0000-000000000000'`,
	).Scan(&id)

	if mapped := mapError(err); !errors.Is(mapped, store.ErrNotFound) {
		t.Fatalf("missing row mapped to %v, want store.ErrNotFound", mapped)
	}
}

// A check constraint must surface as a check violation rather than as an opaque
// driver error, so the cause is obvious in logs.
func TestCheckConstraintIsIdentified(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback-always; the test never commits

	_, err = tx.Exec(ctx, `INSERT INTO works (title, sort_title) VALUES ('   ', 'blank')`)
	mapped := mapError(err)
	if mapped == nil {
		t.Fatal("blank title accepted")
	}
	if !strings.Contains(mapped.Error(), "check constraint") {
		t.Fatalf("error %q does not identify the check constraint", mapped)
	}
}

func TestOpenFailsWhenPostgresIsUnreachable(t *testing.T) {
	p, err := Open(t.Context(), "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	if err == nil {
		p.Close()
		t.Fatal("Open succeeded against a port nothing listens on")
	}
}

func TestPingFailsOnAClosedPool(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	p, err := Open(t.Context(), url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p.Close()
	if err := p.Ping(t.Context()); err == nil {
		t.Fatal("Ping succeeded on a closed pool")
	}
}

// A reference to a row that is not there reads as "not found" above the
// store, and an error that is not Postgres's own passes through untouched.
func TestMapErrorTranslatesForeignKeysAndPassesOthersThrough(t *testing.T) {
	p := testPool(t)

	_, err := p.CreateCollection(t.Context(), uuid.New(), "Orphan", "", nil)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a collection for an absent user: err = %v, want ErrNotFound", err)
	}

	if mapError(nil) != nil {
		t.Error("mapError(nil) is not nil")
	}
	if got := mapError(context.Canceled); !errors.Is(got, context.Canceled) {
		t.Errorf("mapError(context.Canceled) = %v, lost its identity", got)
	}
}

func TestVectorDimensionsReadsTheColumnWidth(t *testing.T) {
	p := testPool(t)
	dims, err := p.VectorDimensions(t.Context())
	if err != nil {
		t.Fatalf("VectorDimensions: %v", err)
	}
	if dims != 768 {
		t.Fatalf("VectorDimensions = %d, want 768", dims)
	}
}
