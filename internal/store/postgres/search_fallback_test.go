// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// partialCorpus is a book in which no passage has every word of the question
// "Why does Victor Frankenstein abandon the creature?" - which, with stop words
// gone, is victor, frankenstein, abandon, creature.
type partialCorpus struct {
	owner                   uuid.UUID
	three, two, one, nought uuid.UUID
}

const partialQuestion = "Why does Victor Frankenstein abandon the creature?"

func seedPartialCorpus(t *testing.T, p *Pool) partialCorpus {
	t.Helper()
	cleanLibrary(t, p)
	c := partialCorpus{owner: testOwner(t, p)}
	_, ids := seedSearchFile(t, p, c.owner, 301, domain.FormatEPUB, "Frankenstein", "Mary Shelley",
		[]searchPassage{
			// Listed out of rank order, so the order of the result is the
			// ranking's and not the table's.
			{text: "A creature of the mountains, seen at a distance.", major: 1,
				vector: searchBasisVector(10), model: searchModel},
			{text: "Walton writes to his sister from the ice.", major: 2,
				vector: searchBasisVector(11), model: searchModel},
			{text: "Victor Frankenstein abandons his work the moment it breathes.", major: 3,
				vector: searchBasisVector(12), model: searchModel},
			{text: "The creature wanders alone through the forest while Victor sleeps.", major: 4,
				vector: searchBasisVector(13), model: searchModel},
		})
	c.one, c.nought, c.three, c.two = ids[0], ids[1], ids[2], ids[3]
	return c
}

