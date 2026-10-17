// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/migrations"
)

// latestMigrationVersion reads the highest version number embedded in the
// binary, which is what Migrate must leave a database at.
func latestMigrationVersion(t *testing.T) int64 {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("reading embedded migrations: %v", err)
	}
	var latest int64
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			continue
		}
		latest = max(latest, v)
	}
	if latest == 0 {
		t.Fatal("no migrations embedded")
	}
	return latest
}

// freshMigrationDatabase creates an empty, uniquely named database and drops
// it when the test ends. It needs ADMIN_DATABASE_URL, a connection allowed to
// create databases.
func freshMigrationDatabase(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("ADMIN_DATABASE_URL")
	if admin == "" {
		t.Skip("ADMIN_DATABASE_URL not set")
	}

	name := "lx_migrate_test_" + strings.ReplaceAll(uuid.NewString()[:13], "-", "")
	conn, err := pgx.Connect(t.Context(), admin)
	if err != nil {
		t.Fatalf("connecting as administrator: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		c, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Errorf("connecting to drop %s: %v", name, err)
			return
		}
		defer func() { _ = c.Close(ctx) }()
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parsing ADMIN_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// One binary and a DATABASE_URL: an empty database is brought to the latest
// schema at startup, and starting again finds nothing to do.
func TestMigrateBringsAnEmptyDatabaseToTheLatestSchema(t *testing.T) {
	dbURL := freshMigrationDatabase(t)
	ctx := t.Context()

	if err := Migrate(ctx, dbURL, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	p, err := Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	version := func() int64 {
		t.Helper()
		var v int64
		if err := p.QueryRow(ctx,
			`SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&v); err != nil {
			t.Fatalf("reading the schema version: %v", err)
		}
		return v
	}
	want := latestMigrationVersion(t)
	if got := version(); got != want {
		t.Fatalf("schema version = %d, want %d", got, want)
	}
	// The schema is usable, not merely recorded: the vector column is there
	// at the width the binary expects.
	if dims, err := p.VectorDimensions(ctx); err != nil || dims != 768 {
		t.Fatalf("VectorDimensions = %d, %v", dims, err)
	}

	// A second start is a no-op.
	if err := Migrate(ctx, dbURL, nil); err != nil {
		t.Fatalf("Migrate (again): %v", err)
	}
	if got := version(); got != want {
		t.Fatalf("schema version after a second run = %d, want %d", got, want)
	}
	var rows int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id = $1`, want).Scan(&rows); err != nil {
		t.Fatalf("counting version rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("the latest migration was recorded %d times", rows)
	}
}

func TestMigrateReportsAnUnreachableDatabase(t *testing.T) {
	err := Migrate(t.Context(), "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2", nil)
	if err == nil {
		t.Fatal("Migrate succeeded against a port nothing listens on")
	}
}
