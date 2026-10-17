// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/llm/prompts"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// Searcher is the retrieval the answerer needs.
type Searcher interface {
	Search(ctx context.Context, q postgres.SearchQuery) ([]domain.RetrievedChunk, error)
	WorksMatchingSubjects(ctx context.Context, subjects []string, limit int) ([]uuid.UUID, error)
	Library(ctx context.Context, q postgres.LibraryQuery) (postgres.LibraryPage, error)
	SearchNotes(ctx context.Context, userID uuid.UUID, text string, limit int) ([]postgres.FoundNote, error)
	MatchingSubjects(ctx context.Context, subjects []string) ([]postgres.SubjectMatch, error)
	LibraryFacts(ctx context.Context, userID uuid.UUID) (postgres.LibraryFacts, error)
}

// Answerer answers questions from the library.
type Answerer struct {
	Store Searcher
	// Chat generates the answer. Nil means retrieval still works and the
	// passages are returned without a written answer, which is a legitimate and
	// useful mode: it is search with citations.
	Chat *llm.Client
	// Embed turns the question into a vector. Nil falls back to keyword search
	// alone, which is worse but not broken.
	Embed *llm.Client
	// Dimensions is the width of the vector column.
	Dimensions int

	// MaxPassages is how many to retrieve. Zero uses DefaultPassages.
	MaxPassages int

	// Reranker scores each candidate against the question with a CROSS-ENCODER,
	// which is the only stage that can tell a table of contents from an
	// explanation.
	//
	// Nil unless one is configured, and configuring one is the switch: there is
	// no boolean, because reranking without a reranker is what this used to be
	// and it made the library worse. Asking the chat model to reorder passages
	// cost 0.081 of work Recall@10 and 0.167 of multi-book coverage, bought
	// 0.010 of passage MRR, and took the median question from 1.6 to 6.8
	// seconds. A decoder asked to rank is doing a job it was not trained for.
	//
	// llama.cpp serves cross-encoders with `--reranking --embedding --pooling
	// rank`. Re-run `make eval` after pointing this at one: the numbers above
	// are what it has to beat.
	Reranker *llm.Client

	// ContextTokens is the chat model's window, from
	// LEXARII_LLM_CONTEXT_TOKENS. Zero means "do not bound the prompt",
	// which is right for a hosted model with a huge window and wrong for the
	// local ones this is built for.
	//
	// It matters because the parts of the prompt were each bounded and their
	// SUM was not. Ten passages at this library's median chunk length is
	// about 3,800 tokens; the scaffold is 550; the history allowance is
	// another 750; and 4,000 are reserved for the answer itself. That is
	// 9,100 against a default window of 8,192, so the MEDIAN question
	// overflowed. What a server does with an overflowing prompt is drop the
	// front of it, silently. The front is the system prompt that says to
	// cite.
	ContextTokens int
}

// budget is how many tokens of prompt the model will take, once room has been
// left for the answer.
//
// Zero means unbounded, and so does a window too small to be worth trimming
// into: if maxAnswerTokens does not fit, the number is misconfigured and
// truncating passages would not save the request anyway.
func (a *Answerer) budget() int {
	if a.ContextTokens <= 0 {
		return 0
	}
	left := a.ContextTokens - maxAnswerTokens
	if left <= 0 {
		slog.Warn("the context window has no room left for an answer; not bounding the prompt",
			"contextTokens", a.ContextTokens, "answerTokens", maxAnswerTokens)
		return 0
	}
	return left
}

// fitPassages drops passages from the end until the prompt fits the window,
// and reports what is left over for the notes.
//
// From the END because retrieval already ranked them: the last passage is the
// one least likely to answer the question. The prompt is rendered to measure
// it rather than estimated part by part, because the scaffold and the per
// passage headers are most of the difference between a chunk and what the
// model actually receives.
//
// At least one passage always survives. An answer from one passage is a worse
// answer; an answer from none is a different and false one ("nothing in your
// library covers this").
func (a *Answerer) fitPassages(passages []domain.RetrievedChunk, history History) ([]domain.RetrievedChunk, int) {
	budget := a.budget()
	if budget == 0 || len(passages) == 0 {
		return passages, budget
	}

	rendered := history.Render()
	for n := len(passages); n > 1; n-- {
		cost, err := promptTokens(passages[:n], rendered)
		if err != nil {
			// Rendering is how the real prompt is built too, so a failure here
			// will fail generation in a moment with a better message. Leave it
			// to say so.
			return passages, budget
		}
		if cost <= budget {
			return passages[:n], budget - cost
		}
	}

	cost, err := promptTokens(passages[:1], rendered)
	if err != nil {
		return passages, budget
	}
	if cost > budget {
		slog.Warn("one passage alone does not fit the context window; the model server will truncate",
			"passageTokens", cost, "budget", budget, "contextTokens", a.ContextTokens)
	}
	return passages[:1], budget - cost
}

