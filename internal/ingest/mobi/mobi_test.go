// SPDX-License-Identifier: AGPL-3.0-or-later

package mobi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// build assembles a minimal but real MOBI: a PalmDB header, a record table,
// record 0 with its two headers, and text records.
//
// Written out rather than checked in as a fixture, because the thing under
// test is the layout itself - a fixture would be this code's output, and two
// copies of the same misunderstanding agree with each other.
func build(t *testing.T, compression int, encryption int, records ...[]byte) []byte {
	t.Helper()

	title := "A Small Book"
	record0 := make([]byte, 0x100)
	binary.BigEndian.PutUint16(record0[0:2], uint16(compression))
	binary.BigEndian.PutUint32(record0[4:8], 4096)
	binary.BigEndian.PutUint16(record0[8:10], uint16(len(records)))
	binary.BigEndian.PutUint16(record0[10:12], 4096)
	binary.BigEndian.PutUint16(record0[12:14], uint16(encryption))
	copy(record0[16:20], "MOBI")
	binary.BigEndian.PutUint32(record0[20:24], 264)
	binary.BigEndian.PutUint32(record0[0x1c:0x20], 65001)
	// Full name, appended after the header block.
	binary.BigEndian.PutUint32(record0[0x54:0x58], uint32(len(record0)))
	binary.BigEndian.PutUint32(record0[0x58:0x5c], uint32(len(title)))
	// No EXTH, no trailing entries.
	binary.BigEndian.PutUint32(record0[0x80:0x84], 0)
	binary.BigEndian.PutUint16(record0[0xf2:0xf4], 0)
	record0 = append(record0, title...)

	all := append([][]byte{record0}, records...)

	var head bytes.Buffer
	name := make([]byte, 32)
	copy(name, "A_Small_Book")
	head.Write(name)
	head.Write(make([]byte, 28))
	head.WriteString("BOOK")
	head.WriteString("MOBI")
	head.Write(make([]byte, 8))
	_ = binary.Write(&head, binary.BigEndian, uint16(len(all)))

	offset := uint32(head.Len() + 8*len(all))
	var table bytes.Buffer
	for _, r := range all {
		_ = binary.Write(&table, binary.BigEndian, offset)
		table.Write([]byte{0, 0, 0, 0})
		offset += uint32(len(r))
	}

	out := append(head.Bytes(), table.Bytes()...)
	for _, r := range all {
		out = append(out, r...)
	}
	return out
}

func TestAnUncompressedBookReads(t *testing.T) {
	file := build(t, compressionNone, 0,
		[]byte("<h1>Chapter One</h1><p>The opening words.</p>"))

	result, err := Parse(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if result.Metadata.Title != "A Small Book" {
		t.Errorf("title = %q, want the full name rather than the PalmDB one",
			result.Metadata.Title)
	}
	text := strings.Join(result.UnitText, " ")
	if !strings.Contains(text, "The opening words.") {
		t.Errorf("text = %q, want the words of the book", text)
	}
	if strings.Contains(text, "<p>") {
		t.Error("the HTML was not parsed")
	}
}

// DRM is not supported and will not be. Saying so is the point: a file that
// fails to decompress looks broken, and it is not - it is locked.
func TestAnEncryptedBookIsRefusedBySaying_so(t *testing.T) {
	file := build(t, compressionPalmDOC, 1, []byte("anything"))

	_, err := Parse(bytes.NewReader(file), int64(len(file)))
	if !errors.Is(err, ErrEncrypted) {
		t.Fatalf("err = %v, want ErrEncrypted", err)
	}
}

func TestSomethingElseEntirelyIsNotAMOBI(t *testing.T) {
	_, err := Parse(bytes.NewReader([]byte("%PDF-1.7\nnot a kindle book at all")), 33)
	if !errors.Is(err, ErrNotMOBI) {
		t.Fatalf("err = %v, want ErrNotMOBI", err)
	}
}

// The compression is the only interesting code here, and its failure mode is
// silent: a wrong back-reference produces text that still looks like text.
func TestPalmDOCRoundTripsTheCasesThatMatter(t *testing.T) {
	tests := map[string]struct {
		in   []byte
		want string
	}{
		"plain characters": {[]byte("Hi"), "Hi"},
		// 0x09..0x7f is its own character; below 0x09 is a literal run.
		"literal run": {[]byte{0x03, 'a', 'b', 'c'}, "abc"},
		// 0xc0 and up: a space and one letter, which is how the format
		// spends one byte on the commonest pair in English.
		"space plus letter": {[]byte{0xc0 | 'x'}, " x"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := string(inflatePalmDOC(tc.in)); got != tc.want {
				t.Errorf("inflatePalmDOC(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A back-reference pointing before the start of the output is the obvious way
// to make a decompressor read memory it should not. Books are untrusted input.
func TestACorruptBackReferenceDoesNotReachBackwards(t *testing.T) {
	// distance 0x7ff, length 3, with nothing yet written to point at.
	corrupt := []byte{0xbf, 0xff}
	got := inflatePalmDOC(corrupt)
	if len(got) != 0 {
		t.Errorf("output = %q, want nothing: there is no text that far back", got)
	}
}

// A run may legitimately overlap what it is copying - that is how the format
// encodes a repeated string - so it has to be copied one byte at a time.
func TestAnOverlappingRunRepeats(t *testing.T) {
	// "ab" then a back-reference of distance 2, length 4.
	pair := (2 << 3) | (4 - 3)
	in := []byte{'a', 'b', byte(0x80 | (pair >> 8)), byte(pair & 0xff)}
	if got := string(inflatePalmDOC(in)); got != "ababab" {
		t.Errorf("got %q, want %q", got, "ababab")
	}
}

// A book is untrusted input, and this decoder sizes memory from numbers the
// book supplies. Both bounds are checked here because both were missing: a
// record's length is the distance to the next offset, so a two-record table
// puts the whole file in one record, and PalmDOC back-references let a small
// record expand into a large one.
func TestABookCannotAskForUnboundedMemory(t *testing.T) {
	t.Run("one enormous record", func(t *testing.T) {
		// A record table claiming a record far larger than any real one.
		file := build(t, compressionNone, 0, make([]byte, 8<<20))

		_, err := Parse(bytes.NewReader(file), int64(len(file)))
		if err == nil {
			t.Fatal("a record of eight megabytes was read without complaint")
		}
		if !errors.Is(err, ErrNotMOBI) {
			t.Errorf("err = %v, want ErrNotMOBI", err)
		}
	})

	t.Run("a small record that expands forever", func(t *testing.T) {
		// Back-references that copy what has already been produced: two bytes
		// of input for eight bytes of output, over and over.
		bomb := []byte{'a'}
		pair := (1 << 3) | (8 - 3) // distance 1, the longest run
		for i := 0; i < 4096; i++ {
			bomb = append(bomb, byte(0x80|(pair>>8)), byte(pair&0xff))
		}

		out := inflatePalmDOC(bomb)
		if len(out) > maxInflatedRecord+16 {
			t.Errorf("one record expanded to %d bytes, past the %d ceiling",
				len(out), maxInflatedRecord)
		}
	})
}
