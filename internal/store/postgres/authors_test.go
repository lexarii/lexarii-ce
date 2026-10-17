// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"fmt"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// authorOf credits one book to one name, by ingesting a file that declares it.
func authorOf(t *testing.T, p *Pool, owner interface{ String() string }, hash int, path, title, author string) {
	t.Helper()
	in := sampleInput(fmt.Sprintf("%064x", hash), path, title, author)
	in.Result.Metadata.Authors = []ingest.Contributor{{Name: author, SortName: author}}
	if _, err := p.UpsertBookFile(t.Context(), in); err != nil {
		t.Fatalf("seeding %q: %v", author, err)
	}
}

// Normalisation runs unattended, so what it does has to be decided by rules
// rather than by resemblance. These are the three outcomes.
func TestNormaliseAuthorsAppliesOnlyWhatTheRulesDecide(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	ctx := t.Context()

	// One person, two spellings. Nothing else connects them.
	authorOf(t, p, owner, 81, "/library/tensorone.epub", "Tensor Analysis", "Mehrzad Tabatabaian")
	authorOf(t, p, owner, 82, "/library/tensortwo.epub", "More Tensors", "M. Tabatabaian")

	// A machine account, which a rule change made unacceptable after the fact.
	if _, err := p.Exec(ctx, `INSERT INTO authors (name, sort_name) VALUES ('radha', 'radha')`); err != nil {
		t.Fatalf("seeding the imposter: %v", err)
	}

	before, err := p.NormaliseAuthors(ctx, 0.35, false)
	if err != nil {
		t.Fatalf("NormaliseAuthors(report): %v", err)
	}
	if len(before.Merged) != 1 {
		t.Fatalf("report found %d merges, want 1: %+v", len(before.Merged), before.Merged)
	}
	if before.Merged[0].KeepName != "Mehrzad Tabatabaian" {
		t.Errorf("the shorter spelling survived: %+v", before.Merged[0])
	}
	if len(before.Pruned) != 1 || before.Pruned[0].Name != "radha" {
		t.Fatalf("report found %+v, want radha", before.Pruned)
	}

	// Reporting changes nothing, which is what makes it reviewable.
	var still int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM authors`).Scan(&still); err != nil {
		t.Fatal(err)
	}
	if still != 3 {
		t.Fatalf("a report changed the catalogue: %d authors", still)
	}

	if _, err := p.NormaliseAuthors(ctx, 0.35, true); err != nil {
		t.Fatalf("NormaliseAuthors(apply): %v", err)
	}

	var names []string
	rows, err := p.Query(ctx, `SELECT name FROM authors ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	if len(names) != 1 || names[0] != "Mehrzad Tabatabaian" {
		t.Fatalf("authors after normalising = %v, want just the full spelling", names)
	}

	// The merge is not lossy: the spelling that lost is searchable as an alias.
	var aliases []string
	if err := p.QueryRow(ctx,
		`SELECT aliases FROM authors WHERE name = 'Mehrzad Tabatabaian'`).Scan(&aliases); err != nil {
		t.Fatalf("reading aliases: %v", err)
	}
	var kept bool
	for _, a := range aliases {
		if a == "M. Tabatabaian" {
			kept = true
		}
	}
	if !kept {
		t.Errorf("the merged spelling was not kept as an alias: %v", aliases)
	}

	// And it is idempotent, which matters for something on a timer.
	second, err := p.NormaliseAuthors(ctx, 0.35, true)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Merged) != 0 || len(second.Pruned) != 0 {
		t.Fatalf("a second pass found more to do: %+v", second)
	}
}

// Two people who wrote a book together are never one person, however alike
// their names. This is the case that makes automatic merging safe, and the one
// that would destroy two bibliographies if it were wrong.
func TestNormaliseAuthorsNeverMergesCoAuthors(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	_ = testOwner(t, p)
	ctx := t.Context()

	in := sampleInput(fmt.Sprintf("%064x", 83), "/library/headfirstsomething.epub",
		"Head First Something", "Dawn Griffiths")
	in.Result.Metadata.Authors = []ingest.Contributor{
		{Name: "Dawn Griffiths", SortName: "Griffiths, Dawn"},
		{Name: "David Griffiths", SortName: "Griffiths, David"},
	}
	if _, err := p.UpsertBookFile(ctx, in); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	out, err := p.NormaliseAuthors(ctx, 0.35, true)
	if err != nil {
		t.Fatalf("NormaliseAuthors: %v", err)
	}
	if len(out.Merged) != 0 {
		t.Fatalf("co-authors were merged: %+v", out.Merged)
	}

	var n int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM authors WHERE name LIKE '%Griffiths'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("%d Griffiths left, want 2", n)
	}
}