// promptTokens measures the prompt these passages would produce.
func promptTokens(passages []domain.RetrievedChunk, history string) (int, error) {
	tmpl, err := prompts.Answer(prompts.AnswerInput{
		// The question is part of the prompt but not of the choice - it is the
		// same length whichever passages are kept - so a placeholder is enough
		// to include the scaffold around it.
		Question: "",
		Passages: toPromptPassages(passages),
		History:  history,
	})
	if err != nil {
		return 0, err
	}
	return EstimateTokens(tmpl.System) + EstimateTokens(tmpl.User), nil
}

// fitNotes drops the reader's own marks that no longer fit.
//
// The notes go last because of what they are: the books answer the question
// and the notes say what this person already thought about it. Dropping a
// passage to keep an opinion would be the wrong trade.
func fitNotes(notes []postgres.FoundNote, budget int) []postgres.FoundNote {
	if budget <= 0 {
		return notes
	}
	spent := 0
	for i, n := range notes {
		// The same clipping the prompt will apply, so the measurement is of
		// what is sent rather than of what was stored.
		cost := EstimateTokens(clipRunes(n.Annotation.SelectedText, maxNoteQuote)) +
			EstimateTokens(clipRunes(n.Annotation.Note, maxNoteText)) +
			EstimateTokens(n.Title) + noteOverheadTokens
		if spent+cost > budget {
			return notes[:i]
		}
		spent += cost
	}
	return notes
}

// noteOverheadTokens is the numbering and labelling around one note in the
// prompt - "Note [4], from The Overstory:" and the blank line after it.
const noteOverheadTokens = 16

func (a *Answerer) passages() int {
	if a.MaxPassages > 0 {
		return a.MaxPassages
	}
	return DefaultPassages
}

// NothingFound is the answer when retrieval returns nothing.
//
// A personal library is finite and mostly about a few things. "You have nothing
// on this" is a real answer to a real question, and far better than an empty
// box that looks like a failure.
const NothingFound = "Nothing in your library covers this. " +
	"Searching found no passages, so there is nothing to answer from."

// DefaultPassages is how many passages an answer is built from.
//
// Ten at roughly 450 tokens is about 4,500 tokens of context - enough for a
// real answer, small enough to leave room in a modest context window, and few
// enough that a person can actually check the citations. Retrieving fifty would
// produce a more confident answer that nobody could verify.
const DefaultPassages = 10

// Answer is what a question produced.
type Answer struct {
	Question string
	Route    llm.Route
	// Text is the written answer. Empty when no chat model is configured, in
	// which case the passages are the whole result.
	Text     string
	Passages []domain.RetrievedChunk

	// Cited lists the passage numbers the answer actually referred to, in the
	// order they first appear. A passage that was retrieved but not cited is
	// still returned - it is why the answer says what it says - but the
	// distinction matters for showing sources honestly.
	Cited []int

	// Books is the answer to a question that wanted books rather than passages.
	Books []postgres.LibraryItem

	// Notes is what this person wrote in the margins that bears on the
	// question. Numbered after the passages and cited the same way, so a
	// reader following [9] does not have to know which kind of thing it is.
	Notes []postgres.FoundNote

	RetrievalMS  int
	GenerationMS int
	// What the answer cost the model. Recorded so that a slow answer can be
	// explained - a long prompt and a long answer are different problems - and
	// so a context window filling up is visible before it overflows.
	PromptTokens     int
	CompletionTokens int
	Model            string
	PromptID         string
}

// History is the conversation so far, oldest first.
type History []Turn

// Turn is one previous exchange.
type Turn struct {
	Role string
	Text string
}

