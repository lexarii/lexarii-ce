// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// ChatStore is what the conversation handlers need.
type ChatStore interface {
	CreateConversation(ctx context.Context, userID uuid.UUID, title string, bookFileID *uuid.UUID) (postgres.Conversation, error)
	Conversation(ctx context.Context, userID, id uuid.UUID) (postgres.Conversation, error)
	Conversations(ctx context.Context, userID uuid.UUID, limit int) ([]postgres.Conversation, error)
	AppendMessage(ctx context.Context, conversationID uuid.UUID, m postgres.Message) (postgres.Message, error)
	DeleteConversation(ctx context.Context, userID, id uuid.UUID) error
	SetConversationTitle(ctx context.Context, userID, id uuid.UUID, title string) error
}

// ChatHandler holds conversations with the library.
type ChatHandler struct {
	Store    ChatStore
	Answerer *rag.Answerer
}

// List serves GET /api/v1/conversations.
func (h *ChatHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	items, err := h.Store.Conversations(r.Context(), userID, 50)
	if err != nil {
		respondError(w, err)
		return
	}

	out := dto.ConversationList{Items: make([]dto.ConversationSummary, 0, len(items))}
	for _, c := range items {
		out.Items = append(out.Items, dto.ConversationSummary{
			ID:        c.ID.String(),
			Title:     c.Title,
			UpdatedAt: c.UpdatedAt,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// Get serves GET /api/v1/conversations/{id}.
func (h *ChatHandler) Get(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("conversation id must be a UUID", err))
		return
	}

	conversation, err := h.Store.Conversation(r.Context(), userID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("conversation"))
			return
		}
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, conversationDTO(conversation))
}

// Delete serves DELETE /api/v1/conversations/{id}.
func (h *ChatHandler) Delete(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("conversation id must be a UUID", err))
		return
	}

	if err := h.Store.DeleteConversation(r.Context(), userID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("conversation"))
			return
		}
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusNoContent, nil)
}

// Post serves POST /api/v1/conversations/{id}/messages, and
// POST /api/v1/conversations for the first question of a new thread.
//
// One endpoint for asking, because a question and its answer are one
// transaction from the user's point of view: they type, they wait, they read.
func (h *ChatHandler) Post(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	question, bookFileID, err := askBody(w, r)
	if err != nil {
		respondError(w, err)
		return
	}

	// An existing thread, or a new one titled with the question that started
	// it. A thread named after its first question is recognisable in a list
	// months later; "Conversation 7" is not.
	conversation, history, err := h.thread(r, userID, question, bookFileID)
	if err != nil {
		respondError(w, err)
		return
	}
	if conversation.BookFileID != nil {
		bookFileID = conversation.BookFileID
	}

	// The question is stored BEFORE the model is asked. A question that takes
	// thirty seconds and then fails should still be in the thread: the person
	// asked it, and losing it because the answer failed would be the rudest
	// possible outcome.
	if _, err := h.Store.AppendMessage(r.Context(), conversation.ID, postgres.Message{
		Role: "user",
		Text: question,
	}); err != nil {
		respondError(w, err)
		return
	}

	answer, err := h.Answerer.AskWithHistory(r.Context(), question,
		rag.Scope{BookFileID: bookFileID, UserID: userID}, history)
	if err != nil {
		respondError(w, err)
		return
	}

	stored, err := h.Store.AppendMessage(r.Context(), conversation.ID, assistantMessage(answer))
	if err != nil {
		respondError(w, err)
		return
	}

	out := answerDTO(answer)
	out.ConversationID = conversation.ID.String()
	out.MessageID = stored.ID.String()
	respondJSON(w, http.StatusOK, out)
}

