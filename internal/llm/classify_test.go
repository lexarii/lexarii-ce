// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/llm/prompts"
)

func sample() prompts.ClassifyInput {
	return prompts.ClassifyInput{
		Title:    "Algebra Essentials",
		Filename: "algebra.epub",
		Sample:   "Chapter 1. Sets and Numbers. A set is a collection of distinct objects.",
	}
}

func TestClassifyReturnsWhatTheModelSaid(t *testing.T) {
	server := newFakeServer(t, says(`{
		"subjects": ["algebra", "mathematics"],
		"audience": "undergraduate",
		"summary": "An introduction to elementary algebra.",
		"series": "Essentials",
		"seriesIndex": 2,
		"title": "Algebra Essentials",
		"authors": ["A Mathematician"],
		"confidence": 0.8
	}`))

	got, err := Classify(context.Background(), server.client(t), sample())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if len(got.Subjects) != 2 || got.Subjects[0] != "algebra" {
		t.Errorf("subjects = %v", got.Subjects)
	}
	if got.Audience != "undergraduate" || got.Summary == "" {
		t.Errorf("classification = %+v", got)
	}
	if got.SeriesIndex == nil || *got.SeriesIndex != 2 {
		t.Errorf("series index = %v", got.SeriesIndex)
	}
	if got.Confidence != 0.8 {
		t.Errorf("confidence = %v", got.Confidence)
	}

	// Provenance: which prompt and which model produced this.
	if got.PromptID != "classify/"+prompts.ClassifyVersion {
		t.Errorf("prompt id = %q", got.PromptID)
	}
	if got.Model != "test-model" {
		t.Errorf("model = %q", got.Model)
	}
}

// The schema is sent, but a server that ignores it must not be able to put
// four hundred subjects into the library's tag list.
func TestClassifyBoundsWhatComesBack(t *testing.T) {
	var subjects []string
	for i := 0; i < 50; i++ {
		subjects = append(subjects, "subject "+strings.Repeat("x", 100))
	}
	body, _ := json.Marshal(map[string]any{
		"subjects":   subjects,
		"summary":    strings.Repeat("long ", 2000),
		"confidence": 4.5,
		"audience":   "postgraduate-ish",
		"authors":    make([]string, 40),
	})
	server := newFakeServer(t, says(string(body)))

	got, err := Classify(context.Background(), server.client(t), sample())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if len(got.Subjects) > maxSubjects {
		t.Errorf("kept %d subjects, want at most %d", len(got.Subjects), maxSubjects)
	}
	for _, s := range got.Subjects {
		if len(s) > maxSubjectLength {
			t.Errorf("subject %q is %d bytes, want at most %d", s, len(s), maxSubjectLength)
		}
	}
	if len(got.Summary) > maxSummaryLength {
		t.Errorf("summary is %d bytes, want at most %d", len(got.Summary), maxSummaryLength)
	}
	if got.Confidence != 1 {
		t.Errorf("confidence = %v, want it clamped to 1", got.Confidence)
	}
	// An audience outside the fixed vocabulary is dropped, or filtering over it
	// would mean nothing.
	if got.Audience != "" {
		t.Errorf("audience = %q, want it dropped", got.Audience)
	}
	// Empty strings are not authors.
	if len(got.Authors) != 0 {
		t.Errorf("authors = %v, want none", got.Authors)
	}
}

func TestClassifyNormalisesSubjects(t *testing.T) {
	server := newFakeServer(t, says(`{
		"subjects": ["Linear Algebra", "linear algebra", "  MATHEMATICS  ", "", "Linear\nAlgebra"],
		"summary": "x", "confidence": 0.5
	}`))

	got, err := Classify(context.Background(), server.client(t), sample())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	// Lower-cased, whitespace-collapsed, and de-duplicated: 5,000 books must not
	// produce 5,000 near-identical tags.
	want := []string{"linear algebra", "mathematics"}
	if len(got.Subjects) != len(want) {
		t.Fatalf("subjects = %v, want %v", got.Subjects, want)
	}
	for i := range want {
		if got.Subjects[i] != want[i] {
			t.Errorf("subject[%d] = %q, want %q", i, got.Subjects[i], want[i])
		}
	}
}

// Extracted book text is full of control characters and they travel straight
// through a model into whatever is stored.
func TestClassifyStripsControlCharacters(t *testing.T) {
	server := newFakeServer(t, says("{\"subjects\":[\"a\\u0000b\"],\"summary\":\"one\\u0007two\",\"confidence\":0.5}"))

	got, err := Classify(context.Background(), server.client(t), sample())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if strings.ContainsRune(got.Summary, '\a') || strings.ContainsRune(strings.Join(got.Subjects, ""), 0) {
		t.Errorf("control characters survived: %+v", got)
	}
}

