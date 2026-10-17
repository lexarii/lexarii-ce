// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/llm/prompts"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

func shelfItems(titles ...string) []postgres.LibraryItem {
	out := make([]postgres.LibraryItem, 0, len(titles))
	for _, title := range titles {
		out = append(out, postgres.LibraryItem{WorkID: uuid.NewSHA1(uuid.Nil, []byte(title)), Title: title})
	}
	return out
}

// "What should I read about X" is answered with books, never with a
// synthesis of whatever passages matched - for a find-book question and for
// one too broad to answer.
func TestAReadingListQuestionReturnsBooksAndRetrievesNothing(t *testing.T) {
	for _, intent := range []domain.QueryIntent{domain.IntentFindBook, domain.IntentBroadTopic} {
		t.Run(string(intent), func(t *testing.T) {
			model := newFakeModel(t)
			model.route = routeJSON(intent, []string{"distributed systems"}, nil, "")
			user := uuid.New()
			store := &pipelineStore{
				matches: []postgres.SubjectMatch{{Slug: "distributed-systems", Books: 2, Relevance: 1}},
				shelf: map[string][]postgres.LibraryItem{
					"distributed-systems": shelfItems("Designing Data-Intensive Applications", "Database Internals"),
				},
			}
			a := &Answerer{Store: store, Chat: model.client("fake-chat")}

			got, err := a.Ask(t.Context(), "what should I read on distributed systems?", Scope{UserID: user})
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if got.Route.Intent != intent {
				t.Errorf("Intent = %q", got.Route.Intent)
			}
			if titles := listTitles(got.Books); len(titles) != 2 {
				t.Errorf("Books = %v, want the two on the shelf", titles)
			}
			if got.Text != "" || len(got.Passages) != 0 {
				t.Errorf("a reading list came with Text %q and %d passages", got.Text, len(got.Passages))
			}
			if len(store.searches) != 0 || len(model.callsOf("answer")) != 0 {
				t.Error("a reading-list question searched passages or wrote an answer")
			}
			// Only books the asker can see.
			for _, q := range store.libraryCalls {
				if q.ViewerID != user {
					t.Errorf("a library listing ran as %v, not the asker", q.ViewerID)
				}
			}
		})
	}
}

// With no subject the library has a tag for, the list falls back to a title
// search: the book the router named, or the question itself.
func TestAReadingListFallsBackToTitleSearch(t *testing.T) {
	t.Run("the named book", func(t *testing.T) {
		store := &pipelineStore{shelf: map[string][]postgres.LibraryItem{"search:Dune": shelfItems("Dune")}}
		a := &Answerer{Store: store}
		books, err := a.findBooks(t.Context(), "is Dune any good?", llm.Route{Book: "Dune"}, Scope{})
		if err != nil {
			t.Fatalf("findBooks: %v", err)
		}
		if !reflect.DeepEqual(listTitles(books), []string{"Dune"}) {
			t.Errorf("Books = %v", listTitles(books))
		}
		last := store.libraryCalls[len(store.libraryCalls)-1]
		if last.Search != "Dune" || last.Limit != maxBooksInAnswer {
			t.Errorf("title search = %+v", last)
		}
	})

	t.Run("the question", func(t *testing.T) {
		store := &pipelineStore{}
		a := &Answerer{Store: store}
		if _, err := a.findBooks(t.Context(), "books by Le Guin", llm.Route{}, Scope{}); err != nil {
			t.Fatalf("findBooks: %v", err)
		}
		if got := store.libraryCalls[0].Search; got != "books by Le Guin" {
			t.Errorf("searched %q, want the question", got)
		}
	})

	t.Run("a subject with an empty shelf is passed over", func(t *testing.T) {
		store := &pipelineStore{
			matches: []postgres.SubjectMatch{{Slug: "empty", Relevance: 1}},
			shelf:   map[string][]postgres.LibraryItem{"search:poetry": shelfItems("Leaves of Grass")},
		}
		a := &Answerer{Store: store}
		books, err := a.findBooks(t.Context(), "poetry", llm.Route{}, Scope{})
		if err != nil {
			t.Fatalf("findBooks: %v", err)
		}
		if !reflect.DeepEqual(listTitles(books), []string{"Leaves of Grass"}) {
			t.Errorf("Books = %v", listTitles(books))
		}
	})
}