// Stream serves POST /api/v1/conversations/{id}/messages/stream, and
// POST /api/v1/conversations/stream for a new thread.
//
// An answer takes fourteen to thirty-six seconds on a local model. The
// non-streaming endpoint stays, because it is simpler and it is what a script
// wants; this one exists for the person watching.
//
// The events, in order: `conversation` as soon as the thread exists, so a
// reload during the answer finds it; `passages` when retrieval finishes, which
// is useful on its own and arrives seconds before the first token; `delta`
// repeatedly; then `done` with the stored message, or `error`.
func (h *ChatHandler) Stream(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	question, bookFileID, err := askBody(w, r)
	if err != nil {
		respondError(w, err)
		return
	}

	// Everything that can fail with a status code happens BEFORE the stream
	// opens. Once the headers are sent a 404 can no longer be reported, and an
	// error event in a stream the client may not be reading is not the same
	// thing as a refused request.
	conversation, history, err := h.thread(r, userID, question, bookFileID)
	if err != nil {
		respondError(w, err)
		return
	}
	if conversation.BookFileID != nil {
		bookFileID = conversation.BookFileID
	}

	if _, err := h.Store.AppendMessage(r.Context(), conversation.ID, postgres.Message{
		Role: "user",
		Text: question,
	}); err != nil {
		respondError(w, err)
		return
	}

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Nginx buffers proxied responses by default, which would hold every token
	// until the answer finished and defeat the entire point.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	writeEvent(w, rc, "conversation", dto.ConversationSummary{
		ID:        conversation.ID.String(),
		Title:     conversation.Title,
		UpdatedAt: conversation.UpdatedAt,
	})

	answer, err := h.Answerer.AskStreaming(r.Context(), question,
		rag.Scope{BookFileID: bookFileID, UserID: userID}, history,
		func(passages []domain.RetrievedChunk) {
			writeEvent(w, rc, "passages", sourcesDTO(passages, nil))
		},
		func(delta string) {
			writeEvent(w, rc, "delta", dto.AnswerDelta{Text: delta})
		},
	)
	if err != nil {
		// The client going away is not an error worth reporting to it.
		if r.Context().Err() == nil {
			writeEvent(w, rc, "error", streamError(err))
		}
		return
	}

	// The answer is stored even if the reader has closed the tab: they asked,
	// the model answered, and the thread should hold it when they come back.
	stored, storeErr := h.Store.AppendMessage(context.WithoutCancel(r.Context()),
		conversation.ID, assistantMessage(answer))
	if storeErr != nil {
		writeEvent(w, rc, "error", streamError(storeErr))
		return
	}

	out := answerDTO(answer)
	out.ConversationID = conversation.ID.String()
	out.MessageID = stored.ID.String()
	writeEvent(w, rc, "done", out)
}

// thread finds the conversation a question belongs to, or starts one.
func (h *ChatHandler) thread(r *http.Request, userID uuid.UUID, question string,
	bookFileID *uuid.UUID) (postgres.Conversation, rag.History, error) {

	raw := r.PathValue("id")
	if raw == "" {
		c, err := h.Store.CreateConversation(r.Context(), userID, titleFor(question), bookFileID)
		return c, nil, err
	}

	id, err := uuid.Parse(raw)
	if err != nil {
		return postgres.Conversation{}, nil, Invalid("conversation id must be a UUID", err)
	}
	conversation, err := h.Store.Conversation(r.Context(), userID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return postgres.Conversation{}, nil, NotFound("conversation")
		}
		return postgres.Conversation{}, nil, err
	}

	var history rag.History
	for _, m := range conversation.Messages {
		history = append(history, historyTurn(m))
	}
	return conversation, history, nil
}

// historyTurn is a stored turn as a follow-up is routed and answered with it:
// the text, and the books it listed or cited, because a reading list has no
// text at all and "the book" in the next question means one of them.
func historyTurn(m postgres.Message) rag.Turn {
	turn := rag.Turn{Role: m.Role, Text: rag.StoredListText(m.Text, len(m.Works))}
	for _, w := range m.Works {
		turn.Listed = append(turn.Listed, rag.WorkLabel(w.Title, w.Authors))
	}
	seen := map[uuid.UUID]bool{}
	for _, c := range m.Citations {
		// A cited book they can no longer open - lent and taken back, let
		// go, archived - keeps its citation on their screen, where it is
		// their own record of what was said; its title is not carried into
		// the prompt for their next question.
		if seen[c.WorkID] || c.WorkTitle == "" || !c.Readable {
			continue
		}
		seen[c.WorkID] = true
		turn.Cited = append(turn.Cited, c.WorkTitle)
	}
	return turn
}

// askBody reads and validates a question.
func askBody(w http.ResponseWriter, r *http.Request) (string, *uuid.UUID, error) {
	var body dto.AskRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxQuestionBytes)).Decode(&body); err != nil {
		return "", nil, Invalid("body must be a question", err)
	}
	question := strings.TrimSpace(body.Question)
	if question == "" {
		return "", nil, Invalid("a question is required", nil)
	}
	if body.BookFileID == "" {
		return question, nil, nil
	}
	id, err := uuid.Parse(body.BookFileID)
	if err != nil {
		return "", nil, Invalid("book file id must be a UUID", err)
	}
	return question, &id, nil
}

