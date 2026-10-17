// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"path"
	"regexp"
	"strings"
	"unicode"
)

// PlausibleTitle reports whether an embedded title is worth trusting.
//
// Lexarii's rule is that publisher metadata beats a model's guess. That rule
// assumes embedded metadata is at least *about the book*, and in real PDFs it
// frequently is not: the /Title field routinely holds the InDesign or LaTeX
// source filename, a "Microsoft Word - draft3.doc" string, or the operator's
// working filename. Measured across a 167-book technical library, four titles
// were InDesign artefacts like "Algebra_P1_Book.indb" - and because they were
// embedded, they outranked every later correction permanently.
//
// So an implausible title is REJECTED rather than recorded. Rejecting is not
// the same as guessing: the field is simply left empty, with a warning, exactly
// as if the file had declared nothing. Enrichment can then fill it, and the
// user can override it, neither of which is possible once junk is enshrined as
// a publisher fact.
//
// The test is deliberately conservative. A real title that looks odd is far
// cheaper than a junk title that cannot be displaced.
func PlausibleTitle(title string) bool {
	t := strings.TrimSpace(title)
	if t == "" {
		return false
	}

	// Producer prefixes. Word writes "Microsoft Word - chapter1.doc" into the
	// title of almost every PDF it exports.
	lower := strings.ToLower(t)
	for _, prefix := range []string{
		"microsoft word -", "microsoft powerpoint -", "microsoft excel -",
		"untitled document", "untitled-", "no title", "document1", "print job",
	} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}

	// A source or output filename: .indb (InDesign), .tex, .doc, .qxd (QuarkXPress).
	if sourceFileExt.MatchString(lower) {
		return false
	}

	// A path rather than a title.
	if strings.ContainsAny(t, `/\`) && path.Ext(lower) != "" {
		return false
	}

	// A hex blob or UUID: some producers write the document id as the title.
	if hexBlob.MatchString(t) {
		return false
	}

	// A single token with no spaces that carries filename punctuation, e.g.
	// "flight.indb", "chapter_01_final", "book-v2-draft". A genuine one-word
	// title ("Dune", "Ulysses") has no underscores and no extension, so it
	// passes.
	if !strings.Contains(t, " ") && filenameish.MatchString(t) {
		return false
	}

	// A model that has started repeating itself and cannot stop.
	if degenerateRepetition(t) {
		return false
	}

	// A whole title page rather than a title.
	//
	// Measured on a real library of 168 books, where the two populations do
	// not overlap: the longest genuine title is 159 characters - a Packt book
	// whose name really does run "Mastering Kubernetes, Third Edition, Level
	// up your container orchestration skills..." - and the shortest dumped
	// title page is 278. All seven of those are front matter with the
	// subtitle, author, publisher and imprint line run together, two of them
	// repeating the whole page twice.
	//
	// 200 sits in the gap. A first attempt at 140 rejected that Kubernetes
	// title, which is exactly the failure that matters: a real book goes on
	// the shelf as "Untitled" and its owner has to guess where it went.
	if len([]rune(t)) > maxTitleRunes {
		return false
	}

	// An edition statement on its own.
	//
	// "Second Edition" is a line on a title page, not the name of a book, and
	// it is on the shelf of the library this was measured against - filed
	// between "Learn Docker" and "The Docker Workshop", being the second
	// edition of one of them.
	if editionOnly.MatchString(lower) {
		return false
	}

	// Front matter that no title contains.
	//
	// These came out of the MODEL, not the file. A title page reads
	// "BIRMINGHAM - MUMBAI" under the publisher's name, and a model asked for
	// the title sometimes answers with the page - or with a remark ABOUT the
	// page, which is worse, because "Note: The title page text is slightly
	// garbled" was accepted as the name of a book and shown on a shelf.
	for _, phrase := range frontMatterPhrases {
		if strings.Contains(lower, phrase) {
			return false
		}
	}

	return true
}

// maxTitleRunes is where a title stops being a title. See PlausibleTitle.
const maxTitleRunes = 200

// editionOnly matches a bare edition statement, written either way people
// write them.
var editionOnly = regexp.MustCompile(
	`^(first|second|third|fourth|fifth|sixth|seventh|eighth|ninth|tenth|revised|\d+(st|nd|rd|th))\s+edition$`)

// frontMatterPhrases appear on title pages and in model commentary, and never
// in the name of a book.
var frontMatterPhrases = []string{
	// Packt's imprint line, with and without the separator the extractor ate.
	"birmingham - mumbai", "birmingham-mumbai", "birminghammumbai",
	// Copyright pages.
	"all rights reserved", "no part of this book", "copyright ©",
	// The model talking about the job instead of doing it.
	"the title page", "title page text", "primary title is",
}

// degenerateRepetition reports a string containing the same short run over and
// over.
//
// This guards against the model rather than the file. Asked to catalogue a book
// under a JSON schema, gemma4-12b sometimes falls into a repetition loop in the
// title field - "Algebra Essentialss.s.s.s.s." or a phrase from the title page
// repeated until the reply is cut off - and a grammar that accepts any string
// has no opinion about it. Twenty-three books of 168 in a real library produced
// no catalogue entry at all this way.
//
// The run is looked for ANYWHERE, not just at the end, because a loop does not
// always run to the last character: one observed title resumed and finished
// "_10101" after repeating for two hundred. A repeated unit must cover at least
// 24 characters before it counts, so deliberate repetition in a real title -
// "Bye Bye Birdie", "New York, New York" - is untouched.
func degenerateRepetition(s string) bool {
	r := []rune(s)
	const minSpan = 24

	// 80 runes is longer than any phrase a loop has been seen to repeat, and
	// short enough that a title built of genuinely similar clauses is safe.
	for period := 2; period <= 80 && period < len(r); period++ {
		run := 0
		for i := 0; i+period < len(r); i++ {
			if r[i] != r[i+period] {
				run = 0
				continue
			}
			run++
			// run+period is how much of the string this repetition covers: the
			// matched span plus the first copy of the unit itself.
			if run+period >= minSpan && run+period >= period*3 {
				return true
			}
		}
	}
	return false
}

var (
	// Source and intermediate formats a finished book is never named after.
	sourceFileExt = regexp.MustCompile(`\.(indb|indd|idml|tex|dvi|doc|docx|odt|rtf|qxd|qxp|pmd|ps|eps|pdf|epub|xml|htm|html)$`)

	// 16 or more hex characters, or a UUID.
	hexBlob = regexp.MustCompile(`^(?i)([0-9a-f]{16,}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

	// One token containing filename punctuation.
	filenameish = regexp.MustCompile(`^[\w.\-]*[_.][\w.\-]*$`)
)

// PlausibleAuthor reports whether an embedded author is worth trusting.
//
// PDF /Author is even less reliable than /Title: it commonly holds the machine
// account or software that produced the file rather than the person who wrote
// the book.
func PlausibleAuthor(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return false
	}

	lower := strings.ToLower(n)
	for _, junk := range []string{
		"administrator", "admin", "user", "unknown", "owner", "guest",
		"acrobat", "pdfcreator", "microsoft word", "word", "ghostscript",
		"latex", "pdftex", "pdflatex", "tex", "writer", "default",
		// Machine accounts, and the names PCs are sold with. "Sys" was the
		// credited author of a book in a real library.
		"sys", "system", "root", "pc", "toshiba", "hp", "dell", "acer",
		"asus", "lenovo", "compaq", "samsung", "sony", "packard bell",
	} {
		if lower == junk {
			return false
		}
	}

	// A single all-lowercase token: a login name, not a person.
	//
	// "radha" was the credited author of five unrelated books in a real
	// library - aerodynamics, linear algebra, mechanical engineering, physics,
	// programming - and made "who is my most collected author?" answer with a
	// stranger's Unix account.
	//
	// Both halves are needed. A single token alone is a mononym, and real ones
	// exist: Homer, Colette, Voltaire. Lowercase alone is a plausible styling
	// choice for a real name, as bell hooks wrote hers. Together, in publisher
	// metadata that is otherwise capitalised, it is somebody's account.
	//
	// The rule the TODO proposed was different - a single token spread across
	// UNRELATED subjects - and measuring it killed it: the subject overlap for
	// "radha" is 0.40, exactly the same as for Arnold Robbins, and better than
	// Dawn Griffiths at 0.25. Authors of introductory books write across
	// unrelated topics, so the signal is not there. Being one lowercase word
	// is.
	if !strings.ContainsAny(n, " \t") && isAllLowercase(n) {
		return false
	}

	// A Windows account path, e.g. "DOMAIN\jsmith", or a filesystem path.
	//
	// Backslash only. A FORWARD slash is a legitimate multi-author separator -
	// "Abi Aghayere/ Jason Vigil" is two authors, not a path - and rejecting it
	// threw away the real authors of three books in a 167-book library before
	// this was narrowed.
	if strings.Contains(n, `\`) || strings.HasPrefix(n, "/") || strings.Contains(n, "://") {
		return false
	}
	if hexBlob.MatchString(n) {
		return false
	}
	return true
}

// isAllLowercase reports a string that has lower-case letters and no capitals.
//
// Not `s == strings.ToLower(s)`, which is the obvious version and is wrong for
// most of the world: Chinese, Japanese, Korean, Arabic, Hebrew and Thai have no
// letter case at all, so every name written in them equals its own lowercase
// and would be rejected as a login. A test with "李伟" in it is the only reason
// this is not still that one line.
func isAllLowercase(s string) bool {
	var hasLower bool
	for _, r := range s {
		if unicode.IsUpper(r) || unicode.IsTitle(r) {
			return false
		}
		if unicode.IsLower(r) {
			hasLower = true
		}
	}
	return hasLower
}

// SplitContributors splits a single embedded author string into individual
// names.
//
// Publisher metadata routinely credits a whole writing team in one <dc:creator>
// element, and every one of those becomes a single fictitious author with a
// name no search will ever match. Measured across a real 169-book library: 27
// author rows joined names with " and ", 8 with " & ", and 17 with a comma -
// while the separators this function used to handle, "/" and ";", appeared
// exactly zero times. "Kevlin Henney and Trisha Gee" was one author with two
// books; neither Henney nor Gee existed.
//
// The comma is the hard case, because it means two opposite things:
//
//	Costa, Peter                  one person, sort-name form
//	Joseph Albahari, Eric Johannsen    two people
//
// They are told apart by what precedes the first comma. A sort name begins with
// a surname - one token, or a particle and a surname ("de Graaf, Robert").
// Anything longer is a full name, so the comma is separating people. When a
// conjunction is present the question does not arise: "A, B, and C" is a list
// by construction, so its commas are separators too.
//
// A string with no separator is returned unchanged rather than guessed at.
func SplitContributors(raw string) []string {
	parts := splitOnConjunctions(raw)

	// A conjunction proves the string is a list, which settles the commas
	// inside it. Without one, a lone comma may be sort-name form.
	if len(parts) > 1 {
		var expanded []string
		for _, part := range parts {
			expanded = append(expanded, strings.Split(part, ",")...)
		}
		parts = expanded
	} else if len(parts) == 1 && !isSortName(parts[0]) {
		parts = strings.Split(parts[0], ",")
	}

	out := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(strings.Trim(strings.TrimSpace(part), ","))
		// A degree or honorific is not a person. Dropped rather than kept,
		// because "PhD" as an author is worse than one fewer credit.
		if name == "" || isHonorific(name) {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// conjunction matches the separators that join co-authors, as words rather than
// substrings: "Alexander" must not split on the "and" inside it.
var conjunction = regexp.MustCompile(`(?i)\s*(?:/|;|&|\band\b)\s*`)

func splitOnConjunctions(raw string) []string {
	parts := conjunction.Split(raw, -1)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// particles are the lowercase words that begin a compound surname, so that
// "de Graaf, Robert" is read as one person rather than two.
var particles = map[string]bool{
	"de": true, "del": true, "della": true, "der": true, "di": true, "da": true,
	"dos": true, "du": true, "la": true, "le": true, "van": true, "von": true,
	"ter": true, "ten": true, "den": true, "bin": true, "ibn": true, "al": true, "st": true,
	"mac": true, "mc": true, "o'": true,
}

// isSortName reports whether a comma-bearing string is one person written
// surname-first.
func isSortName(s string) bool {
	comma := strings.Index(s, ",")
	if comma < 0 {
		return true // No comma at all: nothing to decide, leave it whole.
	}
	// More than one comma is a list: a sort name has exactly one, and a
	// trailing degree was already a list of a different kind.
	if strings.Contains(s[comma+1:], ",") {
		return false
	}

	// A surname may be preceded by any run of particles: "van der Berg".
	surname := strings.Fields(s[:comma])
	for len(surname) > 1 && particles[strings.ToLower(strings.TrimSuffix(surname[0], "."))] {
		surname = surname[1:]
	}
	return len(surname) == 1
}

// honorifics are the degrees and titles that arrive attached to a name by a
// comma and are not themselves people.
var honorifics = map[string]bool{
	"phd": true, "ph.d": true, "md": true, "m.d": true, "msc": true, "m.sc": true,
	"mba": true, "bsc": true, "b.sc": true, "ma": true, "m.a": true, "ba": true,
	"b.a": true, "jr": true, "sr": true, "ii": true, "iii": true, "iv": true,
	"esq": true, "dr": true, "prof": true, "pe": true, "p.e": true, "cissp": true,
}

func isHonorific(s string) bool {
	key := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
	key = strings.ReplaceAll(key, " ", "")
	return honorifics[key]
}
