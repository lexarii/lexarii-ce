// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mobi reads Kindle books: MOBI and the AZW3 that wraps one.
//
// These are shelved-but-unreadable in most self-hosted libraries, this one
// included, and the reason is usually that the format looks worse than it is.
// A MOBI is a PalmDB database: a header, a list of record offsets, and a run of
// records holding one long HTML document compressed with a small LZ77 variant
// that predates the web. Nothing here needs a dependency and nothing here needs
// CGo, which the build forbids anyway.
//
// What this does NOT do is as important. It refuses an encrypted book rather
// than producing gibberish - DRM is not supported and will not be - and it
// refuses HUFF/CDIC compression, which is a different and much larger decoder
// that the two real books this was written against do not use. Both refusals
// say which they are, because "unsupported" with no reason is the thing that
// makes somebody think their file is broken.
package mobi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// ErrEncrypted reports a book with DRM. Said plainly rather than failing to
// decompress: the file is not broken, it is locked.
var ErrEncrypted = errors.New("this book is encrypted; Lexarii does not support DRM")

// ErrCompression reports a compression scheme this decoder does not implement.
var ErrCompression = errors.New("this book uses a compression scheme Lexarii cannot read yet")

// ErrNotMOBI reports a file that is not a Kindle book at all.
var ErrNotMOBI = errors.New("not a MOBI or AZW3 file")

const (
	compressionNone     = 1
	compressionPalmDOC  = 2
	compressionHuffCDIC = 17480

	// maxText bounds the decompressed document. A Kindle book is a book; a
	// file claiming several hundred megabytes of text is either corrupt or
	// hostile, and this runs on somebody's home server.
	maxText = 64 << 20

	// maxRecords bounds the record table, which is attacker-controlled: the
	// count is two bytes, so this is generous rather than restrictive.
	maxRecords = 1 << 16

	// maxRecordBytes bounds ONE record read into memory.
	//
	// A record's length is the distance to the next offset, and both come out
	// of the file: a table declaring two records puts the whole file in the
	// second one. Without this a 2GB book - which the default upload limit
	// allows - is read whole before a single byte of it has been examined.
	// Real records are the 4096 the header asks for; 4MB is room for a cover
	// image and still nothing like a file.
	maxRecordBytes = 4 << 20

	// maxInflatedRecord bounds what ONE record may expand to.
	//
	// PalmDOC's back-references let a small record produce a much larger one,
	// so the decompressor is where a book decides how much memory it gets.
	// The accumulated ceiling is checked after a record is inflated, which is
	// far too late to matter.
	maxInflatedRecord = 1 << 20
)

// Parse reads a Kindle book into the shape the rest of ingest expects.
func Parse(r io.ReaderAt, size int64) (*ingest.Result, error) {
	db, err := openPalmDB(r, size)
	if err != nil {
		return nil, err
	}

	record0, err := db.record(0)
	if err != nil {
		return nil, fmt.Errorf("reading the header record: %w", err)
	}
	head, err := parseHeader(record0)
	if err != nil {
		return nil, err
	}

	text, err := db.text(head)
	if err != nil {
		return nil, err
	}

	out := &ingest.Result{Format: domain.FormatMOBI}
	out.Metadata = head.metadata(db.name)

	// The document is one long HTML string. Cut it where the book says its
	// pages break, because that is the only structure a MOBI6 carries - there
	// is no spine and no manifest, and a reader needs something to turn.
	sections := split(text)
	for i, section := range sections {
		out.Spine = append(out.Spine, ingest.SpineItem{
			Index:     i,
			Path:      fmt.Sprintf("section%04d.html", i),
			ID:        fmt.Sprintf("section%04d", i),
			MediaType: "application/xhtml+xml",
			Linear:    true,
		})
		out.UnitText = append(out.UnitText, ingest.TextFromHTML(section))
	}

	// A book whose records decompressed to nothing readable. Said as a warning
	// rather than an error: the metadata is still worth shelving.
	if strings.TrimSpace(strings.Join(out.UnitText, "")) == "" {
		out.Warnings = append(out.Warnings,
			"the book's text records held no readable text")
		out.NeedsOCR = true
	}
	return out, nil
}

// palmDB is the container: a name, a record table, and the file itself.
type palmDB struct {
	r       io.ReaderAt
	size    int64
	name    string
	offsets []int64
}

