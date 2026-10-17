// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

const holmesTitle = "The Adventures of Sherlock Holmes"

// shelfBook is a book on the asker's shelf with a file of its own.
func shelfBook(title string) postgres.LibraryItem {
	return postgres.LibraryItem{
		WorkID:     uuid.NewSHA1(uuid.Nil, []byte("work:"+title)),
		BookFileID: uuid.NewSHA1(uuid.Nil, []byte("file:"+title)),
		Title:      title,
		Status:     domain.IngestReady,
	}
}

// The thread from the bug, second question. The router resolved "the book" to
// the one book just listed; the search must be inside that book, not across
// the library where the word "book" is in every passage.
func TestAQuestionAboutTheNamedBookSearchesOnlyIt(t *testing.T) {
	model := newFakeModel(t)
	model.route = routeJSON(domain.IntentFactualLookup, nil, nil, holmesTitle)
	model.answer = "Moriarty [1]."
	holmes := shelfBook(holmesTitle)
	user := uuid.New()
	store := &pipelineStore{
		shelf: map[string][]postgres.LibraryItem{"search:" + holmesTitle: {holmes}},
		search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			return pipelinePassages(holmesTitle, 2), nil
		},
	}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}
	history := History{
		{Role: "user", Text: "what other sherlock books are there in my library?"},
		{Role: "assistant", Listed: []string{WorkLabel(holmesTitle, []string{"Arthur Conan Doyle"})}},
	}

	got, err := a.AskWithHistory(t.Context(), "who are the villians in the book?", Scope{UserID: user}, history)
	if err != nil {
		t.Fatalf("AskWithHistory: %v", err)
	}
	if len(store.searches) != 1 {
		t.Fatalf("searched %d times, want once", len(store.searches))
	}
	q := store.searches[0]
	if q.BookFileID == nil || *q.BookFileID != holmes.BookFileID {
		t.Errorf("searched book %v, want %v", q.BookFileID, holmes.BookFileID)
	}
	if q.ViewerID != user {
		t.Errorf("searched as %v, want the asker", q.ViewerID)
	}
	if got.ScopedTo == nil || got.ScopedTo.WorkID != holmes.WorkID {
		t.Errorf("ScopedTo = %+v, want the named book", got.ScopedTo)
	}
	// The book was looked up as the asker, so only their books can match.
	for _, call := range store.libraryCalls {
		if call.ViewerID != user {
			t.Errorf("the named book was looked up as %v", call.ViewerID)
		}
	}
	// Measuring retrieval takes the same path an answer does.
	store.searches = nil
	if _, _, err := a.Retrieve(t.Context(), "who is the villain in The Adventures of Sherlock Holmes?",
		Scope{UserID: user}); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(store.searches) != 1 || store.searches[0].BookFileID == nil {
		t.Errorf("Retrieve did not narrow to the named book: %+v", store.searches)
	}
}

