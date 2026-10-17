// SPDX-License-Identifier: AGPL-3.0-or-later

// Command eval measures retrieval quality against a fixed question set.
//
// Retrieval tuned by vibes regresses silently. Chunk size, fusion constants and
// a reranker all feel like improvements while they are being written, and the
// only way to know is to ask the same questions before and after and compare
// two numbers.
//
// Two are reported, at the work level and at the passage level:
//
//	Recall@k  the share of questions where something correct appeared at all
//	MRR       the mean of 1/rank of the first correct result
//
// Recall says whether the answer was available to the model; MRR says whether
// it was near the top, which is what survives a context window. A change that
// raises recall and lowers MRR has moved the right passage further down.
//
// Questions that expect NOTHING are scored too, and they matter as much: a
// question about the mating habits of the Andromedan sand-eel must be refused,
// and a library question must not reach retrieval at all. A system that scores
// well on the first kind and badly on these is one that confabulates
// confidently, which is worse than one that finds less.
//
// READ THIS BEFORE QUOTING A NUMBER. Each question names ONE correct book, the
// one its passage came from, and a personal library has many books that cover
// the same ground. Asked how to match Greek letters with a regular expression,
// retrieval returned the Regular Expressions Cookbook and Mastering Regular
// Expressions ahead of the labelled grep Pocket Reference, and was marked wrong
// for it. Recall and MRR here are therefore LOWER BOUNDS on real quality.
//
// `-label` narrows that gap: it retrieves for every question, asks the model
// whether each book's passage actually answers it, and records the ones that do
// in `alsoAcceptable`. On the real set that moved work Recall from 0.875 to
// 0.906 and MRR from 0.724 to 0.755 WITHOUT CHANGING RETRIEVAL AT ALL - the
// labels had been wrong, not the results.
//
// Those labels are weaker than the ones written by construction, and are kept
// in a separate field for that reason. Re-running -label invalidates every
// earlier summary: a comparison only means something when both runs were scored
// against the same ground truth, so re-record the baseline after labelling.
//
// Use -out to record a run and -compare to measure the next one against it. The
// difference between two runs is what this is for; do not quote the absolute
// figure as though it described the system.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// Question is one line of the eval set.
type Question struct {
	Comment string `json:"_comment,omitempty"`

	ID       string `json:"id"`
	Question string `json:"question"`
	Intent   string `json:"intent"`

	// ExpectedWorks names the books that should be retrieved. A UUID matches by
	// id; anything else matches by title, case-insensitively. Titles are
	// deliberately allowed, because work ids are reassigned every time a library
	// is re-ingested and an eval set that dies on a reimport will not be kept.
	ExpectedWorks []string `json:"expectedWorks"`
	// ExpectedLocators names the passages that should rank highly. A locator
	// matches a chunk when it falls inside that chunk's range, so the set does
	// not have to be rewritten when chunking changes.
	ExpectedLocators []string `json:"expectedLocators"`
	// AlsoAcceptable names other books that answer the question just as well.
	//
	// Kept separate from ExpectedWorks because it is a WEAKER label: expected
	// works are correct by construction - the question was written from a
	// passage in that book - while these were judged by a model reading the
	// passage. Mixing them would hide which half of the ground truth is solid.
	// Written by `-label`.
	AlsoAcceptable []string `json:"alsoAcceptable,omitempty"`
	// ExpectAll requires EVERY expected work, not just the first one found.
	//
	// Recall and MRR both score the rank of the first matching book, which is
	// the right measure for a question with one answer and the wrong one for a
	// question that needs two books at once: "what do my books say about
	// testing in Go and in Java" is not answered by finding the Go one. Such a
	// question is still scored the ordinary way - so the headline numbers stay
	// comparable across runs - and is additionally counted in the coverage
	// line, which is the number that means anything for it.
	ExpectAll bool `json:"expectAll,omitempty"`

	Notes string `json:"notes,omitempty"`
}

