// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// archivedFileForAccountTest gives a leaver a book only they hold, deletes
// them, and returns the archived file and its recorded path.
func archivedFileForAccountTest(t *testing.T, p *Pool, admin uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	leaver := secondUser(t, p)
	in := owned(sampleInput(fmt.Sprintf("%064x", 0x71), "/library/left-behind.epub",
		"Left Behind", "An Author"), leaver)
	out, err := p.UpsertBookFile(t.Context(), in)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	archived, err := p.DeleteAccount(t.Context(), admin, leaver)
	if err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if len(archived) != 1 {
		t.Fatalf("archived = %+v, want the one book", archived)
	}
	return out.BookFileID, archived[0].Path
}

// Deleting an account that is not there, or on behalf of somebody who is not
// there, is not found - never a partial deletion.
func TestDeleteAccountOfNobody(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	admin := testOwner(t, p)
	ctx := t.Context()
	someone := secondUser(t, p)

	for _, tc := range []struct {
		name          string
		actor, target uuid.UUID
	}{
		{"no target", admin, uuid.Nil},
		{"a target that does not exist", admin, uuid.New()},
		{"an actor that does not exist", uuid.New(), someone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.DeleteAccount(ctx, tc.actor, tc.target); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("DeleteAccount: %v, want store.ErrNotFound", err)
			}
		})
	}
	if n := rowCount(t, p, `SELECT count(*) FROM users WHERE id = $1`, someone); n != 1 {
		t.Error("an unknown actor deleted somebody")
	}
}

// The archive is an administrator's tool. Anybody else is refused, and an
// account that does not exist is not found.
func TestTheArchiveIsForAdministrators(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	admin := testOwner(t, p)
	ctx := t.Context()
	fileID, _ := archivedFileForAccountTest(t, p, admin)
	reader := secondUser(t, p)

	if _, err := p.ArchivedBooks(ctx, reader); !errors.Is(err, store.ErrForbidden) {
		t.Errorf("ArchivedBooks(reader): %v, want store.ErrForbidden", err)
	}
	if _, err := p.ArchivedBooks(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ArchivedBooks(nobody): %v, want store.ErrNotFound", err)
	}
	if err := p.ClaimArchived(ctx, reader, fileID, reader); !errors.Is(err, store.ErrForbidden) {
		t.Errorf("ClaimArchived(reader): %v, want store.ErrForbidden", err)
	}
	if canRead(t, p, fileID, reader) {
		t.Error("a refused claim still put the book on a shelf")
	}
}

func TestClaimArchivedOnlyHandsOnWhatIsWaiting(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	admin := testOwner(t, p)
	ctx := t.Context()
	fileID, _ := archivedFileForAccountTest(t, p, admin)

	t.Run("to somebody who does not exist", func(t *testing.T) {
		if err := p.ClaimArchived(ctx, admin, fileID, uuid.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ClaimArchived: %v, want store.ErrNotFound", err)
		}
	})

	t.Run("a book nobody archived", func(t *testing.T) {
		live := oneBook(t, p, admin)
		other := secondUser(t, p)
		if err := p.ClaimArchived(ctx, admin, live, other); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ClaimArchived: %v, want store.ErrNotFound", err)
		}
		if canRead(t, p, live, other) {
			t.Error("a live book was handed to somebody through the archive")
		}
	})

	t.Run("a book already claimed", func(t *testing.T) {
		first := secondUser(t, p)
		if err := p.ClaimArchived(ctx, admin, fileID, first); err != nil {
			t.Fatalf("ClaimArchived: %v", err)
		}
		second := secondUser(t, p)
		if err := p.ClaimArchived(ctx, admin, fileID, second); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("claiming twice: %v, want store.ErrNotFound", err)
		}
		if !canRead(t, p, fileID, first) || canRead(t, p, fileID, second) {
			t.Error("the second claim changed who holds the book")
		}
	})
}

// Moving an archived file is the filesystem's half of archiving, done after
// the commit: it only happens to a file that is still archived and still where
// the archive left it, and the row follows the move.
func TestMoveArchived(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	admin := testOwner(t, p)
	ctx := t.Context()
	fileID, from := archivedFileForAccountTest(t, p, admin)

	refuse := func(t *testing.T) func(string) (string, error) {
		return func(string) (string, error) {
			t.Error("the file was moved")
			return "", nil
		}
	}

	t.Run("a file that no longer exists is left alone", func(t *testing.T) {
		to, err := p.MoveArchived(ctx, uuid.New(), from, refuse(t))
		if err != nil || to != "" {
			t.Errorf("MoveArchived = %q, %v; want nothing", to, err)
		}
	})

	t.Run("a file that has moved since is left alone", func(t *testing.T) {
		to, err := p.MoveArchived(ctx, fileID, "/somewhere/else.epub", refuse(t))
		if err != nil || to != "" {
			t.Errorf("MoveArchived = %q, %v; want nothing", to, err)
		}
	})

	t.Run("a file the mover declines stays put", func(t *testing.T) {
		to, err := p.MoveArchived(ctx, fileID, from, func(string) (string, error) { return "", nil })
		if err != nil || to != "" {
			t.Errorf("MoveArchived = %q, %v; want nothing", to, err)
		}
		if got, _ := p.BookFilePath(ctx, fmt.Sprintf("%064x", 0x71)); got != from {
			t.Errorf("path = %q, want unchanged %q", got, from)
		}
	})

	t.Run("a failed move records nothing", func(t *testing.T) {
		boom := errors.New("disk full")
		to, err := p.MoveArchived(ctx, fileID, from, func(string) (string, error) { return "/archive/x.epub", boom })
		if !errors.Is(err, boom) || to != "" {
			t.Errorf("MoveArchived = %q, %v; want the mover's error", to, err)
		}
		if got, _ := p.BookFilePath(ctx, fmt.Sprintf("%064x", 0x71)); got != from {
			t.Errorf("path = %q, want unchanged %q", got, from)
		}
	})

	t.Run("a move is recorded", func(t *testing.T) {
		to, err := p.MoveArchived(ctx, fileID, from, func(path string) (string, error) {
			if path != from {
				t.Errorf("asked to move %q, want %q", path, from)
			}
			return "/data/archive/left-behind.epub", nil
		})
		if err != nil || to != "/data/archive/left-behind.epub" {
			t.Fatalf("MoveArchived = %q, %v", to, err)
		}
		waiting, err := p.ArchivedBooks(ctx, admin)
		if err != nil {
			t.Fatalf("ArchivedBooks: %v", err)
		}
		if len(waiting) != 1 || waiting[0].Path != to || waiting[0].ArchivedPath != to {
			t.Errorf("archive = %+v, want it at %s", waiting, to)
		}
	})

	t.Run("a claimed file is not moved", func(t *testing.T) {
		if err := p.ClaimArchived(ctx, admin, fileID, admin); err != nil {
			t.Fatalf("ClaimArchived: %v", err)
		}
		to, err := p.MoveArchived(ctx, fileID, "/data/archive/left-behind.epub", refuse(t))
		if err != nil || to != "" {
			t.Errorf("MoveArchived = %q, %v; want nothing", to, err)
		}
	})
}
