// SPDX-License-Identifier: AGPL-3.0-or-later

package mobi

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"regexp"
	"strconv"
	"strings"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// header is what record 0 says about the book.
type header struct {
	compression int
	textRecords int
	encoding    int
	fullName    string
	exth        map[int][]string

	// trailers is how many variable-width entries are appended to each text
	// record after the text, and multibyte says whether a character carried
	// over from the previous record is appended too.
	trailers  int
	multibyte bool
}

func parseHeader(record0 []byte) (*header, error) {
	// PalmDOC header: 16 bytes, then the MOBI header.
	if len(record0) < 16 {
		return nil, fmt.Errorf("%w: the header record is %d bytes", ErrNotMOBI, len(record0))
	}

	h := &header{
		compression: int(binary.BigEndian.Uint16(record0[0:2])),
		textRecords: int(binary.BigEndian.Uint16(record0[8:10])),
		encoding:    65001,
		exth:        map[int][]string{},
	}
	if encryption := int(binary.BigEndian.Uint16(record0[12:14])); encryption != 0 {
		return nil, fmt.Errorf("%w (scheme %d)", ErrEncrypted, encryption)
	}
	if h.compression == compressionHuffCDIC {
		return nil, fmt.Errorf("%w: HUFF/CDIC", ErrCompression)
	}

	// A PalmDOC with no MOBI header is still readable: it is just text.
	if len(record0) < 24 || string(record0[16:20]) != "MOBI" {
		return h, nil
	}

	mobiLen := int(binary.BigEndian.Uint32(record0[20:24]))
	if mobiLen <= 0 || 16+mobiLen > len(record0) {
		mobiLen = len(record0) - 16
	}
	if len(record0) >= 0x24 {
		h.encoding = int(binary.BigEndian.Uint32(record0[0x1c:0x20]))
	}

	// Every offset below counts from the start of RECORD 0, not from the start
	// of the MOBI header, which is how the format is documented and is the one
	// thing about it that is easy to get wrong. Reading them 16 bytes further
	// on gave a full-name offset of 0 and a length of 6, and the book was
	// shelved under the truncated PalmDB name - "97 Things Ev-er Should Know".

	// The full title. The PalmDB name is cut to 31 characters with the spaces
	// replaced, so it is a last resort rather than an answer.
	if len(record0) >= 0x5c {
		offset := int(binary.BigEndian.Uint32(record0[0x54:0x58]))
		length := int(binary.BigEndian.Uint32(record0[0x58:0x5c]))
		if offset > 0 && length > 0 && offset+length <= len(record0) {
			h.fullName = strings.TrimSpace(string(record0[offset : offset+length]))
		}
	}

	// Trailing entries per text record. Each set bit above the first means one
	// more variable-width entry appended after the text; bit 0 means a
	// multibyte character carried over from the previous record. Both of the
	// real books this was written against answer 3: one trailer, and an
	// overlap byte.
	if len(record0) >= 0xf4 {
		flags := binary.BigEndian.Uint16(record0[0xf2:0xf4])
		h.multibyte = flags&1 != 0
		h.trailers = bits.OnesCount16(flags >> 1)
	}

	// EXTH: the metadata a publisher actually filled in.
	if len(record0) >= 0x84 && binary.BigEndian.Uint32(record0[0x80:0x84])&0x40 != 0 {
		h.readEXTH(record0[16+mobiLen:])
	}
	return h, nil
}

// readEXTH reads the tag/value records that follow the MOBI header.
func (h *header) readEXTH(b []byte) {
	if len(b) < 12 || string(b[0:4]) != "EXTH" {
		return
	}
	count := int(binary.BigEndian.Uint32(b[8:12]))
	if count < 0 || count > 1024 {
		return
	}

	at := 12
	for i := 0; i < count && at+8 <= len(b); i++ {
		tag := int(binary.BigEndian.Uint32(b[at : at+4]))
		size := int(binary.BigEndian.Uint32(b[at+4 : at+8]))
		if size < 8 || at+size > len(b) {
			return
		}
		value := strings.TrimSpace(string(b[at+8 : at+size]))
		if value != "" {
			h.exth[tag] = append(h.exth[tag], value)
		}
		at += size
	}
}

// The EXTH tags worth reading. There are dozens; these are the ones that
// correspond to something this library stores.
const (
	exthAuthor       = 100
	exthPublisher    = 101
	exthDescription  = 103
	exthISBN         = 104
	exthSubject      = 105
	exthPublished    = 106
	exthUpdatedTitle = 503
)