func openPalmDB(r io.ReaderAt, size int64) (*palmDB, error) {
	// 78 bytes of fixed header before the record table begins.
	head := make([]byte, 78)
	if _, err := r.ReadAt(head, 0); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotMOBI, err)
	}
	if string(head[60:64]) != "BOOK" || string(head[64:68]) != "MOBI" {
		return nil, fmt.Errorf("%w: type is %q/%q", ErrNotMOBI,
			string(head[60:64]), string(head[64:68]))
	}

	count := int(binary.BigEndian.Uint16(head[76:78]))
	if count == 0 || count > maxRecords {
		return nil, fmt.Errorf("%w: %d records", ErrNotMOBI, count)
	}

	table := make([]byte, 8*count)
	if _, err := r.ReadAt(table, 78); err != nil {
		return nil, fmt.Errorf("reading the record table: %w", err)
	}

	db := &palmDB{r: r, size: size, name: cString(head[:32])}
	for i := 0; i < count; i++ {
		offset := int64(binary.BigEndian.Uint32(table[8*i : 8*i+4]))
		// Offsets come from the file and are not to be trusted: one past the
		// end is a read of somebody else's memory in a language that allows it,
		// and a panic in this one.
		if offset < 0 || offset > size {
			return nil, fmt.Errorf("%w: record %d starts past the end of the file", ErrNotMOBI, i)
		}
		if i > 0 && offset < db.offsets[i-1] {
			return nil, fmt.Errorf("%w: record %d starts before record %d", ErrNotMOBI, i, i-1)
		}
		db.offsets = append(db.offsets, offset)
	}
	return db, nil
}

// record reads one record whole. A record's end is the next record's start.
func (db *palmDB) record(i int) ([]byte, error) {
	if i < 0 || i >= len(db.offsets) {
		return nil, fmt.Errorf("no record %d", i)
	}
	end := db.size
	if i+1 < len(db.offsets) {
		end = db.offsets[i+1]
	}
	length := end - db.offsets[i]
	if length <= 0 {
		return nil, nil
	}
	if length > maxRecordBytes {
		return nil, fmt.Errorf("%w: record %d claims %d bytes", ErrNotMOBI, i, length)
	}
	buf := make([]byte, length)
	if _, err := db.r.ReadAt(buf, db.offsets[i]); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf, nil
}

// text decompresses the book's text records into one HTML document.
func (db *palmDB) text(h *header) (string, error) {
	var b strings.Builder
	for i := 1; i <= h.textRecords && i < len(db.offsets); i++ {
		record, err := db.record(i)
		if err != nil {
			return "", fmt.Errorf("reading text record %d: %w", i, err)
		}

		var chunk []byte
		switch h.compression {
		case compressionNone:
			chunk = record
		case compressionPalmDOC:
			chunk = inflatePalmDOC(record)
		default:
			return "", fmt.Errorf("%w: scheme %d", ErrCompression, h.compression)
		}

		// Trailing entries - a multibyte overlap byte, an index - are appended
		// after the text and are not text. Dropping them is what keeps a stray
		// byte out of the middle of a word at every 4096-byte boundary.
		chunk = dropTrailingEntries(chunk, h.trailers, h.multibyte)

		if b.Len()+len(chunk) > maxText {
			return "", fmt.Errorf("this book claims more than %d bytes of text", maxText)
		}
		b.Write(chunk)
	}

	out := b.String()
	if h.encoding == 1252 {
		out = fromCP1252(out)
	}
	if !utf8.ValidString(out) {
		out = strings.ToValidUTF8(out, "")
	}
	return out, nil
}

// inflatePalmDOC expands one record of PalmDOC compression.
//
// A small LZ77: bytes under 0x09 are literal counts, 0x09-0x7f are their own
// character, 0x80-0xbf begin a two-byte back-reference, and 0xc0 and up are a
// space followed by one letter. Every bound is checked against what has
// already been produced, because a back-reference pointing before the start of
// the output is the obvious way to make this read memory it should not.
func inflatePalmDOC(in []byte) []byte {
	// Reserved from what the input actually is, and capped. Sizing the
	// allocation from the file's own numbers is how a two-record book asks for
	// eight gigabytes before anything has looked at it.
	reserve := len(in) * 4
	if reserve > maxInflatedRecord {
		reserve = maxInflatedRecord
	}
	out := make([]byte, 0, reserve)

	for i := 0; i < len(in) && len(out) < maxInflatedRecord; {
		c := in[i]
		i++

		switch {
		case c == 0:
			out = append(out, 0)
		case c < 0x09:
			// The next c bytes are literal.
			n := int(c)
			if i+n > len(in) {
				n = len(in) - i
			}
			out = append(out, in[i:i+n]...)
			i += n
		case c <= 0x7f:
			out = append(out, c)
		case c <= 0xbf:
			// Two bytes: 11 bits of distance, 3 bits of length.
			if i >= len(in) {
				return out
			}
			pair := int(c)<<8 | int(in[i])
			i++
			distance := (pair >> 3) & 0x07ff
			length := (pair & 0x0007) + 3
			if distance == 0 || distance > len(out) {
				// Corrupt or hostile: there is nothing that far back.
				continue
			}
			for n := 0; n < length; n++ {
				// Read one byte at a time from the output being built, because
				// a run may legitimately overlap itself.
				out = append(out, out[len(out)-distance])
			}
		default:
			out = append(out, ' ', c^0x80)
		}
	}
	return out
}