// Whenever the name does not settle on one readable book, or narrowing finds
// nothing, the whole library is searched exactly as before, and nothing fails.
func TestANamedBookThatCannotBeResolvedSearchesEverything(t *testing.T) {
	pinned := uuid.New()
	tests := []struct {
		name     string
		route    string
		shelf    map[string][]postgres.LibraryItem
		libErr   error
		scope    Scope
		empty    bool // the narrowed search finds nothing
		searches int
		wantFile *uuid.UUID
	}{
		{
			name:     "nothing matches",
			route:    routeJSON(domain.IntentFactualLookup, nil, nil, "Sherlok"),
			searches: 1,
		},
		{
			name:  "two books match equally",
			route: routeJSON(domain.IntentFactualLookup, nil, nil, "Sherlock Holmes"),
			shelf: map[string][]postgres.LibraryItem{"search:Sherlock Holmes": {
				shelfBook(holmesTitle), shelfBook("The Memoirs of Sherlock Holmes"),
			}},
			searches: 1,
		},
		{
			name:     "the lookup fails",
			route:    routeJSON(domain.IntentFactualLookup, nil, nil, holmesTitle),
			libErr:   errPipelineStore,
			searches: 1,
		},
		{
			name:  "a comparison",
			route: routeJSON(domain.IntentFactualLookup, nil, []string{"villains in Holmes", "villains in Dracula"}, holmesTitle),
			shelf: map[string][]postgres.LibraryItem{"search:" + holmesTitle: {shelfBook(holmesTitle)}},
			// One search per side, neither narrowed.
			searches: 2,
		},
		{
			name:     "the conversation is pinned",
			route:    routeJSON(domain.IntentFactualLookup, nil, nil, holmesTitle),
			shelf:    map[string][]postgres.LibraryItem{"search:" + holmesTitle: {shelfBook(holmesTitle)}},
			scope:    Scope{BookFileID: &pinned},
			searches: 1,
			wantFile: &pinned,
		},
		{
			name:  "the named book has no passages yet",
			route: routeJSON(domain.IntentFactualLookup, nil, nil, holmesTitle),
			shelf: map[string][]postgres.LibraryItem{"search:" + holmesTitle: {shelfBook(holmesTitle)}},
			empty: true,
			// Narrowed, found nothing, searched again without narrowing.
			searches: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newFakeModel(t)
			model.route = tt.route
			model.answer = "From everywhere [1]."
			store := &pipelineStore{shelf: tt.shelf, libraryErr: tt.libErr}
			store.search = func(q postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
				if tt.empty && q.BookFileID != nil {
					return nil, nil
				}
				return pipelinePassages("Anything", 2), nil
			}
			a := &Answerer{Store: store, Chat: model.client("fake-chat")}
			scope := tt.scope
			scope.UserID = uuid.New()

			got, err := a.Ask(t.Context(), "who are the villains in Sherlock Holmes?", scope)
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if got.ScopedTo != nil {
				t.Errorf("ScopedTo = %q, want nil", got.ScopedTo.Title)
			}
			if len(got.Passages) == 0 {
				t.Error("no passages: the question was not answered from the library")
			}
			if len(store.searches) != tt.searches {
				t.Fatalf("searched %d times, want %d", len(store.searches), tt.searches)
			}
			last := store.searches[len(store.searches)-1]
			switch {
			case tt.wantFile == nil && last.BookFileID != nil:
				t.Errorf("the answer came from a narrowed search: %v", *last.BookFileID)
			case tt.wantFile != nil && (last.BookFileID == nil || *last.BookFileID != *tt.wantFile):
				t.Errorf("searched %v, want the pinned %v", last.BookFileID, *tt.wantFile)
			}
		})
	}
}

func TestUnambiguous(t *testing.T) {
	dracula, guest := shelfBook("Dracula"), shelfBook("Dracula's Guest")
	adventures, memoirs := shelfBook(holmesTitle), shelfBook("The Memoirs of Sherlock Holmes")
	byAuthor := shelfBook("A Study in Scarlet")

	tests := []struct {
		name  string
		items []postgres.LibraryItem
		named string
		want  string
	}{
		{"nothing", nil, "Dracula", ""},
		{"one book matched only on the author", []postgres.LibraryItem{byAuthor}, "Doyle", ""},
		{"one book matched only on letters inside a word", []postgres.LibraryItem{shelfBook("Dilemmas of Democracy")}, "Emma", ""},
		{"one book whose title contains the name", []postgres.LibraryItem{adventures}, "Sherlock Holmes", holmesTitle},
		{"one book whose title is the name", []postgres.LibraryItem{dracula}, "Dracula", "Dracula"},
		{"the exact title over a longer one", []postgres.LibraryItem{guest, dracula}, "Dracula", "Dracula"},
		{"the exact title without its article", []postgres.LibraryItem{adventures, memoirs}, "adventures of sherlock holmes", holmesTitle},
		{"the exact title, any case and punctuation", []postgres.LibraryItem{memoirs, adventures}, "the ADVENTURES of Sherlock Holmes!", holmesTitle},
		{"two titles contain it equally", []postgres.LibraryItem{adventures, memoirs}, "Sherlock Holmes", ""},
		{"one title contains it, one matched on the author", []postgres.LibraryItem{byAuthor, adventures}, "Sherlock Holmes", holmesTitle},
		{"two matched only on the author", []postgres.LibraryItem{byAuthor, adventures}, "Doyle", ""},
		{"two with the very same title", []postgres.LibraryItem{dracula, shelfBook("Dracula")}, "Dracula", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unambiguous(tt.items, tt.named)
			switch {
			case tt.want == "" && got != nil:
				t.Errorf("chose %q, want none", got.Title)
			case tt.want != "" && (got == nil || got.Title != tt.want):
				t.Errorf("chose %v, want %q", got, tt.want)
			}
		})
	}
}

