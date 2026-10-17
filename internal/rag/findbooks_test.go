// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// shelves is a library where each subject holds named books.
type shelves struct {
	byTag map[string][]string
}

func (s *shelves) Library(_ context.Context, q postgres.LibraryQuery) (postgres.LibraryPage, error) {
	page := postgres.LibraryPage{}
	for _, title := range s.byTag[q.Tag] {
		page.Items = append(page.Items, postgres.LibraryItem{
			// Deterministic, so the same title is the same work across tags.
			WorkID: uuid.NewSHA1(uuid.Nil, []byte(title)),
			Title:  title,
		})
	}
	return page, nil
}

// MatchingSubjects resolves a requested subject to the tags this shelf really
// uses, by any whole word they share - the same rule the store applies.
func (s *shelves) MatchingSubjects(_ context.Context, subjects []string) ([]postgres.SubjectMatch, error) {
	wanted := map[string]bool{}
	for _, subject := range subjects {
		for _, token := range strings.Split(postgres.TagSlug(subject), "-") {
			if token != "" {
				wanted[token] = true
			}
		}
	}

	var out []postgres.SubjectMatch
	for slug, books := range s.byTag {
		for _, token := range strings.Split(slug, "-") {
			if wanted[token] {
				out = append(out, postgres.SubjectMatch{Slug: slug, Books: len(books), Relevance: 1})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (s *shelves) Search(context.Context, postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
	return nil, nil
}

func (s *shelves) WorksMatchingSubjects(context.Context, []string, int) ([]uuid.UUID, error) {
	return nil, nil
}

func (s *shelves) LibraryFacts(context.Context, uuid.UUID) (postgres.LibraryFacts, error) {
	return postgres.LibraryFacts{}, nil
}

func (s *shelves) SearchNotes(context.Context, uuid.UUID, string, int) ([]postgres.FoundNote, error) {
	return nil, nil
}

func listTitles(items []postgres.LibraryItem) []string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, i.Title)
	}
	return out
}

// The reading list weighs every subject the router named, and weighs a
// SPECIFIC one more heavily than a broad one.
//
// From the real library: "what is a good agile methodology?" routed to agile
// methodology, software development and project management. The first had no
// tag at all, the second had twelve books, and returning those twelve ended
// the search - so the answer was C#, Docker and Fluent Python, and Head First
// Agile, which is tagged project-management, was never reached.
func TestAReadingListPrefersTheNarrowerSubject(t *testing.T) {
	a := &Answerer{Store: &shelves{byTag: map[string][]string{
		// Twelve books, so a hit here barely narrows the shelf.
		"software-development": {
			"C# 8.0 in a Nutshell", "The Docker Workshop", "Fluent Python", "Head First C",
			"Head First C#", "Head First Go", "Head First Learn to Code", "Head First Ruby",
			"Effective Java", "Clean Code", "The Pragmatic Programmer", "Refactoring",
		},
		// Three, so a hit here says something.
		"project-management": {"Head First Agile", "Head First PMP", "Making Work Visible"},
	}}}

	route := llm.Route{Subjects: []string{"agile methodology", "software development", "project management"}}
	books, err := a.findBooks(context.Background(), "what is a good agile methodology?", route, Scope{})
	if err != nil {
		t.Fatalf("findBooks: %v", err)
	}
	if len(books) == 0 {
		t.Fatal("no books at all")
	}

	// The three project-management books come first, ahead of twelve books
	// that merely share a very large subject.
	top := listTitles(books)[:3]
	for _, want := range []string{"Head First Agile", "Head First PMP", "Making Work Visible"} {
		found := false
		for _, got := range top {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not in the top three: %v", want, top)
		}
	}
}

// A book under two of the asked-for subjects beats one under either alone.
func TestAReadingListAddsUpTheSubjects(t *testing.T) {
	a := &Answerer{Store: &shelves{byTag: map[string][]string{
		"kubernetes": {"Mastering Kubernetes", "The Kubernetes Workshop", "Kubernetes on Azure"},
		"devops":     {"Mastering Kubernetes", "Continuous Delivery", "The Phoenix Project"},
	}}}

	route := llm.Route{Subjects: []string{"kubernetes", "devops"}}
	books, err := a.findBooks(context.Background(), "kubernetes and devops", route, Scope{})
	if err != nil {
		t.Fatalf("findBooks: %v", err)
	}
	if got := listTitles(books); len(got) == 0 || got[0] != "Mastering Kubernetes" {
		t.Errorf("first book = %v, want the one under both subjects", got)
	}
}

// A subject the library has no tag for is skipped rather than ending the
// search, which is what it used to do when it was the first one asked for.
func TestAnUnknownSubjectDoesNotEndTheSearch(t *testing.T) {
	a := &Answerer{Store: &shelves{byTag: map[string][]string{
		"project-management": {"Head First Agile"},
	}}}

	route := llm.Route{Subjects: []string{"agile methodology", "project management"}}
	books, err := a.findBooks(context.Background(), "agile", route, Scope{})
	if err != nil {
		t.Fatalf("findBooks: %v", err)
	}
	if got := listTitles(books); len(got) != 1 || got[0] != "Head First Agile" {
		t.Errorf("books = %v, want the one under the subject that exists", got)
	}
}

// The router's words and the library's words are rarely the same words. It
// asked for "agile methodology"; the library files that book under "agile
// software development". Exact slugs found nothing at all, and the reading
// list about agile came back without a single agile book in it.
func TestAReadingListFindsTheSubjectTheLibraryActuallyUses(t *testing.T) {
	a := &Answerer{Store: &shelves{byTag: map[string][]string{
		"agile-software-development": {"Head First Agile"},
		"software-development": {
			"C# 8.0 in a Nutshell", "The Docker Workshop", "Fluent Python", "Head First C",
			"Head First Ruby", "Introducing Go", "Python Cookbook", "Effective Java",
		},
	}}}

	route := llm.Route{Subjects: []string{"agile methodology", "software development"}}
	books, err := a.findBooks(context.Background(), "what is the best agile methodology?", route, Scope{})
	if err != nil {
		t.Fatalf("findBooks: %v", err)
	}
	if got := listTitles(books); len(got) == 0 || got[0] != "Head First Agile" {
		t.Errorf("first book = %v, want Head First Agile", got)
	}
}
