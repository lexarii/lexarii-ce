// SPDX-License-Identifier: AGPL-3.0-or-later

// Package epub parses EPUB 2 and EPUB 3 files.
//
// EPUB is the priority format for Lexarii. Parsing follows the spec path:
// META-INF/container.xml names the OPF package document, the OPF carries the
// metadata and the manifest, and the OPF spine is the reading order.
//
// The spine is the reading order. The table of contents is not: it is
// navigation, it is frequently partial, and in real books it is sometimes out
// of order or points at documents outside the spine entirely.
package epub

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// ErrNotEPUB reports a file that is not a usable EPUB.
var ErrNotEPUB = errors.New("not a valid epub")

const containerPath = "META-INF/container.xml"

// Parse reads an EPUB and returns its structure and claimed metadata.
//
// Parsing is deterministic: the same bytes always produce the same spine
// indices and the same locators. Downstream chunk ids, highlight anchors, and
// chat citations all depend on that, so nothing here may depend on map
// iteration order or on the filesystem.
func Parse(r io.ReaderAt, size int64) (*ingest.Result, error) {
	z, skipped, err := ingest.OpenSafeZip(r, size)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotEPUB, err)
	}

	res := &ingest.Result{Format: domain.FormatEPUB}
	if skipped > 0 {
		res.AddWarning("%d archive entries were skipped as unsafe or oversized", skipped)
	}

	opfPath, err := findOPFPath(z)
	if err != nil {
		return nil, err
	}

	opfData, err := z.ReadFile(opfPath)
	if err != nil {
		return nil, fmt.Errorf("%w: reading package document %s: %v", ErrNotEPUB, opfPath, err)
	}

	var pkg opfPackage
	if err := unmarshalXML(opfData, &pkg); err != nil {
		return nil, fmt.Errorf("%w: parsing package document: %v", ErrNotEPUB, err)
	}

	res.Metadata = buildMetadata(pkg, res)

	manifest := indexManifest(pkg, opfPath, res)
	res.Spine = buildSpine(pkg, manifest, res)
	if len(res.Spine) == 0 {
		return nil, fmt.Errorf("%w: package document has no readable spine", ErrNotEPUB)
	}

	res.TOC = buildTOC(z, pkg, manifest, res.Spine, opfPath, res)
	res.Cover = findCover(z, pkg, manifest, res)

	return res, nil
}

// ---------------------------------------------------------------------------
// container.xml
// ---------------------------------------------------------------------------

type containerXML struct {
	Rootfiles []struct {
		FullPath  string `xml:"full-path,attr"`
		MediaType string `xml:"media-type,attr"`
	} `xml:"rootfiles>rootfile"`
}