// A list of eight is a recommendation; a list of eighty is the library.
func TestAReadingListIsBounded(t *testing.T) {
	var titles []string
	for i := 0; i < 30; i++ {
		titles = append(titles, fmt.Sprintf("Book %02d", i))
	}
	store := &pipelineStore{
		matches: []postgres.SubjectMatch{{Slug: "maths", Relevance: 1}},
		shelf:   map[string][]postgres.LibraryItem{"maths": shelfItems(titles...)},
	}
	a := &Answerer{Store: store}
	books, err := a.findBooks(t.Context(), "maths", llm.Route{Subjects: []string{"maths"}}, Scope{})
	if err != nil {
		t.Fatalf("findBooks: %v", err)
	}
	if len(books) != maxBooksInAnswer {
		t.Errorf("returned %d books, want %d", len(books), maxBooksInAnswer)
	}
	// The subject is read deep, so its size can count against it.
	if store.libraryCalls[0].Limit != subjectPool {
		t.Errorf("subject read with limit %d, want %d", store.libraryCalls[0].Limit, subjectPool)
	}
}

func TestAReadingListReportsStoreFailures(t *testing.T) {
	tests := []struct {
		name  string
		store *pipelineStore
	}{
		{"matching subjects", &pipelineStore{matchesErr: errPipelineStore}},
		{"reading a subject", &pipelineStore{
			matches:    []postgres.SubjectMatch{{Slug: "maths", Relevance: 1}},
			libraryErr: errPipelineStore,
		}},
		{"the title search", &pipelineStore{libraryErr: errPipelineStore}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newFakeModel(t)
			model.route = routeJSON(domain.IntentFindBook, []string{"maths"}, nil, "")
			a := &Answerer{Store: tt.store, Chat: model.client("fake-chat")}
			if _, err := a.Ask(t.Context(), "maths books?", Scope{}); !errors.Is(err, errPipelineStore) {
				t.Errorf("error = %v, want the store's", err)
			}
		})
	}
}

// "How many books do I have" is a fact the database holds exactly. It is
// answered from SQL, phrased by the model, and never searched for.
func TestAMetadataQuestionIsAnsweredFromTheLibraryFacts(t *testing.T) {
	model := newFakeModel(t)
	model.route = routeJSON(domain.IntentMetadata, nil, nil, "")
	model.metadata = "  You have 42 books.  "
	store := &pipelineStore{facts: postgres.LibraryFacts{Works: 42, Files: 50, Authors: 30}}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}

	got, err := a.Ask(t.Context(), "how many books do I have?", Scope{UserID: uuid.New()})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Text != "You have 42 books." {
		t.Errorf("Text = %q", got.Text)
	}
	if got.PromptID != "metadata/"+prompts.MetadataVersion || got.Model != "fake-chat" {
		t.Errorf("PromptID = %q, Model = %q", got.PromptID, got.Model)
	}
	if len(store.searches) != 0 || len(got.Passages) != 0 {
		t.Error("a metadata question searched passages")
	}
	calls := model.callsOf("metadata")
	if len(calls) != 1 {
		t.Fatalf("%d metadata calls, want 1", len(calls))
	}
	if !strings.Contains(calls[0].user, "- 42 books, in 50 files, by 30 authors.") ||
		!strings.Contains(calls[0].user, "how many books do I have?") {
		t.Errorf("the model was not given the facts and the question: %q", calls[0].user)
	}
}

