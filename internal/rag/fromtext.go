// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"context"
	"sort"
	"strings"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// FoundByText introduces a reading list that came from the books' own text.
//
// Said, because it is a weaker kind of list: a subject is somebody's judgement
// about what a book is about, and a passage that mentions the topic is only
// evidence of it.
const FoundByText = "None of your books is tagged with this or has it in the title, " +
	"so these were found by searching their text:"

// NoReadingList is the answer when nothing in the library matches a request
// for books at all.
//
// Better than the alternative it replaces, which was whatever a title search
// for the wrong words turned up.
const NoReadingList = "Nothing in your library seems to be about this: " +
	"no subject, title or passage matched."

// textPool is how many passages are read to rank books by their text.
//
// Three times an answer's worth, because the passages are counted rather than
// read: a book with a dozen passages on the topic should be able to show that
// it has a dozen. Deeper than RerankPool, which is sized for picking ten
// passages to answer from; with a reranker configured, retrieveOne reads at
// least this many, so the reranker reorders all thirty rather than cutting
// the list to twenty.
const textPool = 3 * DefaultPassages

// NoLongerListed stands in for FoundByText on a turn read back after every
// book it listed has left the reader's shelf, which would otherwise end in a
// colon introducing nothing.
const NoLongerListed = "None of the books this list named is in your library any more."

// StoredListText is what a stored turn's text says, given how many of the
// books it listed the reader can still open.
func StoredListText(text string, listed int) string {
	if listed == 0 && text == FoundByText {
		return NoLongerListed
	}
	return text
}

// booksFromText answers a request for books from the passages, when subjects
// and titles had nothing to say.
//
// In a library that was never enriched no work has a subject or a
// description, so "what is a good romantic book?" matched no tag and fell
// through to a title search - which found nothing, or worse, found the book a
// previous question had named. Pride and Prejudice was on the shelf the whole
// time; it just was not tagged.
//
// So the topic is searched for as an ordinary question would be - the same
// search, as the asker, so only their books - and the passages are counted per
// book. A book is listed by the sum of its passages' scores, which rewards
// both how many passages it has and how well they rank; one whose total is
// under half the best book's matched in passing and is left off.
func (a *Answerer) booksFromText(ctx context.Context, question string, route llm.Route,
	scope Scope) ([]postgres.LibraryItem, error) {

	topic := topicWords(question, route.Subjects)
	if topic == nil {
		return nil, nil
	}
	passages, err := a.retrieveOne(ctx, strings.Join(topic, " "), scope, textPool)
	if err != nil {
		return nil, err
	}

	type tally struct {
		item  postgres.LibraryItem
		score float64
		hits  int
	}
	byWork := map[uuid.UUID]*tally{}
	var order []*tally
	for _, p := range passages {
		t, ok := byWork[p.WorkID]
		if !ok {
			item := postgres.LibraryItem{WorkID: p.WorkID, Title: p.WorkTitle, BookFileID: p.Chunk.BookFileID}
			if p.AuthorName != "" {
				item.Authors = []string{p.AuthorName}
			}
			t = &tally{item: item}
			byWork[p.WorkID] = t
			order = append(order, t)
		}
		t.score += passageScore(p.FusedScore, p.RerankScore)
		t.hits++
	}
	if len(order) == 0 {
		return nil, nil
	}

	sort.SliceStable(order, func(i, j int) bool {
		if order[i].score != order[j].score {
			return order[i].score > order[j].score
		}
		return order[i].hits > order[j].hits
	})
	floor := order[0].score / 2

	out := make([]postgres.LibraryItem, 0, min(len(order), maxBooksInAnswer))
	for _, t := range order {
		if len(out) == maxBooksInAnswer || t.score < floor {
			break
		}
		out = append(out, t.item)
	}
	return out, nil
}

// passageScore is what one passage contributes to its book's total: the
// reranker's judgement when there was one, the fused rank otherwise. Never
// below a sliver, so that a passage found at all still counts as found.
func passageScore(fused, rerank float64) float64 {
	score := fused
	if rerank > 0 {
		score = rerank
	}
	return max(score, 1e-6)
}

// requestWords are the words a request for books is made of, rather than its
// topic. Searched for, "book" matches every passage that mentions a book,
// which is how a question about villains was once answered from Dracula and
// Alice.
var requestWords = map[string]bool{
	"a": true, "about": true, "an": true, "any": true, "anything": true, "are": true, "best": true,
	"book": true, "books": true, "can": true, "could": true, "do": true, "find": true, "for": true,
	"give": true, "good": true, "great": true, "have": true, "i": true, "in": true, "is": true,
	"library": true, "like": true, "list": true, "me": true, "more": true, "my": true, "novel": true,
	"novels": true, "of": true, "on": true, "other": true, "please": true, "read": true,
	"reading": true, "recommend": true, "should": true, "show": true, "some": true,
	"something": true, "suggest": true, "tell": true, "that": true, "the": true, "there": true,
	"title": true, "titles": true, "to": true, "what": true, "which": true, "with": true,
	"would": true, "you": true, "similar": true, "another": true, "else": true, "same": true,
	"want": true, "need": true, "looking": true, "look": true, "get": true,
}

// topicWords is what a request for books is about: the question's own words
// and the router's subjects, without the words every such request is made of
// and without function words and pronouns. Nil when nothing is left - "more
// like it", "something similar" - and then nothing is searched, because
// whatever a search for "it" found would be a list of unrelated books.
func topicWords(question string, subjects []string) []string {
	seen := map[string]bool{}
	var kept []string
	for _, w := range tokens(question + " " + strings.Join(subjects, " ")) {
		w = stem(w)
		if w == "" || requestWords[w] || stopWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		kept = append(kept, w)
	}
	return kept
}
