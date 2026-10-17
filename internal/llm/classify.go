// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"scm.poniatowski.dev/adam/lexarii/internal/llm/prompts"
)

// Classification is what the model says a book is.
//
// Every field may be empty, and empty is a legitimate answer: the prompt tells
// the model that a blank is better than a guess, because anything here will be
// stored as though someone had looked.
type Classification struct {
	Subjects    []string `json:"subjects"`
	Audience    string   `json:"audience"`
	Summary     string   `json:"summary"`
	Series      string   `json:"series"`
	SeriesIndex *float64 `json:"seriesIndex"`
	Title       string   `json:"title"`
	// Subtitle and Edition exist so the title does not have to hold them.
	//
	// Measured on a real library: of 105 title suggestions waiting for review,
	// 30% carried an edition statement and 21% a subtitle, even though the
	// prompt forbade both. The model was not ignoring the rule so much as
	// refusing to lose what the page said. Given fields of their own, the
	// title is free to be the title.
	Subtitle   string   `json:"subtitle"`
	Edition    string   `json:"edition"`
	Authors    []string `json:"authors"`
	Confidence float64  `json:"confidence"`

	// PrintedTitleLine and Reasoning are the model's working space, read and
	// discarded. They are here so the decoder has somewhere to put them -
	// writing them is the point, keeping them is not.
	PrintedTitleLine string `json:"printedTitleLine"`
	Reasoning        string `json:"reasoning"`

	// PromptID records which prompt produced this, e.g. "classify/v1". Stored
	// with every enrichment record, so a library enriched before a prompt change
	// can be told from one enriched after.
	PromptID string `json:"-"`
	// Model is the model id that answered.
	Model string `json:"-"`
}

// Empty reports whether the model found nothing worth recording.
func (c Classification) Empty() bool {
	return len(c.Subjects) == 0 && c.Summary == "" && c.Series == "" &&
		c.Title == "" && len(c.Authors) == 0
}

// classifySchema constrains the reply.
//
// Sent to the server AND enforced in Go afterwards. llama.cpp honours it, some
// servers ignore it silently, and neither can be relied on to bound the size of
// what comes back.
var classifySchema = &Schema{
	Name:   "book_classification",
	Strict: true,
	Definition: map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		// Everything is required, and that is about ORDER rather than about
		// wanting every field filled in - an empty string is still a fine
		// answer for most of these.
		//
		// llama.cpp turns this schema into a grammar that emits required
		// properties in the order they appear, and encoding/json sorts map
		// keys, so the alphabet decides what the model writes first. It works
		// out: printedTitleLine, reasoning, subtitle and edition all sort
		// before title. An optional property can be skipped or reordered, and
		// when they were optional the model wrote the title first and then had
		// nowhere to put the thinking it had not done yet - so it put it in
		// the title, which is the bug this was meant to fix.
		"required": []string{
			"audience", "authors", "confidence", "edition", "printedTitleLine",
			"reasoning", "series", "seriesIndex", "subjects", "subtitle",
			"summary", "title",
		},
		"properties": map[string]any{
			"subjects": map[string]any{
				"type":     "array",
				"maxItems": maxSubjects,
				"items":    map[string]any{"type": "string", "maxLength": maxSubjectLength},
			},
			"audience": map[string]any{
				"type": "string",
				"enum": []string{"school", "undergraduate", "professional", "research", "general", ""},
			},
			"summary":     map[string]any{"type": "string", "maxLength": maxSummaryLength},
			"series":      map[string]any{"type": "string", "maxLength": maxSeriesLength},
			"seriesIndex": map[string]any{"type": []string{"number", "null"}},
			"title":       map[string]any{"type": "string", "maxLength": maxTitleLength},
			"subtitle":    map[string]any{"type": "string", "maxLength": maxTitleLength},
			"edition":     map[string]any{"type": "string", "maxLength": maxEditionLength},
			// Bounded like every other string, and for the same reason: a
			// model that starts repeating must hit a wall in the grammar
			// rather than in the token budget. These two are discarded, but an
			// unbounded field is still a field a loop can live in.
			"printedTitleLine": map[string]any{"type": "string", "maxLength": maxTitleLength},
			"reasoning":        map[string]any{"type": "string", "maxLength": maxReasoningLength},
			"authors": map[string]any{
				"type":     "array",
				"maxItems": maxAuthors,
				"items":    map[string]any{"type": "string", "maxLength": maxAuthorLength},
			},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		},
	},
}

