// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// Conversation is a thread of questions about the library.
type Conversation struct {
	ID     uuid.UUID
	UserID uuid.UUID
	Title  string
	// BookFileID pins every question in the thread to one book.
	BookFileID *uuid.UUID
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// Messages is the thread, oldest first. Empty unless asked for.
	Messages []Message
}

// Message is one turn.
type Message struct {
	ID   uuid.UUID
	Seq  int
	Role string
	Text string

	// How an assistant turn was produced, so a disappointing answer can be
	// diagnosed rather than guessed at.
	Intent           domain.QueryIntent
	ModelID          string
	PromptTokens     int
	CompletionTokens int
	RetrievalMS      int
	GenerationMS     int
	Unsupported      bool

	CreatedAt time.Time
	// Citations are the passages this turn drew on, in the order shown.
	Citations []Citation
}

// Citation links an assistant turn to the passage it used.
type Citation struct {
	ChunkID  uuid.UUID
	Position int
	Rank     int
	Score    float64

	// Denormalised for display, so showing a conversation does not need a
	// second round of lookups per citation.
	WorkID     uuid.UUID
	BookFileID uuid.UUID
	WorkTitle  string
	Chapter    string
	Locator    string
	Excerpt    string
}

// CreateConversation starts a thread.
func (p *Pool) CreateConversation(ctx context.Context, userID uuid.UUID, title string, bookFileID *uuid.UUID) (Conversation, error) {
	var c Conversation
	err := p.QueryRow(ctx, `
		INSERT INTO conversations (user_id, title, book_file_id)
		VALUES ($1, NULLIF($2, ''), $3)
		RETURNING id, user_id, COALESCE(title, ''), book_file_id, created_at, updated_at`,
		userID, strings.TrimSpace(title), bookFileID,
	).Scan(&c.ID, &c.UserID, &c.Title, &c.BookFileID, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return Conversation{}, fmt.Errorf("creating conversation: %w", mapError(err))
	}
	return c, nil
}

