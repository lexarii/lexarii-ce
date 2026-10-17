// SPDX-License-Identifier: AGPL-3.0-or-later

package epub

import (
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// Reader serves the contents of an open EPUB.
//
// The archive stays open for the life of the reader, which is what makes
// serving successive spine items cheap: a 50 MB EPUB is parsed once, not once
// per page turn.
type Reader struct {
	zip   *ingest.SafeZip
	spine []ingest.SpineItem
	toc   []ingest.TOCEntry
}

// Open parses an EPUB for reading.
func Open(r io.ReaderAt, size int64) (*Reader, error) {
	res, err := Parse(r, size)
	if err != nil {
		return nil, err
	}
	// Parse already validated the archive; reopening is cheap because the zip
	// central directory is small and nothing is decompressed here.
	z, _, err := ingest.OpenSafeZip(r, size)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotEPUB, err)
	}
	return &Reader{zip: z, spine: res.Spine, toc: res.TOC}, nil
}

// Spine returns the reading order.
func (r *Reader) Spine() []ingest.SpineItem { return r.spine }

// TOC returns the navigation structure.
func (r *Reader) TOC() []ingest.TOCEntry { return r.toc }

// SpineItem returns one document, sanitised and ready to insert into the DOM.
//
// assetURL builds the URL the reader should use for an image inside the
// archive; it is passed in so the storage layer, not the parser, decides how
// assets are addressed.
func (r *Reader) SpineItem(index int, assetURL func(archivePath string) string) (string, error) {
	if index < 0 || index >= len(r.spine) {
		return "", fmt.Errorf("spine index %d out of range (%d items)", index, len(r.spine))
	}

	item := r.spine[index]
	raw, err := r.zip.ReadFile(item.Path)
	if err != nil {
		return "", fmt.Errorf("reading spine item %d (%s): %w", index, item.Path, err)
	}

	return sanitiseDocument(string(raw), item.Path, assetURL, r.spineIndexOf, index), nil
}

// spineIndexOf maps an archive path to its position in reading order.
//
// A link inside a book points at another file in the archive; a reader needs a
// spine index. Nothing else knows both, which is why links are resolved here
// rather than in the client.
func (r *Reader) spineIndexOf(archivePath string) (int, bool) {
	for _, item := range r.spine {
		if item.Path == archivePath {
			return item.Index, true
		}
	}
	return 0, false
}

// Asset returns a file from inside the archive: an image referenced by a spine
// item. Only image media types are served.
func (r *Reader) Asset(archivePath string) ([]byte, string, error) {
	clean, err := ingest.SanitizePath(archivePath)
	if err != nil {
		return nil, "", err
	}

	data, err := r.zip.ReadFile(clean)
	if err != nil {
		return nil, "", err
	}

	mediaType := imageMediaType(clean)
	if mediaType == "" {
		// Only images are served. Anything else in a book archive - scripts,
		// stylesheets, fonts with exotic loaders - has no business being fetched
		// by the reader, and serving it would reintroduce exactly what
		// sanitisation just removed.
		return nil, "", fmt.Errorf("%w: %s is not an image", ingest.ErrUnsafePath, clean)
	}
	return data, mediaType, nil
}

// ---------------------------------------------------------------------------
// Sanitisation
// ---------------------------------------------------------------------------

var policyOnce sync.Once
var policy *bluemonday.Policy