// The probe from the review, as the listing returns it now - exact title first
// - and as it returned it before, the exact title last. Either way only "It"
// is chosen, and only if it is on the page at all.
func TestUnambiguousPicksItAmongTitlesContainingIt(t *testing.T) {
	var habits []postgres.LibraryItem
	for i := 1; i <= 19; i++ {
		habits = append(habits, shelfBook(fmt.Sprintf("A Habit of Mind %02d", i)))
	}
	it, bring := shelfBook("It"), shelfBook("Bring It On")

	ranked := append([]postgres.LibraryItem{it, bring}, habits...)
	if got := unambiguous(ranked, "It"); got == nil || got.Title != "It" {
		t.Errorf("ranked: chose %v, want It", got)
	}
	// Without the exact title on the page, "Bring It On" is alone in grade 1
	// - which is why the listing must rank before it limits.
	cut := append([]postgres.LibraryItem{bring}, habits...)
	if got := unambiguous(cut, "It"); got == nil || got.Title != "Bring It On" {
		t.Errorf("cut: chose %v", got)
	}
}

// The named book is looked up by title match, so the exact title survives the
// limit.
func TestTheNamedBookIsLookedUpByTitleMatch(t *testing.T) {
	model := newFakeModel(t)
	model.route = routeJSON(domain.IntentFactualLookup, nil, nil, "It")
	model.answer = "A clown [1]."
	store := &pipelineStore{search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
		return pipelinePassages("It", 1), nil
	}}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}
	if _, err := a.Ask(t.Context(), "who is the clown in It?", Scope{UserID: uuid.New()}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(store.libraryCalls) != 1 || !store.libraryCalls[0].ByTitleMatch ||
		store.libraryCalls[0].Limit != namedBookCandidates {
		t.Errorf("looked up %+v", store.libraryCalls)
	}
}

func TestBareTitle(t *testing.T) {
	for in, want := range map[string]string{
		holmesTitle + " — Arthur Conan Doyle": holmesTitle,
		" Dracula ":                           "Dracula",
		"Good Omens — Pratchett, Gaiman":      "Good Omens",
		"":                                    "",
	} {
		if got := bareTitle(in); got != want {
			t.Errorf("bareTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// A summary is a question about one book too, and narrows the same way.
func TestASummaryOfTheNamedBookReadsOnlyIt(t *testing.T) {
	model := newFakeModel(t)
	model.route = routeJSON(domain.IntentSummariseBook, nil, nil, "Dracula")
	model.answer = "A count moves to England [1]."
	dracula := shelfBook("Dracula")
	store := &pipelineStore{
		shelf: map[string][]postgres.LibraryItem{"search:Dracula": {dracula, shelfBook("Dracula's Guest")}},
		search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			return pipelinePassages("Dracula", 1), nil
		},
	}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}

	got, err := a.Ask(t.Context(), "what is Dracula about?", Scope{UserID: uuid.New()})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.ScopedTo == nil || got.ScopedTo.Title != "Dracula" {
		t.Errorf("ScopedTo = %+v", got.ScopedTo)
	}
	if q := store.searches[0]; q.BookFileID == nil || *q.BookFileID != dracula.BookFileID {
		t.Errorf("searched %v", q.BookFileID)
	}
}
