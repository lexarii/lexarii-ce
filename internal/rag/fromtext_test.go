// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// bookPassages is n passages of one book, scored from top down.
func bookPassages(title string, n int, top float64) []domain.RetrievedChunk {
	work := uuid.NewSHA1(uuid.Nil, []byte("work:"+title))
	file := uuid.NewSHA1(uuid.Nil, []byte("file:"+title))
	out := make([]domain.RetrievedChunk, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, domain.RetrievedChunk{
			Chunk: domain.Chunk{ID: uuid.NewSHA1(uuid.Nil, []byte(fmt.Sprintf("%s-%d", title, i))),
				BookFileID: file, Content: title + " passage"},
			WorkID: work, WorkTitle: title, FusedScore: top / float64(i+1),
		})
	}
	return out
}

// The third question of the thread from the bug. Enrichment never ran, so no
// work has a subject; no title says "romantic"; and the answer was the
// Sherlock Holmes book the conversation had been about. Pride and Prejudice
// was on the shelf. The books are found by what is in them instead.
func TestAReadingListWithNoSubjectsIsFoundFromTheText(t *testing.T) {
	model := newFakeModel(t)
	model.route = routeJSON(domain.IntentFindBook, []string{"romance"}, nil, "")
	user := uuid.New()
	store := &pipelineStore{
		search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			found := bookPassages("Pride and Prejudice", 6, 0.03)
			// One passage of Dracula mentions it in passing.
			found = append(found, bookPassages("Dracula", 1, 0.01)...)
			return found, nil
		},
	}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}

	got, err := a.Ask(t.Context(), "what is a good romantic book?", Scope{UserID: user})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if titles := listTitles(got.Books); !reflect.DeepEqual(titles, []string{"Pride and Prejudice"}) {
		t.Errorf("Books = %v, want Pride and Prejudice alone", titles)
	}
	if got.Text != FoundByText {
		t.Errorf("Text = %q, want it to say the list came from the text", got.Text)
	}
	if len(got.Passages) != 0 || len(model.callsOf("answer")) != 0 {
		t.Error("a reading list came with passages or a written answer")
	}
	if len(store.searches) != 1 {
		t.Fatalf("searched %d times, want once", len(store.searches))
	}
	q := store.searches[0]
	if q.ViewerID != user {
		t.Errorf("searched as %v, want the asker", q.ViewerID)
	}
	// The topic, not the request: "book" is in every passage that mentions one.
	if q.Text != "romantic romance" {
		t.Errorf("searched for %q, want the topic alone", q.Text)
	}
	if q.Limit != textPool {
		t.Errorf("read %d passages, want %d", q.Limit, textPool)
	}
}

// Nothing matched anywhere: said plainly, with no book at all, rather than
// listing an unrelated one.
func TestAReadingListThatMatchesNothingSaysSo(t *testing.T) {
	model := newFakeModel(t)
	model.route = routeJSON(domain.IntentBroadTopic, []string{"quantum chromodynamics"}, nil, "")
	store := &pipelineStore{}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}

	got, err := a.Ask(t.Context(), "quantum chromodynamics", Scope{UserID: uuid.New()})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(got.Books) != 0 || got.Text != NoReadingList {
		t.Errorf("Books = %v, Text = %q", listTitles(got.Books), got.Text)
	}
}

// A request made only of request words has no topic to search for.
func TestAReadingListWithNoTopicSearchesNothing(t *testing.T) {
	model := newFakeModel(t)
	model.route = routeJSON(domain.IntentFindBook, nil, nil, "")
	store := &pipelineStore{}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}

	got, err := a.Ask(t.Context(), "what is a good book?", Scope{UserID: uuid.New()})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(store.searches) != 0 {
		t.Errorf("searched for %q", store.searches[0].Text)
	}
	if got.Text != NoReadingList {
		t.Errorf("Text = %q", got.Text)
	}
}

// A subject or title match is still preferred, and the text is not searched.
func TestATaggedReadingListDoesNotSearchTheText(t *testing.T) {
	model := newFakeModel(t)
	model.route = routeJSON(domain.IntentFindBook, []string{"romance"}, nil, "")
	store := &pipelineStore{
		matches: []postgres.SubjectMatch{{Slug: "romance", Relevance: 1}},
		shelf:   map[string][]postgres.LibraryItem{"romance": shelfItems("Pride and Prejudice")},
	}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}

	got, err := a.Ask(t.Context(), "what is a good romantic book?", Scope{UserID: uuid.New()})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(store.searches) != 0 || got.Text != "" {
		t.Errorf("searched %d times, Text %q", len(store.searches), got.Text)
	}
}

