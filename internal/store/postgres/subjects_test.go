// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// Subjects are counted over what a viewer may read, so a request with no
// viewer is refused rather than counted over everything.
func TestSubjectsNeedAViewer(t *testing.T) {
	p := testPool(t)
	got, err := p.Subjects(t.Context(), uuid.Nil)
	if !errors.Is(err, store.ErrInvalidQuery) {
		t.Fatalf("err = %v, want ErrInvalidQuery", err)
	}
	if got != nil {
		t.Fatalf("a refused request returned %+v", got)
	}
}
