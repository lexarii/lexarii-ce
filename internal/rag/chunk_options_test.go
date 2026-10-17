// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// A single "word" longer than the ceiling - a base64 blob, a minified line -
// is cut rather than stored whole and silently truncated by the embedding
// server. Nothing is lost in the cutting.
func TestSplitCutsAWordLongerThanTheCeiling(t *testing.T) {
	blob := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo", 400) // ~14k chars, no spaces
	chunks := Split(testUnits("Before the blob. "+blob+" after the blob."), Options{
		Format: domain.FormatEPUB, OverlapTokens: -1,
	})
	if len(chunks) < 2 {
		t.Fatalf("a %d-character word produced %d chunks", len(blob), len(chunks))
	}
	var joined strings.Builder
	for i, c := range chunks {
		if c.TokenCount > DefaultMaxTokens {
			t.Errorf("chunk %d is %d tokens, above the %d ceiling", i, c.TokenCount, DefaultMaxTokens)
		}
		joined.WriteString(c.Content)
	}
	if got := strings.Count(joined.String(), "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo"); got != 400 {
		t.Errorf("the blob survives as %d of 400 repeats", got)
	}
}

// Options that contradict each other are corrected rather than obeyed: a
// ceiling below the target is raised to it, and an overlap as large as the
// target is cut back so a chunk is not mostly a copy of the one before.
func TestOptionSizesAreSane(t *testing.T) {
	tests := []struct {
		name                       string
		opts                       Options
		target, ceiling, overlapTo int
	}{
		{"defaults", Options{}, DefaultTargetTokens, DefaultMaxTokens, DefaultOverlapTokens},
		{"ceiling below target", Options{TargetTokens: 300, MaxTokens: 100}, 300, 300, DefaultOverlapTokens},
		{"overlap disabled", Options{OverlapTokens: -1}, DefaultTargetTokens, DefaultMaxTokens, 0},
		{"overlap as large as the target", Options{TargetTokens: 200, OverlapTokens: 200}, 200, DefaultMaxTokens, 50},
		{"overlap set", Options{OverlapTokens: 30}, DefaultTargetTokens, DefaultMaxTokens, 30},
	}
	for _, tt := range tests {
		target, ceiling, overlap := tt.opts.sizes()
		if target != tt.target || ceiling != tt.ceiling || overlap != tt.overlapTo {
			t.Errorf("%s: sizes = %d/%d/%d, want %d/%d/%d", tt.name,
				target, ceiling, overlap, tt.target, tt.ceiling, tt.overlapTo)
		}
	}
}
