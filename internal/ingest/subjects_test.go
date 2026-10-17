// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import "testing"

func TestRedundantSubject(t *testing.T) {
	tests := []struct {
		specific, general string
		redundant         bool
		why               string
	}{
		// Real pairs from a 215-tag library.
		{"computer programming", "programming", true, "computer adds nothing"},
		{"c# programming", "c#", true, "programming adds nothing to a language"},
		{"command line utilities", "command line", true, "utilities adds nothing"},
		{"computer algorithms", "algorithms", true, "computer adds nothing"},

		// The ones that must survive: each extra word is the subject.
		{"systems programming", "programming", false, "systems programming is its own field"},
		{"abstract algebra", "algebra", false, "abstract algebra is its own field"},
		{"linear algebra", "algebra", false, "linear algebra is its own field"},
		{"applied mathematics", "mathematics", false, "applied mathematics is its own field"},
		{"advanced calculus", "calculus", false, "a level is not a content-free word"},

		// Relatedness is not sameness, which is the whole reason this is not
		// done with embeddings.
		{"deep learning", "machine learning", false, "neither contains the other"},
		{"kubernetes", "docker", false, "related, not the same"},

		// Order does not matter; containment does.
		{"programming", "computer programming", false, "the general tag cannot be the longer one"},
		{"programming", "programming", false, "a tag is not redundant with itself"},
		{"", "programming", false, "empty"},
		{"programming", "", false, "empty"},
	}

	for _, tt := range tests {
		t.Run(tt.specific+" -> "+tt.general, func(t *testing.T) {
			if got := RedundantSubject(tt.specific, tt.general); got != tt.redundant {
				t.Fatalf("RedundantSubject(%q, %q) = %v, want %v (%s)",
					tt.specific, tt.general, got, tt.redundant, tt.why)
			}
		})
	}
}
