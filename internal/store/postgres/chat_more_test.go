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

// A reading list is an answer and is kept as one: in the order shown, with
// each book's title and authors, and only as far as the person may still read
// each book. A follow-up is routed against this list, so a book named here
// that they cannot open would leak its title into their next answer.
func TestAReadingListIsKeptWithTheTurn(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)
	ctx := t.Context()

	holmes, _ := seedSearchFile(t, p, userID, 711, domain.FormatEPUB,
		"The Adventures of Sherlock Holmes", "Arthur Conan Doyle",
		[]searchPassage{{text: "To Sherlock Holmes she is always the woman.", major: 1}})
	dracula, _ := seedSearchFile(t, p, userID, 712, domain.FormatEPUB,
		"Dracula", "Bram Stoker",
		[]searchPassage{{text: "Welcome to my house.", major: 1}})
	// Somebody else's book, which this person cannot open.
	theirs, _ := seedSearchFile(t, p, secondUser(t, p), 713, domain.FormatEPUB,
		"Somebody Else's Book", "A Stranger",
		[]searchPassage{{text: "Private.", major: 1}})

	c, err := p.CreateConversation(ctx, userID, "sherlock", nil)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := p.AppendMessage(ctx, c.ID, Message{Role: "user", Text: "what sherlock books are there?"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := p.AppendMessage(ctx, c.ID, Message{
		Role: "assistant", Intent: domain.IntentFindBook,
		Works: []ListedWork{{WorkID: dracula.WorkID}, {WorkID: theirs.WorkID}, {WorkID: holmes.WorkID},
			// A work that no longer exists is not recorded, and does not
			// fail the turn.
			{WorkID: uuid.New()}},
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	read, err := p.Conversation(ctx, userID, c.ID)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if len(read.Messages) != 2 {
		t.Fatalf("read %d messages", len(read.Messages))
	}
	if len(read.Messages[0].Works) != 0 {
		t.Errorf("the question carries a list: %+v", read.Messages[0].Works)
	}
	got := read.Messages[1].Works
	if len(got) != 2 {
		t.Fatalf("listed = %+v, want Dracula then Holmes", got)
	}
	if got[0].WorkID != dracula.WorkID || got[0].Title != "Dracula" ||
		len(got[0].Authors) != 1 || got[0].Authors[0] != "Bram Stoker" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].WorkID != holmes.WorkID || got[1].Title != "The Adventures of Sherlock Holmes" {
		t.Errorf("second = %+v", got[1])
	}
}

// Where a turn's search was narrowed is kept with it, and named back only
// while the person may still read that book.
func TestATurnKeepsTheBookItSearched(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)
	ctx := t.Context()

	holmes, _ := seedSearchFile(t, p, userID, 721, domain.FormatEPUB,
		"The Adventures of Sherlock Holmes", "Arthur Conan Doyle",
		[]searchPassage{{text: "Moriarty is the Napoleon of crime.", major: 1}})
	theirs, _ := seedSearchFile(t, p, secondUser(t, p), 722, domain.FormatEPUB,
		"Somebody Else's Book", "A Stranger",
		[]searchPassage{{text: "Private.", major: 1}})

	c, err := p.CreateConversation(ctx, userID, "villains", nil)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	for _, m := range []Message{
		{Role: "assistant", Text: "Moriarty [1].", ScopedTo: &ListedWork{WorkID: holmes.WorkID}},
		{Role: "assistant", Text: "Not yours.", ScopedTo: &ListedWork{WorkID: theirs.WorkID}},
		{Role: "assistant", Text: "Gone.", ScopedTo: &ListedWork{WorkID: uuid.New()}},
		{Role: "assistant", Text: "Everywhere."},
	} {
		if _, err := p.AppendMessage(ctx, c.ID, m); err != nil {
			t.Fatalf("AppendMessage %q: %v", m.Text, err)
		}
	}

	read, err := p.Conversation(ctx, userID, c.ID)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if len(read.Messages) != 4 {
		t.Fatalf("read %d messages", len(read.Messages))
	}
	got := read.Messages[0].ScopedTo
	if got == nil || got.WorkID != holmes.WorkID || got.Title != "The Adventures of Sherlock Holmes" ||
		len(got.Authors) != 1 || got.Authors[0] != "Arthur Conan Doyle" {
		t.Errorf("scoped = %+v", got)
	}
	for _, m := range read.Messages[1:] {
		if m.ScopedTo != nil {
			t.Errorf("%q names %+v, want nothing", m.Text, m.ScopedTo)
		}
	}
}

// A citation of a book the person can no longer open stays in their thread,
// marked unreadable, so its title is not carried into their next prompt.
func TestACitationSaysWhetherItsBookIsStillReadable(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)
	ctx := t.Context()

	_, mine := seedSearchFile(t, p, userID, 731, domain.FormatEPUB, "Dracula", "Bram Stoker",
		[]searchPassage{{text: "Welcome to my house.", major: 1}})
	_, theirs := seedSearchFile(t, p, secondUser(t, p), 732, domain.FormatEPUB, "Somebody Else's Book", "A Stranger",
		[]searchPassage{{text: "Lent, once.", major: 1}})

	c, err := p.CreateConversation(ctx, userID, "cited", nil)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := p.AppendMessage(ctx, c.ID, Message{Role: "assistant", Text: "Both [1] [2].",
		Citations: []Citation{{ChunkID: mine[0], Position: 0, Rank: 1}, {ChunkID: theirs[0], Position: 1, Rank: 2}},
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	read, err := p.Conversation(ctx, userID, c.ID)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	cites := read.Messages[0].Citations
	if len(cites) != 2 {
		t.Fatalf("citations = %+v", cites)
	}
	if !cites[0].Readable || cites[0].WorkTitle != "Dracula" {
		t.Errorf("own book = %+v", cites[0])
	}
	if cites[1].Readable {
		t.Errorf("somebody else's book reads as readable: %+v", cites[1])
	}
}

// Merging copies of a book carries what conversations said about the merged
// copy - a reading list that named it, an answer that searched it - to the
// kept work. A list that named both copies names the book once.
func TestMergingCarriesConversationsToTheKeptWork(t *testing.T) {
	p := testPool(t)
	userID, keep, fold := twoCopies(t, p, "Dracula", "Bram Stoker")
	ctx := t.Context()
	if _, err := p.Exec(ctx, `DELETE FROM conversations WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("clearing conversations: %v", err)
	}

	c, err := p.CreateConversation(ctx, userID, "dracula", nil)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	both, err := p.AppendMessage(ctx, c.ID, Message{Role: "assistant", Intent: domain.IntentFindBook,
		Works: []ListedWork{{WorkID: fold}, {WorkID: keep}}})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	one, err := p.AppendMessage(ctx, c.ID, Message{Role: "assistant", Intent: domain.IntentFindBook,
		Works: []ListedWork{{WorkID: fold}}})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	scoped, err := p.AppendMessage(ctx, c.ID, Message{Role: "assistant", Text: "The count [1].",
		ScopedTo: &ListedWork{WorkID: fold}})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	if _, err := p.MergeWorks(ctx, userID, keep, []uuid.UUID{fold}); err != nil {
		t.Fatalf("MergeWorks: %v", err)
	}

	read, err := p.Conversation(ctx, userID, c.ID)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	byID := map[uuid.UUID]Message{}
	for _, m := range read.Messages {
		byID[m.ID] = m
	}
	for _, id := range []uuid.UUID{both.ID, one.ID} {
		if w := byID[id].Works; len(w) != 1 || w[0].WorkID != keep {
			t.Errorf("list %v after the merge = %+v, want the kept work once", id, w)
		}
	}
	if s := byID[scoped.ID].ScopedTo; s == nil || s.WorkID != keep {
		t.Errorf("scope after the merge = %+v, want the kept work", s)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM message_works WHERE work_id = $1`, fold); n != 0 {
		t.Errorf("%d list entries still name the merged work", n)
	}
}

// A work deleted because nothing is left of it leaves conversations that
// named it intact, without the entry and without the scope.
func TestDeletingAnEmptyWorkClearsWhatConversationsSaidAboutIt(t *testing.T) {
	p := testPool(t)
	userID := chatUser(t, p)
	ctx := t.Context()

	gone, _ := seedSearchFile(t, p, userID, 741, domain.FormatEPUB, "Gone", "Somebody",
		[]searchPassage{{text: "Soon gone.", major: 1}})
	kept, _ := seedSearchFile(t, p, userID, 742, domain.FormatEPUB, "Kept", "Somebody Else",
		[]searchPassage{{text: "Still here.", major: 1}})

	c, err := p.CreateConversation(ctx, userID, "lists", nil)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	list, err := p.AppendMessage(ctx, c.ID, Message{Role: "assistant", Intent: domain.IntentFindBook,
		Works: []ListedWork{{WorkID: gone.WorkID}, {WorkID: kept.WorkID}}})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	scoped, err := p.AppendMessage(ctx, c.ID, Message{Role: "assistant", Text: "From it [1].",
		ScopedTo: &ListedWork{WorkID: gone.WorkID}})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	var edition uuid.UUID
	if err := p.QueryRow(ctx, `SELECT edition_id FROM book_files WHERE id = $1`, gone.BookFileID).Scan(&edition); err != nil {
		t.Fatalf("reading the edition: %v", err)
	}
	if _, err := p.Exec(ctx, `DELETE FROM book_files WHERE id = $1`, gone.BookFileID); err != nil {
		t.Fatalf("deleting the file: %v", err)
	}
	if err := p.DeleteEmptyEditions(ctx, []uuid.UUID{edition}); err != nil {
		t.Fatalf("DeleteEmptyEditions: %v", err)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM works WHERE id = $1`, gone.WorkID); n != 0 {
		t.Fatal("the empty work was not deleted")
	}

	if n := rowCount(t, p, `SELECT count(*) FROM message_works WHERE message_id = $1`, list.ID); n != 1 {
		t.Errorf("the list has %d entries, want the one left", n)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM messages WHERE id = $1 AND scoped_work_id IS NULL`, scoped.ID); n != 1 {
		t.Error("the answer still names a deleted work as its scope")
	}
}
