// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

func TestRenamingAConversation(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)
	stranger := secondUser(t, p)
	ctx := t.Context()

	c, err := p.CreateConversation(ctx, userID, "  first title  ", nil)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if c.Title != "first title" {
		t.Errorf("title = %q, want it trimmed", c.Title)
	}

	title := func() string {
		t.Helper()
		got, err := p.Conversation(ctx, userID, c.ID)
		if err != nil {
			t.Fatalf("Conversation: %v", err)
		}
		return got.Title
	}

	if err := p.SetConversationTitle(ctx, userID, c.ID, " Eigenvalues "); err != nil {
		t.Fatalf("SetConversationTitle: %v", err)
	}
	if got := title(); got != "Eigenvalues" {
		t.Errorf("title = %q", got)
	}

	if err := p.SetConversationTitle(ctx, stranger, c.ID, "Mine now"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a stranger renaming: err = %v, want ErrNotFound", err)
	}
	if err := p.SetConversationTitle(ctx, userID, uuid.New(), "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("renaming an absent thread: err = %v, want ErrNotFound", err)
	}
	if got := title(); got != "Eigenvalues" {
		t.Errorf("after refused renames the title is %q", got)
	}

	// A blank title clears it rather than storing whitespace.
	if err := p.SetConversationTitle(ctx, userID, c.ID, "   "); err != nil {
		t.Fatalf("SetConversationTitle (blank): %v", err)
	}
	if got := title(); got != "" {
		t.Errorf("title = %q, want cleared", got)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM conversations WHERE id = $1 AND title IS NULL`, c.ID); n != 1 {
		t.Errorf("a blank title was stored rather than cleared")
	}
}

// References to rows that are not there are "not found", not a driver error.
func TestChatWritesAgainstAbsentRowsAreNotFound(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)
	ctx := t.Context()

	if _, err := p.CreateConversation(ctx, uuid.New(), "nobody's", nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a thread for an absent user: err = %v, want ErrNotFound", err)
	}
	absentFile := uuid.New()
	if _, err := p.CreateConversation(ctx, userID, "pinned", &absentFile); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a thread pinned to an absent file: err = %v, want ErrNotFound", err)
	}
	if _, err := p.AppendMessage(ctx, uuid.New(), Message{Role: "user", Text: "q"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a message in an absent thread: err = %v, want ErrNotFound", err)
	}
	if _, err := p.Conversation(ctx, userID, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading an absent thread: err = %v, want ErrNotFound", err)
	}
}

// What an assistant turn recorded about itself comes back as written, a
// zero meaning "not recorded" rather than a measured zero, and a citation of
// a PDF opens at its page.
func TestAnAssistantTurnKeepsHowItWasProduced(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)
	ctx := t.Context()

	recipes, ids := seedSearchFile(t, p, userID, 701, domain.FormatPDF, "Numerical Recipes", "William Press",
		[]searchPassage{{text: "The singular value decomposition, on page forty-one.", major: 41}})
	pinned := recipes.BookFileID

	c, err := p.CreateConversation(ctx, userID, "", &pinned)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if c.BookFileID == nil || *c.BookFileID != pinned || c.Title != "" {
		t.Errorf("conversation = %+v", c)
	}

	sent := Message{
		Role: "assistant", Text: "Use the SVD [1].",
		Intent: domain.IntentFactualLookup, ModelID: "local-model",
		PromptTokens: 900, CompletionTokens: 40, RetrievalMS: 12, GenerationMS: 0,
		Unsupported: true,
		Citations:   []Citation{{ChunkID: ids[0], Position: 1, Rank: 3, Score: 0.25}},
	}
	out, err := p.AppendMessage(ctx, c.ID, sent)
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if out.Intent != domain.IntentFactualLookup || out.ModelID != "local-model" ||
		out.PromptTokens != 900 || out.CompletionTokens != 40 || out.RetrievalMS != 12 ||
		out.GenerationMS != 0 || !out.Unsupported {
		t.Errorf("appended = %+v", out)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM messages WHERE id = $1 AND generation_ms IS NULL`, out.ID); n != 1 {
		t.Error("an unrecorded generation time was stored as a measured zero")
	}

	read, err := p.Conversation(ctx, userID, c.ID)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if len(read.Messages) != 1 || len(read.Messages[0].Citations) != 1 {
		t.Fatalf("read back %+v", read.Messages)
	}
	m := read.Messages[0]
	if m.Intent != domain.IntentFactualLookup || m.ModelID != "local-model" || !m.Unsupported {
		t.Errorf("read back message = %+v", m)
	}
	cite := m.Citations[0]
	want := domain.Locator{Format: domain.FormatPDF, Page: 41, ElementPath: "body/p[1]"}.String()
	if cite.Locator != want {
		t.Errorf("citation locator = %q, want %q", cite.Locator, want)
	}
	if cite.WorkTitle != "Numerical Recipes" || cite.WorkID != recipes.WorkID || cite.Rank != 3 ||
		cite.Score != 0.25 || cite.Position != 1 || cite.ChunkID != ids[0] {
		t.Errorf("citation = %+v", cite)
	}
}

func TestConversationsHonourTheirLimit(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)
	ctx := t.Context()

	for range 3 {
		if _, err := p.CreateConversation(ctx, userID, "t", nil); err != nil {
			t.Fatalf("CreateConversation: %v", err)
		}
	}
	for _, tc := range []struct{ limit, want int }{{2, 2}, {0, 3}, {500, 3}} {
		list, err := p.Conversations(ctx, userID, tc.limit)
		if err != nil {
			t.Fatalf("Conversations(%d): %v", tc.limit, err)
		}
		if len(list) != tc.want {
			t.Errorf("Conversations(limit %d) = %d threads, want %d", tc.limit, len(list), tc.want)
		}
	}
}
