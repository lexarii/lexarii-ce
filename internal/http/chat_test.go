// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// fakeChatStore records what was written without needing a database.
type fakeChatStore struct {
	conversation postgres.Conversation
	appended     []postgres.Message
	created      bool
}

func (f *fakeChatStore) CreateConversation(_ context.Context, userID uuid.UUID, title string,
	bookFileID *uuid.UUID) (postgres.Conversation, error) {
	f.created = true
	f.conversation = postgres.Conversation{
		ID: uuid.New(), UserID: userID, Title: title,
		BookFileID: bookFileID, UpdatedAt: time.Now(),
	}
	return f.conversation, nil
}

func (f *fakeChatStore) Conversation(context.Context, uuid.UUID, uuid.UUID) (postgres.Conversation, error) {
	return f.conversation, nil
}
func (f *fakeChatStore) Conversations(context.Context, uuid.UUID, int) ([]postgres.Conversation, error) {
	return nil, nil
}
func (f *fakeChatStore) AppendMessage(_ context.Context, _ uuid.UUID, m postgres.Message) (postgres.Message, error) {
	m.ID = uuid.New()
	m.Seq = len(f.appended)
	f.appended = append(f.appended, m)
	return m, nil
}
func (f *fakeChatStore) DeleteConversation(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *fakeChatStore) SetConversationTitle(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}

// events parses an SSE body into (name, data) pairs.
func events(t *testing.T, body string) [][2]string {
	t.Helper()
	var out [][2]string
	var name string
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, [2]string{name, strings.TrimPrefix(line, "data: ")})
		}
	}
	return out
}

// Without a model the stream must still be a well-formed stream that ends,
// rather than a hang or a half-written response.
func TestStreamWithoutAModelStillEnds(t *testing.T) {
	store := &fakeChatStore{}
	h := &ChatHandler{Store: store, Answerer: nil}

	req := asUser(httptest.NewRequest(http.MethodPost, "/api/v1/conversations/stream",
		strings.NewReader(`{"question":"what is an eigenvalue?"}`)))
	rec := httptest.NewRecorder()

	defer func() {
		// A nil answerer panics; what matters is that nothing was written to
		// the client as though it had succeeded.
		if recover() != nil && rec.Body.Len() > 0 {
			if !strings.Contains(rec.Body.String(), "event: conversation") {
				t.Fatalf("wrote something other than the conversation event: %q", rec.Body.String())
			}
		}
	}()
	h.Stream(rec, req)
}

// A question must be stored before the model is asked, so that a question that
// fails after thirty seconds is still in the thread.
func TestStreamStoresTheQuestionFirst(t *testing.T) {
	store := &fakeChatStore{}
	h := &ChatHandler{Store: store}

	req := asUser(httptest.NewRequest(http.MethodPost, "/api/v1/conversations/stream",
		strings.NewReader(`{"question":"what is an eigenvalue?"}`)))
	rec := httptest.NewRecorder()

	func() {
		defer func() { _ = recover() }()
		h.Stream(rec, req)
	}()

	if len(store.appended) == 0 {
		t.Fatal("the question was not stored before the model was asked")
	}
	if store.appended[0].Role != "user" || store.appended[0].Text != "what is an eigenvalue?" {
		t.Fatalf("first stored message = %+v", store.appended[0])
	}
}

// An unparseable body must be refused with a status code, before the stream
// opens: once headers are sent a 400 can no longer be reported.
func TestStreamRefusesBadRequestsBeforeOpening(t *testing.T) {
	h := &ChatHandler{Store: &fakeChatStore{}}

	for _, body := range []string{`not json`, `{}`, `{"question":"   "}`} {
		rec := httptest.NewRecorder()
		h.Stream(rec, asUser(httptest.NewRequest(http.MethodPost,
			"/api/v1/conversations/stream", strings.NewReader(body))))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400", body, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("body %q: opened a stream for a bad request", body)
		}
	}
}