// maxHistoryTurns bounds what is carried into a follow-up.
//
// Six turns is three exchanges: enough for "what about the second one" to mean
// something, bounded so a long thread does not push the passages out of the
// context window. The passages are what the answer must come from; the history
// only says what is being talked about.
const maxHistoryTurns = 6

// maxHistoryChars bounds it again, because one turn can be a long answer.
const maxHistoryChars = 3000

// Render turns history into the text the prompt carries.
//
// Trimmed from the END backwards: the most recent exchange is the one a
// follow-up refers to, so it is the last thing that should be dropped.
func (h History) Render() string {
	if len(h) == 0 {
		return ""
	}

	turns := h
	if len(turns) > maxHistoryTurns {
		turns = turns[len(turns)-maxHistoryTurns:]
	}

	var parts []string
	total := 0
	for i := len(turns) - 1; i >= 0; i-- {
		who := "They asked"
		if turns[i].Role == "assistant" {
			who = "You answered"
		}
		line := who + ": " + strings.TrimSpace(turns[i].Text)
		if total+len(line) > maxHistoryChars && len(parts) > 0 {
			break
		}
		parts = append([]string{line}, parts...)
		total += len(line)
	}
	return strings.Join(parts, "\n\n")
}

// Ask routes a question, retrieves for it, and answers from what it found.
func (a *Answerer) Ask(ctx context.Context, question string, scope Scope) (Answer, error) {
	return a.AskWithHistory(ctx, question, scope, nil)
}

// Retrieve runs everything up to but not including generation: routing, then
// the search that routing implies.
//
// Exported so that retrieval can be measured on its own. `make eval` scores the
// path an answer actually takes rather than a bare index lookup, which is the
// only way a change to routing, scoping or ranking shows up in the numbers.
// The route is returned alongside the passages because a question routed away
// from retrieval has no passages and that is not a failure.
func (a *Answerer) Retrieve(ctx context.Context, question string, scope Scope) (llm.Route, []domain.RetrievedChunk, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return llm.Route{}, nil, fmt.Errorf("no question")
	}

	route, err := llm.RouteQuestion(ctx, a.Chat, prompts.RouteInput{Question: question})
	if err != nil {
		route = llm.FallbackRoute(question)
	}
	if !route.Intent.NeedsRetrieval() {
		return route, nil, nil
	}

	passages, err := a.retrieve(ctx, question, route, scope)
	return route, passages, err
}

// AskWithHistory answers a follow-up in the context of what came before.
func (a *Answerer) AskWithHistory(ctx context.Context, question string, scope Scope, history History) (Answer, error) {
	return a.ask(ctx, question, scope, history, nil)
}

// AskStreaming answers and delivers the text as it is written.
//
// onPassages is called once, as soon as retrieval finishes and before the model
// has written a word. That is the point of it: the passages are what the answer
// will be built from, they are useful on their own, and they arrive seconds
// before the first token. onDelta is then called with each piece of text.
//
// The complete Answer is still returned, so the caller stores exactly what the
// person read rather than reassembling it from the pieces.
func (a *Answerer) AskStreaming(ctx context.Context, question string, scope Scope, history History,
	onPassages func([]domain.RetrievedChunk), onDelta func(string)) (Answer, error) {

	if onDelta == nil {
		return a.ask(ctx, question, scope, history, nil)
	}
	return a.ask(ctx, question, scope, history, &streamHooks{
		passages: onPassages,
		delta:    onDelta,
	})
}

// streamHooks are the callbacks a streaming caller provides.
type streamHooks struct {
	passages func([]domain.RetrievedChunk)
	delta    func(string)
}

