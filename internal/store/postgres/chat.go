// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	// Works is the reading list a find-book or broad-topic turn answered
	// with, in the order it was shown. Only the ids are written; reading a
	// conversation fills in the rest, and leaves out any book its owner may
	// no longer read.
	Works []ListedWork
	// ScopedTo is the book the turn's search was narrowed to because the
	// question named it. Nil when the whole library was searched, and read
	// back as nil once its owner may no longer read that book.
	ScopedTo *ListedWork
}

// ListedWork is one book in a turn's reading list.
type ListedWork struct {
	WorkID  uuid.UUID
	Title   string
	Authors []string
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
	// Readable is whether the conversation's owner may open the cited book
	// now. A citation outlives access to its book - it is their record of
	// what an answer said - but what is no longer theirs to read is not
	// carried into what the model is told next.
	Readable bool
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

	var scoped *uuid.UUID
	if m.ScopedTo != nil {
		scoped = &m.ScopedTo.WorkID
	}

	var out Message
	var storedIntent *string
	err = tx.QueryRow(ctx, `
		INSERT INTO messages
			(conversation_id, seq, role, content, intent, model_id,
			 prompt_tokens, completion_tokens, retrieval_ms, generation_ms, unsupported,
			 scoped_work_id)
		VALUES (
			$1,
			(SELECT COALESCE(max(seq) + 1, 0) FROM messages WHERE conversation_id = $1),
			$2, $3, $4::query_intent, NULLIF($5, ''), $6, $7, $8, $9, $10,
			-- A work deleted since the search is not recorded, rather than
			-- failing the turn: the answer was given either way.
			(SELECT w.id FROM works w WHERE w.id = $11)
		)
		RETURNING id, seq, role, content, intent, COALESCE(model_id, ''),
		          COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0),
		          COALESCE(retrieval_ms, 0), COALESCE(generation_ms, 0),
		          unsupported, created_at`,
		conversationID, m.Role, m.Text, intent, m.ModelID,
		nullableInt(m.PromptTokens), nullableInt(m.CompletionTokens),
		nullableInt(m.RetrievalMS), nullableInt(m.GenerationMS), m.Unsupported, scoped,
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

	// The reading list, in the order shown. A work deleted since it was
	// listed is simply not recorded rather than failing the turn: the
	// answer was given either way.
	for i, w := range m.Works {
		if _, err := tx.Exec(ctx, `
			INSERT INTO message_works (message_id, work_id, position)
			SELECT $1, w.id, $3 FROM works w WHERE w.id = $2
			ON CONFLICT (message_id, work_id) DO NOTHING`,
			out.ID, w.WorkID, i); err != nil {
			return Message{}, fmt.Errorf("recording a listed book: %w", mapError(err))
		}
	}
	out.Works = m.Works
	out.ScopedTo = m.ScopedTo

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
		SELECT m.id, m.seq, m.role, m.content, m.intent, COALESCE(m.model_id, ''),
		       COALESCE(m.prompt_tokens, 0), COALESCE(m.completion_tokens, 0),
		       COALESCE(m.retrieval_ms, 0), COALESCE(m.generation_ms, 0),
		       m.unsupported, m.created_at,
		       sw.id, COALESCE(sw.title, ''),
		       COALESCE((SELECT array_agg(a.name ORDER BY wa.position)
		                 FROM work_authors wa JOIN authors a ON a.id = wa.author_id
		                 WHERE wa.work_id = sw.id), '{}')
		FROM messages m
		-- Named only while the conversation's owner may still read it.
		LEFT JOIN works sw ON sw.id = m.scoped_work_id
		     AND EXISTS (SELECT 1 FROM readable_works rw
		                  WHERE rw.work_id = sw.id AND rw.user_id = $2)
		WHERE m.conversation_id = $1
		ORDER BY m.seq`, id, userID)
	if err != nil {
		return Conversation{}, fmt.Errorf("reading messages: %w", mapError(err))
	}
	defer rows.Close()

	byID := make(map[uuid.UUID]int)
	for rows.Next() {
		var m Message
		var intent *string
		var scopedID *uuid.UUID
		var scoped ListedWork
		if err := rows.Scan(&m.ID, &m.Seq, &m.Role, &m.Text, &intent, &m.ModelID,
			&m.PromptTokens, &m.CompletionTokens, &m.RetrievalMS, &m.GenerationMS,
			&m.Unsupported, &m.CreatedAt, &scopedID, &scoped.Title, &scoped.Authors); err != nil {
			return Conversation{}, fmt.Errorf("scanning message: %w", err)
		}
		if scopedID != nil {
			scoped.WorkID = *scopedID
			m.ScopedTo = &scoped
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

	if err := p.readListedWorks(ctx, &c, byID); err != nil {
		return Conversation{}, err
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
		       COALESCE(ct.excerpt, ''),
		       EXISTS (SELECT 1 FROM readable_works rw
		                WHERE rw.work_id = ed.work_id AND rw.user_id = $2)
		FROM citations ct
		JOIN messages m ON m.id = ct.message_id
		JOIN book_files bf ON bf.id = ct.book_file_id
		JOIN editions ed ON ed.id = bf.edition_id
		JOIN works w ON w.id = ed.work_id
		WHERE m.conversation_id = $1
		ORDER BY ct.message_id, ct.position`, id, userID)
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
			&major, &path, &offset, &format, &cite.Excerpt, &cite.Readable); err != nil {
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

// readListedWorks fills in each turn's reading list.
//
// Through readable_works, as the conversation's owner: a list is a reference
// to books, and a book they may no longer open is no longer named to them -
// not on the screen, and not in the history a follow-up is routed with.
func (p *Pool) readListedWorks(ctx context.Context, c *Conversation, byID map[uuid.UUID]int) error {
	rows, err := p.Query(ctx, `
		SELECT mw.message_id, w.id, w.title,
		       COALESCE((SELECT array_agg(a.name ORDER BY wa.position)
		                 FROM work_authors wa JOIN authors a ON a.id = wa.author_id
		                 WHERE wa.work_id = w.id), '{}')
		FROM message_works mw
		JOIN messages m ON m.id = mw.message_id
		JOIN works w ON w.id = mw.work_id
		JOIN readable_works rw ON rw.work_id = w.id AND rw.user_id = $2
		WHERE m.conversation_id = $1
		ORDER BY mw.message_id, mw.position`, c.ID, c.UserID)
	if err != nil {
		return fmt.Errorf("reading listed books: %w", mapError(err))
	}
	defer rows.Close()

	for rows.Next() {
		var messageID uuid.UUID
		var w ListedWork
		if err := rows.Scan(&messageID, &w.WorkID, &w.Title, &w.Authors); err != nil {
			return fmt.Errorf("scanning a listed book: %w", err)
		}
		if idx, ok := byID[messageID]; ok {
			c.Messages[idx].Works = append(c.Messages[idx].Works, w)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading listed books: %w", mapError(err))
	}
	return nil
}

// moveChatReferences carries what conversations say about works that are
// about to be deleted onto the work that replaces them, or clears it when
// nothing does. Called inside the transaction that deletes them, with the
// works already locked.
//
// A merge moves a reading list's entry and an answer's scope to the kept
// work, so a thread that listed one copy still lists the book. A deletion
// drops the entries and clears the scope, which the foreign keys would do by
// themselves; doing it here first takes the rows in a fixed order.
//
// The order is the one deleting a conversation takes: messages (by id), then
// message_works. Left to the cascade from works, a work deletion took
// message_works first and messages second, the opposite of a conversation
// deletion, and the two could deadlock on a thread holding both kinds of turn.
// See "Works and conversations" in holdings.go for what remains.
func moveChatReferences(ctx context.Context, tx pgx.Tx, from []uuid.UUID, to *uuid.UUID) error {
	if len(from) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		SELECT m.id FROM messages m
		 WHERE m.scoped_work_id = ANY($1)
		    OR m.id IN (SELECT mw.message_id FROM message_works mw WHERE mw.work_id = ANY($1))
		 ORDER BY m.id
		   FOR NO KEY UPDATE`, from); err != nil {
		return fmt.Errorf("locking conversations that name a book: %w", mapError(err))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE messages SET scoped_work_id = $2 WHERE scoped_work_id = ANY($1)`, from, to); err != nil {
		return fmt.Errorf("moving where answers looked: %w", mapError(err))
	}
	if to != nil {
		// Two copies on one list become one entry, at the earlier place.
		if _, err := tx.Exec(ctx, `
			INSERT INTO message_works (message_id, work_id, position)
			SELECT message_id, $2, min(position) FROM message_works
			 WHERE work_id = ANY($1)
			 GROUP BY message_id
			ON CONFLICT (message_id, work_id) DO NOTHING`, from, *to); err != nil {
			return fmt.Errorf("moving reading lists: %w", mapError(err))
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM message_works WHERE work_id = ANY($1)`, from); err != nil {
		return fmt.Errorf("clearing reading lists: %w", mapError(err))
	}
	return nil
}
