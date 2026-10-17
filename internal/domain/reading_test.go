// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"testing"
	"time"
)

func TestFurthest(t *testing.T) {
	early := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)

	tests := []struct {
		name string
		a, b ReadingProgress
		want Locator
	}{
		{
			name: "further position wins even when written earlier",
			a:    ReadingProgress{Position: NewEPUBLocator(7, "body[1]/p[2]", 0), LastReadAt: early},
			b:    ReadingProgress{Position: NewEPUBLocator(4, "body[1]/p[12]", 300), LastReadAt: late},
			want: NewEPUBLocator(7, "body[1]/p[2]", 0),
		},
		{
			name: "same spine, further offset wins",
			a:    ReadingProgress{Position: NewEPUBLocator(3, "body[1]/p[1]", 10), LastReadAt: late},
			b:    ReadingProgress{Position: NewEPUBLocator(3, "body[1]/p[1]", 900), LastReadAt: early},
			want: NewEPUBLocator(3, "body[1]/p[1]", 900),
		},
		{
			name: "pdf pages compare by page",
			a:    ReadingProgress{Position: NewPDFLocator(12, 0), LastReadAt: early},
			b:    ReadingProgress{Position: NewPDFLocator(300, 0), LastReadAt: early},
			want: NewPDFLocator(300, 0),
		},
		{
			// Locators of different formats are incomparable, so recency is the
			// only honest tiebreak.
			name: "mixed formats fall back to most recent",
			a:    ReadingProgress{Position: NewEPUBLocator(90, "body[1]/p[1]", 0), LastReadAt: early},
			b:    ReadingProgress{Position: NewPDFLocator(2, 0), LastReadAt: late},
			want: NewPDFLocator(2, 0),
		},
		{
			// Same format, but the paths name different tags at the point they
			// diverge - structurally undecided, same as a format mismatch, and
			// the same honest fallback applies: recency, not a guess from
			// CompareDecided's own char-offset tiebreak.
			name: "structurally undecided within one format falls back to most recent",
			a:    ReadingProgress{Position: NewEPUBLocator(5, "body[1]/p[3]", 900), LastReadAt: early},
			b:    ReadingProgress{Position: NewEPUBLocator(5, "body[1]/h2[1]", 0), LastReadAt: late},
			want: NewEPUBLocator(5, "body[1]/h2[1]", 0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Furthest(tt.a, tt.b); got.Position != tt.want {
				t.Fatalf("Furthest() = %s, want %s", got.Position, tt.want)
			}
			// The choice must not depend on argument order.
			if got := Furthest(tt.b, tt.a); got.Position != tt.want {
				t.Fatalf("Furthest() reversed = %s, want %s", got.Position, tt.want)
			}
		})
	}
}

func TestAnnotationOrphaned(t *testing.T) {
	a := Annotation{Type: AnnotationHighlight, SelectedText: "a loop invariant"}
	if a.Orphaned() {
		t.Fatal("fresh annotation reported as orphaned")
	}

	now := time.Now()
	a.OrphanedAt = &now
	if !a.Orphaned() {
		t.Fatal("annotation with OrphanedAt set not reported as orphaned")
	}
	// The snapshot is what the user sees once the anchor stops resolving, so it
	// must survive orphaning.
	if a.SelectedText == "" {
		t.Fatal("orphaned annotation lost its selected text")
	}
}
