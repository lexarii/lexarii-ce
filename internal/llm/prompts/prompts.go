// SPDX-License-Identifier: AGPL-3.0-or-later

// Package prompts holds the versioned prompt templates.
//
// Prompts are files, not string literals, for two reasons. They are edited far
// more often than the code around them, and by someone reading them as prose
// rather than as Go. And every enrichment record stores the version that
// produced it, so a library enriched last month can be told apart from one
// enriched after a prompt changed - without which "the model said so" is not a
// statement anyone can check.
//
// A version is never edited in place. Changing what a prompt asks for means a
// new version, because the old records claim to have come from the old text.
package prompts

import (
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed *.md
var files embed.FS

// Template is one rendered prompt, with the version that produced it.
type Template struct {
	Name    string
	Version string
	// System is the instruction, User is the material.
	//
	// Separated because models follow a system message more reliably than the
	// same words buried in a wall of book text, and because the material is the
	// part that varies per book.
	System string
	User   string
}

// ID is what an enrichment record stores, e.g. "classify/v1".
func (t Template) ID() string { return t.Name + "/" + t.Version }

// separator splits the system half of a template from the user half.
//
// A single file rather than two, so the halves of one prompt are read and
// edited together and cannot drift apart in version.
const separator = "\n---\n"

// render loads a template by name and version and fills it in.
func render(name, version string, data any) (Template, error) {
	path := fmt.Sprintf("%s.%s.md", name, version)

	raw, err := files.ReadFile(path)
	if err != nil {
		return Template{}, fmt.Errorf("no prompt %s: %w", path, err)
	}

	// Templates are trusted input compiled into the binary, but the DATA is a
	// book's own text, so it goes in as a value and is never parsed as template
	// source. A book containing "{{" must not be able to change the prompt.
	tmpl, err := template.New(path).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return Template{}, fmt.Errorf("parsing %s: %w", path, err)
	}

	var out strings.Builder
	if err := tmpl.Execute(&out, data); err != nil {
		return Template{}, fmt.Errorf("rendering %s: %w", path, err)
	}

	system, user, found := strings.Cut(out.String(), separator)
	if !found {
		return Template{}, fmt.Errorf("%s has no %q separator between its system and user halves", path, strings.TrimSpace(separator))
	}

	return Template{
		Name:    name,
		Version: version,
		System:  strings.TrimSpace(system),
		User:    strings.TrimSpace(user),
	}, nil
}

// ClassifyInput is what the classifier is told about a book.
//
// Deliberately small. The model gets a sample, never the whole book: a 900-page
// reference work does not fit in any context window worth paying for, and the
// first few thousand words of a book are where its title page, contents, and
// introduction live - which is where the answer actually is.
type ClassifyInput struct {
	// Title and Authors as already known, which may be empty. When they are,
	// the model is asked to read them off the page instead.
	Title    string
	Authors  []string
	Filename string
	Language string
	// Sample is bounded plain text from the start of the book.
	Sample string
}

// ClassifyVersion is the current classifier prompt.
//
// v2 added one rule: the title is the title alone. gemma4-12b answered
// "Algebra Essentials, by Olgha B. Davis, David A. Santos, and Mercury Learning"
// for a book called Algebra Essentials - reading the whole title-page line,
// byline included.
//
// v3 stops asking the rule to do the work on its own. Measured over the 105
// title suggestions v2 left waiting in a real library, the rule mostly held -
// only two were a whole title page - but half carried an edition or a subtitle
// anyway, because the model had nowhere else to put either and dropping them
// would have been unfaithful to the page. v3 gives each part of a title page
// its own field, and a scratch field to be faithful in.
//
// Measured on the twelve books v2 got worst, clean titles went 3/12 to 12/12.
// Most of that was NOT this text: 11 of the 12 came from making every property
// required in the schema, which fixes the ORDER the model writes them in - see
// classifySchema. The words here earned the last one, and they earn the two
// fields being documented rather than guessed at from their names.
//
// A version is never edited in place, because every stored record names the
// version that produced it.
const ClassifyVersion = "v3"

// RouteInput is what the router is told about a question.
type RouteInput struct {
	Question string
	// Library is a short description of what the collection is about, which
	// helps the router pick subjects that actually exist here rather than ones
	// it imagines. Optional.
	Library string
}