func (a *Answerer) ask(ctx context.Context, question string, scope Scope, history History,
	hooks *streamHooks) (Answer, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return Answer{}, fmt.Errorf("no question")
	}

	out := Answer{Question: question}

	// Routing first, because it decides whether to retrieve at all.
	//
	// A follow-up is routed WITH the thread: "what about the second one" is not
	// a question on its own, and routing it alone produces a search for the
	// words "second one".
	routed := question
	if len(history) > 0 {
		routed = history.Render() + "\n\nThey asked: " + question
	}
	// Routing is a model call and costs seconds. It is counted as part of
	// finding, not left out: a person who waited four seconds should not be
	// told the answer took one.
	routeStart := time.Now()
	route, err := llm.RouteQuestion(ctx, a.Chat, prompts.RouteInput{Question: routed})
	routingMS := int(time.Since(routeStart).Milliseconds())
	if err != nil {
		// A router that is unavailable must not stop the library answering.
		// Searching everything and citing what is found is the honest fallback.
		route = llm.FallbackRoute(question)
	}
	out.Route = route
	out.RetrievalMS = routingMS

	if !route.Intent.NeedsRetrieval() {
		// These are answered from the library itself rather than from passages.
		// Retrieving would produce a confident synthesis of whatever happened
		// to match, which is the failure the routing stage exists to prevent.
		switch route.Intent {
		case domain.IntentFindBook, domain.IntentBroadTopic:
			start := time.Now()
			books, err := a.findBooks(ctx, question, route, scope)
			out.RetrievalMS += int(time.Since(start).Milliseconds())
			if err != nil {
				return out, err
			}
			out.Books = books

		case domain.IntentMetadata:
			text, lookup, phrase, err := a.answerAboutLibrary(ctx, question, scope)
			if err != nil {
				return out, err
			}
			out.Text = text
			// Gathering the facts is this intent's retrieval, even though it is
			// SQL rather than search: reporting it as zero would say the answer
			// was free, and a question that took six seconds was not.
			out.RetrievalMS += lookup
			out.GenerationMS = phrase
			out.Model = a.Chat.Model()
			out.PromptID = "metadata/" + prompts.MetadataVersion
		}
		return out, nil
	}

	start := time.Now()
	passages, err := a.retrieve(ctx, question, route, scope)
	out.RetrievalMS += int(time.Since(start).Milliseconds())
	if err != nil {
		return out, err
	}
	// Trimmed to the model's window BEFORE anybody is shown them, so that the
	// sources on the screen are the sources the answer was written from. A
	// passage the model never received is not a source.
	var noteBudget int
	if a.Chat != nil {
		passages, noteBudget = a.fitPassages(passages, history)
	}
	out.Passages = passages

	// The passages go out before generation starts, because they are useful on
	// their own and they are ready seconds earlier.
	if hooks != nil && hooks.passages != nil && len(passages) > 0 {
		hooks.passages(passages)
	}

	if len(passages) == 0 {
		// Silence is not an answer. Saying plainly that the library does not
		// cover this is both true and useful - it is the difference between "I
		// have nothing on this" and "something is broken".
		out.Text = NothingFound
		return out, nil
	}
	if a.Chat == nil {
		// Retrieval without generation: search with citations. Useful on its
		// own, and what the library does when the model is off.
		return out, nil
	}

	// The reader's own notes on the question, alongside the book's answer.
	//
	// Looked up after the passages rather than with them: they are a small
	// keyword search and they must never delay the evidence, which is already
	// on its way to the screen by now. A failure here loses the notes and
	// keeps the answer.
	out.Notes = fitNotes(a.notesFor(ctx, question, scope), noteBudget)

	var onDelta func(string)
	if hooks != nil {
		onDelta = hooks.delta
	}

	start = time.Now()
	reply, err := a.generate(ctx, question, passages, out.Notes, history, onDelta)
	out.GenerationMS = int(time.Since(start).Milliseconds())
	if err != nil {
		return out, err
	}

	out.Text = reply.Content
	out.PromptTokens = reply.PromptTokens
	out.CompletionTokens = reply.CompletionTokens
	out.Cited = CitedNumbers(reply.Content, len(passages)+len(out.Notes))
	out.Model = a.Chat.Model()
	out.PromptID = "answer/" + prompts.AnswerVersion
	return out, nil
}

