// SPDX-License-Identifier: AGPL-3.0-or-later

package epub

import (
	"sort"
	"strings"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// buildTOC reads the navigation structure, preferring the EPUB 3 nav document
// and falling back to the EPUB 2 NCX.
//
// The TOC is navigation, not reading order, and it is allowed to be partial,
// out of order, or absent. A book with no usable TOC is still a perfectly good
// book, so every failure here is a warning rather than an error.
func buildTOC(
	z *ingest.SafeZip,
	pkg opfPackage,
	manifest map[string]manifestItem,
	spine []ingest.SpineItem,
	opfPath string,
	res *ingest.Result,
) []ingest.TOCEntry {
	spineByPath := make(map[string]int, len(spine))
	for _, item := range spine {
		spineByPath[item.Path] = item.Index
	}

	// EPUB 3: a manifest item with properties="nav".
	for _, id := range sortedManifestIDs(manifest) {
		item := manifest[id]
		if !item.hasProperty("nav") {
			continue
		}
		entries, err := parseNavDoc(z, item.Path, spineByPath)
		if err != nil {
			res.AddWarning("navigation document %s could not be read: %v", item.Path, err)
			break
		}
		if len(entries) > 0 {
			return entries
		}
		break
	}

	// EPUB 2: the spine's toc attribute names an NCX in the manifest.
	ncxID := pkg.Spine.TOC
	if ncxID == "" {
		for _, id := range sortedManifestIDs(manifest) {
			if manifest[id].MediaType == "application/x-dtbncx+xml" {
				ncxID = id
				break
			}
		}
	}
	if item, ok := manifest[ncxID]; ok {
		entries, err := parseNCX(z, item.Path, spineByPath)
		if err != nil {
			res.AddWarning("NCX %s could not be read: %v", item.Path, err)
			return nil
		}
		return entries
	}

	res.AddWarning("no table of contents found")
	return nil
}

// ---------------------------------------------------------------------------
// EPUB 2 NCX
// ---------------------------------------------------------------------------

type ncxDoc struct {
	NavMap struct {
		Points []ncxPoint `xml:"navPoint"`
	} `xml:"navMap"`
}

type ncxPoint struct {
	PlayOrder string `xml:"playOrder,attr"`
	NavLabel  struct {
		Text string `xml:"text"`
	} `xml:"navLabel"`
	Content struct {
		Src string `xml:"src,attr"`
	} `xml:"content"`
	Children []ncxPoint `xml:"navPoint"`
}

func parseNCX(z *ingest.SafeZip, ncxPath string, spineByPath map[string]int) ([]ingest.TOCEntry, error) {
	data, err := z.ReadFile(ncxPath)
	if err != nil {
		return nil, err
	}

	var doc ncxDoc
	if err := unmarshalXML(data, &doc); err != nil {
		return nil, err
	}

	points := doc.NavMap.Points
	// playOrder is the authoritative ordering when present; document order is
	// the fallback. Sorting is stable so ties keep document order.
	sortNavPoints(points)
	return convertNCXPoints(points, ncxPath, spineByPath), nil
}

func sortNavPoints(points []ncxPoint) {
	hasOrder := false
	for _, p := range points {
		if p.PlayOrder != "" {
			hasOrder = true
			break
		}
	}
	if hasOrder {
		sort.SliceStable(points, func(i, j int) bool {
			return atoiOrZero(points[i].PlayOrder) < atoiOrZero(points[j].PlayOrder)
		})
	}
	for i := range points {
		sortNavPoints(points[i].Children)
	}
}

func convertNCXPoints(points []ncxPoint, base string, spineByPath map[string]int) []ingest.TOCEntry {
	var out []ingest.TOCEntry
	for _, p := range points {
		title := strings.TrimSpace(p.NavLabel.Text)
		target, fragment := resolveTarget(base, p.Content.Src)

		entry := ingest.TOCEntry{
			Title:      title,
			Target:     target,
			Fragment:   fragment,
			SpineIndex: spineIndexFor(target, spineByPath),
			Children:   convertNCXPoints(p.Children, base, spineByPath),
		}
		// An entry with neither a title nor a target is noise, but one with
		// children is still a useful grouping node.
		if entry.Title == "" && entry.Target == "" && len(entry.Children) == 0 {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// ---------------------------------------------------------------------------
// EPUB 3 navigation document
// ---------------------------------------------------------------------------

type navDoc struct {
	Navs []navElement `xml:"body>nav"`
	// Some publishers wrap the nav in a section or div rather than putting it
	// directly in the body.
	Nested []navElement `xml:"body>section>nav"`
}

type navElement struct {
	Type string  `xml:"type,attr"` // epub:type
	OL   navList `xml:"ol"`
}

type navList struct {
	Items []navItem `xml:"li"`
}

type navItem struct {
	Anchor struct {
		Href string `xml:"href,attr"`
		Text string `xml:",chardata"`
	} `xml:"a"`
	Span struct {
		Text string `xml:",chardata"`
	} `xml:"span"`
	OL navList `xml:"ol"`
}

func parseNavDoc(z *ingest.SafeZip, navPath string, spineByPath map[string]int) ([]ingest.TOCEntry, error) {
	data, err := z.ReadFile(navPath)
	if err != nil {
		return nil, err
	}

	var doc navDoc
	if err := unmarshalXML(data, &doc); err != nil {
		return nil, err
	}

	all := append(append([]navElement{}, doc.Navs...), doc.Nested...)

	// epub:type="toc" is the real table of contents. The same document also
	// carries landmarks and a page list, which are not it.
	for _, nav := range all {
		if strings.Contains(nav.Type, "toc") {
			return convertNavItems(nav.OL.Items, navPath, spineByPath), nil
		}
	}
	// No typed nav: take the first one with content rather than nothing.
	for _, nav := range all {
		if len(nav.OL.Items) > 0 {
			return convertNavItems(nav.OL.Items, navPath, spineByPath), nil
		}
	}
	return nil, nil
}

func convertNavItems(items []navItem, base string, spineByPath map[string]int) []ingest.TOCEntry {
	var out []ingest.TOCEntry
	for _, li := range items {
		title := strings.TrimSpace(li.Anchor.Text)
		if title == "" {
			// A grouping node uses <span> instead of <a>.
			title = strings.TrimSpace(li.Span.Text)
		}
		target, fragment := resolveTarget(base, li.Anchor.Href)

		entry := ingest.TOCEntry{
			Title:      title,
			Target:     target,
			Fragment:   fragment,
			SpineIndex: spineIndexFor(target, spineByPath),
			Children:   convertNavItems(li.OL.Items, base, spineByPath),
		}
		if entry.Title == "" && entry.Target == "" && len(entry.Children) == 0 {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// resolveTarget splits an href into a resolved archive path and a fragment.
// An unresolvable href yields empty values rather than an error: a broken TOC
// link is a flaw in the book, not a reason to reject it.
func resolveTarget(base, href string) (target, fragment string) {
	if href == "" {
		return "", ""
	}
	if i := strings.IndexByte(href, '#'); i >= 0 {
		fragment = href[i+1:]
	}
	resolved, err := ingest.ResolveRelative(base, href)
	if err != nil {
		return "", fragment
	}
	return resolved, fragment
}

// spineIndexFor reports the spine position of a target, or -1 when the target is
// not in the spine. That happens in real books - a TOC pointing at a cover image
// or an external page - and is not an error.
func spineIndexFor(target string, spineByPath map[string]int) int {
	if target == "" {
		return -1
	}
	if idx, ok := spineByPath[target]; ok {
		return idx
	}
	return -1
}

func atoiOrZero(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func sortStrings(s []string) { sort.Strings(s) }
