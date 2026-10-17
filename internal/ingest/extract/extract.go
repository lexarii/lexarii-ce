// SPDX-License-Identifier: AGPL-3.0-or-later

// Package extract routes a file to the parser for its format.
//
// It sits above the format packages rather than inside internal/ingest, because
// the parsers import internal/ingest for the shared Result type and a dispatcher
// there would close the cycle.
package extract

import (
	"context"
	"errors"
	"fmt"
	"os"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest/epub"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest/mobi"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest/pdf"
)

// ErrUnsupportedFormat reports a format with no parser.
var ErrUnsupportedFormat = errors.New("unsupported format")

// File opens a book and parses it according to its format.
//
// The file is opened READ-ONLY. Nothing under the library directory is ever
// written, and that is enforced here at the one place a book file is opened.
func File(ctx context.Context, path string, format domain.Format) (*ingest.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f, err := os.Open(path) // read-only by construction
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}

	return Reader(ctx, f, info.Size(), format)
}

// Reader parses an already-open book.
func Reader(ctx context.Context, r interface {
	ReadAt([]byte, int64) (int, error)
}, size int64, format domain.Format) (*ingest.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	switch format {
	case domain.FormatEPUB:
		return epub.Parse(r, size)
	case domain.FormatPDF:
		return pdf.Parse(r, size)
	case domain.FormatMOBI, domain.FormatAZW3:
		// AZW3 is a MOBI container with a newer document inside it, and the
		// outer structure this reads is the same. A file that turns out to
		// carry only the newer form says so rather than pretending.
		return mobi.Parse(r, size)
	default:
		// CBZ and CBR are library citizens but not retrieval citizens: they have
		// no text to chunk. They belong on the shelf, not in this dispatcher.
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFormat, format)
	}
}

// StatusFor maps a parse outcome to the ingest status stored against a BookFile.
//
// A scanned PDF is needs_ocr rather than ready: it is shelved and readable, but
// it has no text to search, and saying so explicitly is what stops the user
// wondering why searches never find it.
func StatusFor(res *ingest.Result, err error) domain.IngestStatus {
	switch {
	case errors.Is(err, ErrUnsupportedFormat):
		return domain.IngestUnsupported
	case err != nil:
		return domain.IngestFailed
	case res.NeedsOCR:
		return domain.IngestNeedsOCR
	default:
		return domain.IngestReady
	}
}

// Sample reads the opening text of a book, bounded by maxChars.
//
// Separate from File because enrichment needs a few thousand words, not a whole
// book: extracting 1,100 pages to read the first three is the difference
// between a library that enriches overnight and one that does not finish.
//
// Read-only, like every other path that opens a library file.
func Sample(ctx context.Context, path string, format domain.Format, maxChars int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if maxChars <= 0 {
		return "", nil
	}

	f, err := os.Open(path) // read-only by construction
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("%s is empty", path)
	}

	switch format {
	case domain.FormatEPUB:
		return epub.Sample(f, info.Size(), maxChars)
	case domain.FormatPDF:
		return pdf.Sample(f, info.Size(), maxChars)
	case domain.FormatMOBI, domain.FormatAZW3:
		return mobi.Sample(f, info.Size(), maxChars)
	default:
		// A comic has no text to describe. Saying so is better than handing a
		// model an empty string and letting it invent something.
		return "", fmt.Errorf("%w: %s", ErrUnsupportedFormat, format)
	}
}