// Bounds on what is accepted back. A model that returns four hundred subjects
// has misunderstood the question, and storing them would make the tag list
// useless for every book.
const (
	maxSubjects      = 6
	maxAuthors       = 8
	maxSubjectLength = 60
	maxAuthorLength  = 120
	maxSummaryLength = 1200
	maxSeriesLength  = 120
	maxTitleLength   = 300
	maxEditionLength = 60
	// The reasoning is one sentence and is thrown away; the bound is here to
	// stop a loop, not to leave room for an argument.
	maxReasoningLength = 300
)

// maxClassifyTokens bounds the reply.
//
// The answer itself is a few hundred tokens - the schema is small. The budget
// is far larger than that because a REASONING model spends most of it thinking
// before it writes anything at all, and a budget that runs out mid-thought
// produces an empty answer rather than a short one.
//
// Classify asks for no reasoning, so the answer alone needs only a few hundred
// tokens. The ceiling stays high because a server that does not understand that
// request ignores it silently, and gemma4-12b thinking freely will spend every
// token it is given - 4,000 of them, returning no answer - on a single book.
// A ceiling costs nothing when it is never reached.
const maxClassifyTokens = 4000

// Classify asks the model to describe one book.
//
// The sample must already be bounded by the caller: what to send is a decision
// about the user's context window and their patience, not one this package can
// make.
func Classify(ctx context.Context, client *Client, in prompts.ClassifyInput) (Classification, error) {
	return ClassifyAt(ctx, client, prompts.ClassifyVersion, in)
}

// ClassifyAt is Classify at a named prompt version.
//
// Exported for measurement: a prompt change is a claim about what a model will
// do, and the only way to check one is to run the old text and the new against
// the same books. scripts/classifyeval does exactly that.
func ClassifyAt(ctx context.Context, client *Client, version string, in prompts.ClassifyInput) (Classification, error) {
	if client == nil {
		return Classification{}, ErrNotConfigured
	}
	if strings.TrimSpace(in.Sample) == "" {
		// Nothing to read. Asking anyway invites the model to fall back on what
		// it thinks it remembers about the title, which is exactly what the
		// prompt forbids.
		return Classification{}, fmt.Errorf("classify: no text sample")
	}

	tmpl, err := prompts.ClassifyAt(version, in)
	if err != nil {
		return Classification{}, err
	}

	var out Classification
	err = client.JSON(ctx, Request{
		Messages: []Message{System(tmpl.System), User(tmpl.User)},
		Schema:   classifySchema,
		// Cataloguing is extraction, not deliberation: the answer is on the
		// page and the prompt already says what to take from it.
		ReasoningEffort: ReasoningOff,
		// Enrichment yields to anyone asking a question.
		Priority:  Background,
		MaxTokens: maxClassifyTokens,
	}, &out)
	if err != nil {
		return Classification{}, fmt.Errorf("classify: %w", err)
	}

	out.PromptID = tmpl.ID()
	out.Model = client.Model()
	return out.normalised(), nil
}

// normalised bounds and tidies what came back.
//
// Every value here is model output going into a database that a person will
// later read as a statement of fact, so it is cleaned on the way in rather than
// on every read.
func (c Classification) normalised() Classification {
	out := c

	out.Subjects = cleanList(c.Subjects, maxSubjects, maxSubjectLength, func(s string) string {
		return tidy(strings.ToLower(s))
	})
	out.Authors = cleanList(c.Authors, maxAuthors, maxAuthorLength, tidy)
	out.Summary = tidy(clean(c.Summary, maxSummaryLength))
	out.Series = tidy(clean(c.Series, maxSeriesLength))
	out.Title = stripByline(tidy(clean(c.Title, maxTitleLength)))
	out.Subtitle = stripByline(tidy(clean(c.Subtitle, maxTitleLength)))
	out.Edition = tidy(clean(c.Edition, maxEditionLength))

	// Working space, never stored. Cleared here rather than ignored at every
	// call site, so a future caller cannot accidentally persist the model
	// talking to itself.
	out.PrintedTitleLine = ""
	out.Reasoning = ""
	out.Audience = strings.ToLower(clean(c.Audience, 40))

	switch out.Audience {
	case "school", "undergraduate", "professional", "research", "general":
	default:
		// A value outside the list is the model inventing a category. Dropped
		// rather than stored, because the point of a fixed vocabulary is that a
		// filter over it means something.
		out.Audience = ""
	}

	// A series index without a series is meaningless, and a negative one is
	// noise.
	if out.Series == "" || (out.SeriesIndex != nil && *out.SeriesIndex < 0) {
		out.SeriesIndex = nil
	}

	if out.Confidence < 0 {
		out.Confidence = 0
	}
	if out.Confidence > 1 {
		out.Confidence = 1
	}
	return out
}

