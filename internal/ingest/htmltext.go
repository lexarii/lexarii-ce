// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"strings"

	"golang.org/x/net/html"
)

// TextFromHTML reduces a sanitised fragment to readable text.
//
// Block elements become line breaks, because a title page read as one run-on
// line tells a model far less than the same words laid out as they were on the
// page - and the layout of a title page is exactly what makes it recognisable.
//
// golang.org/x/net/html is a direct import here. It was already in the module
// graph as bluemonday's own dependency, so nothing new is being added to the
// build; the alternative within the approved set is bluemonday's strict policy,
// which strips tags without leaving separators and would run a title into its
// author. Regular expressions over HTML are not an alternative.
func TextFromHTML(fragment string) string {
	doc, err := html.Parse(strings.NewReader(fragment))
	if err != nil {
		return ""
	}

	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
		case html.ElementNode:
			// Code and stylesheets parse as text nodes, and are not words.
			if n.Data == "script" || n.Data == "style" {
				return
			}
			if isBlockTag(n.Data) {
				b.WriteString("\n")
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if n.Type == html.ElementNode && isBlockTag(n.Data) {
			b.WriteString("\n")
		}
	}
	walk(doc)

	return collapseBlankLines(b.String())
}

func isBlockTag(tag string) bool {
	switch tag {
	case "p", "div", "section", "article", "li", "tr", "br", "blockquote",
		"h1", "h2", "h3", "h4", "h5", "h6", "figcaption", "pre":
		return true
	}
	return false
}

// collapseBlankLines trims each line and squeezes runs of blank ones, so the
// sample spends its budget on words rather than on whitespace.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := true // leading blanks are dropped

	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if blank {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, line)
	}

	return strings.TrimSpace(strings.Join(out, "\n"))
}