func findOPFPath(z *ingest.SafeZip) (string, error) {
	data, err := z.ReadFile(containerPath)
	if err != nil {
		return "", fmt.Errorf("%w: missing %s", ErrNotEPUB, containerPath)
	}

	var c containerXML
	if err := unmarshalXML(data, &c); err != nil {
		return "", fmt.Errorf("%w: parsing %s: %v", ErrNotEPUB, containerPath, err)
	}

	const opfMediaType = "application/oebps-package+xml"
	// Prefer a correctly declared rootfile, but accept the first one when the
	// media type is missing or wrong - plenty of real EPUBs get it wrong and are
	// otherwise perfectly readable.
	var fallback string
	for _, rf := range c.Rootfiles {
		if rf.FullPath == "" {
			continue
		}
		// full-path is relative to the ARCHIVE ROOT, not to the directory
		// holding container.xml. Resolving it relative to META-INF/ yields
		// META-INF/OEBPS/content.opf and finds nothing.
		clean, err := ingest.SanitizePath(rf.FullPath)
		if err != nil {
			continue
		}
		if !z.Has(clean) {
			continue
		}
		if rf.MediaType == opfMediaType {
			return clean, nil
		}
		if fallback == "" {
			fallback = clean
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("%w: %s names no usable package document", ErrNotEPUB, containerPath)
}

// ---------------------------------------------------------------------------
// OPF package document
// ---------------------------------------------------------------------------

type opfPackage struct {
	Version  string `xml:"version,attr"`
	UniqueID string `xml:"unique-identifier,attr"`

	Metadata struct {
		Titles []struct {
			Value string `xml:",chardata"`
			ID    string `xml:"id,attr"`
		} `xml:"title"`
		Creators []struct {
			Value  string `xml:",chardata"`
			ID     string `xml:"id,attr"`
			Role   string `xml:"role,attr"`    // EPUB 2 opf:role
			FileAs string `xml:"file-as,attr"` // EPUB 2 opf:file-as
		} `xml:"creator"`
		Contributors []struct {
			Value  string `xml:",chardata"`
			Role   string `xml:"role,attr"`
			FileAs string `xml:"file-as,attr"`
		} `xml:"contributor"`
		Languages    []string `xml:"language"`
		Publishers   []string `xml:"publisher"`
		Dates        []string `xml:"date"`
		Subjects     []string `xml:"subject"`
		Descriptions []string `xml:"description"`
		Identifiers  []struct {
			Value  string `xml:",chardata"`
			ID     string `xml:"id,attr"`
			Scheme string `xml:"scheme,attr"` // EPUB 2 opf:scheme
		} `xml:"identifier"`
		// EPUB 3 refinements and EPUB 2 <meta name= content=> both land here.
		Metas []struct {
			Name     string `xml:"name,attr"`
			Content  string `xml:"content,attr"`
			Property string `xml:"property,attr"`
			Refines  string `xml:"refines,attr"`
			Value    string `xml:",chardata"`
		} `xml:"meta"`
	} `xml:"metadata"`

	Manifest struct {
		Items []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			MediaType  string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"item"`
	} `xml:"manifest"`

	Spine struct {
		TOC      string `xml:"toc,attr"` // EPUB 2: manifest id of the NCX
		ItemRefs []struct {
			IDRef  string `xml:"idref,attr"`
			Linear string `xml:"linear,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
}

// manifestItem is a resolved manifest entry.
type manifestItem struct {
	ID         string
	Path       string
	MediaType  string
	Properties []string
}

func (m manifestItem) hasProperty(p string) bool {
	for _, got := range m.Properties {
		if got == p {
			return true
		}
	}
	return false
}

func indexManifest(pkg opfPackage, opfPath string, res *ingest.Result) map[string]manifestItem {
	items := make(map[string]manifestItem, len(pkg.Manifest.Items))
	for _, it := range pkg.Manifest.Items {
		if it.ID == "" || it.Href == "" {
			continue
		}
		resolved, err := ingest.ResolveRelative(opfPath, it.Href)
		if err != nil {
			res.AddWarning("manifest item %q has an unusable href %q", it.ID, it.Href)
			continue
		}
		items[it.ID] = manifestItem{
			ID:         it.ID,
			Path:       resolved,
			MediaType:  it.MediaType,
			Properties: strings.Fields(it.Properties),
		}
	}
	return items
}

func buildSpine(pkg opfPackage, manifest map[string]manifestItem, res *ingest.Result) []ingest.SpineItem {
	spine := make([]ingest.SpineItem, 0, len(pkg.Spine.ItemRefs))
	for _, ref := range pkg.Spine.ItemRefs {
		item, ok := manifest[ref.IDRef]
		if !ok {
			// A dangling idref would shift every later index if we silently
			// skipped it without saying so. Record it; the indices that follow
			// are still stable for this file because parsing is deterministic.
			res.AddWarning("spine references unknown manifest id %q", ref.IDRef)
			continue
		}
		spine = append(spine, ingest.SpineItem{
			Index:     len(spine),
			Path:      item.Path,
			ID:        item.ID,
			MediaType: item.MediaType,
			// linear="no" marks supplementary content. It keeps its spine slot:
			// removing it would renumber every locator after it.
			Linear: !strings.EqualFold(ref.Linear, "no"),
		})
	}
	return spine
}

// ---------------------------------------------------------------------------
// Metadata
// ---------------------------------------------------------------------------

func buildMetadata(pkg opfPackage, res *ingest.Result) ingest.Metadata {
	var m ingest.Metadata

	// EPUB 3 refinements attach to an element by id: <meta refines="#t1"
	// property="title-type">subtitle</meta>. Index them so titles and names can
	// be interpreted correctly.
	refines := make(map[string]map[string]string)
	for _, meta := range pkg.Metadata.Metas {
		if meta.Refines == "" || meta.Property == "" {
			continue
		}
		id := strings.TrimPrefix(meta.Refines, "#")
		if refines[id] == nil {
			refines[id] = make(map[string]string)
		}
		refines[id][meta.Property] = strings.TrimSpace(meta.Value)
	}

	for _, t := range pkg.Metadata.Titles {
		value := strings.TrimSpace(t.Value)
		if value == "" {
			continue
		}
		switch refines[t.ID]["title-type"] {
		case "subtitle":
			if m.Subtitle == "" {
				m.Subtitle = value
				m.SetProvenance("subtitle", domain.SourceEmbedded)
			}
		default:
			if m.Title == "" {
				if ingest.PlausibleTitle(value) {
					m.Title = value
					m.SetProvenance("title", domain.SourceEmbedded)
				} else {
					res.AddWarning("ignoring implausible embedded title %q", value)
				}
			}
		}
	}

	for _, c := range pkg.Metadata.Creators {
		name := strings.TrimSpace(c.Value)
		if name == "" {
			continue
		}
		sortName := strings.TrimSpace(c.FileAs)
		if sortName == "" {
			sortName = refines[c.ID]["file-as"]
		}
		role := c.Role
		if role == "" {
			role = refines[c.ID]["role"]
		}
		// One <dc:creator> often credits the whole writing team. Splitting it
		// is what stops "Kevlin Henney and Trisha Gee" becoming an author.
		people := ingest.SplitContributors(name)
		for _, person := range people {
			// A file-as describes the element as written. Once the element has
			// been split it no longer describes any one of the parts, so it is
			// dropped and the sort name derived per person instead.
			as := sortName
			if len(people) > 1 {
				as = ""
			}
			m.Authors = append(m.Authors, ingest.Contributor{
				Name:     person,
				SortName: as,
				Role:     marcRole(role),
			})
		}
	}
	if len(m.Authors) > 0 {
		m.SetProvenance("authors", domain.SourceEmbedded)
	}

	if v := firstNonEmpty(pkg.Metadata.Languages); v != "" {
		m.Language = v
		m.SetProvenance("language", domain.SourceEmbedded)
	}
	if v := firstNonEmpty(pkg.Metadata.Publishers); v != "" {
		m.Publisher = v
		m.SetProvenance("publisher", domain.SourceEmbedded)
	}
	if v := firstNonEmpty(pkg.Metadata.Dates); v != "" {
		// Kept as claimed. Publication dates in the wild are years, full dates,
		// ranges, and outright junk; normalising here would silently invent
		// precision the file never had.
		m.PublishedOn = v
		m.SetProvenance("publishedOn", domain.SourceEmbedded)
	}
	if v := firstNonEmpty(pkg.Metadata.Descriptions); v != "" {
		m.Description = v
		m.SetProvenance("description", domain.SourceEmbedded)
	}

	for _, s := range pkg.Metadata.Subjects {
		if s = strings.TrimSpace(s); s != "" {
			m.Subjects = append(m.Subjects, s)
		}
	}
	if len(m.Subjects) > 0 {
		m.SetProvenance("subjects", domain.SourceEmbedded)
	}

	for _, id := range pkg.Metadata.Identifiers {
		isbn := normaliseISBN(id.Value)
		switch len(isbn) {
		case 13:
			if m.ISBN13 == "" {
				m.ISBN13 = isbn
				m.SetProvenance("isbn13", domain.SourceEmbedded)
			}
		case 10:
			if m.ISBN10 == "" {
				m.ISBN10 = isbn
				m.SetProvenance("isbn10", domain.SourceEmbedded)
			}
		}
	}

	// Series is not part of the EPUB spec. Calibre's convention is the de facto
	// standard and is what the overwhelming majority of real files use.
	for _, meta := range pkg.Metadata.Metas {
		switch {
		case meta.Name == "calibre:series" && meta.Content != "":
			m.Series = strings.TrimSpace(meta.Content)
			m.SetProvenance("series", domain.SourceEmbedded)
		case meta.Name == "calibre:series_index" && meta.Content != "":
			if f, err := strconv.ParseFloat(strings.TrimSpace(meta.Content), 64); err == nil {
				m.SeriesIndex = &f
				m.SetProvenance("seriesIndex", domain.SourceEmbedded)
			}
		case meta.Property == "belongs-to-collection" && meta.Value != "":
			if m.Series == "" {
				m.Series = strings.TrimSpace(meta.Value)
				m.SetProvenance("series", domain.SourceEmbedded)
			}
		}
	}

	if m.Title == "" {
		// Deliberately not derived from the filename. An empty title is honest
		// and enrichment can fill it later; a guess written as embedded metadata
		// would outrank the truth forever.
		res.AddWarning("package document declares no title")
	}

	return m
}

// marcRole maps a MARC relator code to a domain role. Unknown codes are authors,
// which is what an unmarked dc:creator means.
func marcRole(code string) domain.AuthorRole {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "edt":
		return domain.RoleEditor
	case "trl":
		return domain.RoleTranslator
	case "ill":
		return domain.RoleIllustrator
	default:
		return domain.RoleAuthor
	}
}

// normaliseISBN strips prefixes and separators, returning the bare digits.
func normaliseISBN(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(strings.ToLower(s), "urn:isbn:")
	s = strings.TrimPrefix(s, "isbn:")
	s = strings.TrimPrefix(s, "isbn")

	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == 'X' || r == 'x':
			b.WriteRune('X')
		}
	}
	out := b.String()
	if len(out) != 10 && len(out) != 13 {
		return ""
	}
	// Only the final character of an ISBN-10 may be X.
	if strings.Contains(out[:len(out)-1], "X") {
		return ""
	}
	return out
}

