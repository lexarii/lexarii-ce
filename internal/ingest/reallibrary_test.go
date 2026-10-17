// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// The titles a real library actually produced, good and bad.
//
// PlausibleTitle guards against the model as much as against the file, and the
// only honest test of that is the output a real model produced for real books.
// Kept as a file rather than as a list in the test so it can grow when
// somebody meets a new way for a title to be wrong.
const realTitles = "testdata/real-titles.txt"

func TestPlausibleTitleAgainstARealLibrary(t *testing.T) {
	file, err := os.Open(realTitles)
	if err != nil {
		t.Skipf("no sample of real titles: %v", err)
	}
	defer func() { _ = file.Close() }()

	var wrongly, missed []string
	scanner := bufio.NewScanner(file)
	// A dumped title page is longer than the default token.
	scanner.Buffer(make([]byte, 0, 64<<10), 64<<10)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// "BAD<tab>the title", or the title alone for one that is fine.
		verdict, title, tabbed := strings.Cut(line, "\t")
		if !tabbed {
			title = verdict
			verdict = "OK"
		}

		switch plausible := PlausibleTitle(title); {
		case verdict == "OK" && !plausible:
			wrongly = append(wrongly, title)
		case verdict == "BAD" && plausible:
			missed = append(missed, title)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading %s: %v", realTitles, err)
	}

	// Rejecting a real title is the worse failure: the book goes on the shelf
	// as "Untitled" and the person has to find it by guessing.
	for _, title := range wrongly {
		t.Errorf("a real title was rejected: %q", truncate(title))
	}
	for _, title := range missed {
		t.Errorf("a title page was accepted as a title: %q", truncate(title))
	}
}

func truncate(s string) string {
	if len([]rune(s)) <= 80 {
		return s
	}
	return string([]rune(s)[:80]) + "…"
}
