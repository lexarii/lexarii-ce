// SPDX-License-Identifier: AGPL-3.0-or-later

package mobi

import (
	"os"
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// Real Kindle books, when there are any to hand.
//
// A format decoder written against a specification and never pointed at a file
// somebody actually owns is a decoder that works on the specification. Set
// LEXARII_MOBI_SAMPLES to a colon-separated list of paths to run this.
func TestRealBooks(t *testing.T) {
	paths := strings.Split(os.Getenv("LEXARII_MOBI_SAMPLES"), ":")
	if len(paths) == 0 || paths[0] == "" {
		t.Skip("LEXARII_MOBI_SAMPLES not set")
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Skipf("cannot open: %v", err)
			}
			defer func() { _ = f.Close() }()
			info, err := f.Stat()
			if err != nil {
				t.Fatalf("stat: %v", err)
			}

			result, err := Parse(f, info.Size())
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if result.Metadata.Title == "" {
				t.Error("no title")
			}
			if len(result.Spine) == 0 {
				t.Fatal("no sections, so there is nothing to read")
			}

			text := strings.Join(result.UnitText, "\n")
			if len(text) < 1000 {
				t.Fatalf("only %d characters of text came out of a whole book", len(text))
			}
			// Closing tags, not opening ones. A Java book says "run javap -c
			// <class file>" and means it literally; "</p>" is never prose, so
			// finding one means the HTML was never parsed - which would poison
			// every chunk and every answer built from one.
			for _, tag := range []string{"</p>", "</div>", "</span>", "<br/>"} {
				if strings.Contains(text, tag) {
					t.Errorf("%s survived into the extracted text", tag)
				}
			}

			t.Logf("title=%q authors=%v publisher=%q isbn=%q%s",
				result.Metadata.Title, authorNames(result.Metadata),
				result.Metadata.Publisher, result.Metadata.ISBN13,
				"")
			t.Logf("sections=%d text=%dKB", len(result.Spine), len(text)/1024)
			t.Logf("opening: %.200q", strings.TrimSpace(text))
		})
	}
}

func authorNames(m ingest.Metadata) []string {
	var out []string
	for _, a := range m.Authors {
		out = append(out, a.Name)
	}
	return out
}
