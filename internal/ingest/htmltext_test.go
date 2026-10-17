// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import "testing"

func TestTextFromHTML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// The layout of a title page is what makes it recognisable: a title
		// run into its author is one long, meaningless line.
		{"blocks become lines", "<h1>The Title</h1><p>by <em>An Author</em></p>", "The Title\n\nby An Author"},
		{"inline elements do not split words", "<p>an <b>emphatic</b> <span>claim</span></p>", "an emphatic claim"},
		{"list items are separate", "<ul><li>first</li><li>second</li></ul>", "first\n\nsecond"},
		{"line breaks break", "one line<br>another", "one line\n\nanother"},
		{"table rows are separate", "<table><tr><td>a</td></tr><tr><td>b</td></tr></table>", "a\n\nb"},

		{"entities are decoded", "<p>A &amp; B &eacute;t&eacute; &#8212; &lt;tag&gt;</p>", "A & B été — <tag>"},
		{"a non-breaking space is a space", "<p>x&nbsp;y</p>", "x y"},

		{"runs of whitespace collapse", "<div>\n\n   lots    of\tspace  \n</div>", "lots of space"},
		{"runs of blank lines squeeze to one", "<p>one</p><p></p><p>  </p><p></p><p>two</p>", "one\n\ntwo"},
		{"leading and trailing blanks are dropped", "\n\n<p>only</p>\n\n", "only"},

		// A MOBI hands its raw document over, styles and all. Code is not
		// words, and a stylesheet in a description is not a blurb.
		{"scripts are dropped", "<script>var secret = 1;</script><p>text</p>", "text"},
		{"styles are dropped", "<style>p { color: red }</style><p>text</p>", "text"},
		{"a script inside the body is dropped", "<p>before</p><script>alert(1)</script><p>after</p>", "before\n\nafter"},

		{"plain text passes through", "plain text only", "plain text only"},
		{"empty is empty", "", ""},
		{"whitespace only is empty", " \n\t ", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TextFromHTML(tt.in); got != tt.want {
				t.Errorf("TextFromHTML(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Extraction is deterministic: downstream chunk ids depend on the same input
// always producing the same text.
func TestTextFromHTMLIsDeterministic(t *testing.T) {
	in := "<section><h2>Chapter</h2><p>Some <i>text</i> &amp; more.</p><blockquote>quoted</blockquote></section>"
	first := TextFromHTML(in)
	for i := 0; i < 5; i++ {
		if got := TextFromHTML(in); got != first {
			t.Fatalf("run %d produced %q, first produced %q", i, got, first)
		}
	}
}
