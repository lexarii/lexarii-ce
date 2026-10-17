// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

func chatUser(t *testing.T, p *Pool) uuid.UUID {
	t.Helper()
	cleanLibrary(t, p)

	id, err := p.EnsureDefaultUser(t.Context())
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}
	if _, err := p.Exec(t.Context(), `DELETE FROM conversations WHERE user_id = $1`, id); err != nil {
		t.Fatalf("clearing conversations: %v", err)
	}
	return id
}

func TestConversationRoundTrip(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)

	c, err := p.CreateConversation(t.Context(), userID, "what is an eigenvalue?", nil)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	if _, err := p.AppendMessage(t.Context(), c.ID, Message{
		Role: "user", Text: "what is an eigenvalue?",
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := p.AppendMessage(t.Context(), c.ID, Message{
		Role: "assistant", Text: "A value λ such that Av = λv [1].",
		Intent: domain.IntentFactualLookup, ModelID: "gemma4-12b",
		RetrievalMS: 200, GenerationMS: 17000,
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	read, err := p.Conversation(t.Context(), userID, c.ID)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if len(read.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(read.Messages))
	}
	if read.Messages[0].Seq != 0 || read.Messages[1].Seq != 1 {
		t.Errorf("sequence = %d, %d", read.Messages[0].Seq, read.Messages[1].Seq)
	}
	if read.Messages[0].Role != "user" || read.Messages[1].Role != "assistant" {
		t.Errorf("roles = %q, %q", read.Messages[0].Role, read.Messages[1].Role)
	}
	// How the answer was produced, so a disappointing one can be diagnosed.
	if read.Messages[1].Intent != domain.IntentFactualLookup {
		t.Errorf("intent = %q", read.Messages[1].Intent)
	}
	if read.Messages[1].ModelID != "gemma4-12b" || read.Messages[1].GenerationMS != 17000 {
		t.Errorf("provenance lost: %+v", read.Messages[1])
	}
}

// The sequence is assigned inside the insert, so two questions asked at once
// cannot both think they are turn four.
func TestMessageSequenceIsAssignedByTheStore(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)

	c, err := p.CreateConversation(t.Context(), userID, "thread", nil)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	seen := map[int]bool{}
	for i := 0; i < 6; i++ {
		m, err := p.AppendMessage(t.Context(), c.ID, Message{Role: "user", Text: "q"})
		if err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
		if seen[m.Seq] {
			t.Fatalf("sequence %d was handed out twice", m.Seq)
		}
		seen[m.Seq] = true
	}
}

// A citation is the trust mechanism: it links an answer to a passage, and
// transitively to a locator the reader can open.
func TestCitationsCarryEnoughToOpenTheBook(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)

	// A real book, chunked, so a citation has something to point at.
	out, err := p.UpsertBookFile(t.Context(),
		sampleInput(hashOf("c"), "/library/cited.epub", "The Cited Book", "An Author"))
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if _, err := p.SaveChunks(t.Context(), out.BookFileID, []ChunkInput{{
		Seq: 0,
		Start: domain.Locator{Format: domain.FormatEPUB, SpineIndex: 4,
			ElementPath: "section[1]/p[2]", CharOffset: 0},
		End: domain.Locator{Format: domain.FormatEPUB, SpineIndex: 4,
			ElementPath: "section[1]/p[2]", CharOffset: 120},
		ChapterTitle: "Eigenvalues",
		Content:      "A value λ such that Av = λv, for some non-zero vector v.",
		TokenCount:   14,
	}}, 1); err != nil {
		t.Fatalf("SaveChunks: %v", err)
	}

	var chunkID uuid.UUID
	if err := p.QueryRow(t.Context(),
		`SELECT id FROM chunks WHERE book_file_id = $1`, out.BookFileID).Scan(&chunkID); err != nil {
		t.Fatalf("reading chunk: %v", err)
	}

	c, err := p.CreateConversation(t.Context(), userID, "thread", nil)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := p.AppendMessage(t.Context(), c.ID, Message{
		Role: "assistant", Text: "A value λ such that Av = λv [1].",
		Citations: []Citation{{ChunkID: chunkID, Position: 0, Rank: 1, Score: 0.03}},
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	read, err := p.Conversation(t.Context(), userID, c.ID)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if len(read.Messages) != 1 || len(read.Messages[0].Citations) != 1 {
		t.Fatalf("citations = %+v", read.Messages)
	}

	cite := read.Messages[0].Citations[0]
	if cite.WorkTitle != "The Cited Book" {
		t.Errorf("title = %q", cite.WorkTitle)
	}
	if cite.Chapter != "Eigenvalues" {
		t.Errorf("chapter = %q", cite.Chapter)
	}
	// The locator is what makes "jump to this passage" work at all.
	if cite.Locator != "epub:4/section[1]/p[2]:0" {
		t.Errorf("locator = %q", cite.Locator)
	}
	if cite.Excerpt == "" {
		t.Error("no excerpt to show")
	}
}

// A conversation belongs to the person who had it.
func TestConversationsArePerUser(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)

	c, err := p.CreateConversation(t.Context(), userID, "mine", nil)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	stranger := uuid.New()
	if _, err := p.Conversation(t.Context(), stranger, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a stranger read someone else's conversation: %v", err)
	}
	if err := p.DeleteConversation(t.Context(), stranger, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a stranger deleted someone else's conversation: %v", err)
	}
	if list, err := p.Conversations(t.Context(), stranger, 10); err != nil || len(list) != 0 {
		t.Errorf("a stranger listed %d conversations (err %v)", len(list), err)
	}

	if err := p.DeleteConversation(t.Context(), userID, c.ID); err != nil {
		t.Errorf("the owner could not delete: %v", err)
	}
}

// A list is ordered by when a thread was last used, not when it was started.
func TestConversationsAreOrderedByRecentUse(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)

	first, _ := p.CreateConversation(t.Context(), userID, "older", nil)
	second, _ := p.CreateConversation(t.Context(), userID, "newer", nil)

	// Using the older one moves it to the top.
	if _, err := p.AppendMessage(t.Context(), first.ID, Message{Role: "user", Text: "q"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	list, err := p.Conversations(t.Context(), userID, 10)
	if err != nil {
		t.Fatalf("Conversations: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d conversations", len(list))
	}
	if list[0].ID != first.ID {
		t.Errorf("order puts %q first; the recently used thread should lead", list[0].Title)
	}
	_ = second
}

func TestDeletingAConversationTakesItsMessages(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)

	c, _ := p.CreateConversation(t.Context(), userID, "doomed", nil)
	if _, err := p.AppendMessage(t.Context(), c.ID, Message{Role: "user", Text: "q"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := p.DeleteConversation(t.Context(), userID, c.ID); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}

	var count int
	if err := p.QueryRow(t.Context(),
		`SELECT count(*) FROM messages WHERE conversation_id = $1`, c.ID).Scan(&count); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != 0 {
		t.Errorf("%d messages survived their conversation", count)
	}
}

// A citation has to survive its book being re-chunked.
//
// SaveChunks deletes every chunk of a book before writing the new ones, and
// BookFilesNeedingChunks queues exactly that for the whole library whenever
// chunk_strategy_version changes - so re-chunking is a routine consequence of
// improving retrieval. The citation rows cascaded away with the chunks: the
// answers stayed and the evidence beneath them vanished, which turns months of
// answered questions into unsupported assertions on an ordinary upgrade.
func TestACitationSurvivesReChunking(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)
	ctx := t.Context()

	book, err := p.UpsertBookFile(ctx, sampleInput(hashOf("c"), "/library/cited.epub",
		"The Cited Book", "An Author"))
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if _, err := p.SaveChunks(ctx, book.BookFileID, []ChunkInput{{
		Seq:          0,
		Start:        domain.Locator{Format: domain.FormatEPUB, SpineIndex: 4, ElementPath: "p[2]"},
		End:          domain.Locator{Format: domain.FormatEPUB, SpineIndex: 4, ElementPath: "p[2]", CharOffset: 120},
		ChapterTitle: "Eigenvalues",
		Content:      "A value λ such that Av = λv, for some non-zero vector v.",
		TokenCount:   14,
	}}, 1); err != nil {
		t.Fatalf("SaveChunks: %v", err)
	}

	var chunkID uuid.UUID
	if err := p.QueryRow(ctx,
		`SELECT id FROM chunks WHERE book_file_id = $1`, book.BookFileID).Scan(&chunkID); err != nil {
		t.Fatalf("reading the chunk: %v", err)
	}

	conversation, err := p.CreateConversation(ctx, userID, "a thread", nil)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := p.AppendMessage(ctx, conversation.ID, Message{
		Role: "assistant", Text: "An eigenvalue is defined this way [1].",
		Citations: []Citation{{ChunkID: chunkID, Position: 0, Rank: 1, Score: 0.9}},
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// The whole library is re-chunked under a new strategy.
	if _, err := p.SaveChunks(ctx, book.BookFileID, []ChunkInput{{
		Seq:        0,
		Start:      domain.Locator{Format: domain.FormatEPUB, SpineIndex: 4, ElementPath: "p[1]"},
		End:        domain.Locator{Format: domain.FormatEPUB, SpineIndex: 4, ElementPath: "p[3]"},
		Content:    "A much longer chunk covering the whole section.",
		TokenCount: 40,
	}}, 2); err != nil {
		t.Fatalf("re-chunking: %v", err)
	}

	reopened, err := p.Conversation(ctx, userID, conversation.ID)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if len(reopened.Messages) != 1 {
		t.Fatalf("messages = %d, want the answer to still be there", len(reopened.Messages))
	}
	citations := reopened.Messages[0].Citations
	if len(citations) != 1 {
		t.Fatalf("citations = %d after re-chunking, want the evidence to have survived", len(citations))
	}
	if citations[0].WorkTitle != "The Cited Book" {
		t.Errorf("title = %q, want the book it came from", citations[0].WorkTitle)
	}
	if citations[0].Locator == "" {
		t.Error("the citation lost its locator; following it would open the book at the cover")
	}
	if citations[0].Excerpt == "" {
		t.Error("the citation lost its text; there is nothing left to recognise it by")
	}
	if citations[0].Chapter != "Eigenvalues" {
		t.Errorf("chapter = %q, want the one recorded when the answer was given", citations[0].Chapter)
	}
}