func TestAMetadataQuestionReportsFailures(t *testing.T) {
	t.Run("the facts cannot be read", func(t *testing.T) {
		model := newFakeModel(t)
		model.route = routeJSON(domain.IntentMetadata, nil, nil, "")
		a := &Answerer{Store: &pipelineStore{factsErr: errPipelineStore}, Chat: model.client("fake-chat")}
		if _, err := a.Ask(t.Context(), "how many?", Scope{}); !errors.Is(err, errPipelineStore) {
			t.Errorf("error = %v, want the store's", err)
		}
		if len(model.callsOf("metadata")) != 0 {
			t.Error("the model was asked to phrase facts that were never read")
		}
	})

	t.Run("the model refuses", func(t *testing.T) {
		model := newFakeModel(t)
		model.route = routeJSON(domain.IntentMetadata, nil, nil, "")
		model.metadataStatus = http.StatusBadRequest
		a := &Answerer{Store: &pipelineStore{}, Chat: model.client("fake-chat")}
		_, err := a.Ask(t.Context(), "how many?", Scope{})
		var httpErr *llm.HTTPError
		if !errors.As(err, &httpErr) {
			t.Errorf("error = %v, want the model's", err)
		}
	})
}

// Without a model the facts are still worth showing, unphrased.
func TestLibraryFactsWithoutAModelAreShownAsTheyAre(t *testing.T) {
	store := &pipelineStore{facts: postgres.LibraryFacts{Works: 3, Files: 4, Authors: 2}}
	a := &Answerer{Store: store}
	text, _, phrase, err := a.answerAboutLibrary(t.Context(), "how many?", Scope{})
	if err != nil {
		t.Fatalf("answerAboutLibrary: %v", err)
	}
	if text != renderFacts(store.facts) || phrase != 0 {
		t.Errorf("text = %q, phrase = %d; want the rendered facts and no phrasing time", text, phrase)
	}
}

func TestRenderFacts(t *testing.T) {
	full := renderFacts(postgres.LibraryFacts{
		Works: 120, Files: 130, Authors: 80,
		Formats:     map[domain.Format]int{domain.FormatEPUB: 100, domain.FormatPDF: 30},
		Searchable:  110,
		Untitled:    2,
		NeedsOCR:    5,
		Missing:     1,
		TopSubjects: []postgres.SubjectCount{{Name: "Mathematics", Books: 48}, {Name: "History", Books: 12}},
		TopAuthors:  []postgres.AuthorCount{{Name: "Ursula K. Le Guin", Books: 9}, {Name: "Terry Pratchett", Books: 7}},
		Recent:      []string{"Piranesi", "The Dispossessed"},
		InProgress:  []string{"Middlemarch"},
	})
	for _, want := range []string{
		"- 120 books, in 130 files, by 80 authors.\n",
		"- 100 EPUB files.\n",
		"- 30 PDF files.\n",
		"- 110 books are searchable by passage.\n",
		"- 2 books have no title recorded yet.\n",
		"- 5 files are scans with no text layer, so they cannot be searched.\n",
		"- 1 files are recorded but missing from disk.\n",
		"- The most common subjects are: Mathematics (48), History (12).\n",
		"- The most collected authors are: Ursula K. Le Guin (9), Terry Pratchett (7).\n",
		"- Most recently added: Piranesi; The Dispossessed.\n",
		"- Currently being read: Middlemarch.\n",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("rendered facts lack %q:\n%s", want, full)
		}
	}

	// Facts that are zero or empty are left out rather than stated as zero.
	sparse := renderFacts(postgres.LibraryFacts{Works: 1, Files: 1, Authors: 1, Searchable: 1})
	want := "- 1 books, in 1 files, by 1 authors.\n- 1 books are searchable by passage.\n"
	if sparse != want {
		t.Errorf("sparse facts = %q, want %q", sparse, want)
	}
}