// Result is one question's score.
type Result struct {
	ID       string `json:"id"`
	Question string `json:"question"`

	Intent      string `json:"intent"`
	WantIntent  string `json:"wantIntent"`
	IntentRight bool   `json:"intentRight"`

	WorkRank    int `json:"workRank"`    // 1-based, 0 = not found
	PassageRank int `json:"passageRank"` // 1-based, 0 = not found

	// Covered and Wanted count the expected works actually retrieved, for a
	// question that needs all of them. Zero for every other question.
	Covered int `json:"covered,omitempty"`
	Wanted  int `json:"wanted,omitempty"`
	// PassageWanted is true when the question named a passage to find. A
	// find-book question never does, and must not be counted as having missed
	// one.
	PassageWanted bool `json:"passageWanted,omitempty"`

	// Offered is how many books a reading list came back with. For a control
	// that should find nothing, it is the number that says how badly: a list
	// of eight books about a subject the library does not hold is worse than
	// a list of one.
	Offered int `json:"offered,omitempty"`

	// Abstain is set for questions that expect nothing. Refused records whether
	// the library actually declined to answer, which is the thing being tested.
	Abstain   bool   `json:"abstain,omitempty"`
	Refused   bool   `json:"refused,omitempty"`
	Retrieved int    `json:"retrieved"`
	Answer    string `json:"answer,omitempty"`

	RetrievalMS int    `json:"retrievalMs"`
	Error       string `json:"error,omitempty"`
}

// Summary is the whole run, and is what gets compared between runs.
type Summary struct {
	RunAt  time.Time `json:"runAt"`
	Label  string    `json:"label,omitempty"`
	K      int       `json:"k"`
	Model  string    `json:"embedModel"`
	Scored int       `json:"scored"`
	// Reranked records whether a cross-encoder was in the pipeline, so two
	// summaries cannot be compared without noticing that they measured
	// different things.
	Reranked bool `json:"reranked"`
	Abstains int  `json:"abstains"`

	WorkRecall    float64 `json:"workRecall"`
	WorkMRR       float64 `json:"workMrr"`
	PassageRecall float64 `json:"passageRecall"`
	PassageMRR    float64 `json:"passageMrr"`
	// PassageAsked is how many questions named a passage, which is the
	// denominator of the two figures above.
	PassageAsked int     `json:"passageQuestions,omitempty"`
	IntentAcc    float64 `json:"intentAccuracy"`
	AbstainRate  float64 `json:"abstainRate"`
	MedianMS     int     `json:"medianMs"`

	// Coverage is the share of the books a multi-book question needed that were
	// actually retrieved, over the questions that named more than one.
	Coverage      float64 `json:"coverage,omitempty"`
	CoverageAsked int     `json:"coverageQuestions,omitempty"`

	Results []Result `json:"results"`
}

func main() {
	var (
		path      = flag.String("questions", "eval/questions.jsonl", "eval set, one JSON object per line")
		k         = flag.Int("k", 10, "cut-off for Recall@k")
		out       = flag.String("out", "", "write the summary as JSON to this path")
		compare   = flag.String("compare", "", "an earlier summary JSON to show deltas against")
		labelName = flag.String("name", "", "a name for this run, recorded in the summary")
		verbose   = flag.Bool("v", false, "print every question, not just the summary")
		// Default ON when a reranker is configured, because the eval has to
		// measure the pipeline this installation actually runs - the same
		// reason it calls Retrieve rather than the index directly. Pass
		// -rerank=false to measure without one and compare.
		rerank = flag.Bool("rerank", true, "use the configured cross-encoder, when there is one")
		label  = flag.Bool("label", false, "judge retrieved books and record the acceptable ones, rewriting the question file")
	)
	flag.Parse()

	if err := run(*path, *k, *out, *compare, *labelName, *verbose, *rerank, *label); err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		os.Exit(1)
	}
}

