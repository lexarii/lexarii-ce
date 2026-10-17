// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// askSearcherFake is the retrieval side of the answerer. With no model
// configured an answer is search with citations, which is exactly what is
// left to test here once the model is gone.
type askSearcherFake struct {
	found   []domain.RetrievedChunk
	err     error
	queries []postgres.SearchQuery
}

func (f *askSearcherFake) Search(_ context.Context, q postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
	f.queries = append(f.queries, q)
	return f.found, f.err
}

func (f *askSearcherFake) WorksMatchingSubjects(context.Context, []string, int) ([]uuid.UUID, error) {
	return nil, nil
}

func (f *askSearcherFake) Library(context.Context, postgres.LibraryQuery) (postgres.LibraryPage, error) {
	return postgres.LibraryPage{}, nil
}

func (f *askSearcherFake) SearchNotes(context.Context, uuid.UUID, string, int) ([]postgres.FoundNote, error) {
	return nil, nil
}

func (f *askSearcherFake) MatchingSubjects(context.Context, []string) ([]postgres.SubjectMatch, error) {
	return nil, nil
}

func (f *askSearcherFake) LibraryFacts(context.Context, uuid.UUID) (postgres.LibraryFacts, error) {
	return postgres.LibraryFacts{}, nil
}

func askRouter(f *askSearcherFake) *Router {
	return &Router{Ask: &AskHandler{Answerer: &rag.Answerer{Store: f}}}
}

func retrievedPassage(title, chapter, content string, dense, sparse int) domain.RetrievedChunk {
	return domain.RetrievedChunk{
		Chunk: domain.Chunk{ID: uuid.New(), BookFileID: uuid.New(), ChapterTitle: chapter, Content: content,
			Range: domain.Range{Start: domain.NewPDFLocator(12, 40), End: domain.NewPDFLocator(12, 400)}},
		WorkID: uuid.New(), WorkTitle: title, DenseRank: dense, SparseRank: sparse, FusedScore: 0.42,
	}
}

func TestAskValidatesBeforeAsking(t *testing.T) {
	user := uuid.New()
	for _, tc := range []struct {
		name, body string
		viewer     uuid.UUID
		status     int
	}{
		{"a body that is not JSON", `{"question":`, user, http.StatusBadRequest},
		{"no question", `{"question":"   "}`, user, http.StatusBadRequest},
		{"a question longer than a question", `{"question":"` + strings.Repeat("why ", 1100) + `"}`, user, http.StatusBadRequest},
		{"a book id that is not a UUID", `{"question":"why?","bookFileId":"chapter-3"}`, user, http.StatusBadRequest},
		{"nobody signed in", `{"question":"why?"}`, uuid.Nil, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &askSearcherFake{}
			rec := serveAsViewer(t, askRouter(f), tc.viewer, http.MethodPost, "/api/v1/ask", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if len(f.queries) != 0 {
				t.Error("a refused question was searched for")
			}
		})
	}
}

