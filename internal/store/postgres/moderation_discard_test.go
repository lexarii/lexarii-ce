// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// A removal on behalf of nobody, or of an account that does not exist, is
// not found and changes nothing.
func TestRemovalByNobodyIsNotFound(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice := secondUser(t, p)
	out, err := p.UpsertBookFile(ctx, uploadInput(fmt.Sprintf("%064x", 0xd7), "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	for _, tc := range []struct {
		name        string
		file, admin uuid.UUID
	}{
		{"no administrator", out.BookFileID, uuid.Nil},
		{"no file", uuid.Nil, testOwner(t, p)},
		{"an administrator who does not exist", out.BookFileID, uuid.New()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.RemoveBookFileForEveryone(ctx, tc.file, tc.admin, "a reason", RemovalOptions{}); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("RemoveBookFileForEveryone: %v, want store.ErrNotFound", err)
			}
		})
	}
	if !canRead(t, p, out.BookFileID, alice) || rowCount(t, p, `SELECT count(*) FROM blocked_content`) != 0 {
		t.Error("a refused removal changed something")
	}
}

// A refused upload's own copy is deleted only while its bytes are blocked; if
// the block was lifted meanwhile the copy may be somebody's upload now and is
// left alone.
func TestDiscardBlockedUpload(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, alice := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xd8)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	t.Run("bytes that are not blocked are not touched", func(t *testing.T) {
		called := false
		if err := p.DiscardBlockedUpload(ctx, fmt.Sprintf("%064x", 0xd9), func() error { called = true; return nil }); err != nil {
			t.Fatalf("DiscardBlockedUpload: %v", err)
		}
		if called {
			t.Error("a copy of bytes nobody blocked was deleted")
		}
	})

	if _, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "gone", RemovalOptions{}); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if err := p.DiscardBlockedCopies(ctx, hash, func(StoredCopies) error { return nil }); err != nil {
		t.Fatalf("DiscardBlockedCopies: %v", err)
	}

	t.Run("blocked bytes are deleted, and nothing is left to finish", func(t *testing.T) {
		called := false
		if err := p.DiscardBlockedUpload(ctx, hash, func() error { called = true; return nil }); err != nil {
			t.Fatalf("DiscardBlockedUpload: %v", err)
		}
		if !called {
			t.Error("the refused upload's copy was not deleted")
		}
		pending, err := p.BlocksWithCopies(ctx)
		if err != nil {
			t.Fatalf("BlocksWithCopies: %v", err)
		}
		if len(pending) != 0 {
			t.Errorf("a successful deletion left work for startup: %v", pending)
		}
	})

	t.Run("lifted meanwhile, the copy stays", func(t *testing.T) {
		if err := p.Unblock(ctx, admin, hash); err != nil {
			t.Fatalf("Unblock: %v", err)
		}
		called := false
		if err := p.DiscardBlockedUpload(ctx, hash, func() error { called = true; return nil }); err != nil {
			t.Fatalf("DiscardBlockedUpload: %v", err)
		}
		if called {
			t.Error("a copy was deleted after its block was lifted")
		}
	})
}