func run(path string, k int, out, compare, label string, verbose, rerank, relabel bool) error {
	questions, err := load(path)
	if err != nil {
		return err
	}
	if len(questions) == 0 {
		return fmt.Errorf("%s has no questions", path)
	}

	ctx := context.Background()
	// An unset DATABASE_URL otherwise fails as a Unix socket error naming a
	// path nobody chose - `dial unix /tmp/.s.PGSQL.5432` - which reads as "the
	// database is down" rather than "this needs the environment". The Makefile
	// loads .env; a bare `go run` does not.
	database := os.Getenv("DATABASE_URL")
	if database == "" {
		return errors.New("DATABASE_URL is not set; run `set -a; . ./.env; set +a` first, " +
			"or pass it inline")
	}

	pool, err := postgres.Open(ctx, database)
	if err != nil {
		return fmt.Errorf("connecting to the database: %w", err)
	}
	defer pool.Close()

	embed := llm.New(config.ModelConfig{
		BaseURL:        os.Getenv("LEXARII_EMBED_BASE_URL"),
		Model:          os.Getenv("LEXARII_EMBED_MODEL"),
		APIKey:         os.Getenv("LEXARII_EMBED_API_KEY"),
		MaxConcurrency: 1,
		Timeout:        60 * time.Second,
	})
	if os.Getenv("LEXARII_EMBED_BASE_URL") == "" {
		return errors.New("no embedding model configured; set LEXARII_EMBED_BASE_URL")
	}
	chat := llm.New(config.ModelConfig{
		BaseURL:        os.Getenv("LEXARII_LLM_BASE_URL"),
		Model:          os.Getenv("LEXARII_LLM_MODEL"),
		APIKey:         os.Getenv("LEXARII_LLM_API_KEY"),
		MaxConcurrency: 2,
		Timeout:        300 * time.Second,
	})

	dimensions := 768
	if n := os.Getenv("LEXARII_EMBED_DIMENSIONS"); n != "" {
		if _, err := fmt.Sscanf(n, "%d", &dimensions); err != nil {
			return fmt.Errorf("LEXARII_EMBED_DIMENSIONS: %w", err)
		}
	}

	// The same window the server runs with. An eval measuring a pipeline that
	// sends more to the model than production does is measuring nothing.
	contextTokens := 8192
	if n := os.Getenv("LEXARII_LLM_CONTEXT_TOKENS"); n != "" {
		if _, err := fmt.Sscanf(n, "%d", &contextTokens); err != nil {
			return fmt.Errorf("LEXARII_LLM_CONTEXT_TOKENS: %w", err)
		}
	}

	summary := Summary{
		RunAt: time.Now().UTC(),
		Label: label,
		K:     k,
		Model: embed.Model(),
	}

	// The eval reads somebody's library, and books belong to people: a search
	// with no viewer is refused rather than run unfiltered. Without this every
	// question scores zero, which is exactly what happened when per-book access
	// landed and this was not updated.
	viewer, err := libraryOwner(ctx, pool)
	if err != nil {
		return err
	}

	// The reranker this installation is configured with, if any. llm.New
	// returns nil for an unconfigured endpoint, which is exactly the "no
	// reranking" case and needs no special handling.
	var reranker *llm.Client
	if rerank {
		reranker = llm.New(config.ModelConfig{
			BaseURL:        os.Getenv("LEXARII_RERANK_BASE_URL"),
			Model:          os.Getenv("LEXARII_RERANK_MODEL"),
			APIKey:         os.Getenv("LEXARII_RERANK_API_KEY"),
			MaxConcurrency: 1,
			Timeout:        60 * time.Second,
		})
	}

	summary.Reranked = reranker != nil

	answerer := &rag.Answerer{
		Store: pool, Chat: chat, Embed: embed,
		Dimensions: dimensions, Reranker: reranker,
		ContextTokens: contextTokens,
	}

	if relabel {
		return relabelSet(ctx, answerer, chat, questions, path, k, viewer)
	}

	for _, q := range questions {
		summary.Results = append(summary.Results, score(ctx, answerer, q, k, viewer))
	}
	tally(&summary)

	report(summary, verbose)

	if compare != "" {
		before, err := loadSummary(compare)
		if err != nil {
			return err
		}
		fmt.Println()
		delta(before, summary)
	}

	if out != "" {
		body, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, append(body, '\n'), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", out, err)
		}
		fmt.Printf("\nwrote %s\n", out)
	}
	return nil
}

