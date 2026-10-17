// SPDX-License-Identifier: AGPL-3.0-or-later

package epub

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// blocksOf runs the path walk over a fragment directly, without a whole EPUB.
func blocksOf(t *testing.T, fragment string) []TextBlock {
	t.Helper()

	doc, err := html.Parse(strings.NewReader(fragment))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	body := findBody(doc)
	if body == nil {
		t.Fatal("no body")
	}

	var out []TextBlock
	var current *TextBlock
	var walk func(n *html.Node, path string)
	walk = func(n *html.Node, path string) {
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			switch child.Type {
			case html.TextNode:
				if strings.TrimSpace(child.Data) == "" {
					continue
				}
				if current == nil {
					out = append(out, TextBlock{Path: path, Text: child.Data})
					continue
				}
				current.Text += child.Data
			case html.ElementNode:
				childPath := joinPath(path, elementStep(child))
				if blockTags[child.Data] {
					out = append(out, TextBlock{Path: childPath})
					previous := current
					current = &out[len(out)-1]
					walk(child, childPath)
					current = previous
					continue
				}
				walk(child, childPath)
			}
		}
	}
	walk(body, "")

	kept := out[:0]
	for _, b := range out {
		if strings.TrimSpace(b.Text) != "" {
			kept = append(kept, b)
		}
	}
	return kept
}

// The paths here are the ones the reader's own elementPath() produces for the
// same markup. They were checked against it in a browser, not derived twice
// from the same assumption.
func TestBlockPathsMatchTheReader(t *testing.T) {
	fragment := `<section>
		<div>
			<h1>Chapter One</h1>
			<p>First paragraph.</p>
			<p>Second <em>with emphasis</em> paragraph.</p>
		</div>
		<div>
			<p>In the second div.</p>
		</div>
	</section>`

	blocks := blocksOf(t, fragment)

	want := []TextBlock{
		{Path: "section[1]/div[1]/h1[1]", Text: "Chapter One"},
		{Path: "section[1]/div[1]/p[1]", Text: "First paragraph."},
		{Path: "section[1]/div[1]/p[2]", Text: "Second with emphasis paragraph."},
		{Path: "section[1]/div[2]/p[1]", Text: "In the second div."},
	}

	if len(blocks) != len(want) {
		t.Fatalf("got %d blocks, want %d: %+v", len(blocks), len(want), blocks)
	}
	for i := range want {
		if blocks[i].Path != want[i].Path {
			t.Errorf("block %d path = %q, want %q", i, blocks[i].Path, want[i].Path)
		}
		if strings.Join(strings.Fields(blocks[i].Text), " ") != want[i].Text {
			t.Errorf("block %d text = %q, want %q", i, blocks[i].Text, want[i].Text)
		}
	}
}

// Sibling counting is per TAG, not per position: the reader counts only
// preceding siblings with the same tag name.
func TestSiblingIndexCountsOnlyMatchingTags(t *testing.T) {
	blocks := blocksOf(t, `<div><h2>A heading</h2><p>One</p><h2>Another</h2><p>Two</p></div>`)

	want := []string{"div[1]/h2[1]", "div[1]/p[1]", "div[1]/h2[2]", "div[1]/p[2]"}
	if len(blocks) != len(want) {
		t.Fatalf("got %d blocks, want %d", len(blocks), len(want))
	}
	for i, path := range want {
		if blocks[i].Path != path {
			t.Errorf("block %d path = %q, want %q", i, blocks[i].Path, path)
		}
	}
}

// Inline markup inside a block is part of that block's text, exactly as the
// reader's offsets count it: over the block's TEXT, not its markup.
func TestInlineMarkupStaysWithinItsBlock(t *testing.T) {
	blocks := blocksOf(t, `<p>Before <strong>bold</strong> and <em>italic</em> after.</p>`)

	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1: %+v", len(blocks), blocks)
	}
	if got := strings.Join(strings.Fields(blocks[0].Text), " "); got != "Before bold and italic after." {
		t.Errorf("text = %q", got)
	}
}

// A list item is a block in its own right, which is what the reader anchors to.
func TestListItemsAreBlocks(t *testing.T) {
	blocks := blocksOf(t, `<ul><li>First</li><li>Second</li></ul>`)

	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2: %+v", len(blocks), blocks)
	}
	if blocks[1].Path != "ul[1]/li[2]" {
		t.Errorf("second item path = %q", blocks[1].Path)
	}
}

// A block nested inside another gets its own entry rather than being folded
// into its parent, because that is the element the reader would anchor to.
func TestNestedBlocksAreSeparate(t *testing.T) {
	blocks := blocksOf(t, `<blockquote><p>Quoted text.</p></blockquote>`)

	for _, b := range blocks {
		if strings.TrimSpace(b.Text) == "Quoted text." && b.Path != "blockquote[1]/p[1]" {
			t.Errorf("nested paragraph path = %q, want blockquote[1]/p[1]", b.Path)
		}
	}
}

func TestBlocksIgnoreEmptyMarkup(t *testing.T) {
	blocks := blocksOf(t, `<div><p></p><p>   </p><p>Real text.</p></div>`)

	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1: %+v", len(blocks), blocks)
	}
	if strings.TrimSpace(blocks[0].Text) != "Real text." {
		t.Errorf("text = %q", blocks[0].Text)
	}
	// The path still counts the empty paragraphs, because the reader's own
	// count includes them: they are elements in the tree either way.
	if blocks[0].Path != "div[1]/p[3]" {
		t.Errorf("path = %q, want div[1]/p[3]", blocks[0].Path)
	}
}