// assistantMessage turns an answer into the turn that is stored.
func assistantMessage(a rag.Answer) postgres.Message {
	m := postgres.Message{
		Role:             "assistant",
		Text:             a.Text,
		Intent:           a.Route.Intent,
		ModelID:          a.Model,
		RetrievalMS:      a.RetrievalMS,
		GenerationMS:     a.GenerationMS,
		PromptTokens:     a.PromptTokens,
		CompletionTokens: a.CompletionTokens,
		// An answer built from no passages is unsupported by definition, and
		// recording that is how confabulation is caught later.
		Unsupported: len(a.Passages) == 0 && a.Route.Intent.NeedsRetrieval(),
	}

	// The reading list, so the thread remembers what it was told and a
	// follow-up can refer to it.
	for _, b := range a.Books {
		m.Works = append(m.Works, postgres.ListedWork{WorkID: b.WorkID, Title: b.Title, Authors: b.Authors})
	}

	if a.ScopedTo != nil {
		m.ScopedTo = &postgres.ListedWork{WorkID: a.ScopedTo.WorkID, Title: a.ScopedTo.Title,
			Authors: a.ScopedTo.Authors}
	}

	// Only what the answer actually cited is recorded as a citation. The rest
	// were retrieved and shown, but a citation is a claim about what was used.
	cited := make(map[int]bool, len(a.Cited))
	for _, n := range a.Cited {
		cited[n] = true
	}
	position := 0
	for i, p := range a.Passages {
		if !cited[i+1] {
			continue
		}
		m.Citations = append(m.Citations, postgres.Citation{
			ChunkID:  p.Chunk.ID,
			Position: position,
			Rank:     i + 1,
			Score:    p.FusedScore,
		})
		position++
	}
	return m
}

// titleFor names a thread after the question that started it.
func titleFor(question string) string {
	title := strings.Join(strings.Fields(question), " ")
	const maxTitle = 80
	if len(title) <= maxTitle {
		return title
	}

	cut := maxTitle
	for cut > 0 && title[cut]&0xC0 == 0x80 {
		cut--
	}
	if idx := strings.LastIndex(title[:cut], " "); idx > maxTitle/2 {
		cut = idx
	}
	return strings.TrimSpace(title[:cut]) + "…"
}

func conversationDTO(c postgres.Conversation) dto.Conversation {
	out := dto.Conversation{
		ID:        c.ID.String(),
		Title:     c.Title,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
		Messages:  make([]dto.Message, 0, len(c.Messages)),
	}
	if c.BookFileID != nil {
		out.BookFileID = c.BookFileID.String()
	}

	for _, m := range c.Messages {
		message := dto.Message{
			ID:           m.ID.String(),
			Seq:          m.Seq,
			Role:         m.Role,
			Content:      rag.StoredListText(m.Text, len(m.Works)),
			Intent:       string(m.Intent),
			Model:        m.ModelID,
			RetrievalMS:  m.RetrievalMS,
			GenerationMS: m.GenerationMS,
			Unsupported:  m.Unsupported,
			CreatedAt:    m.CreatedAt,
			Citations:    make([]dto.Source, 0, len(m.Citations)),
		}
		for _, c := range m.Citations {
			// Rank, not Position. Rank is the number the ANSWER TEXT used;
			// Position is where the citation sits in this compacted list of
			// the ones actually cited.
			//
			// They differ the moment the model skips a passage, which is
			// almost always. Cite [2] and [5] of ten and the stored rows are
			// (rank 2, position 0) and (rank 5, position 1); rendering
			// position+1 made the chip for [2] resolve to passage 5 - a
			// different book, a different locator, and a deep link that opens
			// the reader in the wrong place - while [5] matched nothing and
			// rendered as dead text. Live answers were right and only reopened
			// threads were wrong, which is why it went unseen. 38 of the 64
			// citations in this library were affected.
			number := c.Rank
			if number == 0 {
				number = c.Position + 1
			}
			message.Citations = append(message.Citations, dto.Source{
				Number:   number,
				Cited:    true,
				WorkID:   c.WorkID.String(),
				BookFile: c.BookFileID.String(),
				Title:    c.WorkTitle,
				Chapter:  c.Chapter,
				Locator:  c.Locator,
				Excerpt:  excerpt(c.Excerpt),
				Score:    c.Score,
			})
		}
		if m.ScopedTo != nil {
			message.ScopedTo = &dto.AnswerBook{
				WorkID:  m.ScopedTo.WorkID.String(),
				Title:   m.ScopedTo.Title,
				Authors: m.ScopedTo.Authors,
			}
		}
		for _, w := range m.Works {
			message.Books = append(message.Books, dto.AnswerBook{
				WorkID:  w.WorkID.String(),
				Title:   w.Title,
				Authors: w.Authors,
			})
		}
		out.Messages = append(out.Messages, message)
	}
	return out
}

// streamError is the `error` event: the same code and message a request that
// failed before the stream opened would have carried, because the status line
// has already gone. A wait the model server asked for travels in the details,
// as the Retry-After header would have.
//
// It used to carry err.Error(), which put the model server's own reply - and
// any internal error - in front of the person reading.
func streamError(err error) dto.ErrorDetail {
	f := describe(err)
	detail := f.detail()
	if f.retryAfter > 0 {
		detail.Details = map[string]any{"retryAfterSeconds": retryAfterSeconds(f.retryAfter)}
	}
	return detail
}
