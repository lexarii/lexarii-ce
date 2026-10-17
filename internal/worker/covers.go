// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// An EPUB carries its cover as a file inside the archive, declared in the
// manifest. Extracting it needs no rasteriser and no browser - the parser has
// been pulling it out since ingest was written, into Result.Cover, where
// nothing ever read it. The bytes were found, held in memory, and dropped.
//
// This is the other half. PDFs still have to be rendered in a browser, because
// their cover is a page rather than a file, but an EPUB's arrives free with
// the parse and should never have needed a round trip through a client.

// coverExtensions maps the media types an EPUB declares onto a file extension.
//
// An allow-list rather than a guess from the path: the media type is what the
// image will be served as, and a book is untrusted input, so a cover claiming
// to be an SVG - which can carry script - is not written at all.
var coverExtensions = map[string]string{
	"image/jpeg": "jpg",
	"image/jpg":  "jpg",
	"image/png":  "png",
	"image/webp": "webp",
	"image/gif":  "gif",
}

// maxCoverBytes bounds what is written from an archive.
//
// A cover is a picture of a book jacket. Anything past a few megabytes is a
// full-resolution scan that will be displayed at 180 pixels, or a book doing
// something strange, and neither is worth the disk.
const maxCoverBytes = 8 << 20

// writeCover saves an extracted cover and returns its stored filename.
//
// Returns an empty name and no error when there is nothing worth writing: a
// book without a usable cover is completely ordinary, and failing ingest over
// a missing picture would be absurd.
func writeCover(dir string, cover *ingest.Cover) (string, error) {
	if cover == nil || len(cover.Data) == 0 || dir == "" {
		return "", nil
	}
	ext, ok := coverExtensions[cover.MediaType]
	if !ok {
		return "", nil
	}
	if len(cover.Data) > maxCoverBytes {
		return "", nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating the cover directory: %w", err)
	}

	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	name := hex.EncodeToString(buf[:]) + "." + ext

	// Written and renamed, so a crash halfway leaves no truncated image to be
	// served as somebody's cover.
	tmp, err := os.CreateTemp(dir, ".cover-*")
	if err != nil {
		return "", fmt.Errorf("writing the cover: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(cover.Data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("writing the cover: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("writing the cover: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("storing the cover: %w", err)
	}
	return name, nil
}