// AppendMessage adds a turn, numbering it after the last one.
//
// The sequence is assigned here rather than by the caller, inside the same
// statement that inserts, so two questions asked at once cannot both think they
// are turn four.
func (p *Pool) AppendMessage(ctx context.Context, conversationID uuid.UUID, m Message) (Message, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return Message{}, fmt.Errorf("appending message: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var intent *string
	if m.Intent != "" {
		s := string(m.Intent)
		intent = &s
	}

	var out Message
	var storedIntent *string
	err = tx.QueryRow(ctx, `
		INSERT INTO messages
			(conversation_id, seq, role, content, intent, model_id,
			 prompt_tokens, completion_tokens, retrieval_ms, generation_ms, unsupported)
		VALUES (
			$1,
			(SELECT COALESCE(max(seq) + 1, 0) FROM messages WHERE conversation_id = $1),
			$2, $3, $4::query_intent, NULLIF($5, ''), $6, $7, $8, $9, $10
		)
		RETURNING id, seq, role, content, intent, COALESCE(model_id, ''),
		          COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0),
		          COALESCE(retrieval_ms, 0), COALESCE(generation_ms, 0),
		          unsupported, created_at`,
		conversationID, m.Role, m.Text, intent, m.ModelID,
		nullableInt(m.PromptTokens), nullableInt(m.CompletionTokens),
		nullableInt(m.RetrievalMS), nullableInt(m.GenerationMS), m.Unsupported,
	).Scan(&out.ID, &out.Seq, &out.Role, &out.Text, &storedIntent, &out.ModelID,
		&out.PromptTokens, &out.CompletionTokens, &out.RetrievalMS, &out.GenerationMS,
		&out.Unsupported, &out.CreatedAt)
	if err != nil {
		return Message{}, fmt.Errorf("appending message: %w", mapError(err))
	}
	if storedIntent != nil {
		out.Intent = domain.QueryIntent(*storedIntent)
	}

	for _, c := range m.Citations {
		// The book, the place and enough text to recognise are copied onto the
		// citation rather than left in the chunk. Re-chunking deletes every
		// chunk of a book, and a citation that cannot outlive that is a
		// citation that disappears the first time retrieval is improved.
		if _, err := tx.Exec(ctx, `
			INSERT INTO citations
				(message_id, chunk_id, position, rank, score,
				 book_file_id, start_major, start_path, start_offset,
				 chapter_title, excerpt)
			SELECT $1, $2, $3, $4, $5,
			       c.book_file_id, c.start_major, c.start_path, c.start_offset,
			       c.chapter_title, left(c.content, 400)
			FROM chunks c WHERE c.id = $2
			ON CONFLICT (message_id, chunk_id) DO NOTHING`,
			out.ID, c.ChunkID, c.Position, c.Rank, c.Score); err != nil {
			return Message{}, fmt.Errorf("recording citation: %w", mapError(err))
		}
	}
	out.Citations = m.Citations

	// The thread's own timestamp moves, so a list of conversations is ordered
	// by when they were last used rather than when they were started.
	if _, err := tx.Exec(ctx,
		`UPDATE conversations SET updated_at = now() WHERE id = $1`, conversationID); err != nil {
		return Message{}, fmt.Errorf("touching conversation: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return Message{}, fmt.Errorf("appending message: %w", mapError(err))
	}
	return out, nil
}

// Conversation reads a thread and its messages, scoped to its owner.
func (p *Pool) Conversation(ctx context.Context, userID, id uuid.UUID) (Conversation, error) {
	var c Conversation
	err := p.QueryRow(ctx, `
		SELECT id, user_id, COALESCE(title, ''), book_file_id, created_at, updated_at
		FROM conversations WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&c.ID, &c.UserID, &c.Title, &c.BookFileID, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return Conversation{}, mapError(err)
	}

	rows, err := p.Query(ctx, `
		SELECT id, seq, role, content, intent, COALESCE(model_id, ''),
		       COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0),
		       COALESCE(retrieval_ms, 0), COALESCE(generation_ms, 0),
		       unsupported, created_at
		FROM messages WHERE conversation_id = $1
		ORDER BY seq`, id)
	if err != nil {
		return Conversation{}, fmt.Errorf("reading messages: %w", mapError(err))
	}
	defer rows.Close()

	byID := make(map[uuid.UUID]int)
	for rows.Next() {
		var m Message
		var intent *string
		if err := rows.Scan(&m.ID, &m.Seq, &m.Role, &m.Text, &intent, &m.ModelID,
			&m.PromptTokens, &m.CompletionTokens, &m.RetrievalMS, &m.GenerationMS,
			&m.Unsupported, &m.CreatedAt); err != nil {
			return Conversation{}, fmt.Errorf("scanning message: %w", err)
		}
		if intent != nil {
			m.Intent = domain.QueryIntent(*intent)
		}
		byID[m.ID] = len(c.Messages)
		c.Messages = append(c.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return Conversation{}, fmt.Errorf("reading messages: %w", mapError(err))
	}

	if len(c.Messages) == 0 {
		return c, nil
	}

	// Citations in one query rather than one per message: a long thread would
	// otherwise be dozens of round trips to show.
	citeRows, err := p.Query(ctx, `
		-- Read from the citation's own copy, not from the chunk. The chunk may
		-- have been re-divided or deleted since the answer was given; what was
		-- cited has not changed.
		SELECT ct.message_id, COALESCE(ct.chunk_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       ct.position, COALESCE(ct.rank, 0), COALESCE(ct.score, 0),
		       ed.work_id, ct.book_file_id, w.title, COALESCE(ct.chapter_title, ''),
		       COALESCE(ct.start_major, 0), COALESCE(ct.start_path, ''),
		       COALESCE(ct.start_offset, 0), bf.format,
		       COALESCE(ct.excerpt, '')
		FROM citations ct
		JOIN messages m ON m.id = ct.message_id
		JOIN book_files bf ON bf.id = ct.book_file_id
		JOIN editions ed ON ed.id = bf.edition_id
		JOIN works w ON w.id = ed.work_id
		WHERE m.conversation_id = $1
		ORDER BY ct.message_id, ct.position`, id)
	if err != nil {
		return Conversation{}, fmt.Errorf("reading citations: %w", mapError(err))
	}
	defer citeRows.Close()

	for citeRows.Next() {
		var messageID uuid.UUID
		var cite Citation
		var major int
		var path string
		var offset int
		var format domain.Format

		if err := citeRows.Scan(&messageID, &cite.ChunkID, &cite.Position, &cite.Rank, &cite.Score,
			&cite.WorkID, &cite.BookFileID, &cite.WorkTitle, &cite.Chapter,
			&major, &path, &offset, &format, &cite.Excerpt); err != nil {
			return Conversation{}, fmt.Errorf("scanning citation: %w", err)
		}

		locator := domain.Locator{Format: format, ElementPath: path, CharOffset: offset}
		if format == domain.FormatPDF {
			locator.Page = major
		} else {
			locator.SpineIndex = major
		}
		cite.Locator = locator.String()

		if idx, ok := byID[messageID]; ok {
			c.Messages[idx].Citations = append(c.Messages[idx].Citations, cite)
		}
	}
	return c, citeRows.Err()
}

// Conversations lists a user's threads, most recently used first.
func (p *Pool) Conversations(ctx context.Context, userID uuid.UUID, limit int) ([]Conversation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := p.Query(ctx, `
		SELECT id, user_id, COALESCE(title, ''), book_file_id, created_at, updated_at
		FROM conversations
		WHERE user_id = $1
		ORDER BY updated_at DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("reading conversations: %w", mapError(err))
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.UserID, &c.Title, &c.BookFileID,
			&c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning conversation: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConversation removes a thread, scoped to its owner.
func (p *Pool) DeleteConversation(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := p.Exec(ctx,
		`DELETE FROM conversations WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("deleting conversation: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// SetConversationTitle renames a thread.
func (p *Pool) SetConversationTitle(ctx context.Context, userID, id uuid.UUID, title string) error {
	tag, err := p.Exec(ctx, `
		UPDATE conversations SET title = NULLIF($3, '')
		WHERE id = $1 AND user_id = $2`, id, userID, strings.TrimSpace(title))
	if err != nil {
		return fmt.Errorf("renaming conversation: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// nullableInt keeps a zero out of a column where zero and "not recorded" are
// different claims.
func nullableInt(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}