// contentPolicy builds the allowlist applied to every piece of book HTML.
//
// Book content is untrusted: an EPUB is a zip file from the internet that can
// contain arbitrary markup, scripts, and remote references. The policy is an
// allowlist, never a denylist, because a denylist is a list of the attacks
// somebody has already thought of.
func contentPolicy() *bluemonday.Policy {
	policyOnce.Do(func() {
		p := bluemonday.NewPolicy()

		// Structure a book actually needs.
		p.AllowElements(
			"p", "div", "span", "br", "hr",
			"h1", "h2", "h3", "h4", "h5", "h6",
			"em", "i", "strong", "b", "u", "s", "small", "mark",
			"sub", "sup", "q", "cite", "abbr", "dfn", "code", "kbd", "samp", "var",
			"blockquote", "pre",
			"ul", "ol", "li", "dl", "dt", "dd",
			"table", "thead", "tbody", "tfoot", "tr", "th", "td", "caption", "colgroup", "col",
			"figure", "figcaption", "section", "article", "aside", "header", "footer", "nav",
			"ruby", "rt", "rp", "bdi", "bdo", "wbr", "time",
		)

		// id is essential: it is how a table-of-contents fragment and a locator
		// find their target.
		p.AllowAttrs("id").Globally()
		p.AllowAttrs("lang", "dir", "title").Globally()
		// class is kept so semantic hooks survive, even though the book's own
		// stylesheet is dropped.
		p.AllowAttrs("class").Globally()

		p.AllowAttrs("colspan", "rowspan", "scope", "headers").OnElements("td", "th")
		p.AllowAttrs("start", "type", "reversed").OnElements("ol")
		p.AllowAttrs("value").OnElements("li")
		p.AllowAttrs("datetime").OnElements("time")

		// Images are rewritten to point at the asset endpoint before this runs,
		// so only same-origin relative URLs survive.
		p.AllowAttrs("src", "alt", "width", "height").OnElements("img")
		p.AllowAttrs("href").OnElements("a")

		// Links may only be internal. A book cannot send the reader to an
		// arbitrary URL, and a relative link stays inside the reader where the
		// client resolves it against the spine.
		p.AllowURLSchemes("data")
		p.AllowRelativeURLs(true)
		p.RequireNoFollowOnLinks(true)

		// MathML, which a technical library is full of.
		//
		// AllowElements alone is NOT enough: bluemonday drops an allowed element
		// that has no permitted attributes, so every one of these also needs
		// AllowNoAttrs. Without it "H<sub>2</sub>O" in MathML silently collapses
		// to the bare text "H2O" - the markup vanishes and the equation is
		// quietly wrong, which is worse than failing loudly.
		mathml := []string{
			"math", "mi", "mn", "mo", "ms", "mtext", "mspace",
			"mrow", "mfrac", "msqrt", "mroot", "mstyle", "merror", "mpadded",
			"mphantom", "mfenced", "menclose",
			"msub", "msup", "msubsup", "munder", "mover", "munderover",
			"mmultiscripts", "mtable", "mtr", "mtd", "maligngroup", "malignmark",
			"semantics", "annotation",
		}
		p.AllowElements(mathml...)
		p.AllowNoAttrs().OnElements(mathml...)

		// The same trap applies to structural HTML elements that legitimately
		// carry no attributes.
		p.AllowNoAttrs().OnElements(
			"p", "div", "span", "br", "hr",
			"h1", "h2", "h3", "h4", "h5", "h6",
			"em", "i", "strong", "b", "u", "s", "small", "mark",
			"sub", "sup", "q", "cite", "abbr", "dfn", "code", "kbd", "samp", "var",
			"blockquote", "pre",
			"ul", "ol", "li", "dl", "dt", "dd",
			"table", "thead", "tbody", "tfoot", "tr", "th", "td", "caption", "colgroup", "col",
			"figure", "figcaption", "section", "article", "aside", "header", "footer", "nav",
			"ruby", "rt", "rp", "bdi", "bdo", "wbr", "time",
		)

		policy = p
	})
	return policy
}