func cleanList(values []string, maxItems, maxLen int, transform func(string) string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(values))
	out := make([]string, 0, min(len(values), maxItems))

	for _, v := range values {
		v = clean(v, maxLen)
		if transform != nil {
			v = transform(v)
		}
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
		if len(out) == maxItems {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// clean collapses whitespace, strips control characters, and truncates.
//
// Control characters because extracted book text carries them and they travel
// straight through a model; whitespace because a model asked for a subject
// sometimes returns one wrapped across lines.
func clean(s string, maxLen int) string {
	var b strings.Builder
	b.Grow(len(s))

	space := true // leading space is dropped
	for _, r := range s {
		switch {
		case r == '\uFEFF' || (unicode.IsControl(r) && r != '\n' && r != '\t'):
			continue
		case unicode.IsSpace(r):
			if !space {
				b.WriteByte(' ')
				space = true
			}
		default:
			b.WriteRune(r)
			space = false
		}
	}

	out := strings.TrimSpace(b.String())
	if len(out) <= maxLen {
		return out
	}

	// Cut on a rune boundary, and prefer a word boundary when one is near.
	cut := maxLen
	for cut > 0 && !isBoundary(out[cut]) {
		cut--
	}
	if cut < maxLen/2 {
		cut = maxLen
		for cut > 0 && out[cut]&0xC0 == 0x80 {
			cut--
		}
	}
	return strings.TrimSpace(out[:cut])
}

func isBoundary(b byte) bool { return b == ' ' || b == ',' || b == ';' }

// bylineStart matches a trailing ", by ..." on a title.
//
// Anchored to a comma or dash followed by "by", so it only fires on the shape a
// title page actually produces. A title containing the word "by" - "Death by
// Black Hole", "Programming by Contract" - has no separator in front of it and
// is untouched.
var bylineStart = regexp.MustCompile(`(?i)\s*[,;—–-]\s+by\s+\S`)

// stripByline removes an author list the model copied into the title.
//
// The prompt tells it not to, and v2 says so explicitly, but a model reading a
// title page reads the whole line: gemma4-12b answered "Algebra Essentials, by
// Olgha B. Davis, David A. Santos, and Mercury Learning" for a book called
// Algebra Essentials. An instruction is not a guarantee, and this is the field
// a person sees first.
func stripByline(title string) string {
	loc := bylineStart.FindStringIndex(title)
	if loc == nil {
		return title
	}
	// Refuse to cut a title down to nothing: if what is left is implausibly
	// short, the match was part of the title rather than a byline after it.
	head := strings.TrimSpace(title[:loc[0]])
	if len([]rune(head)) < 3 {
		return title
	}
	return head
}

// tidy strips the punctuation models leave on the ends of extracted values.
//
// gemma4-12b returned a title of "Algebra Essentials, " for a book called
// Algebra Essentials - a trailing comma and space INSIDE the JSON string, not a
// parsing error. It is reading a line off a title page and bringing the
// punctuation that followed it. Stored as-is, that becomes the book's name.
//
// Sentence-ending punctuation is kept: a book really can be called "Who Owns
// the Future?" and trimming that would be a different kind of wrong. Only
// separators and unbalanced quotes go.
func tidy(s string) string {
	for {
		before := s
		s = strings.TrimSpace(s)
		s = strings.Trim(s, ",;:·—–-")
		s = strings.TrimSpace(s)

		// Surrounding quotes, only when both ends have them: a title may
		// legitimately contain one.
		if len(s) >= 2 {
			first, last := s[0], s[len(s)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				s = s[1 : len(s)-1]
			}
		}

		// An unbalanced trailing quote is the model running past the end of the
		// value it was copying.
		if n := strings.Count(s, `"`); n == 1 && strings.HasSuffix(s, `"`) {
			s = strings.TrimSuffix(s, `"`)
		}

		if s == before {
			return s
		}
	}
}