func TestBooksFromTextRanksAndBounds(t *testing.T) {
	var found []domain.RetrievedChunk
	// Twelve books, each strong: the list stops at maxBooksInAnswer.
	for i := 0; i < 12; i++ {
		found = append(found, bookPassages(fmt.Sprintf("Strong %02d", i), 3, 0.03-float64(i)*0.0005)...)
	}
	// And one that matched in passing.
	found = append(found, bookPassages("Weak", 1, 0.001)...)
	store := &pipelineStore{search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) { return found, nil }}
	a := &Answerer{Store: store}

	books, err := a.booksFromText(t.Context(), "gothic", llm.Route{}, Scope{UserID: uuid.New()})
	if err != nil {
		t.Fatalf("booksFromText: %v", err)
	}
	if len(books) != maxBooksInAnswer {
		t.Fatalf("listed %d, want %d", len(books), maxBooksInAnswer)
	}
	if books[0].Title != "Strong 00" {
		t.Errorf("first = %q, want the best-scoring book", books[0].Title)
	}
	for _, b := range books {
		if b.Title == "Weak" {
			t.Error("a book that matched in passing was listed")
		}
		if b.BookFileID == uuid.Nil || b.WorkID == uuid.Nil {
			t.Errorf("%q has no work or file", b.Title)
		}
	}

	// More passages beat fewer at the same rank.
	found = append(bookPassages("Once", 1, 0.03), bookPassages("Often", 4, 0.03)...)
	books, err = a.booksFromText(t.Context(), "gothic", llm.Route{}, Scope{UserID: uuid.New()})
	if err != nil {
		t.Fatalf("booksFromText: %v", err)
	}
	if got := listTitles(books); len(got) == 0 || got[0] != "Often" {
		t.Errorf("order = %v, want the book with more passages first", got)
	}
}

func TestBooksFromTextReportsAFailedSearch(t *testing.T) {
	store := &pipelineStore{search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
		return nil, errPipelineStore
	}}
	a := &Answerer{Store: store}
	if _, err := a.booksFromText(t.Context(), "gothic", llm.Route{}, Scope{UserID: uuid.New()}); !errors.Is(err, errPipelineStore) {
		t.Errorf("error = %v, want the store's", err)
	}
}

func TestTopicWords(t *testing.T) {
	for _, tt := range []struct {
		question string
		subjects []string
		want     string // joined; "" is nil
	}{
		{"what is a good romantic book?", nil, "romantic"},
		{"what is a good romantic book?", []string{"romance", "romantic fiction"}, "romantic romance fiction"},
		{"Do I have anything on Kubernetes?", nil, "kubernetes"},
		{"what other sherlock books are there in my library?", []string{"detective fiction"}, "sherlock detective fiction"},
		{"anything like Dracula's?", nil, "dracula"},
		{"what should I read?", nil, ""},
		{"recommend me a good book", nil, ""},
		{"like it", nil, ""},
		{"similar", nil, ""},
		{"something similar to that one", nil, ""},
		{"more like this, please", nil, ""},
	} {
		got := topicWords(tt.question, tt.subjects)
		if tt.want == "" && got != nil {
			t.Errorf("topicWords(%q, %q) = %q, want nil", tt.question, tt.subjects, got)
		}
		if strings.Join(got, " ") != tt.want {
			t.Errorf("topicWords(%q, %q) = %q, want %q", tt.question, tt.subjects, got, tt.want)
		}
	}
}

// "More like it" has no topic, so nothing is searched and nothing listed.
func TestAReadingListWithOnlyAPronounSaysNothingMatched(t *testing.T) {
	model := newFakeModel(t)
	model.route = routeJSON(domain.IntentFindBook, nil, nil, "")
	store := &pipelineStore{search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
		return bookPassages("Anything", 3, 0.03), nil
	}}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}
	got, err := a.Ask(t.Context(), "something similar", Scope{UserID: uuid.New()})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(store.searches) != 0 || len(got.Books) != 0 || got.Text != NoReadingList {
		t.Errorf("searched %d times, Books %v, Text %q", len(store.searches), listTitles(got.Books), got.Text)
	}
}

// A thread pinned to one book cannot rank books by their passages: its
// passages are all from that book.
func TestAPinnedThreadDoesNotRankBooksByTheirText(t *testing.T) {
	model := newFakeModel(t)
	model.route = routeJSON(domain.IntentFindBook, []string{"romance"}, nil, "")
	store := &pipelineStore{search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
		return bookPassages("The Pinned Book", 3, 0.03), nil
	}}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}
	pinned := uuid.New()
	got, err := a.Ask(t.Context(), "what is a good romantic book?", Scope{UserID: uuid.New(), BookFileID: &pinned})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(store.searches) != 0 || len(got.Books) != 0 || got.Text != NoReadingList {
		t.Errorf("searched %d times, Books %v, Text %q", len(store.searches), listTitles(got.Books), got.Text)
	}
}

// With a reranker, the text is still read textPool deep: the rerank pool is
// for an answer's ten passages and must not cut a reading list to twenty.
func TestATextReadingListIsReadDeepWithAReranker(t *testing.T) {
	model := newFakeModel(t)
	model.rerank = func(string) float64 { return 1 }
	store := &pipelineStore{search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
		return bookPassages("Gothic", textPool, 0.03), nil
	}}
	a := &Answerer{Store: store, Reranker: model.client("fake-rerank")}
	if _, err := a.booksFromText(t.Context(), "gothic", llm.Route{}, Scope{UserID: uuid.New()}); err != nil {
		t.Fatalf("booksFromText: %v", err)
	}
	if got := store.searches[0].Limit; got != textPool {
		t.Errorf("search limit = %d, want %d", got, textPool)
	}
	if len(model.rerankDocs) != 1 || len(model.rerankDocs[0]) != textPool {
		t.Errorf("reranker saw %d passages, want %d", len(model.rerankDocs[0]), textPool)
	}
}

func TestStoredListText(t *testing.T) {
	if got := StoredListText(FoundByText, 0); got != NoLongerListed {
		t.Errorf("an emptied text list reads %q", got)
	}
	if got := StoredListText(FoundByText, 2); got != FoundByText {
		t.Errorf("a list with books reads %q", got)
	}
	if got := StoredListText("Moriarty [1].", 0); got != "Moriarty [1]." {
		t.Errorf("an answer reads %q", got)
	}
}
