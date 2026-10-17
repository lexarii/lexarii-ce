// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// The embeddings column is fixed at 768 wide, so a "small" known vector is a
// basis vector: one 1 and 767 zeros. Two different basis vectors are
// orthogonal, so the nearest neighbour of e_i is exactly the chunk carrying
// e_i, with nothing left to chance.
const searchTestDims = 768

func searchBasisVector(i int) []float32 {
	v := make([]float32, searchTestDims)
	v[i] = 1
	return v
}

// searchPassage is one chunk a search fixture writes, and the vector it gets.
type searchPassage struct {
	text   string
	major  int // spine index for EPUB, page for PDF
	vector []float32
	model  string
}

// searchBook ingests one book held by owner and gives it passages, returning
// the ingest outcome and the stored chunk ids in seq order.
func seedSearchFile(t *testing.T, p *Pool, owner uuid.UUID, seed int, format domain.Format,
	title, author string, passages []searchPassage) (IngestOutcome, []uuid.UUID) {
	t.Helper()
	ctx := t.Context()

	in := owned(sampleInput(fmt.Sprintf("%064x", seed),
		fmt.Sprintf("/library/search-%d.%s", seed, format), title, author), owner)
	in.Format = format
	in.Result.Format = format
	out, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("seeding %q: %v", title, err)
	}

	chunks := make([]ChunkInput, 0, len(passages))
	for i, ps := range passages {
		start := domain.Locator{Format: format, ElementPath: "body/p[1]"}
		if format == domain.FormatPDF {
			start.Page = ps.major
		} else {
			start.SpineIndex = ps.major
		}
		end := start
		end.CharOffset = len(ps.text)
		chunks = append(chunks, ChunkInput{
			Seq: i, Start: start, End: end, ChapterTitle: "Chapter",
			Content: ps.text, TokenCount: 10,
		})
	}
	if n, err := p.SaveChunks(ctx, out.BookFileID, chunks, 1); err != nil || n != len(chunks) {
		t.Fatalf("SaveChunks %q: wrote %d, err %v", title, n, err)
	}

	var ids []uuid.UUID
	rows, err := p.Query(ctx, `SELECT id FROM chunks WHERE book_file_id = $1 ORDER BY seq`, out.BookFileID)
	if err != nil {
		t.Fatalf("reading chunk ids: %v", err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning chunk id: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	for i, ps := range passages {
		if ps.vector == nil {
			continue
		}
		if _, err := p.SaveEmbeddings(ctx, ps.model, searchTestDims,
			[]EmbeddingInput{{ChunkID: ids[i], Vector: ps.vector}}); err != nil {
			t.Fatalf("SaveEmbeddings: %v", err)
		}
	}
	return out, ids
}

// searchCorpus is a small library with the shapes the search has to get right.
type searchCorpus struct {
	owner, stranger uuid.UUID

	algebra  IngestOutcome // owner's EPUB
	concrete IngestOutcome // owner's PDF
	diary    IngestOutcome // stranger's own book

	algebraEigen, algebraDet uuid.UUID
	concreteGen, concreteSum uuid.UUID
	diaryEigen               uuid.UUID
	goneEigen                uuid.UUID
}

const (
	searchModel      = "test-embed"
	searchOtherModel = "other-embed"
)

func seedSearchCorpus(t *testing.T, p *Pool) searchCorpus {
	t.Helper()
	cleanLibrary(t, p)
	c := searchCorpus{owner: testOwner(t, p), stranger: secondUser(t, p)}

	var ids []uuid.UUID
	c.algebra, ids = seedSearchFile(t, p, c.owner, 101, domain.FormatEPUB,
		"Linear Algebra Done Right", "Sheldon Axler", []searchPassage{
			{text: "Eigenvalues and eigenvectors of a linear operator on a finite dimensional space.",
				major: 3, vector: searchBasisVector(0), model: searchModel},
			{text: "Determinants appear late in this treatment of vector spaces.",
				major: 4, vector: searchBasisVector(1), model: searchModel},
		})
	c.algebraEigen, c.algebraDet = ids[0], ids[1]

	c.concrete, ids = seedSearchFile(t, p, c.owner, 102, domain.FormatPDF,
		"Concrete Mathematics", "Ronald Graham", []searchPassage{
			{text: "Generating functions turn recurrences into algebra, and eigenvalues appear briefly.",
				major: 7, vector: searchBasisVector(2), model: searchModel},
			// Embedded only by a different model: its vector must never be
			// compared with a question embedded by the first one.
			{text: "Sums, floors and ceilings.",
				major: 8, vector: searchBasisVector(3), model: searchOtherModel},
		})
	c.concreteGen, c.concreteSum = ids[0], ids[1]

	// Somebody else's book, about the same thing, with the very vector the
	// owner will ask with. It is the nearest passage in the whole table and it
	// must still never come back to the owner.
	c.diary, ids = seedSearchFile(t, p, c.stranger, 103, domain.FormatEPUB,
		"A Private Diary", "Someone Else", []searchPassage{
			{text: "My notes on eigenvalues, which nobody else should read.",
				major: 1, vector: searchBasisVector(0), model: searchModel},
		})
	c.diaryEigen = ids[0]

	// The owner's, but on a drive that is not mounted.
	gone, ids := seedSearchFile(t, p, c.owner, 104, domain.FormatEPUB,
		"A Lost Volume", "Missing Person", []searchPassage{
			{text: "Eigenvalues in a book whose drive is unmounted.",
				major: 1, vector: searchBasisVector(0), model: searchModel},
		})
	c.goneEigen = ids[0]
	if _, err := p.Exec(t.Context(),
		`UPDATE book_files SET missing_since = now() WHERE id = $1`, gone.BookFileID); err != nil {
		t.Fatalf("marking a file missing: %v", err)
	}
	return c
}

func searchResultIDs(results []domain.RetrievedChunk) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(results))
	for _, r := range results {
		out = append(out, r.Chunk.ID)
	}
	return out
}

