// SPDX-License-Identifier: AGPL-3.0-or-later

package epub

import (
	"strings"
	"testing"
)

// An EPUB is XHTML, where any element may close itself. A browser parses what
// it is served as HTML, where only void elements may - so `<a id="x"/>` is an
// OPENING tag that never closes, and everything after it becomes link content.
//
// Found in a real book: one chapter of `97 Things Every Java Programmer Should
// Know` renders entirely underlined, because the anchors it uses as link
// targets swallow the rest of the document. It is not only cosmetic. The
// element paths that locators, citations and highlights are built from are
// counted through that swallowed structure, so an anchor in the middle of a
// paragraph changes the address of everything after it.
func TestSelfClosingTagsAreClosedProperly(t *testing.T) {
	tests := []struct {
		name string
		in   string
		// mustNotContain catches the self-closing form surviving.
		mustNotContain string
		// after is text that must NOT end up inside the anchor.
		anchorMustNotSwallow string
	}{
		{
			name:                 "an anchor used as a link target",
			in:                   `<body><p>before<a id="x"/>after</p></body>`,
			mustNotContain:       `<a id="x"/>`,
			anchorMustNotSwallow: "after",
		},
		{
			name:                 "an anchor with no attributes",
			in:                   `<body><p>before<a/>after</p></body>`,
			anchorMustNotSwallow: "after",
		},
		{
			name:                 "a self-closing span",
			in:                   `<body><p>before<span id="y"/>after</p></body>`,
			mustNotContain:       `<span id="y"/>`,
			anchorMustNotSwallow: "after",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitiseDocument(tt.in, "OEBPS/ch01.xhtml", func(s string) string { return s }, nil, 0)

			if tt.mustNotContain != "" && strings.Contains(got, tt.mustNotContain) {
				t.Fatalf("the self-closing form survived: %s", got)
			}

			// The text after the empty element must be a sibling of it, not a
			// child. Everything between the anchor's open and close tag is what
			// a browser will underline.
			open := strings.Index(got, "<a")
			if open < 0 {
				open = strings.Index(got, "<span")
			}
			if open >= 0 {
				closeTag := strings.Index(got[open:], ">")
				rest := got[open+closeTag+1:]
				end := strings.Index(rest, "</")
				if end > 0 && strings.Contains(rest[:end], tt.anchorMustNotSwallow) {
					t.Fatalf("%q was swallowed by the empty element: %s",
						tt.anchorMustNotSwallow, got)
				}
			}
		})
	}
}

// Void elements are still allowed to close themselves: that is correct HTML and
// re-rendering must not turn <br/> into something a parser reads differently.
func TestVoidElementsSurvive(t *testing.T) {
	got := sanitiseDocument(`<body><p>one<br/>two</p><hr/></body>`,
		"OEBPS/ch01.xhtml", func(s string) string { return s }, nil, 0)

	for _, want := range []string{"one", "two"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lost %q: %s", want, got)
		}
	}
	if !strings.Contains(got, "<br/>") && !strings.Contains(got, "<br>") {
		t.Fatalf("the line break vanished: %s", got)
	}
}