// score runs one question and marks it.
func score(ctx context.Context, answerer *rag.Answerer, q Question, k int, viewer uuid.UUID) Result {

	res := Result{
		ID:            q.ID,
		Question:      q.Question,
		WantIntent:    q.Intent,
		Abstain:       len(q.ExpectedWorks) == 0 && len(q.ExpectedLocators) == 0,
		PassageWanted: len(q.ExpectedLocators) > 0,
	}

	// The whole pipeline, not a bare index lookup: routing narrows which books
	// are searched and ranking decides the order, and a change to either must
	// show up here or the eval is measuring something nobody uses.
	start := time.Now()
	route, hits, err := answerer.Retrieve(ctx, q.Question, rag.Scope{UserID: viewer})
	res.RetrievalMS = int(time.Since(start).Milliseconds())
	if err != nil {
		res.Error = fmt.Sprintf("retrieving: %v", err)
		return res
	}

	// Routing is scored separately from ranking, because an intent error and a
	// ranking error need different fixes and averaging them hides both.
	res.Intent = string(route.Intent)
	res.IntentRight = q.Intent == "" || string(route.Intent) == q.Intent

	// A question routed away from retrieval never reaches the index, and
	// scoring it on ranking would be scoring a search that was correctly never
	// run. For a question that expects nothing, being routed away IS the right
	// outcome: a library question answered from SQL has not confabulated from
	// passages, which is the whole point of the control.
	if !route.Intent.NeedsRetrieval() {
		// A question that WANTS books is answered from the catalogue rather
		// than from passages, and is scored where the product answers it.
		// Until this set had a find-book question with an expectation, every
		// question routed away from retrieval was a control and returning here
		// was right; now it would report the product's own answer as a miss.
		if len(q.ExpectedWorks) > 0 {
			return scoreBookList(ctx, answerer, q, res, viewer)
		}

		// A control that asks for a SHELF has to be marked on the shelf it got.
		//
		// Without this it was marked on having been routed away from
		// retrieval, which for a reading list is not a refusal at all - it is
		// the normal path. "Cookery and recipes" would have scored as a
		// correct abstention while returning eight books called Cookbook, none
		// of which are about food.
		if res.Abstain && listsBooks(route.Intent) {
			answer, err := answerer.Ask(ctx, q.Question, rag.Scope{UserID: viewer})
			if err != nil {
				res.Error = fmt.Sprintf("answering: %v", err)
				return res
			}
			res.Answer = answer.Text
			res.Offered = len(answer.Books)
			res.Refused = len(answer.Books) == 0
			return res
		}

		res.Refused = res.Abstain
		return res
	}

	if len(hits) > k {
		hits = hits[:k]
	}
	res.Retrieved = len(hits)

	// An out-of-library question cannot be scored on what came back, because
	// dense search ALWAYS returns its nearest neighbours - there is no such
	// thing as an empty vector result. Ten loosely related passages is the
	// normal, correct output of retrieval here. What is being tested is what
	// the library then does with them, so the question is answered in full and
	// the answer is what gets marked.
	if res.Abstain {
		answer, err := answerer.Ask(ctx, q.Question, rag.Scope{UserID: viewer})
		if err != nil {
			res.Error = fmt.Sprintf("answering: %v", err)
			return res
		}
		res.Answer = answer.Text
		// An answer grounded in the library cites the library. One that cites
		// nothing has either refused, which is right, or invented something,
		// which the citation rule in answer.v1.md is there to prevent.
		res.Refused = answer.Text == rag.NothingFound || len(answer.Cited) == 0
		return res
	}

	res.WorkRank = firstMatch(hits, func(h domain.RetrievedChunk) bool {
		return matchesWork(h, q.ExpectedWorks) || matchesWork(h, q.AlsoAcceptable)
	})
	res.PassageRank = firstMatch(hits, func(h domain.RetrievedChunk) bool {
		return matchesLocator(h, q.ExpectedLocators)
	})

	// A question that needs several books is measured on how many of them came
	// back, because the rank of the first one says nothing about the rest.
	if q.ExpectAll {
		res.Wanted = len(q.ExpectedWorks)
		for _, want := range q.ExpectedWorks {
			one := []string{want}
			if firstMatch(hits, func(h domain.RetrievedChunk) bool {
				return matchesWork(h, one)
			}) > 0 {
				res.Covered++
			}
		}
	}
	return res
}