// A comparison searches each side with its own words and interleaves them,
// so the side that matches better cannot take the whole answer.
func TestAComparisonSearchesEachSideAndInterleaves(t *testing.T) {
	model := newFakeModel(t)
	model.route = routeJSON(domain.IntentFactualLookup, nil, []string{"rust ownership", "C++ RAII", "rust ownership "}, "")
	model.answer = "Both [1][2]."

	shared := pipelinePassages("Shared", 1)[0]
	store := &pipelineStore{
		search: func(q postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			switch q.Text {
			case "rust ownership":
				return append([]domain.RetrievedChunk{shared}, pipelinePassages("Rust", 4)...), nil
			case "C++ RAII":
				return append([]domain.RetrievedChunk{shared}, pipelinePassages("Cpp", 4)...), nil
			}
			return nil, fmt.Errorf("unexpected search %q", q.Text)
		},
	}
	a := &Answerer{Store: store, Chat: model.client("fake-chat"), MaxPassages: 5}

	got, err := a.Ask(t.Context(), "how does rust ownership compare to C++ RAII?", Scope{})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(store.searches) != 2 {
		t.Fatalf("%d searches, want one per distinct side", len(store.searches))
	}
	for _, q := range store.searches {
		if q.Limit != 3 {
			t.Errorf("an arm asked for %d, want its share rounded up (3)", q.Limit)
		}
	}
	var order []string
	for _, p := range got.Passages {
		order = append(order, p.WorkTitle)
	}
	// The shared passage once - the second side's copy is the same passage -
	// then the sides alternating.
	if want := []string{"Shared", "Rust", "Cpp", "Rust", "Cpp"}; !reflect.DeepEqual(order, want) {
		t.Errorf("passages = %v, want %v", order, want)
	}
}

// One side failing must not lose the other; every side failing is a failure.
func TestAComparisonSurvivesOneFailedSide(t *testing.T) {
	route := routeJSON(domain.IntentFactualLookup, nil, []string{"left", "right"}, "")

	t.Run("one side fails", func(t *testing.T) {
		model := newFakeModel(t)
		model.route = route
		store := &pipelineStore{
			search: func(q postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
				if q.Text == "left" {
					return nil, errPipelineStore
				}
				return pipelinePassages("Right", 2), nil
			},
		}
		model.answer = "Right only [1]."
		a := &Answerer{Store: store, Chat: model.client("fake-chat")}
		got, err := a.Ask(t.Context(), "left or right?", Scope{})
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if len(got.Passages) != 2 || got.Passages[0].WorkTitle != "Right" {
			t.Errorf("passages = %+v, want the surviving side", got.Passages)
		}
	})

	t.Run("every side fails", func(t *testing.T) {
		model := newFakeModel(t)
		model.route = route
		store := &pipelineStore{
			search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) { return nil, errPipelineStore },
		}
		a := &Answerer{Store: store, Chat: model.client("fake-chat")}
		if _, err := a.Ask(t.Context(), "left or right?", Scope{}); err == nil {
			t.Error("a comparison with no evidence from any side succeeded")
		}
	})
}

func TestInterleave(t *testing.T) {
	a, b := pipelinePassages("A", 3), pipelinePassages("B", 1)

	got := interleave([][]domain.RetrievedChunk{a, b}, 10)
	var order []string
	for _, p := range got {
		order = append(order, p.WorkTitle+strings.TrimPrefix(p.Chunk.ChapterTitle, "Chapter "))
	}
	if want := []string{"A1", "B1", "A2", "A3"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v: a short arm simply stops contributing", order, want)
	}

	if got := interleave([][]domain.RetrievedChunk{a, b}, 2); len(got) != 2 {
		t.Errorf("limit 2 returned %d", len(got))
	}
	if got := interleave(nil, 5); len(got) != 0 {
		t.Errorf("no arms returned %d", len(got))
	}
}

// The meaning arm: with an embedding model, the question goes to the search
// as a vector as well as words, tagged with the model that made it.
func TestRetrievalEmbedsTheQuestionWhenItCan(t *testing.T) {
	model := newFakeModel(t)
	model.embedDims = 4
	store := &pipelineStore{}
	a := &Answerer{Store: store, Embed: model.client("fake-embed"), Dimensions: 4}

	if _, err := a.Ask(t.Context(), "what is a monad?", Scope{}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	q := store.searches[0]
	if len(q.Vector) != 4 || q.ModelID != "fake-embed" {
		t.Errorf("Vector = %v, ModelID = %q", q.Vector, q.ModelID)
	}
	if !reflect.DeepEqual(model.embedInputs, []string{"what is a monad?"}) {
		t.Errorf("embedded %q", model.embedInputs)
	}
}

// An embedding model that is down narrows the search to keywords; it does not
// fail the question.
func TestRetrievalFallsBackToKeywordsWhenEmbeddingFails(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		dims   int
	}{
		{"the server refuses", http.StatusBadRequest, 4},
		{"the vector is the wrong width", 0, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model := newFakeModel(t)
			model.embedStatus = tt.status
			model.embedDims = tt.dims
			store := &pipelineStore{
				search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
					return pipelinePassages("SICP", 1), nil
				},
			}
			a := &Answerer{Store: store, Embed: model.client("fake-embed"), Dimensions: 4}
			got, err := a.Ask(t.Context(), "q", Scope{})
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if store.searches[0].Vector != nil || store.searches[0].ModelID != "" {
				t.Errorf("a vector was sent after embedding failed: %+v", store.searches[0])
			}
			if len(got.Passages) != 1 {
				t.Errorf("Passages = %d, want the keyword results", len(got.Passages))
			}
		})
	}
}

