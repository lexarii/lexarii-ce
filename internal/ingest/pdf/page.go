// SPDX-License-Identifier: AGPL-3.0-or-later

package pdf

import (
	"bytes"
	"fmt"
	"os"
	"sync"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/fault"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// A PDF shared for reading must not be handed over as a file, and the reader
// cannot render what it has not got. Extracting one page at a time is the
// compromise: pdf.js is given a single-page document, the recipient never
// receives the book, and taking the whole thing means asking for every page
// individually.
//
// Be clear that this is friction, not prevention. Several hundred requests is
// a script, and anything on screen can be photographed. What it buys is that
// "save this book" stops being one click, and that the requests are visible.

// ExtractPage returns page n of a PDF as a one-page PDF document.
//
// Convenient but slow: it parses the whole book to produce one page, which on a
// 54 MB reference work is nearly four seconds. Use Open for anything that will
// ask for more than one page.
//
// The page number is 1-based, matching how PDFs are numbered everywhere else in
// Lexarii and how a person would ask for it.
func ExtractPage(path string, n int) ([]byte, error) {
	book, err := Open(path)
	if err != nil {
		return nil, err
	}
	return book.Page(n)
}

// Book is a PDF parsed once and held open for page-by-page reading.
//
// Parsing is what costs: reading, validating and optimising a 54 MB book takes
// about 3.8 seconds, and writing one page out of the parsed result takes
// milliseconds. Doing that work per page turn would make a shared book
// unreadable, so it is done once and the result reused.
type Book struct {
	// mu serialises access because a context is mutable shared state and
	// extraction walks it. Page turns are human-paced; the lock is never
	// contended in a way anyone can feel.
	mu    sync.Mutex
	ctx   *model.Context
	pages int
}

// Open parses a PDF and keeps it ready for page extraction.
//
// Reading only - no validation pass, no optimisation pass - and that is the
// difference between a reader somebody can run at home and one they cannot.
// Measured over the 25 largest PDFs in a real library, against the
// ReadValidateAndOptimize this used to call:
//
//	memory held   5,923 MB -> 2,391 MB   (2.5x less)
//	time to open     22.2s ->     2.0s   (11x faster, over twelve books)
//
// The worst single book went from 874MB and 5.2 seconds to 176MB and 320ms.
// Every one of the 25 reported the same page count and produced the same first
// page afterwards, so nothing was traded away for it.
//
// Validation is what costs, not optimisation: ReadAndValidate held 873MB of
// that 874. And it buys nothing here. Its job is to report that a file breaks
// the specification, and this package's answer to that was already "relaxed,
// then read it anyway" - a book that opens in every other viewer must open
// here. A file too broken to read fails at the read, which is where it is
// caught.
func Open(path string) (_ *Book, err error) {
	// pdfcpu indexes into structures it derives from the file, and a malformed
	// one can panic rather than return. A bad book must fail against itself,
	// not take down the server for everybody reading a different one.
	defer fault.Catch(&err)

	f, err := os.Open(path) // read-only
	if err != nil {
		return nil, fmt.Errorf("opening the book: %w", err)
	}
	defer func() { _ = f.Close() }()

	conf := model.NewDefaultConfiguration()
	// Nothing here is written back to the user's library, and a validation
	// failure on a book that opens fine in every other viewer would make it
	// unreadable for no benefit.
	conf.ValidationMode = model.ValidationRelaxed
	conf.Cmd = model.TRIM

	ctx, err := api.ReadContext(f, conf)
	if err != nil {
		return nil, fmt.Errorf("reading the book: %w", err)
	}
	// ReadValidateAndOptimize used to do this on the way past. Page extraction
	// needs the count, and without this it is zero.
	if err := ctx.EnsurePageCount(); err != nil {
		return nil, fmt.Errorf("counting the pages: %w", err)
	}
	return &Book{ctx: ctx, pages: ctx.PageCount}, nil
}

// Pages is how many pages the book has.
func (b *Book) Pages() int { return b.pages }

// Page returns one page as a standalone PDF document.
func (b *Book) Page(n int) (_ []byte, err error) {
	defer fault.Catch(&err)

	if n < 1 || n > b.pages {
		return nil, fmt.Errorf("page %d is outside a book of %d pages", n, b.pages)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// ExtractPages builds a NEW context holding only the wanted page, so the
	// parsed book is not consumed and can be asked for another page after this
	// one.
	out, err := pdfcpu.ExtractPages(b.ctx, []int{n}, false)
	if err != nil {
		return nil, fmt.Errorf("extracting page %d: %w", n, err)
	}

	var buf bytes.Buffer
	if err := api.WriteContext(out, &buf); err != nil {
		return nil, fmt.Errorf("writing page %d: %w", n, err)
	}
	if buf.Len() == 0 {
		return nil, fmt.Errorf("page %d produced nothing", n)
	}
	return buf.Bytes(), nil
}
