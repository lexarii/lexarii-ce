// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"strings"
	"unicode"
)

// keepFollowUpBook decides whether the book the router named for a follow-up
// is really the one the question is about.
//
// A follow-up is routed with the whole thread in front of the router, so a
// title from three turns ago is right there to be copied. In a real thread
// "what is a good romantic book?", asked after a conversation about Sherlock
// Holmes, came back with that book attached, and the reading list was
// Sherlock Holmes. The router prompt says not to do that; this is the check
// that does not depend on a small model having listened.
//
// The book is kept when the question names it (namesTitle) or refers back to
// something already said (refersBack). Otherwise it was inherited, and is
// dropped.
//
// The two mistakes cost different things, and the check leans towards
// dropping. A book wrongly kept NARROWS the search to that one book - a
// question about Python answered from Sherlock Holmes. A book wrongly dropped
// searches the whole library, which is what happened before any of this.
func keepFollowUpBook(book, question string) string {
	if strings.TrimSpace(book) == "" {
		return ""
	}
	asked := tokens(question)
	if refersBack(asked) || namesTitle(asked, tokens(bareTitle(book))) {
		return book
	}
	return ""
}

// tokens splits text into lower-case words of letters, digits and
// apostrophes, with curly apostrophes made straight.
func tokens(s string) []string {
	s = strings.NewReplacer("’", "'", "‘", "'").Replace(strings.ToLower(s))
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
}

// stem drops a possessive: "dracula's" and "jesus'" become "dracula" and
// "jesus". Only for comparing words with a title; the pointer check reads
// "it's" as it is.
func stem(w string) string {
	w = strings.TrimSuffix(w, "'s")
	return strings.Trim(w, "'")
}

// pointers are the words that point back at something already said.
var pointers = set("it", "it's", "its", "itself", "them")

// determiners and referents make the two-word references: "the book", "that
// one", "the second one", "the other".
var (
	determiners = set("the", "this", "that", "same")
	referents   = set("book", "novel", "story", "one", "author", "writer",
		"first", "second", "third", "fourth", "fifth", "last", "latter", "former", "other", "title")
)

// refersBack reports whether the question points at something said earlier:
// "it", "the book", "that one", "the second one" - and nothing it could be
// pointing at instead follows. "Who is the villain in it?" refers back;
// "is it worth reading a romance novel?" is about romance novels, and "what's
// the first book on Python?" about Python.
//
// The rule is crude in one known direction: a reference followed by a topic
// of its own - "what does it say about love?" - is not counted. That drops
// the book, which searches the whole library: the cheaper of the two
// mistakes.
func refersBack(asked []string) bool {
	for i, w := range asked {
		reference := pointers[w] || (i > 0 && determiners[asked[i-1]] && referents[w])
		if reference && !topicFollows(asked[i+1:]) {
			return true
		}
	}
	return false
}

// topicFollows reports whether any of these words is a topic of its own.
func topicFollows(rest []string) bool {
	for _, w := range rest {
		w = stem(w)
		if len([]rune(w)) >= 3 && !stopWords[w] && !referents[w] && !fillerWords[w] {
			return true
		}
	}
	return false
}

// namesTitle reports whether the question names the title.
//
// A title's distinctive words are the ones that are neither function words
// nor very common nouns and adjectives: "Bleak" in Bleak House, not "House".
// Most of them - more than half - must be in the question, spelled right or,
// for a long word, one letter off ("drakula's" for Dracula). A title with no
// distinctive word at all ("Little Women", "Clean Code") must be there whole.
func namesTitle(asked, title []string) bool {
	question := make([]string, 0, len(asked))
	for _, w := range asked {
		question = append(question, stem(w))
	}

	var distinctive, content []string
	for _, w := range title {
		w = stem(w)
		if w == "" || stopWords[w] {
			continue
		}
		content = append(content, w)
		if len([]rune(w)) >= 3 && !commonWords[w] {
			distinctive = append(distinctive, w)
		}
	}

	if len(distinctive) == 0 {
		if len(content) == 0 {
			return false
		}
		for _, w := range content {
			if !contains(question, w) {
				return false
			}
		}
		return true
	}

	matched := 0
	for _, d := range distinctive {
		for _, q := range question {
			if sameWord(q, d) {
				matched++
				break
			}
		}
	}
	return matched*2 > len(distinctive)
}

func contains(words []string, w string) bool {
	for _, x := range words {
		if x == w {
			return true
		}
	}
	return false
}

