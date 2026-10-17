// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"reflect"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// Getting this wrong shifts every locator in a book by one, silently
// corrupting every highlight and citation.
func TestMajorForIsOneBasedOnlyForPDF(t *testing.T) {
	tests := []struct {
		format domain.Format
		unit   int
		want   int
	}{
		{domain.FormatPDF, 0, 1},
		{domain.FormatPDF, 41, 42},
		{domain.FormatEPUB, 0, 0},
		{domain.FormatEPUB, 7, 7},
		{domain.FormatMOBI, 3, 3},
	}
	for _, tt := range tests {
		r := &Result{Format: tt.format}
		if got := r.MajorFor(tt.unit); got != tt.want {
			t.Errorf("%s MajorFor(%d) = %d, want %d", tt.format, tt.unit, got, tt.want)
		}
	}
}

func TestAddWarningRecordsInOrder(t *testing.T) {
	var r Result
	r.AddWarning("skipped %d unsafe archive entries", 2)
	r.AddWarning("no table of contents")

	want := []string{"skipped 2 unsafe archive entries", "no table of contents"}
	if !reflect.DeepEqual(r.Warnings, want) {
		t.Errorf("Warnings = %q, want %q", r.Warnings, want)
	}
}

// Provenance must be recordable on a zero Metadata: parsers build these up
// field by field and the first SetProvenance is the one that needs the map.
func TestSetProvenanceOnZeroMetadata(t *testing.T) {
	var m Metadata
	m.SetProvenance("title", domain.SourceEmbedded)
	m.SetProvenance("authors", domain.SourceFilename)
	m.SetProvenance("title", domain.SourceFilename)

	want := map[string]domain.MetadataSource{
		"title":   domain.SourceFilename,
		"authors": domain.SourceFilename,
	}
	if !reflect.DeepEqual(m.Provenance, want) {
		t.Errorf("Provenance = %v, want %v", m.Provenance, want)
	}
}