// metadata turns what the header knows into what ingest stores.
func (h *header) metadata(palmName string) ingest.Metadata {
	m := ingest.Metadata{Provenance: map[string]domain.MetadataSource{}}

	// The title, best first: a publisher's corrected title, then the full name
	// from the header, then the PalmDB name - which is truncated to 31
	// characters with the spaces replaced, and is a last resort rather than an
	// answer.
	for _, candidate := range []string{
		first(h.exth[exthUpdatedTitle]),
		h.fullName,
		strings.ReplaceAll(palmName, "_", " "),
	} {
		if title := strings.TrimSpace(candidate); title != "" && ingest.PlausibleTitle(title) {
			m.Title = title
			m.Provenance["title"] = domain.SourceEmbedded
			break
		}
	}

	for _, name := range h.exth[exthAuthor] {
		// A single EXTH author record sometimes holds the whole byline.
		for _, one := range splitAuthors(name) {
			m.Authors = append(m.Authors, ingest.Contributor{Name: one, Role: domain.RoleAuthor})
		}
	}
	if len(m.Authors) > 0 {
		m.Provenance["authors"] = domain.SourceEmbedded
	}

	if publisher := first(h.exth[exthPublisher]); publisher != "" {
		m.Publisher = publisher
		m.Provenance["publisher"] = domain.SourceEmbedded
	}
	if description := first(h.exth[exthDescription]); description != "" {
		// The description is HTML often enough to matter, and a blurb full of
		// <p> tags is not a blurb.
		m.Description = ingest.TextFromHTML(description)
		m.Provenance["description"] = domain.SourceEmbedded
	}
	if isbn := digitsOnly(first(h.exth[exthISBN])); isbn != "" {
		switch len(isbn) {
		case 13:
			m.ISBN13 = isbn
			m.Provenance["isbn13"] = domain.SourceEmbedded
		case 10:
			m.ISBN10 = isbn
			m.Provenance["isbn10"] = domain.SourceEmbedded
		}
	}
	if subjects := h.exth[exthSubject]; len(subjects) > 0 {
		m.Subjects = subjects
		m.Provenance["subjects"] = domain.SourceEmbedded
	}
	if published := first(h.exth[exthPublished]); published != "" {
		if year := yearIn(published); year != "" {
			m.PublishedOn = year
			m.Provenance["publishedOn"] = domain.SourceEmbedded
		}
	}
	return m
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// splitAuthors separates a byline that arrived as one string.
//
// Not on a comma, which is how "Kernighan, Brian" is written and would become
// two people. Only on the separators that can only be separators.
func splitAuthors(s string) []string {
	parts := regexp.MustCompile(`\s*(?:;|&| and )\s*`).Split(s, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// yearIn finds a four-digit year in a publication date written any of the ways
// a publisher writes them.
func yearIn(s string) string {
	for i := 0; i+4 <= len(s); i++ {
		chunk := s[i : i+4]
		if year, err := strconv.Atoi(chunk); err == nil && year >= 1400 && year <= 2200 {
			return chunk
		}
	}
	return ""
}

// pageBreak is how a MOBI6 says a page ends. It is the only structure the
// format carries: there is no spine and no manifest.
var pageBreak = regexp.MustCompile(`(?i)<mbp:pagebreak\s*/?>`)

// split cuts the document into reading-order sections.
//
// A MOBI is one HTML document, and a reader needs something to turn. Page
// breaks first, because that is the book's own answer. Where a book has none -
// or so few that a section would be a hundred pages - it is cut by size, which
// is arbitrary and says so.
func split(document string) []string {
	sections := trimEmpty(pageBreak.Split(document, -1))

	// Sections this large are unreadable in a browser and useless as a
	// locator: "somewhere in this 400KB" is not a position.
	const maxSection = 48 << 10
	var out []string
	for _, section := range sections {
		for len(section) > maxSection {
			cut := breakNear(section, maxSection)
			out = append(out, section[:cut])
			section = section[cut:]
		}
		out = append(out, section)
	}

	out = trimEmpty(out)
	if len(out) == 0 {
		return []string{document}
	}
	return out
}

// breakNear finds a tag boundary near the target, so a cut never lands inside
// a tag and turns the rest of the section into visible angle brackets.
func breakNear(s string, target int) int {
	if target >= len(s) {
		return len(s)
	}
	if i := strings.LastIndex(s[:target], "><"); i > target/2 {
		return i + 1
	}
	if i := strings.LastIndex(s[:target], " "); i > target/2 {
		return i
	}
	return target
}

func trimEmpty(sections []string) []string {
	out := make([]string, 0, len(sections))
	for _, section := range sections {
		if strings.TrimSpace(section) != "" {
			out = append(out, section)
		}
	}
	return out
}
