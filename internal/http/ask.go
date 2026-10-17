// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// AskHandler answers questions from the library.
type AskHandler struct {
	Answerer *rag.Answerer
}

// maxQuestionBytes bounds a question.
//
// A question is a sentence. Anything longer is either a paste of a whole
// document - which belongs in the library, not in the question box - or an
// attempt to fill the context window.
const maxQuestionBytes = 4 << 10

// Ask serves POST /api/v1/ask.
func (h *AskHandler) Ask(w http.ResponseWriter, r *http.Request) {
	var body dto.AskRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxQuestionBytes)).Decode(&body); err != nil {
		respondError(w, Invalid("body must be a question", err))
		return
	}

	question := strings.TrimSpace(body.Question)
	if question == "" {
		respondError(w, Invalid("a question is required", nil))
		return
	}

	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	scope := rag.Scope{UserID: userID}
	if body.BookFileID != "" {
		id, err := uuid.Parse(body.BookFileID)
		if err != nil {
			respondError(w, Invalid("book file id must be a UUID", err))
			return
		}
		scope.BookFileID = &id
	}

	answer, err := h.Answerer.Ask(r.Context(), question, scope)
	if err != nil {
		respondError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, answerDTO(answer))
}

func answerDTO(a rag.Answer) dto.Answer {
	out := dto.Answer{
		Question:     a.Question,
		Answer:       a.Text,
		Intent:       string(a.Route.Intent),
		Subjects:     a.Route.Subjects,
		SearchedFor:  a.Route.Queries(a.Question),
		Model:        a.Model,
		PromptID:     a.PromptID,
		RetrievalMS:  a.RetrievalMS,
		GenerationMS: a.GenerationMS,
		Books:        make([]dto.AnswerBook, 0, len(a.Books)),
	}

	for _, b := range a.Books {
		out.Books = append(out.Books, dto.AnswerBook{
			WorkID:  b.WorkID.String(),
			Title:   b.Title,
			Authors: b.Authors,
		})
	}

	if a.ScopedTo != nil {
		out.ScopedTo = &dto.AnswerBook{
			WorkID:  a.ScopedTo.WorkID.String(),
			Title:   a.ScopedTo.Title,
			Authors: a.ScopedTo.Authors,
		}
	}

	out.Sources = sourcesDTO(a.Passages, a.Cited)
	// The reader's own notes come after the passages in the same numbering the
	// answer cites.
	out.Sources = append(out.Sources, noteSources(a.Notes, len(a.Passages), a.Cited)...)
	return out
}

// sourcesDTO renders the passages an answer was built from.
//
// Split out because a streaming answer sends these before the model has written
// a word - they are what the answer will be built from, they are useful on
// their own, and they are ready seconds earlier. At that point nothing has been
// cited yet, which is what a nil `cited` means.
func sourcesDTO(passages []domain.RetrievedChunk, cited []int) []dto.Source {
	marked := make(map[int]bool, len(cited))
	for _, n := range cited {
		marked[n] = true
	}

	out := make([]dto.Source, 0, len(passages))
	for i, p := range passages {
		number := i + 1
		out = append(out, dto.Source{
			Number:   number,
			Cited:    marked[number],
			WorkID:   p.WorkID.String(),
			Title:    p.WorkTitle,
			Label:    rag.SourceLabel(p),
			Chapter:  p.Chunk.ChapterTitle,
			Locator:  p.Chunk.Range.Start.String(),
			BookFile: p.Chunk.BookFileID.String(),
			Excerpt:  excerpt(p.Chunk.Content),
			Score:    p.FusedScore,
			FoundBy:  foundBy(p),
			Kind:     "passage",
		})
	}
	return out
}

// noteSources renders the reader's own marks as sources.
//
// Numbered after the passages, in one sequence, because that is what the
// answer cites: [9] is the ninth thing it was shown, and a reader following it
// should not have to know which sort of thing it turned out to be. The kind
// says which, so the screen can label it honestly.
func noteSources(notes []postgres.FoundNote, after int, cited []int) []dto.Source {
	marked := map[int]bool{}
	for _, n := range cited {
		marked[n] = true
	}

	out := make([]dto.Source, 0, len(notes))
	for i, n := range notes {
		number := after + i + 1
		out = append(out, dto.Source{
			Number:   number,
			Cited:    marked[number],
			WorkID:   n.WorkID.String(),
			Title:    n.Title,
			Label:    "your note on " + n.Title,
			Locator:  n.Annotation.Range.Start.String(),
			BookFile: n.BookFileID.String(),
			Excerpt:  excerpt(n.Annotation.Note),
			Kind:     "note",
		})
	}
	return out
}

// foundBy says which arm of the search found a passage.
//
// Shown because it is genuinely informative when an answer is wrong: a passage
// found only by keyword usually shares a word with the question rather than its
// meaning, and one found only by meaning may be about the right idea in the
// wrong words.
func foundBy(p domain.RetrievedChunk) string {
	switch {
	case p.DenseRank > 0 && p.SparseRank > 0:
		return "both"
	case p.DenseRank > 0:
		return "meaning"
	case p.SparseRank > 0:
		return "keyword"
	default:
		return ""
	}
}

// excerptLength bounds what is shown of a passage in the sources list.
//
// Enough to recognise the passage, not so much that the sources list becomes
// the answer. The whole passage is a click away in the reader.
const excerptLength = 320

func excerpt(content string) string {
	content = strings.Join(strings.Fields(content), " ")
	if len(content) <= excerptLength {
		return content
	}

	cut := excerptLength
	for cut > 0 && content[cut]&0xC0 == 0x80 {
		cut--
	}
	if idx := strings.LastIndex(content[:cut], " "); idx > excerptLength/2 {
		cut = idx
	}
	return strings.TrimSpace(content[:cut]) + "…"
}