func searchSameIDs(got, want []uuid.UUID) bool {
	a, b := sortedIDs(got), sortedIDs(want)
	return slices.Equal(a, b)
}

func TestSearchRefusesAQueryItCannotScope(t *testing.T) {
	p := testPool(t)
	viewer := testOwner(t, p)

	for name, q := range map[string]SearchQuery{
		"no question at all":    {ViewerID: viewer},
		"a blank question":      {Text: "   \t", ViewerID: viewer},
		"no viewer, by text":    {Text: "eigenvalues"},
		"no viewer, by meaning": {Vector: searchBasisVector(0), ModelID: searchModel},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := p.Search(t.Context(), q)
			if !errors.Is(err, store.ErrInvalidQuery) {
				t.Fatalf("err = %v, want ErrInvalidQuery", err)
			}
			if got != nil {
				t.Fatalf("a refused search still returned %d passages", len(got))
			}
		})
	}
}

// The access join is the whole point of a viewer: a passage from a book this
// person cannot open is absent, not ranked low.
func TestKeywordSearchFindsOnlyWhatTheViewerCanOpen(t *testing.T) {
	p := testPool(t)
	c := seedSearchCorpus(t, p)
	ctx := t.Context()

	got, err := p.Search(ctx, SearchQuery{Text: "eigenvalues", ViewerID: c.owner})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if want := []uuid.UUID{c.algebraEigen, c.concreteGen}; !searchSameIDs(searchResultIDs(got), want) {
		t.Fatalf("owner found %v, want exactly their own two readable passages %v",
			searchResultIDs(got), want)
	}
	for _, r := range got {
		if r.DenseRank != 0 || r.SparseRank == 0 {
			t.Errorf("keyword-only result has dense rank %d, sparse rank %d", r.DenseRank, r.SparseRank)
		}
	}

	// The stranger finds their own diary and nothing of the owner's.
	got, err = p.Search(ctx, SearchQuery{Text: "eigenvalues", ViewerID: c.stranger})
	if err != nil {
		t.Fatalf("Search (stranger): %v", err)
	}
	if want := []uuid.UUID{c.diaryEigen}; !searchSameIDs(searchResultIDs(got), want) {
		t.Fatalf("stranger found %v, want only their diary", searchResultIDs(got))
	}

	// Lending one book makes exactly that book's passages findable.
	if err := p.ShareBook(ctx, c.algebra.BookFileID, c.owner, c.stranger, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	got, err = p.Search(ctx, SearchQuery{Text: "eigenvalues", ViewerID: c.stranger})
	if err != nil {
		t.Fatalf("Search (after lending): %v", err)
	}
	if want := []uuid.UUID{c.diaryEigen, c.algebraEigen}; !searchSameIDs(searchResultIDs(got), want) {
		t.Fatalf("after lending one book the stranger found %v, want %v", searchResultIDs(got), want)
	}
}

// The citation has to open where the passage is, and "where" is spelled
// differently by each format.
func TestSearchResultsCarryTheirFormatsLocatorAndWork(t *testing.T) {
	p := testPool(t)
	c := seedSearchCorpus(t, p)

	got, err := p.Search(t.Context(), SearchQuery{Text: "eigenvalues", ViewerID: c.owner})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	byID := map[uuid.UUID]domain.RetrievedChunk{}
	for _, r := range got {
		byID[r.Chunk.ID] = r
	}

	pdf := byID[c.concreteGen]
	if pdf.Chunk.Range.Start.Format != domain.FormatPDF || pdf.Chunk.Range.Start.Page != 7 ||
		pdf.Chunk.Range.Start.SpineIndex != 0 || pdf.Chunk.Range.End.Page != 7 {
		t.Errorf("PDF passage locator = %+v, want page 7", pdf.Chunk.Range)
	}
	if pdf.WorkID != c.concrete.WorkID || pdf.WorkTitle != "Concrete Mathematics" {
		t.Errorf("PDF passage cites work %s %q", pdf.WorkID, pdf.WorkTitle)
	}

	epub := byID[c.algebraEigen]
	if epub.Chunk.Range.Start.Format != domain.FormatEPUB || epub.Chunk.Range.Start.SpineIndex != 3 ||
		epub.Chunk.Range.Start.Page != 0 || epub.Chunk.Range.Start.ElementPath != "body/p[1]" {
		t.Errorf("EPUB passage locator = %+v, want spine 3", epub.Chunk.Range)
	}
	if epub.Chunk.BookFileID != c.algebra.BookFileID || epub.Chunk.ChapterTitle != "Chapter" ||
		epub.Chunk.TokenCount != 10 || epub.Chunk.Content == "" {
		t.Errorf("EPUB passage = %+v", epub.Chunk)
	}
}

// Vectors from two models are not comparable. A search under one model must
// neither see the other's vectors nor be ranked against them.
func TestVectorSearchStaysWithinOneModel(t *testing.T) {
	p := testPool(t)
	c := seedSearchCorpus(t, p)
	ctx := t.Context()

	got, err := p.Search(ctx, SearchQuery{
		Vector: searchBasisVector(0), ModelID: searchModel, ViewerID: c.owner,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 || got[0].Chunk.ID != c.algebraEigen {
		t.Fatalf("nearest passage = %v, want the owner's eigenvalue passage", searchResultIDs(got))
	}
	if got[0].DenseRank != 1 || got[0].SparseRank != 0 {
		t.Errorf("meaning-only best match has ranks dense %d sparse %d", got[0].DenseRank, got[0].SparseRank)
	}
	// Every readable passage with a vector under this model, and nothing else.
	if want := []uuid.UUID{c.algebraEigen, c.algebraDet, c.concreteGen}; !searchSameIDs(searchResultIDs(got), want) {
		t.Fatalf("meaning search returned %v, want %v", searchResultIDs(got), want)
	}

	// Under the other model, only the one passage it embedded.
	got, err = p.Search(ctx, SearchQuery{
		Vector: searchBasisVector(3), ModelID: searchOtherModel, ViewerID: c.owner,
	})
	if err != nil {
		t.Fatalf("Search (other model): %v", err)
	}
	if want := []uuid.UUID{c.concreteSum}; !searchSameIDs(searchResultIDs(got), want) {
		t.Fatalf("other-model search returned %v, want only %v", searchResultIDs(got), want)
	}

	// A model nothing was embedded with finds nothing by meaning.
	got, err = p.Search(ctx, SearchQuery{
		Vector: searchBasisVector(0), ModelID: "never-used", ViewerID: c.owner,
	})
	if err != nil {
		t.Fatalf("Search (unknown model): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an unknown model matched %v", searchResultIDs(got))
	}
}

// A question whose vector does not fit the column is an error, not an empty
// answer that looks like "nothing in your library".
func TestVectorSearchRejectsTheWrongDimensions(t *testing.T) {
	p := testPool(t)
	c := seedSearchCorpus(t, p)

	got, err := p.Search(t.Context(), SearchQuery{
		Vector: []float32{1, 0, 0}, ModelID: searchModel, ViewerID: c.owner,
	})
	if err == nil {
		t.Fatalf("a 3-dimensional question against a 768-wide column returned %d passages", len(got))
	}
}

// Both arms agreeing is what hybrid search exists for, and the score is RRF.
func TestHybridSearchFusesBothArms(t *testing.T) {
	p := testPool(t)
	c := seedSearchCorpus(t, p)

	const k = 10.0
	// "eigenvectors" is in one passage only, and e_0 is that passage's
	// vector, so both arms rank it first and nothing ties with it.
	got, err := p.Search(t.Context(), SearchQuery{
		Text: "eigenvectors", Vector: searchBasisVector(0), ModelID: searchModel,
		ViewerID: c.owner, RRFK: k,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 || got[0].Chunk.ID != c.algebraEigen {
		t.Fatalf("results %v, want the passage both arms rank first on top", searchResultIDs(got))
	}
	top := got[0]
	if top.DenseRank != 1 || top.SparseRank == 0 {
		t.Fatalf("top result ranks: dense %d sparse %d, want both arms", top.DenseRank, top.SparseRank)
	}
	want := 1/(k+1) + 1/(k+float64(top.SparseRank))
	if math.Abs(top.FusedScore-want) > 1e-9 {
		t.Errorf("fused score = %v, want %v", top.FusedScore, want)
	}
	for i := 1; i < len(got); i++ {
		if got[i].FusedScore > got[i-1].FusedScore {
			t.Fatalf("results out of order at %d: %v after %v", i, got[i].FusedScore, got[i-1].FusedScore)
		}
	}
	// A passage only one arm found is still there, below.
	if !slices.Contains(searchResultIDs(got), c.algebraDet) {
		t.Errorf("the meaning-only passage was dropped from the fused list")
	}
}

// Filters narrow both arms, so a pinned book gets its own passages and only
// those, whichever arm found them.
func TestSearchFiltersNarrowBothArms(t *testing.T) {
	p := testPool(t)
	c := seedSearchCorpus(t, p)
	ctx := t.Context()

	got, err := p.Search(ctx, SearchQuery{
		Text: "eigenvalues", Vector: searchBasisVector(0), ModelID: searchModel,
		ViewerID: c.owner, WorkIDs: []uuid.UUID{c.concrete.WorkID},
	})
	if err != nil {
		t.Fatalf("Search (works): %v", err)
	}
	if want := []uuid.UUID{c.concreteGen}; !searchSameIDs(searchResultIDs(got), want) {
		t.Fatalf("narrowed to one work, got %v want %v", searchResultIDs(got), want)
	}

	pinned := c.algebra.BookFileID
	got, err = p.Search(ctx, SearchQuery{
		Text: "eigenvalues", Vector: searchBasisVector(2), ModelID: searchModel,
		ViewerID: c.owner, BookFileID: &pinned,
	})
	if err != nil {
		t.Fatalf("Search (file): %v", err)
	}
	if want := []uuid.UUID{c.algebraEigen, c.algebraDet}; !searchSameIDs(searchResultIDs(got), want) {
		t.Fatalf("pinned to one file, got %v want %v", searchResultIDs(got), want)
	}

	// Pinned to somebody else's file, the viewer gets nothing at all.
	foreign := c.diary.BookFileID
	got, err = p.Search(ctx, SearchQuery{Text: "eigenvalues", ViewerID: c.owner, BookFileID: &foreign})
	if err != nil {
		t.Fatalf("Search (foreign file): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("pinned to a stranger's file the owner found %v", searchResultIDs(got))
	}
}

func TestSearchLimitAndCandidates(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)

	passages := make([]searchPassage, 15)
	for i := range passages {
		passages[i] = searchPassage{text: fmt.Sprintf("Lemma %d holds for every prime.", i), major: i}
	}
	seedSearchFile(t, p, owner, 201, domain.FormatEPUB, "A Book of Lemmas", "Paul Erdos", passages)

	tests := []struct {
		name       string
		limit      int
		candidates int
		want       int
	}{
		{"zero uses the default", 0, 0, DefaultSearchLimit},
		{"negative uses the default", -4, 0, DefaultSearchLimit},
		{"too large uses the default", 201, 0, DefaultSearchLimit},
		{"an explicit limit", 3, 0, 3},
		{"candidates are raised to the limit", 14, 5, 14},
		{"absurd candidates fall back to the default", 13, 5000, 13},
		{"more than there is returns what there is", 200, 0, 15},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.Search(t.Context(), SearchQuery{
				Text: "lemma prime", ViewerID: owner, Limit: tc.limit, Candidates: tc.candidates,
			})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d passages, want %d", len(got), tc.want)
			}
			// Equal keyword scores are broken deterministically, so the
			// sparse ranks are 1..n in result order with no repeats.
			for i, r := range got {
				if r.SparseRank != i+1 {
					t.Fatalf("result %d has sparse rank %d", i, r.SparseRank)
				}
			}
		})
	}
}

func TestEFSearchDepthFollowsTheCandidateCount(t *testing.T) {
	for _, tc := range []struct{ candidates, want int }{
		{0, 40},
		{12, 40},
		{20, 40},
		{30, 60},
		{60, 120},
		{200, 400},
		{1000, 400},
	} {
		if got := efSearchFor(tc.candidates); got != tc.want {
			t.Errorf("efSearchFor(%d) = %d, want %d", tc.candidates, got, tc.want)
		}
	}
}

func TestWorksMatchingSubjects(t *testing.T) {
	p := testPool(t)
	subjectLibrary(t, p)
	ctx := t.Context()

	titles := func(ids []uuid.UUID) []string {
		t.Helper()
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			var title string
			if err := p.QueryRow(ctx, `SELECT title FROM works WHERE id = $1`, id).Scan(&title); err != nil {
				t.Fatalf("reading title: %v", err)
			}
			out = append(out, title)
		}
		return out
	}

	t.Run("books matching more of the subjects come first", func(t *testing.T) {
		ids, err := p.WorksMatchingSubjects(ctx, []string{"Docker", "DevOps"}, 0)
		if err != nil {
			t.Fatalf("WorksMatchingSubjects: %v", err)
		}
		want := []string{"Docker for Developers", "Docker Cookbook"}
		if got := titles(ids); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("equal matches are ordered by title", func(t *testing.T) {
		ids, err := p.WorksMatchingSubjects(ctx, []string{"docker"}, 10)
		if err != nil {
			t.Fatalf("WorksMatchingSubjects: %v", err)
		}
		want := []string{"Docker Cookbook", "Docker for Developers"}
		if got := titles(ids); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("the limit is honoured", func(t *testing.T) {
		ids, err := p.WorksMatchingSubjects(ctx, []string{"docker"}, 1)
		if err != nil {
			t.Fatalf("WorksMatchingSubjects: %v", err)
		}
		if len(ids) != 1 {
			t.Fatalf("got %d works, want 1", len(ids))
		}
	})

	for name, subjects := range map[string][]string{
		"no subjects":               nil,
		"subjects with no letters":  {"  ", "!!!"},
		"a subject nobody has used": {"medieval history"},
	} {
		t.Run(name, func(t *testing.T) {
			ids, err := p.WorksMatchingSubjects(ctx, subjects, 10)
			if err != nil {
				t.Fatalf("WorksMatchingSubjects: %v", err)
			}
			if len(ids) != 0 {
				t.Fatalf("got %v", titles(ids))
			}
		})
	}
}

// Words that match half the shelf narrow nothing, and are not searched for.
func TestMatchingSubjectsIgnoresStopWords(t *testing.T) {
	p := testPool(t)
	subjectLibrary(t, p)

	matches, err := p.MatchingSubjects(t.Context(), []string{"The Guide", "books and", "with"})
	if err != nil {
		t.Fatalf("MatchingSubjects: %v", err)
	}
	if matches != nil {
		t.Fatalf("a question of nothing but stop words matched %+v", matches)
	}
}

// Anything far weaker than the best match is cut rather than ranked last: a
// question about agile software development that finds an agile shelf does
// not want every book that merely shares the word "development".
func TestMatchingSubjectsDropsFarWeakerMatches(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)

	for i, subjects := range [][]string{
		{"agile software development"},
		{"agile"},
		{"android development"},
		{"web development"},
		{"game development"},
	} {
		in := owned(sampleInput(fmt.Sprintf("%064x", 801+i), fmt.Sprintf("/library/agile-%d.epub", i),
			fmt.Sprintf("Shelf Item %d", i), "An Author"), owner)
		in.Result.Metadata.Subjects = subjects
		if _, err := p.UpsertBookFile(t.Context(), in); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	matches, err := p.MatchingSubjects(t.Context(), []string{"agile software development"})
	if err != nil {
		t.Fatalf("MatchingSubjects: %v", err)
	}
	slugs := make([]string, 0, len(matches))
	for _, m := range matches {
		slugs = append(slugs, m.Slug)
	}
	// "agile" alone, and the four "development" tags, are each worth less
	// than half of the tag matching all three words.
	if want := []string{"agile-software-development"}; !slices.Equal(slugs, want) {
		t.Fatalf("matches = %v, want %v", slugs, want)
	}
	if matches[0].Books != 1 || matches[0].Relevance <= 0 {
		t.Errorf("match = %+v", matches[0])
	}
}

func TestLibraryFactsDescribeOnlyTheViewersShelf(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner := testOwner(t, p)
	stranger := secondUser(t, p)

	// Three of the owner's: two by one author, one untitled.
	seedSearchFile(t, p, owner, 301, domain.FormatEPUB, "Structure and Interpretation", "Harold Abelson",
		[]searchPassage{{text: "Procedures and the processes they generate.", major: 1}})
	second, _ := seedSearchFile(t, p, owner, 302, domain.FormatPDF, "Essentials of Programming", "Harold Abelson", nil)
	seedSearchFile(t, p, owner, 303, domain.FormatEPUB, "", "Anonymous Writer", nil)
	// The stranger's, with a subject and an author nobody else has.
	in := owned(sampleInput(fmt.Sprintf("%064x", 304), "/library/secret.epub", "A Secret Work", "Hidden Author"), stranger)
	in.Result.Metadata.Subjects = []string{"Secret Subject"}
	if _, err := p.UpsertBookFile(ctx, in); err != nil {
		t.Fatalf("seeding the stranger's book: %v", err)
	}

	save(t, p, owner, second.BookFileID, domain.Locator{Format: domain.FormatPDF, Page: 3}, 0.1)

	facts, err := p.LibraryFacts(ctx, owner)
	if err != nil {
		t.Fatalf("LibraryFacts: %v", err)
	}
	if facts.Works != 3 || facts.Files != 3 {
		t.Errorf("works %d files %d, want 3 and 3", facts.Works, facts.Files)
	}
	if facts.Untitled != 1 {
		t.Errorf("untitled = %d, want 1", facts.Untitled)
	}
	if facts.Searchable != 1 {
		t.Errorf("searchable = %d, want only the chunked file", facts.Searchable)
	}
	if facts.Formats[domain.FormatEPUB] != 2 || facts.Formats[domain.FormatPDF] != 1 {
		t.Errorf("formats = %v", facts.Formats)
	}
	// sampleInput tags every book "Computer science" and "Algorithms": a tie
	// at three books each, broken by name.
	wantSubjects := []SubjectCount{{"Algorithms", 3}, {"Computer science", 3}}
	if !slices.Equal(facts.TopSubjects, wantSubjects) {
		t.Errorf("subjects = %+v, want %+v", facts.TopSubjects, wantSubjects)
	}
	if len(facts.TopAuthors) == 0 || facts.TopAuthors[0] != (AuthorCount{"Harold Abelson", 2}) {
		t.Errorf("top authors = %+v, want Harold Abelson with 2 first", facts.TopAuthors)
	}
	for _, a := range facts.TopAuthors {
		if a.Name == "Hidden Author" {
			t.Errorf("the stranger's author is in the owner's facts")
		}
	}
	if len(facts.Recent) != 3 || slices.Contains(facts.Recent, "A Secret Work") {
		t.Errorf("recent = %v", facts.Recent)
	}
	if !slices.Equal(facts.InProgress, []string{"Essentials of Programming"}) {
		t.Errorf("in progress = %v", facts.InProgress)
	}

	theirs, err := p.LibraryFacts(ctx, stranger)
	if err != nil {
		t.Fatalf("LibraryFacts (stranger): %v", err)
	}
	if theirs.Works != 1 || theirs.Searchable != 0 || len(theirs.InProgress) != 0 ||
		!slices.Equal(theirs.Recent, []string{"A Secret Work"}) {
		t.Errorf("stranger's facts = %+v", theirs)
	}
	if !slices.Equal(theirs.TopSubjects, []SubjectCount{{"Secret Subject", 1}}) {
		t.Errorf("stranger's subjects = %+v", theirs.TopSubjects)
	}
	if !slices.Equal(theirs.TopAuthors, []AuthorCount{{"Hidden Author", 1}}) {
		t.Errorf("stranger's authors = %+v", theirs.TopAuthors)
	}

	if _, err := p.LibraryFacts(ctx, uuid.Nil); !errors.Is(err, store.ErrInvalidQuery) {
		t.Errorf("facts without a viewer: err = %v, want ErrInvalidQuery", err)
	}
}