// scoreBookList scores a question whose answer is a list of books.
//
// The rank is a position in that list rather than in a passage ranking, and it
// is the same question being asked of it: was the right book found, and how far
// down. A passage rank is meaningless here and stays zero.
func scoreBookList(ctx context.Context, answerer *rag.Answerer, q Question, res Result, viewer uuid.UUID) Result {
	answer, err := answerer.Ask(ctx, q.Question, rag.Scope{UserID: viewer})
	if err != nil {
		res.Error = fmt.Sprintf("asking: %v", err)
		return res
	}

	matches := func(want []string, item postgres.LibraryItem) bool {
		for _, w := range want {
			if id, err := uuid.Parse(w); err == nil {
				if item.WorkID == id {
					return true
				}
				continue
			}
			// Leading-substring match, because several titles in a real library
			// are title-page dumps with the real title at the front.
			title := strings.ToLower(strings.TrimSpace(item.Title))
			if strings.HasPrefix(title, strings.ToLower(strings.TrimSpace(w))) {
				return true
			}
		}
		return false
	}

	for i, item := range answer.Books {
		if matches(q.ExpectedWorks, item) || matches(q.AlsoAcceptable, item) {
			res.WorkRank = i + 1
			break
		}
	}

	if q.ExpectAll {
		res.Wanted = len(q.ExpectedWorks)
		for _, want := range q.ExpectedWorks {
			one := []string{want}
			for _, item := range answer.Books {
				if matches(one, item) {
					res.Covered++
					break
				}
			}
		}
	}
	return res
}

func firstMatch(hits []domain.RetrievedChunk, ok func(domain.RetrievedChunk) bool) int {
	for i, h := range hits {
		if ok(h) {
			return i + 1
		}
	}
	return 0
}

// matchesWork accepts either a work id or a title, so the set survives a
// re-ingestion that reassigns every id.
func matchesWork(h domain.RetrievedChunk, want []string) bool {
	for _, w := range want {
		if id, err := uuid.Parse(w); err == nil {
			if h.WorkID == id {
				return true
			}
			continue
		}
		if strings.EqualFold(strings.TrimSpace(w), strings.TrimSpace(h.WorkTitle)) {
			return true
		}
	}
	return false
}