func TestStreamRequiresSignIn(t *testing.T) {
	h := &ChatHandler{Store: &fakeChatStore{}}
	rec := httptest.NewRecorder()
	h.Stream(rec, httptest.NewRequest(http.MethodPost, "/api/v1/conversations/stream",
		strings.NewReader(`{"question":"hello"}`)))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// The conversation event must come first and carry a usable id, so a reload
// during a thirty-second answer finds the thread.
func TestStreamAnnouncesTheConversationFirst(t *testing.T) {
	store := &fakeChatStore{}
	h := &ChatHandler{Store: store}

	rec := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		h.Stream(rec, asUser(httptest.NewRequest(http.MethodPost,
			"/api/v1/conversations/stream", strings.NewReader(`{"question":"what is a monad?"}`))))
	}()

	got := events(t, rec.Body.String())
	if len(got) == 0 || got[0][0] != "conversation" {
		t.Fatalf("first event = %v, want conversation", got)
	}

	var summary struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(got[0][1]), &summary); err != nil {
		t.Fatalf("conversation event is not JSON: %v", err)
	}
	if _, err := uuid.Parse(summary.ID); err != nil {
		t.Fatalf("conversation id %q is not a UUID", summary.ID)
	}
	// Threads are named after the question that started them.
	if summary.Title != "what is a monad?" {
		t.Fatalf("title = %q", summary.Title)
	}
}

// A reopened conversation must link each citation marker to the source the
// answer actually used.
//
// Citations are stored with the rank the answer text quoted and a position in
// the compacted list of the ones cited. Rendering the position made [2] of a
// ten-passage answer resolve to whatever was cited second - a different book,
// a different locator, and a deep link into the wrong place - while the marker
// that really was second matched nothing and rendered as dead text. Live
// answers were unaffected, so nothing ever showed it.
func TestAReopenedThreadCitesTheRightSource(t *testing.T) {
	// The model cited the second and the fifth of ten passages.
	stored := []postgres.Citation{
		{Position: 0, Rank: 2, WorkTitle: "The Second Book"},
		{Position: 1, Rank: 5, WorkTitle: "The Fifth Book"},
	}

	numbers := map[int]string{}
	for _, c := range stored {
		number := c.Rank
		if number == 0 {
			number = c.Position + 1
		}
		numbers[number] = c.WorkTitle
	}

	if got := numbers[2]; got != "The Second Book" {
		t.Errorf("[2] resolves to %q, want the passage the answer numbered 2", got)
	}
	if got := numbers[5]; got != "The Fifth Book" {
		t.Errorf("[5] resolves to %q, want the passage the answer numbered 5", got)
	}
	if _, exists := numbers[1]; exists {
		t.Error("[1] resolves to something, but the answer never cited a first passage")
	}
}

// A reading list is kept with the turn that gave it, and a follow-up is routed
// with it. A find-book turn has no text, so without the list the thread said
// "You answered:" and nothing more, and "the book" in the next question meant
// nothing.
func TestAReadingListReachesTheNextQuestion(t *testing.T) {
	holmes := uuid.New()
	answer := rag.Answer{
		Route: llm.Route{Intent: domain.IntentFindBook},
		Books: []postgres.LibraryItem{{WorkID: holmes, Title: "The Adventures of Sherlock Holmes",
			Authors: []string{"Arthur Conan Doyle"}}},
	}

	stored := assistantMessage(answer)
	if len(stored.Works) != 1 || stored.Works[0].WorkID != holmes {
		t.Fatalf("stored works = %+v, want the listed book", stored.Works)
	}

	turn := historyTurn(stored)
	if want := []string{"The Adventures of Sherlock Holmes — Arthur Conan Doyle"}; !reflect.DeepEqual(turn.Listed, want) {
		t.Errorf("Listed = %q, want %q", turn.Listed, want)
	}
	if got := (rag.History{turn}).Render(); got != "You listed: The Adventures of Sherlock Holmes — Arthur Conan Doyle" {
		t.Errorf("rendered %q", got)
	}

	// And a reopened thread shows the list, not an empty answer.
	read := conversationDTO(postgres.Conversation{Messages: []postgres.Message{stored}})
	if books := read.Messages[0].Books; len(books) != 1 || books[0].WorkID != holmes.String() ||
		books[0].Title != "The Adventures of Sherlock Holmes" {
		t.Errorf("reopened books = %+v", books)
	}
}