// A series index with no series names nothing.
func TestClassifyDropsAnOrphanSeriesIndex(t *testing.T) {
	server := newFakeServer(t, says(`{"subjects":["x"],"summary":"y","seriesIndex":3,"confidence":0.5}`))

	got, err := Classify(context.Background(), server.client(t), sample())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.SeriesIndex != nil {
		t.Errorf("series index = %v with no series, want nil", got.SeriesIndex)
	}
}

// An empty answer is a legitimate one: the prompt asks for a blank rather than
// a guess, and storing "the model found nothing" is the honest outcome.
func TestClassifyAcceptsAnEmptyAnswer(t *testing.T) {
	server := newFakeServer(t, says(`{"subjects":[],"summary":"","confidence":0.1}`))

	got, err := Classify(context.Background(), server.client(t), sample())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if !got.Empty() {
		t.Errorf("classification = %+v, want it reported empty", got)
	}
}

// Asking about a book with no text invites the model to answer from whatever it
// thinks it remembers, which is what the prompt forbids.
func TestClassifyRefusesAnEmptySample(t *testing.T) {
	server := newFakeServer(t, says(`{"subjects":["invented"],"summary":"x","confidence":0.9}`))

	in := sample()
	in.Sample = "   \n  "
	if _, err := Classify(context.Background(), server.client(t), in); err == nil {
		t.Fatal("a book with no text was sent to the model anyway")
	}
	if server.calls.Load() != 0 {
		t.Errorf("made %d calls for a book with no text", server.calls.Load())
	}
}

func TestClassifyWithNoModelConfigured(t *testing.T) {
	if _, err := Classify(context.Background(), nil, sample()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("error = %v, want ErrNotConfigured", err)
	}
}

func TestClassifySendsTheSchema(t *testing.T) {
	server := newFakeServer(t, says(`{"subjects":["x"],"summary":"y","confidence":0.5}`))
	if _, err := Classify(context.Background(), server.client(t), sample()); err != nil {
		t.Fatalf("Classify: %v", err)
	}

	sent := server.lastRequest(t)
	if sent.ResponseFormat == nil || sent.ResponseFormat.JSONSchema.Name != "book_classification" {
		t.Fatalf("schema not sent: %+v", sent.ResponseFormat)
	}
	if sent.MaxTokens != maxClassifyTokens {
		t.Errorf("max_tokens = %d, want %d", sent.MaxTokens, maxClassifyTokens)
	}
	// System instruction and book material are separate messages: a model
	// follows a system message far more reliably than the same words buried in
	// a wall of book text.
	if len(sent.Messages) != 2 || sent.Messages[0].Role != "system" || sent.Messages[1].Role != "user" {
		t.Errorf("messages = %+v", sent.Messages)
	}
	if !strings.Contains(sent.Messages[1].Content, "Chapter 1. Sets and Numbers") {
		t.Error("the book sample did not reach the model")
	}
}

func TestClean(t *testing.T) {
	tests := []struct {
		in     string
		maxLen int
		want   string
	}{
		{"  spaced  out  ", 100, "spaced out"},
		{"line\nbreaks\there", 100, "line breaks here"},
		{"", 100, ""},
		{"     ", 100, ""},
		{"exactly ten", 11, "exactly ten"},
		// Cut at a word boundary when one is near the limit.
		{"a sentence that runs on and on", 12, "a sentence"},
		// A word longer than the limit is cut at the limit rather than vanishing.
		{strings.Repeat("x", 50), 10, strings.Repeat("x", 10)},
	}

	for _, tt := range tests {
		if got := clean(tt.in, tt.maxLen); got != tt.want {
			t.Errorf("clean(%q, %d) = %q, want %q", tt.in, tt.maxLen, got, tt.want)
		}
	}
}