func firstNonEmpty(values []string) string {
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Cover
// ---------------------------------------------------------------------------

func findCover(z *ingest.SafeZip, pkg opfPackage, manifest map[string]manifestItem, res *ingest.Result) *ingest.Cover {
	// EPUB 3: the manifest item carries properties="cover-image".
	for _, id := range sortedManifestIDs(manifest) {
		if manifest[id].hasProperty("cover-image") {
			return readCover(z, manifest[id], res)
		}
	}

	// EPUB 2: <meta name="cover" content="manifest-id">.
	for _, meta := range pkg.Metadata.Metas {
		if meta.Name == "cover" && meta.Content != "" {
			if item, ok := manifest[meta.Content]; ok {
				return readCover(z, item, res)
			}
		}
	}

	// Last resort: a manifest image whose id or path says "cover". Deliberately
	// last, because it is a guess and the two paths above are declarations.
	for _, id := range sortedManifestIDs(manifest) {
		item := manifest[id]
		if !strings.HasPrefix(item.MediaType, "image/") {
			continue
		}
		if strings.Contains(strings.ToLower(item.ID), "cover") ||
			strings.Contains(strings.ToLower(path.Base(item.Path)), "cover") {
			return readCover(z, item, res)
		}
	}

	return nil
}

func readCover(z *ingest.SafeZip, item manifestItem, res *ingest.Result) *ingest.Cover {
	data, err := z.ReadFile(item.Path)
	if err != nil {
		res.AddWarning("cover image %s could not be read: %v", item.Path, err)
		return nil
	}
	return &ingest.Cover{Path: item.Path, MediaType: item.MediaType, Data: data}
}

// sortedManifestIDs gives deterministic iteration over the manifest. Map order
// in Go is randomised, and a cover that changes between runs would change the
// stored cover path on every re-ingest.
func sortedManifestIDs(manifest map[string]manifestItem) []string {
	ids := make([]string, 0, len(manifest))
	for id := range manifest {
		ids = append(ids, id)
	}
	sortStrings(ids)
	return ids
}

// ---------------------------------------------------------------------------
// XML
// ---------------------------------------------------------------------------

// unmarshalXML parses XML from an untrusted book.
//
// Go's encoding/xml does not resolve external entities at all, so XXE is not
// reachable here the way it is in C or Java parsers. What it does do is reject
// undefined entities, and real EPUBs are full of HTML entities like &nbsp;, so
// the HTML entity table is supplied. Strict is left on: a malformed package
// document should fail loudly against this one file rather than be half-read.
func unmarshalXML(data []byte, v any) error {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = true
	dec.Entity = xml.HTMLEntity
	// Books in the wild declare encodings Go does not know. Without a charset
	// reader those fail outright; returning the bytes unchanged treats them as
	// UTF-8, which is right far more often than it is wrong.
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) {
		return input, nil
	}
	return dec.Decode(v)
}
