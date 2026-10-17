// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"context"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// namedBookCandidates is how many title matches are read to decide whether a
// name is unambiguous. More than one page of a real answer, so that an exact
// title is found even when a dozen longer titles contain it.
const namedBookCandidates = 20

// retrieveNamed retrieves for a question, narrowed to the book the question
// names when it names exactly one.
//
// The router has always returned the book a question is about, and only the
// reading list used it. So "who are the villains in the book?", with the book
// resolved to The Adventures of Sherlock Holmes, still searched the whole
// library - and the keyword fallback matched the word "book" in Dracula and
// Alice. A question about one book is answered from that book.
//
// Narrowing never fails a question. A name that matches nothing, or matches
// several books with nothing to choose between them, searches everything as
// before; so does a lookup that errors; and so does a narrowed search that
// finds nothing - a book that is not indexed yet has no passages, and "nothing
// in your library covers this" would then be false.
func (a *Answerer) retrieveNamed(ctx context.Context, question string, route llm.Route,
	scope Scope) ([]domain.RetrievedChunk, *postgres.LibraryItem, error) {

	named := a.namedBook(ctx, question, route, scope)
	if named == nil {
		passages, err := a.retrieve(ctx, question, route, scope)
		return passages, nil, err
	}

	narrowed := scope
	file := named.BookFileID
	narrowed.BookFileID = &file
	passages, err := a.retrieve(ctx, question, route, narrowed)
	if err == nil && len(passages) > 0 {
		return passages, named, nil
	}
	if err != nil {
		slog.Warn("searching the named book failed; searching the whole library",
			"title", named.Title, "error", err)
	}
	passages, err = a.retrieve(ctx, question, route, scope)
	return passages, nil, err
}

// namedBook resolves the router's book to one the asker may read, or nil.
//
// Resolved through the same listing the library and the reading list use,
// as the asker, so it can only ever find a book they can open - a title they
// cannot read is simply not found, exactly as on their shelf.
//
// Nil, and the whole library is searched, when:
//   - the conversation is already pinned to a book, which wins;
//   - the router named no book, or the question is a comparison, which by
//     definition is about more than one thing;
//   - the name is ambiguous - see unambiguous.
func (a *Answerer) namedBook(ctx context.Context, question string, route llm.Route,
	scope Scope) *postgres.LibraryItem {

	if scope.BookFileID != nil || scope.UserID == uuid.Nil || len(route.Queries(question)) > 1 {
		return nil
	}
	name := bareTitle(route.Book)
	if name == "" {
		return nil
	}

	page, err := a.Store.Library(ctx, postgres.LibraryQuery{
		ViewerID: scope.UserID,
		Search:   name,
		// Best matches first, so an exact title is never cut from the page
		// by twenty longer titles that merely contain it.
		ByTitleMatch: true,
		Limit:        namedBookCandidates,
	})
	if err != nil {
		slog.Warn("looking up the book a question named failed; searching the whole library",
			"book", name, "error", err)
		return nil
	}
	return unambiguous(page.Items, name)
}

// bareTitle is the router's book without an author after it.
//
// The history names books as "Title — Author", and a router told to take a
// title from it may copy the whole label; the listing matches a title or an
// author, never the two run together.
func bareTitle(book string) string {
	if title, _, found := strings.Cut(book, " — "); found {
		book = title
	}
	return strings.TrimSpace(book)
}

// unambiguous picks the one book a name means, or nil.
//
// The candidates are every book whose title, series, author or file name
// contains the name, best title matches first. Each title is graded against
// the name:
//
//	2  the title IS the name, ignoring case, punctuation and a leading article
//	   ("Dracula" for Dracula, "adventures of sherlock holmes" for The
//	   Adventures of Sherlock Holmes)
//	1  the title contains the name as whole words ("Sherlock Holmes" in The
//	   Adventures of Sherlock Holmes)
//	0  it matched on something else - the author, the series, the file name,
//	   or the letters inside a longer word ("Emma" in Dilemmas of Democracy)
//
// and the book is chosen only when it is ALONE in the best grade, and that
// grade is not 0 - a single candidate included. So "Dracula" picks Dracula
// over Dracula's Guest, while "Sherlock Holmes" with both the Adventures and
// the Memoirs on the shelf, "Doyle", or "Emma" with no book called Emma,
// picks nothing and searches everything.
func unambiguous(items []postgres.LibraryItem, name string) *postgres.LibraryItem {
	want := postgres.TitleKey(name)
	if want == "" {
		return nil
	}
	best, bestGrade, tied := -1, 0, false
	for i, item := range items {
		grade := 0
		switch have := postgres.TitleKey(item.Title); {
		case have == want:
			grade = 2
		case strings.Contains(" "+have+" ", " "+want+" "):
			grade = 1
		}
		switch {
		case grade > bestGrade:
			best, bestGrade, tied = i, grade, false
		case grade == bestGrade && grade > 0:
			tied = true
		}
	}
	if best < 0 || tied {
		return nil
	}
	return &items[best]
}
