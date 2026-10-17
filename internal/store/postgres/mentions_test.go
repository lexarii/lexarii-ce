// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// mentionFixture puts two books in a library: one that will be named and one
// that does the naming.
func mentionFixture(t *testing.T, p *Pool) (userID uuid.UUID, named uuid.UUID) {
	t.Helper()
	cleanLibrary(t, p)
	ctx := t.Context()

	userID, err := p.EnsureDefaultUser(ctx)
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}

	in := sampleInput(hashOf("a"), "/library/named.epub", "Learning the bash Shell", "Cameron Newham")
	in.OwnerID = userID
	book, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	return userID, book.WorkID
}

// mentioner adds a second book whose text says whatever the test wants.
func mentioner(t *testing.T, p *Pool, userID uuid.UUID, hash, title, text string) uuid.UUID {
	t.Helper()
	in := sampleInput(hashOf(hash), "/library/"+hash+".epub", title, "Some Author")
	in.OwnerID = userID
	book, err := p.UpsertBookFile(t.Context(), in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if _, err := p.SaveChunks(t.Context(), book.BookFileID, []ChunkInput{{
		Seq:        0,
		Start:      domain.Locator{Format: domain.FormatEPUB, SpineIndex: 2, ElementPath: "section[1]/p[1]"},
		End:        domain.Locator{Format: domain.FormatEPUB, SpineIndex: 2, ElementPath: "section[1]/p[1]", CharOffset: len(text)},
		Content:    text,
		TokenCount: 20,
	}}, 1); err != nil {
		t.Fatalf("SaveChunks: %v", err)
	}
	return book.WorkID
}

// The whole feature: a bibliography entry in one book becomes a backlink on
// another's page, with a locator so following it lands on the entry.
func TestABibliographyEntryBecomesAMention(t *testing.T) {
	p := testPool(t)
	userID, named := mentionFixture(t, p)

	citing := mentioner(t, p, userID, "b", "bash Cookbook",
		"For a fuller treatment see Learning the bash Shell, 3rd Edition, by Cameron Newham (O'Reilly).")

	found, err := p.Mentions(t.Context(), named, userID, 20)
	if err != nil {
		t.Fatalf("Mentions: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d mentions, want 1: %+v", len(found), found)
	}
	if found[0].WorkID != citing {
		t.Errorf("mention is work %s, want %s", found[0].WorkID, citing)
	}
	if found[0].Title != "bash Cookbook" {
		t.Errorf("title = %q", found[0].Title)
	}
	// The locator is the point: a backlink to the cover is a gesture.
	if found[0].Locator.SpineIndex != 2 || found[0].Locator.Format != domain.FormatEPUB {
		t.Errorf("locator = %+v, want the chunk's own", found[0].Locator)
	}
	if found[0].Excerpt == "" {
		t.Error("no excerpt, so nothing shows why this is a mention")
	}
}

// The title alone matches every book ABOUT the subject. Measured on a real
// library: "Linear Algebra" as a phrase appears in 23 other books, citing
// nothing. The surname is what makes a mention a mention.
func TestTheTitleWithoutTheAuthorIsNotAMention(t *testing.T) {
	p := testPool(t)
	userID, named := mentionFixture(t, p)

	mentioner(t, p, userID, "c", "Some Other Book",
		"This chapter is about learning the bash shell and writing shell scripts.")

	found, err := p.Mentions(t.Context(), named, userID, 20)
	if err != nil {
		t.Fatalf("Mentions: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found %d mentions of a subject phrase, want 0: %+v", len(found), found)
	}
}

// A book naming another in its index AND its bibliography is one backlink
// with two places, not two backlinks.
func TestOneBookNamingAnotherTwiceIsOneMention(t *testing.T) {
	p := testPool(t)
	userID, named := mentionFixture(t, p)

	citing := mentioner(t, p, userID, "d", "bash Cookbook",
		"See Learning the bash Shell by Cameron Newham.")
	// A second passage in the same book.
	var bookFileID uuid.UUID
	if err := p.QueryRow(t.Context(),
		`SELECT bf.id FROM book_files bf JOIN editions e ON e.id = bf.edition_id
		 WHERE e.work_id = $1`, citing).Scan(&bookFileID); err != nil {
		t.Fatalf("finding the citing file: %v", err)
	}
	if _, err := p.Exec(t.Context(),
		`INSERT INTO chunks (book_file_id, seq, start_major, start_path, start_offset,
		                     end_major, end_path, end_offset, content, token_count, strategy_version)
		 VALUES ($1, 1, 2, 'section[1]/p[9]', 0, 2, 'section[1]/p[9]', 40,
		         'Newham, Cameron (Learning the bash Shell), 212', 12, 1)`,
		bookFileID); err != nil {
		t.Fatalf("adding an index line: %v", err)
	}

	found, err := p.Mentions(t.Context(), named, userID, 20)
	if err != nil {
		t.Fatalf("Mentions: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d mentions, want 1: %+v", len(found), found)
	}
	if found[0].Places != 2 {
		t.Errorf("places = %d, want 2", found[0].Places)
	}
}

// A book nobody may open is not named on anybody's page, and neither is one
// they cannot see the mention in.
func TestMentionsAreScopedToWhatTheViewerCanOpen(t *testing.T) {
	p := testPool(t)
	userID, named := mentionFixture(t, p)
	mentioner(t, p, userID, "e", "bash Cookbook",
		"See Learning the bash Shell by Cameron Newham.")

	stranger := uuid.New()
	found, err := p.Mentions(t.Context(), named, stranger, 20)
	if err == nil && len(found) > 0 {
		t.Fatalf("a stranger was shown %d mentions", len(found))
	}
}

// mentionTarget adds a book to be named, held by owner.
func mentionTarget(t *testing.T, p *Pool, owner uuid.UUID, hash, title, author string) uuid.UUID {
	t.Helper()
	in := sampleInput(hashOf(hash), "/library/target-"+hash+".epub", title, author)
	in.OwnerID = owner
	if author == "" {
		in.Result.Metadata.Authors = nil
	}
	bookFile, err := p.UpsertBookFile(t.Context(), in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	return bookFile.WorkID
}

// Not being allowed to see a book reports "not found": its backlinks are no
// way round that.
func TestMentionsOfAWorkTheViewerCannotSeeAreNotFound(t *testing.T) {
	p := testPool(t)
	userID, named := mentionFixture(t, p)
	mentioner(t, p, userID, "e", "bash Cookbook", "See Learning the bash Shell by Cameron Newham.")
	stranger := secondUser(t, p)

	for name, tc := range map[string]struct{ work, viewer uuid.UUID }{
		"no viewer":             {named, uuid.Nil},
		"a work that is absent": {uuid.New(), userID},
		"somebody else's work":  {named, stranger},
	} {
		t.Run(name, func(t *testing.T) {
			found, err := p.Mentions(t.Context(), tc.work, tc.viewer, 20)
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			if len(found) != 0 {
				t.Fatalf("returned %d mentions alongside the error", len(found))
			}
		})
	}
}

// A one-word title matches every discussion of the subject, and a short
// surname is a syllable: both are skipped rather than searched for.
func TestMentionsSkipNamesTooWeakToMatchOn(t *testing.T) {
	p := testPool(t)

	for _, tc := range []struct {
		name, title, author, text string
		want                      int
	}{
		{"a one-word title", "Refactoring", "Martin Fowler",
			"As Refactoring by Martin Fowler explains, small steps are safer.", 0},
		{"a surname under four letters", "Deep Learning Systems", "Mu Li",
			"See Deep Learning Systems by Mu Li for the details.", 0},
		{"no author at all", "Deep Learning Systems", "",
			"See Deep Learning Systems for the details.", 0},
		{"a name written surname first", "Refactoring Databases", "Ambler, Scott",
			"Refactoring Databases (Ambler and Sadalage, 2006) covers schema change.", 1},
		{"a surname that is only a prefix of a word", "Patterns of Enterprise", "Martin Fowl",
			"Patterns of Enterprise Application Architecture by Martin Fowler.", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanLibrary(t, p)
			owner := testOwner(t, p)
			named := mentionTarget(t, p, owner, "1", tc.title, tc.author)
			mentioner(t, p, owner, "2", "A Citing Book", tc.text)

			found, err := p.Mentions(t.Context(), named, owner, 20)
			if err != nil {
				t.Fatalf("Mentions: %v", err)
			}
			if len(found) != tc.want {
				t.Fatalf("found %d mentions, want %d: %+v", len(found), tc.want, found)
			}
		})
	}
}

// The surname comes from a book's own metadata, which is untrusted. It is
// matched literally: a bracket is not a syntax error and a plus is not "one
// or more".
func TestMentionsMatchTheSurnameLiterally(t *testing.T) {
	p := testPool(t)

	for _, tc := range []struct{ name, author string }{
		{"an unbalanced bracket", "Ann (Smith"},
		{"a quantifier", "Ann Smith+"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanLibrary(t, p)
			owner := testOwner(t, p)
			named := mentionTarget(t, p, owner, "1", "Practical Parsing Techniques", tc.author)
			mentioner(t, p, owner, "2", "A Citing Book",
				"Practical Parsing Techniques by Ann Smith, and Smithhh too.")

			found, err := p.Mentions(t.Context(), named, owner, 20)
			if err != nil {
				t.Fatalf("Mentions: %v", err)
			}
			if len(found) != 0 {
				t.Fatalf("a surname with a metacharacter matched as a pattern: %+v", found)
			}
		})
	}
}