// dropTrailingEntries removes the multibyte-overlap and index entries appended
// after a record's text.
//
// Each is a backwards variable-width integer at the end of the record, which is
// why they are stripped from the back and in this order.
func dropTrailingEntries(record []byte, trailers int, multibyte bool) []byte {
	for i := 0; i < trailers; i++ {
		record = dropOneTrailer(record)
	}
	if multibyte && len(record) > 0 {
		// The last byte's low two bits say how many bytes of a character were
		// carried over from the previous record.
		n := int(record[len(record)-1]&3) + 1
		if n <= len(record) {
			record = record[:len(record)-n]
		}
	}
	return record
}

func dropOneTrailer(record []byte) []byte {
	size := 0
	for i := 0; i < 4 && i < len(record); i++ {
		b := record[len(record)-1-i]
		size = size<<7 | int(b&0x7f)
		if b&0x80 != 0 {
			break
		}
	}
	if size <= 0 || size > len(record) {
		return record
	}
	return record[:len(record)-size]
}

func cString(b []byte) string {
	if i := indexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// fromCP1252 widens the one legacy encoding these files use. Everything else
// this decoder meets is UTF-8, which needs nothing.
func fromCP1252(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x80 {
			b.WriteByte(c)
			continue
		}
		b.WriteRune(cp1252[c-0x80])
	}
	return b.String()
}

// cp1252 is the upper half of Windows-1252, whose 0x80-0x9f differ from
// Latin-1 and are exactly the characters a book uses: curly quotes and dashes.
var cp1252 = [128]rune{
	'€', '�', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '�', 'Ž', '�',
	'�', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '�', 'ž', 'Ÿ',
	' ', '¡', '¢', '£', '¤', '¥', '¦', '§', '¨', '©', 'ª', '«', '¬', '­', '®', '¯',
	'°', '±', '²', '³', '´', 'µ', '¶', '·', '¸', '¹', 'º', '»', '¼', '½', '¾', '¿',
	'À', 'Á', 'Â', 'Ã', 'Ä', 'Å', 'Æ', 'Ç', 'È', 'É', 'Ê', 'Ë', 'Ì', 'Í', 'Î', 'Ï',
	'Ð', 'Ñ', 'Ò', 'Ó', 'Ô', 'Õ', 'Ö', '×', 'Ø', 'Ù', 'Ú', 'Û', 'Ü', 'Ý', 'Þ', 'ß',
	'à', 'á', 'â', 'ã', 'ä', 'å', 'æ', 'ç', 'è', 'é', 'ê', 'ë', 'ì', 'í', 'î', 'ï',
	'ð', 'ñ', 'ò', 'ó', 'ô', 'õ', 'ö', '÷', 'ø', 'ù', 'ú', 'û', 'ü', 'ý', 'þ', 'ÿ',
}

// Sample reads the opening text of a Kindle book, bounded by maxChars.
//
// The whole document is decompressed to get it, which sounds wasteful and is
// not: the text records of these books are a few hundred kilobytes even when
// the file is 87MB, because the rest is images this never touches.
func Sample(r io.ReaderAt, size int64, maxChars int) (string, error) {
	result, err := Parse(r, size)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for _, unit := range result.UnitText {
		if b.Len() >= maxChars {
			break
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(unit)
	}

	out := b.String()
	if len(out) > maxChars {
		// Cut on a rune boundary: a half-written character in a model's
		// prompt is a character the model has to guess at.
		out = out[:maxChars]
		for len(out) > 0 && !utf8.ValidString(out) {
			out = out[:len(out)-1]
		}
	}
	return out, nil
}
