// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"context"
	"fmt"
	"os"
	"strings"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest/epub"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest/pdf"
)

// ErrNoText reports a book with nothing to index.
//
// Not a failure: a scan with no OCR layer is a perfectly good book to read and
// simply has no text to search. Saying so plainly is what stops it being
// retried as though something were broken.
var ErrNoText = fmt.Errorf("no text could be extracted")

// Units reads a book into the units the chunker works on.
//
// The file is opened READ-ONLY, like every other path that touches the library.
func Units(ctx context.Context, path string, format domain.Format) ([]Unit, error) {
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

	switch format {
	case domain.FormatEPUB:
		return epubUnits(f, info.Size())
	case domain.FormatPDF:
		return pdfUnits(f, info.Size())
	default:
		// A comic has pictures, not text. There is nothing here to retrieve.
		return nil, fmt.Errorf("%w: %s has no text layer", ErrNoText, format)
	}
}

// epubUnits reads one spine item per unit, each split into the same blocks the
// reader anchors positions to.
func epubUnits(r interface {
	ReadAt([]byte, int64) (int, error)
}, size int64) ([]Unit, error) {
	reader, err := epub.Open(r, size)
	if err != nil {
		return nil, fmt.Errorf("opening epub: %w", err)
	}

	spine := reader.Spine()
	titles := titlesByUnit(reader.TOC())
	units := make([]Unit, 0, len(spine))

	for i, item := range spine {
		// Non-linear items are supplementary - adverts, colophons, notes pages
		// - and retrieving from them answers questions with the wrong material.
		if !item.Linear {
			continue
		}

		blocks, err := reader.Blocks(i)
		if err != nil {
			// One unreadable document must not cost the other forty chapters.
			continue
		}
		if len(blocks) == 0 {
			continue
		}

		unit := Unit{Index: i, Title: titleAt(titles, i), Blocks: make([]Block, 0, len(blocks))}
		for _, b := range blocks {
			unit.Blocks = append(unit.Blocks, Block{Path: b.Path, Text: b.Text})
		}
		units = append(units, unit)
	}

	if len(units) == 0 {
		return nil, ErrNoText
	}
	return units, nil
}

// pdfUnits reads one page per unit.
//
// A PDF has no element tree, so a position is a page and an offset into it. The
// page number is ONE-BASED, which is what a PDF locator means and what the
// reader's page control shows.
func pdfUnits(r interface {
	ReadAt([]byte, int64) (int, error)
}, size int64) ([]Unit, error) {
	result, err := pdf.Parse(r, size)
	if err != nil {
		return nil, fmt.Errorf("parsing pdf: %w", err)
	}
	if result.NeedsOCR {
		// Saying this plainly matters: without it, a library of scans looks like
		// a library where search is broken.
		return nil, fmt.Errorf("%w: this looks like a scan and needs OCR", ErrNoText)
	}

	titles := titlesByUnit(result.TOC)
	units := make([]Unit, 0, len(result.UnitText))
	for i, text := range result.UnitText {
		if text == "" {
			continue
		}
		page := i + 1 // pages are one-based
		units = append(units, Unit{
			Index:  page,
			Title:  titleAt(titles, page),
			Blocks: []Block{{Text: text}},
		})
	}

	if len(units) == 0 {
		return nil, ErrNoText
	}
	return units, nil
}

// titlesByUnit flattens a table of contents into unit index -> heading.
//
// Nothing set Unit.Title before this, so chapter_title was NULL on all 81,230
// chunks in a real library. Three things depended on it and quietly did
// without: a passage's label in the answer and in the reader degraded to the
// bare book title, so ten passages from one book arrived as ten identical
// labels with nothing to tell them apart; the B-weighted half of the chunk
// tsvector indexed an empty string on every row; and the documented promise
// that a passage carries "book, author, chapter, locator" was two thirds true.
//
// A TOC is nested, partial, and sometimes points outside the spine entirely -
// which real books do and which is not an error - so entries that resolve to
// nothing are skipped rather than guessed at.
func titlesByUnit(toc []ingest.TOCEntry) map[int]string {
	out := map[int]string{}

	var walk func([]ingest.TOCEntry)
	walk = func(entries []ingest.TOCEntry) {
		for _, entry := range entries {
			title := strings.TrimSpace(entry.Title)
			// The first entry for a unit wins: a nested sub-heading pointing at
			// the same place should not displace the chapter it sits under.
			if title != "" && entry.SpineIndex >= 0 {
				if _, taken := out[entry.SpineIndex]; !taken {
					out[entry.SpineIndex] = title
				}
			}
			walk(entry.Children)
		}
	}
	walk(toc)
	return out
}

// titleAt is the heading a unit belongs to: its own, or the nearest one before
// it.
//
// Filling forward because a chapter is not one unit. An EPUB chapter split
// across three spine items, or a PDF chapter running from page 40 to page 62,
// has a heading on the first and nothing on the rest - and "somewhere in
// chapter 4" is what a person needs from a citation, not "page 57 of nothing".
func titleAt(titles map[int]string, unit int) string {
	if title, ok := titles[unit]; ok {
		return title
	}
	best, bestAt := "", -1
	for at, title := range titles {
		if at <= unit && at > bestAt {
			best, bestAt = title, at
		}
	}
	return best
}
