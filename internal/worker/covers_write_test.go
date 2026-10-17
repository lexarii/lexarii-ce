// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"os"
	"path/filepath"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// A cover directory that cannot be made or written to is an error the caller
// logs, with no name returned that would point at a file that is not there.
func TestWriteCoverReportsAnUnusableDirectory(t *testing.T) {
	cover := &ingest.Cover{MediaType: "image/png", Data: []byte("pretend png")}

	t.Run("the directory cannot be created", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "a-file")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		name, err := writeCover(filepath.Join(file, "covers"), cover)
		if err == nil || name != "" {
			t.Fatalf("writeCover = %q, %v; want no name and an error", name, err)
		}
	})

	t.Run("the directory cannot be written to", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes to read-only directories")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

		name, err := writeCover(dir, cover)
		if err == nil || name != "" {
			t.Fatalf("writeCover = %q, %v; want no name and an error", name, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("left %d files behind", len(entries))
		}
	})
}