// Backlinks come only from books the viewer may open: somebody else's
// bibliography is not shown, until they lend the book.
func TestMentionsComeOnlyFromFilesTheViewerCanOpen(t *testing.T) {
	p := testPool(t)
	owner, named := mentionFixture(t, p)
	ctx := t.Context()
	stranger := secondUser(t, p)

	in := sampleInput(hashOf("f"), "/library/f.epub", "Shell Scripting Secrets", "Some Author")
	in.OwnerID = stranger
	theirs, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if _, err := p.SaveChunks(ctx, theirs.BookFileID, []ChunkInput{{
		Start:      domain.Locator{Format: domain.FormatEPUB, SpineIndex: 1},
		End:        domain.Locator{Format: domain.FormatEPUB, SpineIndex: 1, CharOffset: 50},
		Content:    "Learning the bash Shell by Cameron Newham is the classic.",
		TokenCount: 12,
	}}, 1); err != nil {
		t.Fatalf("SaveChunks: %v", err)
	}

	found, err := p.Mentions(ctx, named, owner, 20)
	if err != nil {
		t.Fatalf("Mentions: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("the owner was shown a mention in somebody else's book: %+v", found)
	}

	if err := p.ShareBook(ctx, theirs.BookFileID, stranger, owner, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	found, err = p.Mentions(ctx, named, owner, 20)
	if err != nil {
		t.Fatalf("Mentions (after lending): %v", err)
	}
	if len(found) != 1 || found[0].WorkID != theirs.WorkID {
		t.Fatalf("after lending, mentions = %+v, want the lent book", found)
	}
}

func TestMentionsAreOrderedByPlacesThenTitle(t *testing.T) {
	p := testPool(t)
	owner, named := mentionFixture(t, p)
	const cite = "Learning the bash Shell by Cameron Newham."

	beta := mentioner(t, p, owner, "b", "Beta Scripts", cite)
	alpha := mentioner(t, p, owner, "c", "Alpha Scripts", cite)
	// A PDF naming it twice, on page 12 first.
	zeta, _ := seedSearchFile(t, p, owner, 501, domain.FormatPDF, "Zeta Scripts", "Some Author",
		[]searchPassage{{text: "Intro.", major: 11}, {text: cite, major: 12}, {text: "Again: " + cite, major: 30}})

	found, err := p.Mentions(t.Context(), named, owner, 0)
	if err != nil {
		t.Fatalf("Mentions: %v", err)
	}
	var got []uuid.UUID
	for _, m := range found {
		got = append(got, m.WorkID)
	}
	if want := []uuid.UUID{zeta.WorkID, alpha, beta}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v (most places first, then by title)", got, want)
	}
	z := found[0]
	if z.Places != 2 || z.Locator.Format != domain.FormatPDF || z.Locator.Page != 12 || z.Locator.SpineIndex != 0 {
		t.Errorf("PDF mention = places %d locator %+v, want 2 places at page 12", z.Places, z.Locator)
	}
	if z.BookFileID != zeta.BookFileID || !slices.Equal(z.Authors, []string{"Some Author"}) {
		t.Errorf("PDF mention = %+v", z)
	}

	limited, err := p.Mentions(t.Context(), named, owner, 1)
	if err != nil {
		t.Fatalf("Mentions (limit 1): %v", err)
	}
	if len(limited) != 1 || limited[0].WorkID != zeta.WorkID {
		t.Fatalf("limit 1 = %+v, want the most-cited book", limited)
	}
}

func TestSurnameOf(t *testing.T) {
	for in, want := range map[string]string{
		"Cameron Newham":       "Newham",
		"  Newham, Cameron  ":  "Newham",
		"Ursula K. Le Guin":    "Guin",
		"Plato":                "Plato",
		"   ":                  "",
		"de Saint-Exupéry, A.": "de Saint-Exupéry",
	} {
		if got := surnameOf(in); got != want {
			t.Errorf("surnameOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegexpQuoteEscapesEveryMetacharacter(t *testing.T) {
	const meta = `\^$.|?*+()[]{}`
	got := regexpQuote("a" + meta + "é")
	want := `a\\\^\$\.\|\?\*\+\(\)\[\]\{\}é`
	if got != want {
		t.Fatalf("regexpQuote = %q, want %q", got, want)
	}
	// And the quoted form matches exactly the original text under Go's
	// regexp, whose syntax agrees with Postgres on these characters.
	if !regexp.MustCompile("^" + got + "$").MatchString("a" + meta + "é") {
		t.Error("the quoted pattern does not match its own text")
	}
}

func TestAroundQuotesTheNameInContext(t *testing.T) {
	const phrase = "Learning the bash Shell"

	t.Run("a short passage is returned whole, whitespace collapsed", func(t *testing.T) {
		got := around("See \n\n  "+phrase+"\tby Newham.", phrase, 240)
		if want := "See " + phrase + " by Newham."; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("a long passage is cut around the name, at word boundaries", func(t *testing.T) {
		content := strings.Repeat("other entry ", 50) + "LEARNING THE BASH SHELL, Newham. " + strings.Repeat("more text ", 50)
		got := around(content, phrase, 120)
		if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
			t.Fatalf("got %q, want ellipses on both sides", got)
		}
		if !strings.Contains(got, "LEARNING THE BASH SHELL") {
			t.Fatalf("got %q, the name itself was cut out", got)
		}
		body := strings.TrimSuffix(strings.TrimPrefix(got, "…"), "…")
		if len(body) > 120 {
			t.Errorf("excerpt body is %d bytes, wider than 120", len(body))
		}
		for _, w := range strings.Fields(body) {
			if w != "other" && w != "entry" && w != "more" && w != "text" &&
				!strings.Contains("LEARNING THE BASH SHELL, Newham.", w) {
				t.Errorf("excerpt cut a word in half: %q in %q", w, got)
			}
		}
	})

	t.Run("a name not in the passage shows its start", func(t *testing.T) {
		content := strings.Repeat("abcdefghi ", 40)
		got := around(content, phrase, 100)
		if strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
			t.Fatalf("got %q, want the start of the passage", got)
		}
	})

	t.Run("a name at the very end keeps the end", func(t *testing.T) {
		content := strings.Repeat("abcdefghi ", 40) + phrase
		got := around(content, phrase, 100)
		if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, phrase) {
			t.Fatalf("got %q, want the passage's end", got)
		}
	})

	t.Run("a rune is never cut in half", func(t *testing.T) {
		content := strings.Repeat("é", 300) + phrase + strings.Repeat("ü", 300)
		got := around(content, phrase, 101)
		if !utf8.ValidString(got) {
			t.Fatalf("excerpt %q is not valid UTF-8", got)
		}
		if !strings.Contains(got, phrase) {
			t.Fatalf("got %q, the name was cut out", got)
		}
	})
}