// sameWord is the word, or a misspelling of it: six letters or more, the same
// first letter, one edit apart - and not merely its plural, which is a
// different word for this purpose ("habit" is not Atomic Habits).
func sameWord(q, t string) bool {
	if q == t {
		return true
	}
	rq, rt := []rune(q), []rune(t)
	if len(rq) < 6 || len(rt) < 6 || rq[0] != rt[0] {
		return false
	}
	if q == t+"s" || t == q+"s" || q == t+"es" || t == q+"es" {
		return false
	}
	return editDistanceAtMostOne(q, t)
}

// editDistanceAtMostOne reports whether one insertion, deletion or
// substitution turns a into b.
func editDistanceAtMostOne(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	if len(ra) > len(rb) {
		ra, rb = rb, ra
	}
	if len(rb)-len(ra) > 1 {
		return false
	}
	i, j, edits := 0, 0, 0
	for i < len(ra) && j < len(rb) {
		if ra[i] == rb[j] {
			i++
			j++
			continue
		}
		edits++
		if edits > 1 {
			return false
		}
		if len(ra) == len(rb) {
			i++
		}
		j++
	}
	return edits+(len(rb)-j)+(len(ra)-i) <= 1
}

func set(words ...string) map[string]bool {
	out := make(map[string]bool, len(words))
	for _, w := range words {
		out[w] = true
	}
	return out
}

// stopWords are English function words: they say nothing about which book.
var stopWords = set(
	"a", "an", "the", "and", "or", "but", "nor", "of", "in", "on", "at", "to", "for", "from", "by",
	"with", "about", "into", "onto", "over", "under", "after", "before", "between", "through",
	"is", "are", "was", "were", "be", "been", "being", "am", "do", "does", "did", "done", "doing",
	"have", "has", "had", "having", "it", "it's", "its", "itself", "this", "that", "these", "those",
	"there", "here", "what", "what's", "who", "who's", "whom", "whose", "which", "when", "where",
	"why", "how", "i", "i'm", "me", "my", "mine", "you", "your", "he", "him", "his", "she", "her",
	"they", "them", "their", "we", "us", "our", "not", "no", "yes", "so", "too", "very", "just",
	"also", "then", "than", "if", "as", "all", "any", "some", "more", "most", "much", "many",
	"such", "can", "could", "would", "should", "will", "shall", "may", "might", "must", "really",
	"again", "still", "ever", "even", "only", "up", "out", "down", "off", "own", "same", "other",
	"others", "one", "ones", "else", "each", "every", "both", "either", "neither", "s",
)

// fillerWords are what a follow-up is made of around its reference: "is it
// worth reading", "what happens at the end of it". Never a topic.
var fillerWords = set(
	"worth", "read", "reads", "reading", "good", "bad", "best", "like", "liked", "think", "say",
	"says", "said", "tell", "mean", "means", "meant", "happen", "happens", "happened", "end",
	"ending", "start", "beginning", "please", "know", "explain", "summarise", "summarize",
	"summary", "chapter", "chapters", "part", "page", "pages", "again", "now", "there's", "get",
)

// commonWords are nouns and adjectives so frequent in titles and questions
// that one alone does not name a book: "house", "code", "women", "little".
// A word of a title on this list still counts when the whole title is asked
// for; it just cannot carry the match on its own.
var commonWords = set(
	"house", "home", "code", "clean", "little", "big", "small", "great", "good", "new", "old",
	"man", "men", "woman", "women", "girl", "girls", "boy", "boys", "child", "children",
	"mother", "father", "family", "friend", "friends", "king", "queen", "people", "person",
	"life", "lives", "death", "time", "times", "world", "day", "days", "night", "year", "years",
	"war", "love", "story", "stories", "tale", "tales", "book", "books", "way", "city", "town",
	"mind", "heart", "head", "hand", "hands", "eye", "eyes", "face", "body", "name", "place",
	"school", "work", "money", "power", "light", "dark", "black", "white", "red", "blue", "green",
	"long", "short", "first", "last", "secret", "secrets", "art", "history", "guide",
	"introduction", "complete", "edition", "volume", "water", "fire", "earth", "sea", "land",
	"road", "star", "stars", "sun", "moon", "god", "gods", "game", "games", "end", "beginning",
	"part", "parts", "case", "thing", "things", "word", "words", "street", "room", "door",
)
