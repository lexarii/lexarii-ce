// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

func TestKeepFollowUpBook(t *testing.T) {
	const holmes = "The Adventures of Sherlock Holmes"
	tests := []struct {
		title    string
		question string
		keep     bool
	}{
		// Refers back, with nothing after the reference to be about instead.
		{holmes, "who are the villians in the book?", true},
		{holmes, "who wrote it?", true},
		{holmes, "who is the villain in it", true},
		{holmes, "what happens in that one?", true},
		{holmes, "and the second one?", true},
		{holmes, "who is the author?", true},
		{holmes, "what happens at the end of it?", true},
		{holmes, "is it's ending good?", true},
		{holmes, "is it any good?", true},
		// Names it: most of the distinctive words, or one letter off.
		{holmes, "who is Moriarty in Sherlock Holmes?", true},
		{holmes, "what about Sherlok Holmes?", true},
		{"Dracula", "Where is Dracula's castle?", true},
		{"Dracula", "Where is Dracula’s castle?", true},
		{"Dracula", "what about drakula?", true},
		{"The Great Gatsby", "is Gatsby's love real?", true},
		{"The Great Gatsby", "is Gatsby’ love real?", true},
		{"Little Women", "who is the eldest in Little Women?", true},
		// Stands on its own: the book was inherited. Every row the review found.
		{"Little Women", "what does it mean to be a woman", false},
		{"Atomic Habits", "how do I build a habit", false},
		{"Bleak House", "how do I look after a horse", false},
		{"Pride and Prejudice", "where can I ride a bike", false},
		{"Clean Code", "how should a code review go", false},
		{"Sherlock", "Is it worth reading a romance novel?", false},
		{"Sherlock", "What's the first book on Python for beginners?", false},
		{holmes, "what about sherlok?", false},
		{holmes, "what is a good romantic book?", false},
		{holmes, "do I have anything about vampires?", false},
		{holmes, "tell me about Pride and Prejudice", false},
	}
	for _, tt := range tests {
		got := keepFollowUpBook(tt.title, tt.question)
		if (got != "") != tt.keep {
			t.Errorf("keepFollowUpBook(%q, %q) = %q, want kept %v", tt.title, tt.question, got, tt.keep)
		}
	}
	if got := keepFollowUpBook("", "who wrote it?"); got != "" {
		t.Errorf("no book became %q", got)
	}
	// The router may copy the history's "Title — Author" label.
	if got := keepFollowUpBook("Dracula — Bram Stoker", "is Dracula scary?"); got == "" {
		t.Error("a title with its author attached was not recognised")
	}
}

func TestSameWord(t *testing.T) {
	for _, tt := range []struct {
		q, title string
		want     bool
	}{
		{"dracula", "dracula", true},
		{"drakula", "dracula", true},
		{"sherlok", "sherlock", true},
		{"woman", "women", false},  // too short to be a misspelling
		{"horse", "house", false},  // too short
		{"habit", "habits", false}, // a plural
		{"adventure", "adventures", false},
		{"kracula", "dracula", false}, // another first letter
	} {
		if got := sameWord(tt.q, tt.title); got != tt.want {
			t.Errorf("sameWord(%q, %q) = %v, want %v", tt.q, tt.title, got, tt.want)
		}
	}
}

func TestEditDistanceAtMostOne(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"dracula", "dracula", true},
		{"drakula", "dracula", true},
		{"dracul", "dracula", true},
		{"draculas", "dracula", true},
		{"drakulas", "dracula", false},
		{"holmes", "homes", true},
		{"sherlock", "shylock", false},
	} {
		if got := editDistanceAtMostOne(tt.a, tt.b); got != tt.want {
			t.Errorf("editDistanceAtMostOne(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// The thread from the bug: a romance question after a conversation about
// Sherlock Holmes, which the router answered with the earlier book still
// attached. The reading list must not be a title search for that book.
func TestAFollowUpDoesNotInheritABook(t *testing.T) {
	model := newFakeModel(t)
	model.route = routeJSON(domain.IntentFindBook, []string{"romance"}, nil, "The Adventures of Sherlock Holmes")
	store := &pipelineStore{shelf: map[string][]postgres.LibraryItem{
		"search:The Adventures of Sherlock Holmes": shelfItems("The Adventures of Sherlock Holmes"),
	}}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}
	history := History{
		{Role: "user", Text: "what other sherlock books are there in my library?"},
		{Role: "assistant", Listed: []string{"The Adventures of Sherlock Holmes — Arthur Conan Doyle"}},
	}

	got, err := a.AskWithHistory(t.Context(), "what is a good romantic book?", Scope{}, history)
	if err != nil {
		t.Fatalf("AskWithHistory: %v", err)
	}
	if got.Route.Book != "" {
		t.Errorf("the follow-up kept the book %q", got.Route.Book)
	}
	for _, b := range got.Books {
		if b.Title == "The Adventures of Sherlock Holmes" {
			t.Errorf("the reading list for romance is the earlier book: %v", listTitles(got.Books))
		}
	}
	for _, q := range store.libraryCalls {
		if q.Search == "The Adventures of Sherlock Holmes" {
			t.Error("the inherited book was searched for by title")
		}
	}
}