// With a reranker the search reads a deeper pool, and the cross-encoder's
// order decides which passages make the answer.
func TestRetrievalRerankIsDeepThenNarrow(t *testing.T) {
	model := newFakeModel(t)
	// The fused order is 1..12; the reranker prefers the last ones.
	model.rerank = func(doc string) float64 {
		var n int
		_, _ = fmt.Sscanf(doc[strings.Index(doc, "passage ")+len("passage "):], "%d", &n)
		return float64(n)
	}
	store := &pipelineStore{
		search: func(q postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			return pipelinePassages("SICP", 12), nil
		},
	}
	a := &Answerer{Store: store, Reranker: model.client("fake-rerank"), MaxPassages: 3}

	got, err := a.Ask(t.Context(), "q", Scope{})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if store.searches[0].Limit != RerankPool {
		t.Errorf("search limit = %d, want the rerank pool %d", store.searches[0].Limit, RerankPool)
	}
	var chapters []string
	for _, p := range got.Passages {
		chapters = append(chapters, p.Chunk.ChapterTitle)
	}
	if want := []string{"Chapter 12", "Chapter 11", "Chapter 10"}; !reflect.DeepEqual(chapters, want) {
		t.Errorf("kept %v, want the reranker's top three %v", chapters, want)
	}
	if len(model.rerankDocs) != 1 || len(model.rerankDocs[0]) != 12 {
		t.Errorf("reranker saw %v", model.rerankDocs)
	}
}

func TestSourceLabel(t *testing.T) {
	tests := []struct {
		name string
		p    domain.RetrievedChunk
		want string
	}{
		{"title alone", domain.RetrievedChunk{WorkTitle: "Dune"}, "Dune"},
		{"with chapter", domain.RetrievedChunk{WorkTitle: "Dune",
			Chunk: domain.Chunk{ChapterTitle: "Book One"}}, "Dune, Book One"},
		{"a PDF page", domain.RetrievedChunk{WorkTitle: "SICP", Chunk: domain.Chunk{
			ChapterTitle: "1.1 The Elements",
			Range:        domain.Range{Start: domain.Locator{Format: domain.FormatPDF, Page: 57}},
		}}, "SICP, 1.1 The Elements, page 57"},
		{"an EPUB has no page", domain.RetrievedChunk{WorkTitle: "Dune", Chunk: domain.Chunk{
			Range: domain.Range{Start: domain.Locator{Format: domain.FormatEPUB, Page: 3}},
		}}, "Dune"},
	}
	for _, tt := range tests {
		if got := SourceLabel(tt.p); got != tt.want {
			t.Errorf("%s: SourceLabel = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestClipRunes(t *testing.T) {
	if got := clipRunes("short", 10); got != "short" {
		t.Errorf("a short string was clipped: %q", got)
	}
	// Cut on the last word boundary in reach, and say so.
	if got := clipRunes("the quick brown fox jumps", 18); got != "the quick brown…" {
		t.Errorf("clipRunes = %q", got)
	}
	// No boundary near enough: cut at the count, in runes, never mid-rune.
	if got := clipRunes("ééééééééééé", 4); got != "éééé…" {
		t.Errorf("clipRunes = %q", got)
	}
}