var (
	// Everything between <body> and </body>, which is all a reader needs. The
	// head carries the book's own <link rel=stylesheet>, which is deliberately
	// discarded - see below.
	bodyRe = regexp.MustCompile(`(?is)<body[^>]*>(.*?)</body>`)
	// Image sources, rewritten to the asset endpoint before sanitisation.
	imgSrcRe = regexp.MustCompile(`(?is)(<img\b[^>]*?\bsrc\s*=\s*)("([^"]*)"|'([^']*)')`)
	// Link targets, rewritten to reader coordinates before sanitisation.
	aHrefRe = regexp.MustCompile(`(?is)(<a\b[^>]*?\bhref\s*=\s*)("([^"]*)"|'([^']*)')`)
	// An inline SVG, and the raster pictures inside one. See svgPictures.
	svgRe      = regexp.MustCompile(`(?is)<svg\b[^>]*>(.*?)</svg\s*>`)
	svgImageRe = regexp.MustCompile(`(?is)<image\b([^>]*)>`)
	// href or xlink:href, as a whole attribute name: not data-href.
	svgHrefRe = regexp.MustCompile(`(?is)(?:^|\s)(?:xlink:)?href\s*=\s*("([^"]*)"|'([^']*)')`)
	svgSizeRe = regexp.MustCompile(`(?is)(?:^|\s)(width|height)\s*=\s*["']?\s*(\d{1,5})(?:\.\d+)?(?:px)?\s*["']?`)
)

// svgPictures turns an SVG that only frames a picture into that picture.
//
// The cover of every Project Gutenberg EPUB, and of many others, is a page
// holding nothing but
//
//	<svg viewBox="0 0 1500 2114"><image xlink:href="cover.jpg"/></svg>
//
// SVG is not in the content policy and must not be: it is a script host with
// its own event attributes and its own ways of fetching things. So the
// sanitiser dropped the svg, the image went with it, and the first page of
// the book was blank.
//
// What the reader needs from such an SVG is the picture, so each <image> in it
// becomes an <img> with the same href, BEFORE the image rewrite below runs:
// from there it is an ordinary image - resolved inside the archive, sent to
// the asset endpoint, remote references refused - and nothing about SVG ever
// reaches the policy. Its width and height come along when they are plain
// numbers, so the browser can reserve the picture's space before it has
// loaded instead of moving the text under the reader when it arrives.
//
// An SVG with no <image> in it is left alone, for the policy to drop as
// before. Locators are unaffected: they are paths of block elements, an <img>
// is not one and has no children, so no block's path changes.
func svgPictures(body string) string {
	return svgRe.ReplaceAllStringFunc(body, func(svg string) string {
		inner := svgRe.FindStringSubmatch(svg)[1]
		var b strings.Builder
		for _, image := range svgImageRe.FindAllStringSubmatch(inner, -1) {
			attrs := image[1]
			href := svgHrefRe.FindStringSubmatch(attrs)
			if href == nil {
				continue
			}
			src := href[2]
			if src == "" {
				src = href[3]
			}
			b.WriteString(`<img src="`)
			b.WriteString(strings.ReplaceAll(src, `"`, "&#34;"))
			b.WriteString(`" alt=""`)
			for _, size := range svgSizeRe.FindAllStringSubmatch(attrs, 2) {
				fmt.Fprintf(&b, ` %s="%s"`, strings.ToLower(size[1]), size[2])
			}
			b.WriteString(`>`)
		}
		if b.Len() == 0 {
			return svg
		}
		return b.String()
	})
}