// matchesLocator accepts a passage as correct when the expected locator falls
// anywhere inside the retrieved chunk, rather than requiring the chunk to start
// exactly there. Chunk boundaries move whenever the strategy changes; the
// passage the person needs does not.
func matchesLocator(h domain.RetrievedChunk, want []string) bool {
	for _, raw := range want {
		loc, err := domain.ParseLocator(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		if h.Chunk.Range.Contains(loc) {
			return true
		}
	}
	return false
}

func tally(s *Summary) {
	var workHits, passageHits, abstained, intentRight, intentScored int
	var covered, wanted, passageAsked int
	var workRR, passageRR float64
	var durations []int

	for _, r := range s.Results {
		if r.WantIntent != "" {
			intentScored++
			if r.IntentRight {
				intentRight++
			}
		}
		if r.RetrievalMS > 0 {
			durations = append(durations, r.RetrievalMS)
		}

		if r.Abstain {
			s.Abstains++
			if r.Refused {
				abstained++
			}
			continue
		}

		s.Scored++
		if r.WorkRank > 0 {
			workHits++
			workRR += 1 / float64(r.WorkRank)
		}
		// Passage scores are counted only over questions that name a passage.
		//
		// A find-book question has no locator to expect and never will: its
		// answer is a list of books. Counting it as a passage MISS punished
		// retrieval for a question it answered correctly, and moved passage
		// Recall from 0.625 to 0.541 the moment five such questions were added
		// - a fall that was entirely the denominator.
		if r.PassageWanted {
			passageAsked++
			if r.PassageRank > 0 {
				passageHits++
				passageRR += 1 / float64(r.PassageRank)
			}
		}
		if r.Wanted > 0 {
			covered += r.Covered
			wanted += r.Wanted
			s.CoverageAsked++
		}
	}

	if s.Scored > 0 {
		s.WorkRecall = float64(workHits) / float64(s.Scored)
		s.WorkMRR = workRR / float64(s.Scored)
	}
	if passageAsked > 0 {
		s.PassageRecall = float64(passageHits) / float64(passageAsked)
		s.PassageMRR = passageRR / float64(passageAsked)
		s.PassageAsked = passageAsked
	}
	if wanted > 0 {
		s.Coverage = float64(covered) / float64(wanted)
	}
	if s.Abstains > 0 {
		s.AbstainRate = float64(abstained) / float64(s.Abstains)
	}
	if intentScored > 0 {
		s.IntentAcc = float64(intentRight) / float64(intentScored)
	}
	if len(durations) > 0 {
		sort.Ints(durations)
		s.MedianMS = durations[len(durations)/2]
	}
}

func report(s Summary, verbose bool) {
	if verbose {
		fmt.Printf("%-22s %-9s %-7s %-7s %6s\n", "id", "intent", "work", "passage", "ms")
		for _, r := range s.Results {
			work, passage := rankOf(r.WorkRank), rankOf(r.PassageRank)
			if r.Abstain {
				work, passage = "refused", "-"
				if !r.Refused {
					work = "ANSWERED"
				}
			}
			intent := r.Intent
			if r.WantIntent != "" && !r.IntentRight {
				intent = r.Intent + "✗"
			}
			fmt.Printf("%-22s %-9s %-7s %-7s %6d %s\n",
				truncate(r.ID, 22), truncate(intent, 9), work, passage, r.RetrievalMS, r.Error)
		}
		fmt.Println()
	}

	if s.Reranked {
		fmt.Println("reranked by a cross-encoder")
	}
	fmt.Printf("%d questions scored, %d abstentions, k=%d, %s\n",
		s.Scored, s.Abstains, s.K, s.Model)
	fmt.Printf("  work    Recall@%-2d %.3f   MRR %.3f\n", s.K, s.WorkRecall, s.WorkMRR)
	fmt.Printf("  passage Recall@%-2d %.3f   MRR %.3f  (%d questions naming a passage)\n",
		s.K, s.PassageRecall, s.PassageMRR, s.PassageAsked)
	if s.CoverageAsked > 0 {
		// Separate from Recall on purpose: recall is the rank of the FIRST
		// expected book, which a question needing two of them can satisfy
		// while answering half the question.
		fmt.Printf("  multi-book coverage %.3f  (%d questions needing more than one)\n",
			s.Coverage, s.CoverageAsked)
	}
	if s.Abstains > 0 {
		fmt.Printf("  refused correctly   %.3f  (%d out-of-library questions)\n",
			s.AbstainRate, s.Abstains)
	}
	if s.IntentAcc > 0 {
		fmt.Printf("  intent accuracy     %.3f\n", s.IntentAcc)
	}
	fmt.Printf("  median retrieval    %d ms\n", s.MedianMS)
}

func delta(before, after Summary) {
	name := before.Label
	if name == "" {
		name = before.RunAt.Format(time.RFC3339)
	}
	fmt.Printf("against %s:\n", name)
	line := func(label string, was, now float64) {
		d := now - was
		mark := "="
		switch {
		case d > 0.0005:
			mark = "+"
		case d < -0.0005:
			mark = "-"
		}
		fmt.Printf("  %-20s %.3f -> %.3f  %s%.3f\n", label, was, now, mark, math.Abs(d))
	}
	line("work Recall", before.WorkRecall, after.WorkRecall)
	line("work MRR", before.WorkMRR, after.WorkMRR)
	line("passage Recall", before.PassageRecall, after.PassageRecall)
	line("passage MRR", before.PassageMRR, after.PassageMRR)
	line("abstained correctly", before.AbstainRate, after.AbstainRate)
	fmt.Printf("  %-20s %d -> %d ms\n", "median retrieval", before.MedianMS, after.MedianMS)
}

func rankOf(n int) string {
	if n == 0 {
		return "miss"
	}
	return fmt.Sprintf("#%d", n)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func load(path string) ([]Question, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var out []Question
	for i, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		var q Question
		if err := json.Unmarshal([]byte(line), &q); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		// The file documents itself on its first line; that is not a question.
		if q.Question == "" {
			continue
		}
		out = append(out, q)
	}
	return out, nil
}

func loadSummary(path string) (Summary, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Summary{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var s Summary
	if err := json.Unmarshal(body, &s); err != nil {
		return Summary{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return s, nil
}

// judgePrompt asks whether a passage actually answers a question.
//
// This lives here rather than in internal/llm/prompts because nothing it
// produces is stored against a work or shown to a reader: it labels a developer
// eval set. Prompts in that package are versioned because enrichment records
// which version produced a value, and none of that applies here.
const judgePrompt = `A reader asked this question of their library:

<<<QUESTION
%s
QUESTION

Below is a passage that the search returned, from the book %q.

Does this passage actually ANSWER the question - does a person reading it come
away knowing the answer? Being about the right topic is not enough: a table of
contents, an index entry, a passing mention, or a promise to explain something
later all fail this test.

Answer with one word, YES or NO, and nothing else.

Passage:
<<<PASSAGE
%s
PASSAGE`

// relabelSet records, for each question, the other books whose passages also
// answer it.
//
// The eval set names ONE correct book per question, because each question was
// written from a passage in that book. A personal library covers the same
// ground many times over, so retrieval that returns a different and equally
// good book is marked wrong - measured on the real set, three of the four
// work-level misses were exactly that. Recall and MRR are lower bounds until
// this is run.
//
// The judge is the same model that answers questions, which is a real
// limitation and the reason these labels are kept in a separate field. It is a
// far easier task than answering - it reads one passage and one question and
// says whether one addresses the other - and it is judging passages the
// retriever chose rather than its own output. It is not a substitute for
// reading them, and `alsoAcceptable` should be treated as evidence rather than
// truth.
func relabelSet(ctx context.Context, answerer *rag.Answerer, chat *llm.Client,
	questions []Question, path string, k int, viewer uuid.UUID) error {

	if chat == nil || os.Getenv("LEXARII_LLM_BASE_URL") == "" {
		return errors.New("labelling needs a chat model; set LEXARII_LLM_BASE_URL")
	}

	added, judged, failed := 0, 0, 0
	for i := range questions {
		q := &questions[i]
		// Controls expect nothing on purpose and must not acquire labels.
		if len(q.ExpectedWorks) == 0 && len(q.ExpectedLocators) == 0 {
			continue
		}

		_, hits, err := answerer.Retrieve(ctx, q.Question, rag.Scope{UserID: viewer})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", q.ID, err)
			continue
		}
		if len(hits) > k {
			hits = hits[:k]
		}

		// One judgement per BOOK, not per passage: the question is which books
		// answer this, and judging eight passages from one book eight times
		// costs eight calls to learn one thing.
		seen := map[string]bool{}
		for _, w := range q.ExpectedWorks {
			seen[strings.ToLower(strings.TrimSpace(w))] = true
		}
		for _, w := range q.AlsoAcceptable {
			seen[strings.ToLower(strings.TrimSpace(w))] = true
		}

		for _, h := range hits {
			title := strings.TrimSpace(h.WorkTitle)
			if title == "" || seen[strings.ToLower(title)] {
				continue
			}
			seen[strings.ToLower(title)] = true

			reply, err := chat.Complete(ctx, llm.Request{
				Messages: []llm.Message{llm.User(fmt.Sprintf(judgePrompt,
					q.Question, title, clip(h.Chunk.Content, 1500)))},
				ReasoningEffort: llm.ReasoningOff,
				MaxTokens:       10,
			})
			if err != nil {
				// Counted, not swallowed. "added 0" from a run where the judge
				// answered NO every time and "added 0" from a run that never
				// reached the model are the same sentence, and the difference
				// is the difference between a saturated label set and a broken
				// one.
				failed++
				if failed <= 3 {
					fmt.Fprintf(os.Stderr, "  %s: judging %q: %v\n", q.ID, title, err)
				}
				continue
			}
			judged++
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(reply.Content)), "YES") {
				q.AlsoAcceptable = append(q.AlsoAcceptable, title)
				added++
				fmt.Printf("  %s  + %s\n", q.ID, title)
			}
		}
	}

	if err := writeQuestions(path, questions); err != nil {
		return err
	}
	fmt.Printf("\nadded %d acceptable books from %d judgements across %d questions; rewrote %s\n",
		added, judged, len(questions), path)
	if failed > 0 {
		fmt.Printf("%d judgements failed and were not counted either way\n", failed)
	}
	if judged == 0 {
		return errors.New("the judge was never reached: these labels say nothing")
	}
	return nil
}

// writeQuestions rewrites the eval set, preserving the header comment.
func writeQuestions(path string, questions []Question) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	header := ""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, `"_comment"`) {
			header = line
			break
		}
	}

	var b strings.Builder
	if header != "" {
		b.WriteString(header)
		b.WriteString("\n")
	}
	for _, q := range questions {
		line, err := json.Marshal(q)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteString("\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// clip shortens a passage at a rune and word boundary.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	if idx := strings.LastIndex(s[:cut], " "); idx > n/2 {
		cut = idx
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

// libraryOwner picks the account whose library the eval measures.
//
// The one that holds the most books, which on a single-account installation is
// the only one and on a shared one is the person whose shelf the questions were
// written from. A retrieval eval against an account that owns nothing measures
// nothing, and would report it as a score of zero rather than as a mistake -
// which is precisely what happened the first time this ran after books became
// per-person.
func libraryOwner(ctx context.Context, pool *postgres.Pool) (uuid.UUID, error) {
	var id uuid.UUID
	err := pool.QueryRow(ctx, `
		SELECT user_id FROM book_file_holders
		GROUP BY user_id
		ORDER BY count(*) DESC, user_id
		LIMIT 1`).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("no account owns any books: %w", err)
	}
	return id, nil
}

// listsBooks reports an intent whose answer is a shelf rather than a passage
// or a sentence.
func listsBooks(intent domain.QueryIntent) bool {
	return intent == domain.IntentBroadTopic || intent == domain.IntentFindBook
}