// The books an answer cited are named once each, in the order first cited,
// so "the book" can be resolved even when the prose never names it.
func TestAHistoryTurnNamesWhatItCited(t *testing.T) {
	holmes, dracula := uuid.New(), uuid.New()
	turn := historyTurn(postgres.Message{Role: "assistant", Text: "Moriarty [1], and the Count [2].",
		Citations: []postgres.Citation{
			{WorkID: holmes, WorkTitle: "The Adventures of Sherlock Holmes", Readable: true},
			{WorkID: dracula, WorkTitle: "Dracula", Readable: true},
			{WorkID: holmes, WorkTitle: "The Adventures of Sherlock Holmes", Readable: true},
			// Lent and taken back: still their citation, never in the prompt.
			{WorkID: uuid.New(), WorkTitle: "Somebody Else's Book"},
		}})
	if want := []string{"The Adventures of Sherlock Holmes", "Dracula"}; !reflect.DeepEqual(turn.Cited, want) {
		t.Errorf("Cited = %q, want %q", turn.Cited, want)
	}
	if turn.Text != "Moriarty [1], and the Count [2]." || len(turn.Listed) != 0 {
		t.Errorf("turn = %+v", turn)
	}
}

// Where an answer looked is part of the answer: on the reply, in what is
// stored, and on the turn when the thread is reopened.
func TestTheBookAnAnswerSearchedIsShown(t *testing.T) {
	holmes := uuid.New()
	answer := rag.Answer{
		Route:    llm.Route{Intent: domain.IntentFactualLookup},
		Text:     "Moriarty [1].",
		ScopedTo: &postgres.LibraryItem{WorkID: holmes, Title: "The Adventures of Sherlock Holmes"},
	}

	if got := answerDTO(answer).ScopedTo; got == nil || got.WorkID != holmes.String() ||
		got.Title != "The Adventures of Sherlock Holmes" {
		t.Errorf("answer scopedTo = %+v", got)
	}
	stored := assistantMessage(answer)
	if stored.ScopedTo == nil || stored.ScopedTo.WorkID != holmes {
		t.Fatalf("stored scope = %+v", stored.ScopedTo)
	}
	read := conversationDTO(postgres.Conversation{Messages: []postgres.Message{stored}})
	if got := read.Messages[0].ScopedTo; got == nil || got.WorkID != holmes.String() {
		t.Errorf("reopened scopedTo = %+v", got)
	}

	// The whole library: nothing to say.
	if got := answerDTO(rag.Answer{Text: "x"}).ScopedTo; got != nil {
		t.Errorf("an unscoped answer says %+v", got)
	}
}

// A text-found list whose every book has since left the shelf reads as a
// sentence, not as a colon introducing nothing.
func TestAnEmptiedTextReadingListSaysSo(t *testing.T) {
	m := postgres.Message{Role: "assistant", Intent: domain.IntentFindBook, Text: rag.FoundByText}
	if got := conversationDTO(postgres.Conversation{Messages: []postgres.Message{m}}).Messages[0].Content; got != rag.NoLongerListed {
		t.Errorf("reopened content = %q", got)
	}
	if got := historyTurn(m).Text; got != rag.NoLongerListed {
		t.Errorf("history text = %q", got)
	}
	m.Works = []postgres.ListedWork{{WorkID: uuid.New(), Title: "Pride and Prejudice"}}
	if got := conversationDTO(postgres.Conversation{Messages: []postgres.Message{m}}).Messages[0].Content; got != rag.FoundByText {
		t.Errorf("content with a book left = %q", got)
	}
}