// sanitiseDocument extracts a document's body and makes it safe to insert.
//
// The book's own CSS is discarded entirely, not sanitised. This is deliberate
// and is what a reader does rather than what a browser does: publisher
// stylesheets set fixed widths, absolute positioning, and their own page
// geometry, all of which fight the reader's column layout and none of which the
// user asked for. Dropping it also removes a whole category of layout and
// exfiltration tricks. Lexarii's own typography is applied instead, which is
// the point of having typography controls at all.
func sanitiseDocument(
	raw, docPath string,
	assetURL func(string) string,
	spineIndexOf func(string) (int, bool),
	currentIndex int,
) string {
	body := raw
	if m := bodyRe.FindStringSubmatch(raw); len(m) == 2 {
		body = m[1]
	}

	// A cover drawn as an SVG <image> becomes an <img> first, so the rewrite
	// below treats it like any other picture.
	body = svgPictures(body)

	// Rewrite image sources to the asset endpoint BEFORE sanitising, so the
	// policy sees only relative same-origin URLs and rejects anything else.
	body = imgSrcRe.ReplaceAllStringFunc(body, func(match string) string {
		groups := imgSrcRe.FindStringSubmatch(match)
		src := groups[3]
		if src == "" {
			src = groups[4]
		}
		resolved, err := ingest.ResolveRelative(docPath, src)
		if err != nil {
			// A remote or traversing image reference. Dropping the attribute
			// leaves the alt text, which is more useful than a broken icon.
			return `<img data-blocked="1" `
		}
		return groups[1] + `"` + assetURL(resolved) + `"`
	})

	// Links, rewritten to reader coordinates BEFORE sanitising, for the same
	// reason as images: the policy then sees only a relative same-origin URL
	// and rejects anything that is not one.
	body = rewriteLinks(body, docPath, spineIndexOf, currentIndex)

	return closeSelfClosingTags(contentPolicy().Sanitize(body))
}

// rewriteLinks turns a book's internal references into reader coordinates.
//
// A link inside an EPUB names another file in the archive - "ch05.xhtml#sec2" -
// which means nothing to a browser pointed at /read/<id>: followed as written
// it navigates out of the reader and 404s. Every cross-reference, index entry
// and footnote in the library was dead.
//
// Rewritten to "?spine=5&frag=sec2", which is relative to the current page, so
// it needs no book id and resolves to /read/<id>?spine=5&frag=sec2. It stays a
// real link: middle-click opens it, the keyboard reaches it, and it can be
// copied and shared.
//
// A reference this book cannot satisfy - a remote URL, a file outside the
// spine - loses its href and keeps its text. A link that goes nowhere is worse
// than plain words, because it invites a click that does nothing.
func rewriteLinks(
	body, docPath string,
	spineIndexOf func(string) (int, bool),
	currentIndex int,
) string {
	if spineIndexOf == nil {
		return body
	}

	return aHrefRe.ReplaceAllStringFunc(body, func(match string) string {
		groups := aHrefRe.FindStringSubmatch(match)
		href := groups[3]
		if href == "" {
			href = groups[4]
		}

		target, fragment, ok := resolveLink(href, docPath, spineIndexOf, currentIndex)
		if !ok {
			// The attribute goes, the text stays.
			return `<a data-external="1" `
		}

		out := fmt.Sprintf("?spine=%d", target)
		if fragment != "" {
			out += "&frag=" + url.QueryEscape(fragment)
		}
		return groups[1] + `"` + out + `"`
	})
}

// resolveLink works out which spine item a href points at, and where inside it.
func resolveLink(
	href, docPath string,
	spineIndexOf func(string) (int, bool),
	currentIndex int,
) (spine int, fragment string, ok bool) {
	href = strings.TrimSpace(href)
	if href == "" {
		return 0, "", false
	}

	if i := strings.IndexByte(href, '#'); i >= 0 {
		fragment = href[i+1:]
		// A bare fragment points inside the document being read.
		if i == 0 {
			return currentIndex, fragment, true
		}
		href = href[:i]
	}

	resolved, err := ingest.ResolveRelative(docPath, href)
	if err != nil {
		return 0, "", false
	}
	index, found := spineIndexOf(resolved)
	if !found {
		return 0, "", false
	}
	return index, fragment, true
}