// RouteVersion is the current routing prompt.
//
// v2 asks a comparison to name each of its sides, so retrieval can look for
// each separately. One search for "compare testing in Go with testing in Java"
// finds whichever side it matches better and nothing from the other, which is
// why multi-book coverage sat at 0.500 through every other change.
//
// v3 narrows broad-topic to a TOPIC with no question in it. "What is the best
// agile methodology?" was routed broad-topic, which means no retrieval and no
// answer at all - a shelf of eight books, when the library holds one that
// compares Scrum, Kanban and XP and could simply have answered. Refusing to
// synthesise is right for "tell me about mathematics" and wrong for a question
// somebody could reply to in a paragraph.
const RouteVersion = "v3"

// Route builds the prompt that decides what kind of question was asked.
func Route(in RouteInput) (Template, error) {
	return render("route", RouteVersion, map[string]any{
		"Question": strings.TrimSpace(in.Question),
		"Library":  strings.TrimSpace(in.Library),
	})
}

// MetadataInput is the library's own facts, and a question about them.
type MetadataInput struct {
	Question string
	// Facts is a plain rendering of what the library knows about itself:
	// counts, formats, subjects, recent additions.
	Facts string
}

// MetadataVersion is the current library-facts prompt.
const MetadataVersion = "v1"

// Metadata builds the prompt that answers questions about the collection.
func Metadata(in MetadataInput) (Template, error) {
	return render("metadata", MetadataVersion, map[string]any{
		"Question": strings.TrimSpace(in.Question),
		"Facts":    strings.TrimSpace(in.Facts),
	})
}

// Passage is one retrieved chunk as the answer prompt sees it.
type Passage struct {
	// Number is the citation marker the answer refers to: [1], [2].
	Number int
	// Source names the book and where in it, so a citation can be read without
	// following it.
	Source string
	Text   string
}

// Note is one of the reader's own marks as the answer prompt sees it.
//
// Numbered in the same sequence as the passages, because a citation is a
// citation: the answer says [7] and the reader follows it, and whether 7 is a
// paragraph of a book or something they wrote beside one is not a distinction
// a citation marker should have to carry.
type Note struct {
	Number int
	Source string
	// Quote is the passage the note was written beside, when there is one.
	Quote string
	Text  string
}

// AnswerInput is everything the answer prompt needs.
type AnswerInput struct {
	Question string
	Passages []Passage
	// Notes is what this person wrote in the margins of these books. Kept
	// apart from the passages all the way to the prompt, because a note is
	// the reader's own view and a passage is the book's, and an answer that
	// confuses the two is worse than one that has neither.
	Notes []Note
	// History is the conversation so far, already summarised or trimmed by the
	// caller. Empty for a first question.
	History string
}

// AnswerVersion is the current answering prompt.
//
// v2 can be given the reader's own notes alongside the passages, and is told
// what they are: somebody's margin note is their view, not the book's, and an
// answer that reports one as though a book said it would be worse than not
// finding it at all.
const AnswerVersion = "v2"

// Answer builds the prompt that turns passages into a cited answer.
func Answer(in AnswerInput) (Template, error) {
	return render("answer", AnswerVersion, map[string]any{
		"Question": strings.TrimSpace(in.Question),
		"Passages": in.Passages,
		"Notes":    in.Notes,
		"History":  strings.TrimSpace(in.History),
	})
}

// Classify builds the enrichment prompt for one book.
func Classify(in ClassifyInput) (Template, error) {
	return ClassifyAt(ClassifyVersion, in)
}

// ClassifyAt builds the enrichment prompt at a named version.
//
// Exported so a prompt change can be measured against the one it replaces on
// the same books, rather than swapped in and hoped about. Every claim about
// what a version fixed should come from running both.
func ClassifyAt(version string, in ClassifyInput) (Template, error) {
	return render("classify", version, map[string]any{
		"Title":       strings.TrimSpace(in.Title),
		"Authors":     strings.Join(in.Authors, ", "),
		"Filename":    strings.TrimSpace(in.Filename),
		"Language":    strings.TrimSpace(in.Language),
		"Sample":      strings.TrimSpace(in.Sample),
		"NeedsTitle":  strings.TrimSpace(in.Title) == "",
		"NeedsAuthor": len(in.Authors) == 0,
	})
}