// A natural question finds nothing when every word is required, and finds the
// passages that have most of them when it is not.
func TestKeywordSearchFallsBackToAnyWordWhenEveryWordFindsTooLittle(t *testing.T) {
	p := testPool(t)
	c := seedPartialCorpus(t, p)
	ctx := t.Context()

	// The premise: no passage has all four words.
	var strict int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM chunks
		WHERE tsv @@ websearch_to_tsquery('english', $1)`, partialQuestion).Scan(&strict); err != nil {
		t.Fatalf("counting strict matches: %v", err)
	}
	if strict != 0 {
		t.Fatalf("%d passages have every word; the fixture is wrong", strict)
	}

	got, err := p.Search(ctx, SearchQuery{Text: partialQuestion, ViewerID: c.owner, Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Most words first. The passage with none of them is not found at all.
	if want := []uuid.UUID{c.three, c.two, c.one}; !slices.Equal(searchResultIDs(got), want) {
		t.Fatalf("found %v, want three words, two, one: %v", searchResultIDs(got), want)
	}
	for i, r := range got {
		if r.SparseRank != i+1 || r.DenseRank != 0 {
			t.Errorf("result %d: sparse rank %d, dense rank %d", i, r.SparseRank, r.DenseRank)
		}
	}
}

// With a meaning arm the fallback is off, and hybrid search is what it was.
// The fallback's passages would otherwise compete at full fusion weight: the
// passage the vector points at has none of the question's words, and the
// three with some of them would climb over it on their keyword ranks alone.
func TestHybridSearchDoesNotFallBack(t *testing.T) {
	p := testPool(t)
	c := seedPartialCorpus(t, p)

	got, err := p.Search(t.Context(), SearchQuery{
		Text: partialQuestion, Vector: searchBasisVector(11), ModelID: searchModel,
		ViewerID: c.owner, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 || got[0].Chunk.ID != c.nought {
		t.Fatalf("found %v, want the passage found by meaning first", searchResultIDs(got))
	}
	for i, r := range got {
		if r.SparseRank != 0 {
			t.Errorf("result %d has keyword rank %d; no passage has every word", i, r.SparseRank)
		}
		if r.DenseRank != i+1 {
			t.Errorf("result %d has dense rank %d; the fusion is the meaning arm alone", i, r.DenseRank)
		}
	}
}

// When requiring every word already finds enough, nothing changes: the same
// passages in the same order, and nothing that only has some of the words.
func TestKeywordSearchKeepsEveryWordWhenThatFindsEnough(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)

	var passages []searchPassage
	for i := range 4 {
		passages = append(passages, searchPassage{
			text: fmt.Sprintf("Lemma %d holds for every prime.", i), major: i})
	}
	passages = append(passages,
		searchPassage{text: "A lemma with no number theory in it.", major: 10},
		searchPassage{text: "A prime with nothing proved about it.", major: 11})
	_, ids := seedSearchFile(t, p, owner, 302, domain.FormatEPUB, "Lemmas", "Paul Erdos", passages)
	strict, partial := ids[:4], ids[4:]

	for _, limit := range []int{2, 4} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			got, err := p.Search(t.Context(), SearchQuery{Text: "lemma prime", ViewerID: owner, Limit: limit})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(got) != limit {
				t.Fatalf("found %d, want %d", len(got), limit)
			}
			for i, r := range got {
				if !slices.Contains(strict, r.Chunk.ID) {
					t.Errorf("result %d has only some of the words", i)
				}
				if r.SparseRank != i+1 {
					t.Errorf("result %d has sparse rank %d", i, r.SparseRank)
				}
			}
		})
	}

	// Asking for more than every word finds tops the list up after them.
	got, err := p.Search(t.Context(), SearchQuery{Text: "lemma prime", ViewerID: owner, Limit: 6})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids6 := searchResultIDs(got)
	if len(ids6) != 6 || !searchSameIDs(ids6[:4], strict) || !searchSameIDs(ids6[4:], partial) {
		t.Fatalf("found %v, want the four with both words and then the two with one", ids6)
	}
}

// A question that excludes a word has said what it does not want. Matching
// any word would bring the excluded one back, so there is no fallback.
func TestKeywordSearchDoesNotFallBackPastAnExclusion(t *testing.T) {
	p := testPool(t)
	c := seedPartialCorpus(t, p)

	for _, tc := range []struct {
		query string
		want  []uuid.UUID
	}{
		// One strict match, fewer than asked for.
		{"creature -victor", []uuid.UUID{c.one}},
		// No strict match at all: the passage that abandons has Victor in it.
		{"abandon -victor", nil},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got, err := p.Search(t.Context(), SearchQuery{Text: tc.query, ViewerID: c.owner, Limit: 10})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if !slices.Equal(searchResultIDs(got), tc.want) {
				t.Fatalf("found %v, want %v", searchResultIDs(got), tc.want)
			}
		})
	}
}

// A question hundreds of words long searches for its rarest words, not all of
// them, and stays fast. Unbounded, a 400-word question took 11.6 seconds on
// 100,000 passages.
//
// "Rarest" is what the column statistics can say: the lexemes they list as
// common go last, and every other word counts as rare, in the order asked.
func TestKeywordFallbackSearchesTheRarestWordsOfALongQuestion(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)

	// Thirty common words in every passage, so the statistics know them.
	var commonWords []string
	for i := range 30 {
		commonWords = append(commonWords, fmt.Sprintf("common%sword", strings.Repeat("q", i+1)))
	}
	common := strings.Join(commonWords, " ")
	passages := make([]searchPassage, 0, 50)
	for i := range 49 {
		passages = append(passages, searchPassage{text: fmt.Sprintf("%s number %d", common, i), major: i})
	}
	// The one passage with the rare word.
	passages = append(passages, searchPassage{text: "The galvanism experiment succeeded.", major: 50})
	_, ids := seedSearchFile(t, p, owner, 303, domain.FormatEPUB, "Letters", "Robert Walton", passages)
	rare := ids[len(ids)-1]
	if _, err := p.Exec(t.Context(), `ANALYZE chunks`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	// The common words first - the first sixteen words would be nothing but
	// them - then the rare one, then four hundred words in no passage, which
	// are not searched for at all.
	var question strings.Builder
	question.WriteString(common + " galvanism")
	for i := range 400 {
		fmt.Fprintf(&question, " zq%dx", i)
	}

	start := time.Now()
	got, err := p.Search(t.Context(), SearchQuery{Text: question.String(), ViewerID: owner, Limit: 60})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// It has one of the sixteen words where the others have fifteen, so it
	// is last - but it was searched for.
	if !slices.Contains(searchResultIDs(got), rare) {
		t.Fatalf("the passage with the rare word was not found among %d", len(got))
	}
	// Generous, so a loaded machine does not fail it; the unbounded query
	// was far over it.
	if elapsed > 2*time.Second {
		t.Errorf("a long question took %v", elapsed)
	}
}

// A fallback that runs past the statement timeout costs the fallback, not the
// question: the search is run again without it.
func TestKeywordSearchThatTimesOutRetriesWithoutTheFallback(t *testing.T) {
	p := testPool(t)
	c := seedPartialCorpus(t, p)

	slowFallbackForTest = true
	timeout := searchStatementTimeout
	searchStatementTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		slowFallbackForTest = false
		searchStatementTimeout = timeout
	})

	// The strict arm finds one passage; the fallback would add two more.
	got, err := p.Search(t.Context(), SearchQuery{Text: "Victor creature forest", ViewerID: c.owner, Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v, want the search without the fallback", err)
	}
	if want := []uuid.UUID{c.two}; !slices.Equal(searchResultIDs(got), want) {
		t.Fatalf("found %v, want only the passage with every word", searchResultIDs(got))
	}
}