// Truncation must not cut a multi-byte character in half and leave invalid
// UTF-8 in the database.
func TestCleanCutsOnRuneBoundaries(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("é", 40),
		strings.Repeat("日", 40),
		strings.Repeat("🙂", 20),
	} {
		for limit := 1; limit < 30; limit++ {
			got := clean(in, limit)
			if !isValidUTF8(got) {
				t.Fatalf("clean(%q, %d) produced invalid UTF-8: %q", in[:9], limit, got)
			}
			if len(got) > limit {
				t.Fatalf("clean(..., %d) returned %d bytes", limit, len(got))
			}
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestCleanListDeduplicatesAndCaps(t *testing.T) {
	got := cleanList([]string{"a", "A", "b", "", "c", "d", "e", "f", "g"}, 3, 10, strings.ToLower)
	if len(got) != 3 {
		t.Fatalf("got %v, want 3 items", got)
	}
	if got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("got %v, want the first three distinct values", got)
	}
	if cleanList(nil, 3, 10, nil) != nil {
		t.Error("an empty list did not stay nil")
	}
	if cleanList([]string{"", "   "}, 3, 10, nil) != nil {
		t.Error("a list of blanks did not become nil")
	}
}

// Real output from gemma4-12b: a title read off a page brings the punctuation
// that followed it. Stored as-is, that becomes the book's name.
func TestTidyStripsTrailingPunctuation(t *testing.T) {
	tests := map[string]string{
		"Algebra Essentials, ":   "Algebra Essentials",
		`Algebra Essentials",`:   "Algebra Essentials",
		`"Algebra Essentials"`:   "Algebra Essentials",
		"Algebra Essentials;":    "Algebra Essentials",
		"  Algebra Essentials  ": "Algebra Essentials",
		"Algebra Essentials -":   "Algebra Essentials",

		// Kept: these are part of the title, not punctuation after it.
		"Who Owns the Future?":    "Who Owns the Future?",
		"Eats, Shoots & Leaves":   "Eats, Shoots & Leaves",
		"Go!":                     "Go!",
		`The "Real" Story`:        `The "Real" Story`,
		"C++ Primer":              "C++ Primer",
		"Programming in Ada 2012": "Programming in Ada 2012",
		"":                        "",
	}

	for in, want := range tests {
		if got := tidy(in); got != want {
			t.Errorf("tidy(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassifyTidiesTheTitle(t *testing.T) {
	server := newFakeServer(t, says(`{
		"subjects": ["algebra, "],
		"summary": "A textbook.",
		"title": "Algebra Essentials, ",
		"series": "Essentials;",
		"confidence": 1.0
	}`))

	got, err := Classify(context.Background(), server.client(t), sample())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.Title != "Algebra Essentials" {
		t.Errorf("title = %q", got.Title)
	}
	if got.Series != "Essentials" {
		t.Errorf("series = %q", got.Series)
	}
	if len(got.Subjects) != 1 || got.Subjects[0] != "algebra" {
		t.Errorf("subjects = %v", got.Subjects)
	}
}

// Cataloguing is extraction: the answer is on the page, and thinking about it
// costs a minute a book for nothing.
func TestClassifyAsksForNoReasoning(t *testing.T) {
	server := newFakeServer(t, says(`{"subjects":["x"],"summary":"y","confidence":0.5}`))
	if _, err := Classify(context.Background(), server.client(t), sample()); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if effort := server.lastRequest(t).ReasoningEffort; effort != ReasoningOff {
		t.Errorf("reasoning_effort = %q, want %q", effort, ReasoningOff)
	}
}

// Real output from gemma4-12b: the model read a whole title-page line, byline
// and all, into the title field.
func TestStripByline(t *testing.T) {
	tests := map[string]string{
		"Algebra Essentials, by Olgha B. Davis, David A. Santos, and Mercury Learning": "Algebra Essentials",
		"Algebra Essentials, by A. Author":                                             "Algebra Essentials",
		"An Introduction to Logic - by Someone":                                        "An Introduction to Logic",
		"Clean Code; by Robert C. Martin":                                              "Clean Code",

		// Untouched: "by" is part of the title, with no separator in front of it.
		"Death by Black Hole":     "Death by Black Hole",
		"Programming by Contract": "Programming by Contract",
		"Learning by Doing":       "Learning by Doing",
		"Algebra Essentials":      "Algebra Essentials",
		"Eats, Shoots & Leaves":   "Eats, Shoots & Leaves",
		"Sorting, by Hand":        "Sorting",
		"":                        "",
	}

	for in, want := range tests {
		if got := stripByline(in); got != want {
			t.Errorf("stripByline(%q) = %q, want %q", in, got, want)
		}
	}
}

// A title must never be cut down to nothing: if what is left is implausibly
// short, the match was part of the title rather than a byline after it.
func TestStripBylineKeepsShortTitlesWhole(t *testing.T) {
	for _, in := range []string{"Go, by Example", "R, by Example"} {
		if got := stripByline(in); got != in {
			t.Errorf("stripByline(%q) = %q, want it left alone", in, got)
		}
	}
}

func TestClassifyStripsABylineFromTheTitle(t *testing.T) {
	server := newFakeServer(t, says(`{
		"subjects": ["algebra"],
		"summary": "A textbook.",
		"title": "Algebra Essentials, by Olgha B. Davis and David A. Santos",
		"confidence": 1.0
	}`))

	got, err := Classify(context.Background(), server.client(t), sample())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.Title != "Algebra Essentials" {
		t.Errorf("title = %q", got.Title)
	}
}