// closeSelfClosingTags turns `<a id="x"/>` into `<a id="x"></a>`.
//
// An EPUB is XHTML, where any element may close itself. A browser parses what
// it is served as HTML, where only void elements may - so that anchor is an
// OPENING tag that never closes, and every element after it becomes a child of
// the link. bluemonday works on tokens and passes the form through untouched.
//
// Found in a real book. One chapter of `97 Things Every Java Programmer Should
// Know` rendered entirely underlined, because the empty anchors it uses as link
// targets swallowed the rest of the document. That is the visible half. The
// invisible half is worse: element paths are counted through the tree a parser
// builds, so an anchor in the middle of a paragraph silently changes the
// address of everything after it - and those addresses are what locators,
// citations and highlights are made of.
//
// This has to happen at the TOKEN level. Parsing and re-rendering does not fix
// it: by then the tree builder has already swallowed the text, and rendering
// reproduces the swallow faithfully. The repair must land before any tree
// exists.
func closeSelfClosingTags(fragment string) string {
	z := html.NewTokenizer(strings.NewReader(fragment))
	var b strings.Builder

	for {
		switch z.Next() {
		case html.ErrorToken:
			// io.EOF, or markup too broken to tokenise. Either way what has
			// been written is what there is.
			return b.String()

		case html.SelfClosingTagToken:
			t := z.Token()
			if voidElements[t.Data] {
				// Correct as it stands: a browser reads `<br/>` the way the
				// book meant it.
				b.WriteString(t.String())
				continue
			}
			// Written as an explicit pair. The element keeps its attributes -
			// the id is the whole point of these, since it is what a table of
			// contents and a cross-reference point at.
			t.Type = html.StartTagToken
			b.WriteString(t.String())
			b.WriteString("</" + t.Data + ">")

		default:
			b.WriteString(z.Token().String())
		}
	}
}

// voidElements are the HTML elements that have no closing tag, and so are the
// only ones a browser accepts as self-closing.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

// imageMediaType maps an archive path to a servable image type, or "" when the
// file is not an image.
func imageMediaType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".svg":
		// SVG is an image and a script host at once. Served as a download-safe
		// type so a browser renders it as a picture and never as a document.
		return "image/svg+xml"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	default:
		return ""
	}
}

// AssetPathFromURL recovers the archive path from a URL-escaped segment.
func AssetPathFromURL(escaped string) (string, error) {
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ingest.ErrUnsafePath, err)
	}
	return ingest.SanitizePath(decoded)
}

// maxSampleUnits bounds how many spine items the sampler will read.
const maxSampleUnits = 12

// Sample returns the opening plain text of an EPUB, stopping once it has
// enough.
//
// Enrichment sends a model a sample, never a whole book. The front matter and
// first chapters are where a title page, contents, and introduction live, which
// is where a description of the book actually comes from.
func Sample(r io.ReaderAt, size int64, maxChars int) (string, error) {
	if maxChars <= 0 {
		return "", nil
	}

	reader, err := Open(r, size)
	if err != nil {
		return "", err
	}

	// Two slices, not one continuous read from the first document.
	//
	// The front of a book is a title page, a copyright page, and very often a
	// contents list running to several documents. A sample made entirely of that
	// says what the chapters are CALLED and nothing about what the book is. So:
	// the opening, which is where the title lives, and a slice of prose from far
	// enough in to be past the apparatus.
	spine := reader.Spine()
	head := readSpine(reader, spine, 0, maxChars/4)

	bodyStart := len(spine) / 5
	if bodyStart < 1 {
		bodyStart = 1
	}
	body := readSpine(reader, spine, bodyStart, maxChars-len(head))

	var b strings.Builder
	b.WriteString(head)
	if head != "" && body != "" {
		b.WriteString("\n\n")
	}
	b.WriteString(body)

	return truncateSample(b.String(), maxChars), nil
}