// With the model off, a question still gets its passages: the library works
// without the LLM, and only the written answer degrades.
func TestAskWithoutAModelIsSearchWithCitations(t *testing.T) {
	user, fileID := uuid.New(), uuid.New()
	passage := retrievedPassage("Linear Algebra Done Right", "Eigenvalues", "An  eigenvalue\nof an operator...", 1, 3)
	f := &askSearcherFake{found: []domain.RetrievedChunk{passage}}

	rec := serveAsViewer(t, askRouter(f), user, http.MethodPost, "/api/v1/ask",
		`{"question":"  what is an eigenvalue?  ","bookFileId":"`+fileID.String()+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	// Scoped to the asker's own library, and to the one book when asked.
	if len(f.queries) != 1 {
		t.Fatalf("searched %d times", len(f.queries))
	}
	q := f.queries[0]
	if q.ViewerID != user || q.BookFileID == nil || *q.BookFileID != fileID || q.Text != "what is an eigenvalue?" {
		t.Errorf("search = %+v", q)
	}

	out := decodeInto[dto.Answer](t, rec)
	if out.Question != "what is an eigenvalue?" || out.Answer != "" || out.Intent != string(domain.IntentFactualLookup) {
		t.Errorf("answer = %+v", out)
	}
	if len(out.Sources) != 1 {
		t.Fatalf("sources = %+v", out.Sources)
	}
	s := out.Sources[0]
	if s.Number != 1 || s.Cited || s.Kind != "passage" || s.WorkID != passage.WorkID.String() ||
		s.BookFile != passage.Chunk.BookFileID.String() || s.Locator != "pdf:12:40" ||
		s.Label != "Linear Algebra Done Right, Eigenvalues, page 12" || s.Chapter != "Eigenvalues" ||
		s.Excerpt != "An eigenvalue of an operator..." || s.FoundBy != "both" || s.Score != 0.42 {
		t.Errorf("source = %+v", s)
	}
	if out.Books == nil || len(out.Books) != 0 {
		t.Errorf("books = %#v, want an empty list", out.Books)
	}
}

func TestAskWithNothingFoundSaysSo(t *testing.T) {
	f := &askSearcherFake{}
	rec := serveAsViewer(t, askRouter(f), uuid.New(), http.MethodPost, "/api/v1/ask", `{"question":"who wrote the Necronomicon?"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := decodeInto[dto.Answer](t, rec)
	if out.Answer != rag.NothingFound || len(out.Sources) != 0 {
		t.Errorf("answer = %+v, want the plain statement that nothing was found", out)
	}
	if f.queries[0].BookFileID != nil {
		t.Error("an unscoped question was pinned to a book")
	}
}

func TestAskReportsARetrievalFailure(t *testing.T) {
	f := &askSearcherFake{err: errors.New("index unavailable")}
	rec := serveAsViewer(t, askRouter(f), uuid.New(), http.MethodPost, "/api/v1/ask", `{"question":"why?"}`)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "index unavailable") {
		t.Fatalf("status = %d, body = %s; want a 500 that says nothing internal", rec.Code, rec.Body)
	}
}

// The reader's own notes are numbered after the passages, in the one sequence
// the answer cites from.
func TestAnswerSourcesNumberNotesAfterPassages(t *testing.T) {
	passages := []domain.RetrievedChunk{
		retrievedPassage("A", "", "meaning only", 2, 0),
		retrievedPassage("B", "", "keyword only", 0, 5),
		retrievedPassage("C", "", "neither, somehow", 0, 0),
	}
	note := postgres.FoundNote{
		Annotation: domain.Annotation{Note: "my   own\nthought",
			Range: domain.Range{Start: domain.NewEPUBLocator(4, "p[2]", 9)}},
		BookFileID: uuid.New(), WorkID: uuid.New(), Title: "SICP",
	}
	answer := rag.Answer{
		Question: "q", Text: "See [2] and [4].",
		Route:    llm.Route{Intent: domain.IntentFactualLookup, Subjects: []string{"lisp"}},
		Passages: passages, Notes: []postgres.FoundNote{note}, Cited: []int{2, 4},
		Books: []postgres.LibraryItem{{WorkID: uuid.New(), Title: "SICP", Authors: []string{"Abelson", "Sussman"}}},
		Model: "local", PromptID: "answer/v9", RetrievalMS: 12, GenerationMS: 340,
	}

	out := answerDTO(answer)
	if len(out.Sources) != 4 {
		t.Fatalf("sources = %+v", out.Sources)
	}
	for i, want := range []struct {
		cited   bool
		foundBy string
		kind    string
	}{
		{false, "meaning", "passage"},
		{true, "keyword", "passage"},
		{false, "", "passage"},
		{true, "", "note"},
	} {
		s := out.Sources[i]
		if s.Number != i+1 || s.Cited != want.cited || s.FoundBy != want.foundBy || s.Kind != want.kind {
			t.Errorf("source %d = %+v, want cited=%v foundBy=%q kind=%q", i+1, s, want.cited, want.foundBy, want.kind)
		}
	}
	n := out.Sources[3]
	if n.Label != "your note on SICP" || n.Excerpt != "my own thought" || n.Locator != "epub:4/p[2]:9" ||
		n.WorkID != note.WorkID.String() || n.BookFile != note.BookFileID.String() {
		t.Errorf("note source = %+v", n)
	}
	if len(out.Books) != 1 || out.Books[0].Title != "SICP" || len(out.Books[0].Authors) != 2 {
		t.Errorf("books = %+v", out.Books)
	}
	if out.Model != "local" || out.PromptID != "answer/v9" || out.RetrievalMS != 12 || out.GenerationMS != 340 ||
		len(out.Subjects) != 1 || len(out.SearchedFor) != 1 || out.SearchedFor[0] != "q" {
		t.Errorf("answer metadata = %+v", out)
	}
}

// An excerpt is enough to recognise a passage, cut at a word and never in
// the middle of a character.
func TestExcerptCutsCleanly(t *testing.T) {
	if got := excerpt("  short\n\n passage "); got != "short passage" {
		t.Errorf("short = %q", got)
	}

	words := strings.Repeat("eigenvalue ", 60)
	got := excerpt(words)
	if !strings.HasSuffix(got, "…") || len(got) > excerptLength+len("…") || strings.HasSuffix(got, " …") {
		t.Errorf("long excerpt = %q", got)
	}
	for _, word := range strings.Fields(strings.TrimSuffix(got, "…")) {
		if word != "eigenvalue" {
			t.Fatalf("cut mid-word, leaving %q: %q", word, got)
		}
	}

	// No spaces to cut at, and a multi-byte character straddling the limit.
	dense := strings.Repeat("a", excerptLength-1) + "ééé"
	got = excerpt(dense)
	if !utf8.ValidString(got) {
		t.Fatalf("excerpt split a character: %q", got)
	}
	if !strings.HasSuffix(got, "…") || len(got) > excerptLength+len("…") {
		t.Errorf("dense excerpt = %q (%d bytes)", got, len(got))
	}
}