// findBooks answers "what should I read about X" with books, not passages.
//
// A reading list is the honest response to a question that names a topic rather
// than asking something. Synthesising an answer from six passages about
// Kubernetes would sound authoritative and tell the person less than the titles
// of the three books they already own about it.
func (a *Answerer) findBooks(ctx context.Context, question string, route llm.Route,
	scope Scope) ([]postgres.LibraryItem, error) {
	// EVERY subject the router named, weighed together - not the first one
	// that happens to answer.
	//
	// "What is a good agile methodology?" routed perfectly: agile methodology,
	// software development, project management. The first had no tag, the
	// second had twelve books, and returning those twelve alphabetically ended
	// the search - so the answer was C#, Docker and Fluent Python, and Head
	// First Agile, which is tagged project-management, was never reached.
	//
	// A hit on a small subject is worth more than a hit on a large one, for
	// the same reason a rare word tells you more about a document than a
	// common one: twelve books are tagged software development and three are
	// tagged project management, so the second says something and the first
	// barely narrows the shelf. A book matching several subjects adds them up.
	type candidate struct {
		item  postgres.LibraryItem
		score float64
	}
	found := map[uuid.UUID]*candidate{}

	// The tags the library REALLY has for what was asked.
	//
	// Not the router's words turned straight into a slug: it asked for "agile
	// methodology" and this library files that book under "agile software
	// development", so the exact slug found nothing and the reading list about
	// agile had no agile book in it. Neither name is wrong - they are two
	// people describing the same shelf.
	matches, err := a.Store.MatchingSubjects(ctx, route.Subjects)
	if err != nil {
		return nil, err
	}

	for _, match := range matches {
		page, err := a.Store.Library(ctx, postgres.LibraryQuery{
			ViewerID: scope.UserID,
			Tag:      match.Slug,
			// Deeper than the answer needs, so the weight below reflects how
			// big the subject really is rather than where the limit fell.
			Limit: subjectPool,
		})
		if err != nil {
			return nil, err
		}
		if len(page.Items) == 0 {
			continue
		}

		// Two things decide what a subject is worth: how informative the
		// words that matched it are, and how small it is. A rare word
		// matching a short shelf is the strongest signal there is; a common
		// word matching a long one is barely a signal at all.
		weight := match.Relevance / math.Log(math.E+float64(len(page.Items)))
		for _, item := range page.Items {
			if seen, ok := found[item.WorkID]; ok {
				seen.score += weight
				continue
			}
			found[item.WorkID] = &candidate{item: item, score: weight}
		}
	}

	if len(found) > 0 {
		ranked := make([]*candidate, 0, len(found))
		for _, c := range found {
			ranked = append(ranked, c)
		}
		sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

		out := make([]postgres.LibraryItem, 0, maxBooksInAnswer)
		for _, c := range ranked {
			if len(out) == maxBooksInAnswer {
				break
			}
			out = append(out, c.item)
		}
		return out, nil
	}

	// Then by title and author, which catches a question that names a book.
	search := route.Book
	if search == "" {
		search = route.Query(question)
	}
	page, err := a.Store.Library(ctx, postgres.LibraryQuery{
		ViewerID: scope.UserID,
		Search:   search,
		Limit:    maxBooksInAnswer,
	})
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

// answerAboutLibrary answers a question about the collection from SQL.
//
// No retrieval at all: "how many books do I have" is a fact the database holds
// exactly, and searching passages for it would return prose about counting.
// The model is given the facts and asked to phrase them, under the same rule as
// everywhere else - answer from what you are given, or say you cannot.
// It returns the answer and how long the two halves took: gathering the facts,
// and phrasing them.
func (a *Answerer) answerAboutLibrary(ctx context.Context, question string, scope Scope) (text string, lookupMS, phraseMS int, err error) {
	start := time.Now()
	facts, err := a.Store.LibraryFacts(ctx, scope.UserID)
	lookupMS = int(time.Since(start).Milliseconds())
	if err != nil {
		return "", lookupMS, 0, err
	}
	if a.Chat == nil {
		// Without a model the facts are still worth showing, unphrased.
		return renderFacts(facts), lookupMS, 0, nil
	}

	tmpl, err := prompts.Metadata(prompts.MetadataInput{
		Question: question,
		Facts:    renderFacts(facts),
	})
	if err != nil {
		return "", lookupMS, 0, err
	}

	start = time.Now()
	reply, err := a.Chat.Complete(ctx, llm.Request{
		Messages:        []llm.Message{llm.System(tmpl.System), llm.User(tmpl.User)},
		ReasoningEffort: llm.ReasoningOff,
		MaxTokens:       1500,
	})
	phraseMS = int(time.Since(start).Milliseconds())
	if err != nil {
		return "", lookupMS, phraseMS, fmt.Errorf("answering about the library: %w", err)
	}
	return strings.TrimSpace(reply.Content), lookupMS, phraseMS, nil
}

// renderFacts writes the library's statistics as plain sentences.
//
// Prose rather than JSON: the model reads it better, and the same text is what
// is shown when there is no model to phrase it.
func renderFacts(f postgres.LibraryFacts) string {
	var b strings.Builder

	fmt.Fprintf(&b, "- %d books, in %d files, by %d authors.\n", f.Works, f.Files, f.Authors)
	for format, n := range f.Formats {
		fmt.Fprintf(&b, "- %d %s files.\n", n, strings.ToUpper(string(format)))
	}
	fmt.Fprintf(&b, "- %d books are searchable by passage.\n", f.Searchable)
	if f.Untitled > 0 {
		fmt.Fprintf(&b, "- %d books have no title recorded yet.\n", f.Untitled)
	}
	if f.NeedsOCR > 0 {
		fmt.Fprintf(&b, "- %d files are scans with no text layer, so they cannot be searched.\n", f.NeedsOCR)
	}
	if f.Missing > 0 {
		fmt.Fprintf(&b, "- %d files are recorded but missing from disk.\n", f.Missing)
	}

	if len(f.TopSubjects) > 0 {
		b.WriteString("- The most common subjects are: ")
		for i, s := range f.TopSubjects {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s (%d)", s.Name, s.Books)
		}
		b.WriteString(".\n")
	}
	if len(f.TopAuthors) > 0 {
		b.WriteString("- The most collected authors are: ")
		for i, a := range f.TopAuthors {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s (%d)", a.Name, a.Books)
		}
		b.WriteString(".\n")
	}
	if len(f.Recent) > 0 {
		fmt.Fprintf(&b, "- Most recently added: %s.\n", strings.Join(f.Recent, "; "))
	}
	if len(f.InProgress) > 0 {
		fmt.Fprintf(&b, "- Currently being read: %s.\n", strings.Join(f.InProgress, "; "))
	}
	return b.String()
}

// maxBooksInAnswer bounds a reading list.
//
// A list of eight is a recommendation. A list of eighty is the library, which
// the person can already see.
const maxBooksInAnswer = 8

// subjectPool is how deep each subject is read before the list is ranked.
//
// Deeper than the answer needs, because the count is what says how specific a
// subject is: a tag cut off at eight looks the same size as one with four
// hundred books under it, and the whole point of the weighting is to tell
// those apart.
const subjectPool = 100

// Scope narrows a question to part of the library.
type Scope struct {
	// BookFileID pins the search to one book, for a conversation about it.
	BookFileID *uuid.UUID
	// UserID owns the reading history a metadata question may refer to.
	UserID uuid.UUID
}

// retrieve runs the search the route asked for.
// retrieve searches for the question, in as many arms as it has.
//
// Nearly every question is one arm and this is the search it always was. A
// comparison is several, and they are kept apart on purpose: each arm searches
// and ranks with its own words, and the results interleave. Fusing them into
// one pool and ranking that was the obvious alternative and it does not work -
// whichever side matches better takes the whole pool, which is the failure this
// exists to fix.
func (a *Answerer) retrieve(ctx context.Context, question string, route llm.Route, scope Scope) ([]domain.RetrievedChunk, error) {
	arms := route.Queries(question)
	if len(arms) == 1 {
		return a.retrieveOne(ctx, arms[0], scope, a.passages())
	}

	// Each arm brings back its share, rounded up, so two arms of a five-passage
	// answer bring three each and the interleave picks five from six.
	per := (a.passages() + len(arms) - 1) / len(arms)
	found := make([][]domain.RetrievedChunk, 0, len(arms))
	for _, arm := range arms {
		chunks, err := a.retrieveOne(ctx, arm, scope, per)
		if err != nil {
			// One side failing must not lose the other. A comparison answered
			// from half its material is a worse answer; no answer at all is
			// not better.
			slog.Warn("a retrieval arm failed; answering from the others",
				"query", arm, "error", err)
			continue
		}
		found = append(found, chunks)
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("every retrieval arm failed")
	}
	return interleave(found, a.passages()), nil
}

// interleave takes one passage from each arm in turn.
//
// Round-robin rather than by score: the scores come from different queries and
// are not comparable, and the point is that both sides are present at all. A
// short arm simply stops contributing.
func interleave(arms [][]domain.RetrievedChunk, limit int) []domain.RetrievedChunk {
	out := make([]domain.RetrievedChunk, 0, limit)
	seen := make(map[uuid.UUID]bool, limit)
	for depth := 0; len(out) < limit; depth++ {
		added := false
		for _, arm := range arms {
			if depth >= len(arm) || len(out) >= limit {
				continue
			}
			chunk := arm[depth]
			// Two arms of one comparison can find the same passage; it is one
			// passage either way.
			if seen[chunk.Chunk.ID] {
				continue
			}
			seen[chunk.Chunk.ID] = true
			out = append(out, chunk)
			added = true
		}
		if !added {
			break
		}
	}
	return out
}

func (a *Answerer) retrieveOne(ctx context.Context, text string, scope Scope, limit int) ([]domain.RetrievedChunk, error) {
	query := postgres.SearchQuery{
		// The router's rewrite when it made one, the question as asked
		// otherwise. A router that returns nothing must not silently search for
		// an empty string.
		Text: text,
		// Whose library is being searched. Books belong to people now, and an
		// answer must never quote a passage from a book the asker cannot open.
		ViewerID:   scope.UserID,
		BookFileID: scope.BookFileID,
		Limit:      limit,
	}
	if query.Text == "" {
		return nil, fmt.Errorf("routing produced no query")
	}

	// The meaning arm, when there is an embedding model. Without one the search
	// is keyword-only: narrower, but it still finds exact terms, which is what
	// most technical questions are made of.
	if a.Embed != nil {
		vectors, err := a.Embed.Embed(ctx, []string{query.Text}, a.Dimensions)
		if err == nil && len(vectors) == 1 {
			query.Vector = vectors[0]
			query.ModelID = a.Embed.Model()
		}
	}

	// The router's SUBJECT guess does not touch retrieval, and this is the one
	// decision in the pipeline that was made by measurement rather than taste.
	//
	// It used to narrow the search to the books whose subjects matched. On the
	// eval set that cost 28 points of work Recall@10 - 0.875 down to 0.594 -
	// and 21 of MRR. The old guard only abandoned the narrowing when it
	// returned NOTHING, and a narrowed search that confidently returns ten
	// passages from the wrong books looks exactly like a search that worked.
	//
	// Demoting it from a filter to a boost recovered some of that, to 0.750,
	// and was still worse than ignoring subjects entirely. Per question: it
	// promoted five and demoted nine, and five of the nine fell from rank 1 to
	// outside the top ten - a boost cannot lift the right passage, so what
	// happened is that ten wrong-but-on-topic passages climbed over it. "About
	// the same subject" and "answers this question" are not the same property,
	// and in a library where 48 books are tagged mathematics the first is
	// nearly free.
	//
	// So routing keeps the job it is good at - deciding whether to retrieve at
	// all, which it does with perfect accuracy on the eval set - and has no say
	// in the ranking. Scoping by subject is worth revisiting at a library size
	// where searching everything is actually slow; at 81,230 passages it costs
	// 11ms and buys nothing.

	// Reranking reads a deeper pool than the answer needs, because it can only
	// reorder what it is handed.
	if a.Reranker != nil {
		query.Limit = RerankPool
	}

	found, err := a.Store.Search(ctx, query)
	if err != nil {
		return nil, err
	}

	if a.Reranker != nil {
		found = Rerank(ctx, a.Reranker, query.Text, found, limit)
	}
	return found, nil
}

// notesFor finds the reader's own marks that bear on the question.
//
// Never fatal. A question answered from the books with no notes attached is
// the normal case and a good answer; a question that fails because the note
// search failed would be neither.
func (a *Answerer) notesFor(ctx context.Context, question string, scope Scope) []postgres.FoundNote {
	if scope.UserID == uuid.Nil {
		return nil
	}
	notes, err := a.Store.SearchNotes(ctx, scope.UserID, question, maxNotesInAnswer)
	if err != nil {
		slog.Warn("searching your own notes failed; answering from the books alone", "error", err)
		return nil
	}
	return notes
}

// generate writes the answer from the passages.
func (a *Answerer) generate(ctx context.Context, question string, passages []domain.RetrievedChunk,
	notes []postgres.FoundNote, history History, onDelta func(string)) (llm.Reply, error) {

	tmpl, err := prompts.Answer(prompts.AnswerInput{
		Question: question,
		Passages: toPromptPassages(passages),
		Notes:    toPromptNotes(notes, len(passages)),
		History:  history.Render(),
	})
	if err != nil {
		return llm.Reply{}, err
	}

	req := llm.Request{
		Messages: []llm.Message{llm.System(tmpl.System), llm.User(tmpl.User)},
		// Measured, not assumed. This looked like the one stage where reasoning
		// might earn its cost - and against gemma4-12b with ten passages it
		// spent four minutes and its entire 4,000-token budget thinking, then
		// returned nothing at all. With reasoning off the same question took 36
		// seconds and produced a correct, cited answer that correctly refused to
		// invent a definition the passages did not contain.
		//
		// The passages are already in front of it and the prompt already says
		// what to do with them. There is nothing left to deliberate about.
		ReasoningEffort: llm.ReasoningOff,
		MaxTokens:       maxAnswerTokens,
		Priority:        llm.Interactive,
	}

	// onDelta decides which call is made. Everything before this point -
	// routing, retrieval, reranking, the prompt - is identical either way, so
	// there is one path through the pipeline and only the last step differs.
	var reply llm.Reply
	if onDelta != nil {
		reply, err = a.Chat.Stream(ctx, req, onDelta)
	} else {
		reply, err = a.Chat.Complete(ctx, req)
	}
	if err != nil {
		return llm.Reply{}, fmt.Errorf("generating an answer: %w", err)
	}
	reply.Content = strings.TrimSpace(reply.Content)
	return reply, nil
}

// maxAnswerTokens bounds an answer.
//
// Generous, because a comparison across three books is a legitimate answer and
// being cut off mid-sentence is worse than being long.
const maxAnswerTokens = 4000

// maxNotesInAnswer bounds how many of the reader's own marks go into a prompt.
//
// Three. They are context, not evidence: the books answer the question and the
// notes say what this person already thought about it. A prompt filled with
// somebody's own opinions would get an answer that agrees with them.
const maxNotesInAnswer = 3

// toPromptNotes numbers the notes AFTER the passages, so one citation sequence
// covers both and [9] means the ninth thing the answer was given.
// maxNoteQuote and maxNoteText bound what one note contributes to the prompt.
//
// A passage is bounded at chunk time; a note was not bounded anywhere. The
// annotation endpoint accepts 64KB, and the reader sends the selected text
// with the mark - so highlighting a whole chapter and writing one line about
// it put sixteen thousand tokens of quoted book in front of the question.
// On a local model with an 8k window that pushes every retrieved passage out
// of the context, and the answer is then built from almost nothing while still
// claiming to be grounded in the library.
//
// Generous for a real note and far below what breaks a prompt.
const (
	maxNoteQuote = 1200
	maxNoteText  = 800
)

func toPromptNotes(notes []postgres.FoundNote, after int) []prompts.Note {
	out := make([]prompts.Note, 0, len(notes))
	for i, n := range notes {
		out = append(out, prompts.Note{
			Number: after + i + 1,
			Source: n.Title,
			Quote:  clipRunes(n.Annotation.SelectedText, maxNoteQuote),
			Text:   clipRunes(n.Annotation.Note, maxNoteText),
		})
	}
	return out
}

// clipRunes shortens text to a rune count, on a word boundary where there is
// one near enough, and says that it was shortened.
func clipRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	cut := string(runes[:max])
	if at := strings.LastIndexByte(cut, ' '); at > max/2 {
		cut = cut[:at]
	}
	return strings.TrimSpace(cut) + "…"
}

// toPromptPassages numbers the passages and names their sources.
func toPromptPassages(passages []domain.RetrievedChunk) []prompts.Passage {
	out := make([]prompts.Passage, 0, len(passages))
	for i, p := range passages {
		out = append(out, prompts.Passage{
			Number: i + 1,
			Source: SourceLabel(p),
			Text:   p.Chunk.Content,
		})
	}
	return out
}

// SourceLabel describes where a passage came from, readably.
//
// The label is shown to the model and to the person, so it says what a person
// would say: the book, the chapter, and the page when there is one.
func SourceLabel(p domain.RetrievedChunk) string {
	var b strings.Builder
	b.WriteString(p.WorkTitle)

	if p.Chunk.ChapterTitle != "" {
		b.WriteString(", ")
		b.WriteString(p.Chunk.ChapterTitle)
	}
	if p.Chunk.Range.Start.Format == domain.FormatPDF && p.Chunk.Range.Start.Page > 0 {
		fmt.Fprintf(&b, ", page %d", p.Chunk.Range.Start.Page)
	}
	return b.String()
}
