// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

func TestWriteCover(t *testing.T) {
	tests := []struct {
		name    string
		cover   *ingest.Cover
		written bool
		why     string
	}{
		{
			name:    "a jpeg cover",
			cover:   &ingest.Cover{MediaType: "image/jpeg", Data: []byte("pretend jpeg")},
			written: true,
		},
		{
			name:    "a png cover",
			cover:   &ingest.Cover{MediaType: "image/png", Data: []byte("pretend png")},
			written: true,
		},

		// A book is untrusted input and an SVG can carry script. It is not
		// written at all rather than written and served carefully later.
		{
			name:    "an svg is refused",
			cover:   &ingest.Cover{MediaType: "image/svg+xml", Data: []byte("<svg onload=alert(1)>")},
			written: false,
			why:     "an SVG can carry script",
		},
		{
			name:    "an unknown media type is refused",
			cover:   &ingest.Cover{MediaType: "application/octet-stream", Data: []byte("who knows")},
			written: false,
			why:     "the media type decides how it will be served",
		},

		// None of these is an error: a book without a usable cover is entirely
		// ordinary, and failing ingest over a picture would be absurd.
		{name: "no cover at all", cover: nil, written: false},
		{name: "an empty cover", cover: &ingest.Cover{MediaType: "image/png"}, written: false},
		{
			name:    "an absurdly large cover",
			cover:   &ingest.Cover{MediaType: "image/png", Data: make([]byte, maxCoverBytes+1)},
			written: false,
			why:     "it will be shown at 180 pixels",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			name, err := writeCover(dir, tt.cover)
			if err != nil {
				t.Fatalf("writeCover returned an error for %s: %v", tt.name, err)
			}

			if tt.written != (name != "") {
				t.Fatalf("name = %q, want written=%v (%s)", name, tt.written, tt.why)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("reading the cover directory: %v", err)
			}
			want := 0
			if tt.written {
				want = 1
			}
			if len(entries) != want {
				t.Fatalf("%d files on disk, want %d", len(entries), want)
			}

			if !tt.written {
				return
			}
			if strings.ContainsAny(name, "/\\") {
				t.Fatalf("the stored name contains a path separator: %q", name)
			}
			stored, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("reading it back: %v", err)
			}
			if string(stored) != string(tt.cover.Data) {
				t.Fatal("the stored bytes are not the ones extracted")
			}
		})
	}
}

// The name comes from here, never from the archive: a path inside a book is
// attacker-controlled and must not decide where anything is written.
func TestCoverNameIgnoresTheArchivePath(t *testing.T) {
	dir := t.TempDir()
	name, err := writeCover(dir, &ingest.Cover{
		Path:      "../../../etc/passwd",
		MediaType: "image/png",
		Data:      []byte("pretend png"),
	})
	if err != nil {
		t.Fatalf("writeCover: %v", err)
	}
	if strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		t.Fatalf("the archive path reached the stored name: %q", name)
	}
	if !strings.HasSuffix(name, ".png") {
		t.Fatalf("the extension should come from the media type: %q", name)
	}
}