// readSpine accumulates text from spine items, up to a character budget.
func readSpine(reader *Reader, spine []ingest.SpineItem, from, budget int) string {
	if budget <= 0 || from >= len(spine) {
		return ""
	}

	var b strings.Builder
	read := 0
	for i := from; i < len(spine) && b.Len() < budget && read < maxSampleUnits; i++ {
		// Non-linear items are supplementary - notes, adverts, colophons - and
		// are not what the book is about.
		if !spine[i].Linear {
			continue
		}

		// Assets are not resolved: the sampler wants words, and an image URL in
		// a prompt is tokens spent on nothing.
		html, err := reader.SpineItem(i, func(string) string { return "" })
		if err != nil {
			// One unreadable document must not cost the sample. A book whose
			// first chapter is broken usually has a readable second one.
			continue
		}

		text := strings.TrimSpace(ingest.TextFromHTML(html))
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
		read++
	}
	return truncateSample(b.String(), budget)
}

// truncateSample cuts to a rune boundary, preferring a sentence or line end.
func truncateSample(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	cut := maxChars
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	if idx := strings.LastIndexAny(s[:cut], ".\n"); idx > maxChars/2 {
		cut = idx + 1
	}
	return strings.TrimSpace(s[:cut])
}

// TextBlock is one block-level element's text, with the path that identifies it.
//
// The path is in exactly the form the reader's own locator code produces, and
// that is not a coincidence to be maintained by hand: a citation resolves
// through the same function an annotation anchor does, so if these disagree a
// retrieved passage opens at the wrong paragraph.
type TextBlock struct {
	Path string
	Text string
}

// blockTags are the elements a position anchors to.
//
// Deliberately identical to the reader's own list. A text node's position is
// given by its NEAREST block ancestor, so both sides must agree on what counts
// as one.
var blockTags = map[string]bool{
	"p": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true,
	"h6": true, "li": true, "blockquote": true, "pre": true, "figure": true,
	"td": true, "dd": true, "dt": true,
}

// Blocks returns the text of one spine item, grouped by the element each run
// belongs to.
//
// Built from the SANITISED html - the same bytes the reader renders - because a
// path is only meaningful relative to the tree it was computed against. Paths
// derived from the book's original markup would point into a document that no
// browser ever sees.
func (r *Reader) Blocks(index int) ([]TextBlock, error) {
	fragment, err := r.SpineItem(index, func(string) string { return "" })
	if err != nil {
		return nil, err
	}

	doc, err := html.Parse(strings.NewReader(fragment))
	if err != nil {
		return nil, fmt.Errorf("parsing spine item %d: %w", index, err)
	}

	// html.Parse wraps a fragment in html/head/body, but the reader inserts the
	// fragment straight into its own container. So body's children are what the
	// reader's root has, and paths are counted from there.
	body := findBody(doc)
	if body == nil {
		return nil, nil
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
					// Text outside any block element. It is still text, and the
					// reader would anchor it to nothing, so it is kept without a
					// path rather than silently dropped.
					out = append(out, TextBlock{Path: path, Text: child.Data})
					continue
				}
				current.Text += child.Data

			case html.ElementNode:
				childPath := joinPath(path, elementStep(child))
				if blockTags[child.Data] {
					// A new block: its text is collected here, and nested blocks
					// inside it start their own.
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

	// Blocks that turned out to hold nothing but markup.
	kept := out[:0]
	for _, b := range out {
		if strings.TrimSpace(b.Text) != "" {
			kept = append(kept, b)
		}
	}
	return kept, nil
}

// elementStep renders one path segment, counting preceding siblings of the same
// tag, one-based - the same rule the reader uses.
func elementStep(n *html.Node) string {
	index := 1
	for sibling := n.PrevSibling; sibling != nil; sibling = sibling.PrevSibling {
		if sibling.Type == html.ElementNode && sibling.Data == n.Data {
			index++
		}
	}
	return fmt.Sprintf("%s[%d]", n.Data, index)
}

func joinPath(parent, step string) string {
	if parent == "" {
		return step
	}
	return parent + "/" + step
}

func findBody(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.Data == "body" {
		return n
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if found := findBody(child); found != nil {
			return found
		}
	}
	return nil
}
