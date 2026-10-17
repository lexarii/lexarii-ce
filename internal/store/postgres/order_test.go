package postgres

import (
	"fmt"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// Whichever file is processed first creates the Work. With parallel workers
// that order is arbitrary, so the merged book's metadata must not depend on it.
func TestMergedMetadataDoesNotDependOnIngestOrder(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()

	poor := func(hash string) IngestInput {
		in := sampleInput(hash, "/library/done/algebraessentials.pdf", "Untitled", "")
		in.Format = domain.FormatPDF
		in.Result.Metadata = sampleInput(hash, "", "Untitled", "").Result.Metadata
		in.Result.Metadata.Title = "Untitled"
		in.Result.Metadata.Authors = nil
		in.Result.Metadata.Publisher = ""
		in.Result.Metadata.Subjects = nil
		in.Result.Metadata.Provenance = nil
		return in
	}
	rich := func(hash string) IngestInput {
		in := sampleInput(hash, "/library/algebraessentials.epub", "Algebra Essentials", "Olgha B. Davis")
		in.Result.Metadata.ISBN13 = "9781937585341"
		in.Result.Metadata.Publisher = "Mercury Learning"
		in.Result.Metadata.Provenance["isbn13"] = domain.SourceEmbedded
		return in
	}

	for _, order := range []struct {
		name  string
		first func(string) IngestInput
		secnd func(string) IngestInput
	}{
		{"rich first", rich, poor},
		{"poor first", poor, rich},
	} {
		t.Run(order.name, func(t *testing.T) {
			cleanLibrary(t, p)
			if _, err := p.UpsertBookFile(ctx, order.first(fmt.Sprintf("%064x", 1))); err != nil {
				t.Fatalf("first: %v", err)
			}
			if _, err := p.UpsertBookFile(ctx, order.secnd(fmt.Sprintf("%064x", 2))); err != nil {
				t.Fatalf("second: %v", err)
			}

			var title string
			var isbn, publisher *string
			if err := p.QueryRow(ctx,
				`SELECT w.title, e.isbn13, e.publisher FROM works w JOIN editions e ON e.work_id = w.id`,
			).Scan(&title, &isbn, &publisher); err != nil {
				t.Fatalf("reading: %v", err)
			}
			t.Logf("title=%q isbn=%v publisher=%v", title, deref(isbn), deref(publisher))

			if title != "Algebra Essentials" {
				t.Errorf("title = %q, want the real title regardless of ingest order", title)
			}
			if deref(isbn) != "9781937585341" {
				t.Errorf("isbn = %q, want it preserved regardless of ingest order", deref(isbn))
			}
		})
	}
}
