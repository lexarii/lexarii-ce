// SPDX-License-Identifier: AGPL-3.0-or-later

package epub

import (
	"strings"
	"testing"
)

// A book's spine, as a resolver.
func testSpine(paths map[string]int) func(string) (int, bool) {
	return func(p string) (int, bool) {
		i, ok := paths[p]
		return i, ok
	}
}

// A link inside an EPUB names a file in the archive, which means nothing to a
// browser pointed at /read/<id>. Every cross-reference, index entry and
// footnote in the library was dead until these were rewritten.
func TestLinksBecomeReaderCoordinates(t *testing.T) {
	spine := testSpine(map[string]int{
		"OEBPS/ch04.xhtml":  3,
		"OEBPS/ch05.xhtml":  4,
		"OEBPS/notes.xhtml": 9,
	})

	tests := []struct {
		name string
		href string
		want string
		why  string
	}{
		{
			name: "another document",
			href: "ch05.xhtml",
			want: `href="?spine=4"`,
		},
		{
			name: "another document at a position inside it",
			href: "ch05.xhtml#sec2",
			want: `href="?spine=4&amp;frag=sec2"`,
			why:  "a cross-reference names a section, not just a file",
		},
		{
			name: "a footnote in this document",
			href: "#fn12",
			want: `href="?spine=3&amp;frag=fn12"`,
			why:  "a bare fragment stays in the document being read",
		},
		{
			name: "a fragment needing escaping",
			href: "notes.xhtml#a b",
			want: `frag=a+b`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitiseDocument(
				`<body><p><a href="`+tt.href+`">see</a></p></body>`,
				"OEBPS/ch04.xhtml", func(s string) string { return s }, spine, 3)

			if !strings.Contains(got, tt.want) {
				t.Fatalf("got %s, want it to contain %s (%s)", got, tt.want, tt.why)
			}
			if !strings.Contains(got, ">see<") {
				t.Fatalf("the link text was lost: %s", got)
			}
		})
	}
}

// A reference this book cannot satisfy loses its href and keeps its text. A
// link that goes nowhere is worse than plain words: it invites a click that
// does nothing.
func TestUnsatisfiableLinksAreNotLinks(t *testing.T) {
	spine := testSpine(map[string]int{"OEBPS/ch04.xhtml": 3})

	for _, href := range []string{
		"https://example.com/elsewhere",
		"//example.com/protocol-relative",
		"../../../etc/passwd",
		"missing.xhtml",
		"mailto:someone@example.com",
		"",
	} {
		t.Run(href, func(t *testing.T) {
			got := sanitiseDocument(
				`<body><p><a href="`+href+`">text</a></p></body>`,
				"OEBPS/ch04.xhtml", func(s string) string { return s }, spine, 3)

			if strings.Contains(got, "href=") {
				t.Fatalf("an unsatisfiable reference kept its href: %s", got)
			}
			if !strings.Contains(got, "text") {
				t.Fatalf("the text was lost with the href: %s", got)
			}
		})
	}
}

// The rewritten form has to survive the sanitiser, which only permits relative
// URLs. A query-only href is relative to the page, which is the whole reason
// this shape was chosen over an absolute one carrying the book id.
func TestRewrittenLinksSurviveSanitisation(t *testing.T) {
	spine := testSpine(map[string]int{"OEBPS/ch05.xhtml": 4})
	got := sanitiseDocument(
		`<body><p><a href="ch05.xhtml#x">see</a></p></body>`,
		"OEBPS/ch04.xhtml", func(s string) string { return s }, spine, 3)

	if !strings.Contains(got, "spine=4") {
		t.Fatalf("the sanitiser dropped the rewritten link: %s", got)
	}
}
