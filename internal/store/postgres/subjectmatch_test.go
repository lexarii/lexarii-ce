// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// subjectLibrary is a small shelf with the shape that matters: several books
// about one thing, and one book whose tag merely contains a common word.
func subjectLibrary(t *testing.T, p *Pool) {
	t.Helper()
	cleanLibrary(t, p)

	userID, err := p.EnsureDefaultUser(t.Context())
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}

	books := []struct {
		seed, title string
		subjects    []string
	}{
		{"a", "Docker Cookbook", []string{"docker", "containerization"}},
		{"b", "Docker for Developers", []string{"docker", "devops"}},
		{"c", "A History of Mathematics", []string{"mathematical history", "mathematics"}},
	}
	for _, b := range books {
		in := sampleInput(hashOf(b.seed), "/library/"+b.seed+".epub", b.title, "An Author")
		in.OwnerID = userID
		in.Result.Metadata.Subjects = b.subjects
		in.Result.Metadata.Provenance = map[string]domain.MetadataSource{
			"title": domain.SourceEmbedded, "subjects": domain.SourceEmbedded,
		}
		if _, err := p.UpsertBookFile(t.Context(), in); err != nil {
			t.Fatalf("UpsertBookFile %s: %v", b.title, err)
		}
	}
}

// A subject the library has: the shelf comes back.
func TestASubjectTheLibraryHasIsMatched(t *testing.T) {
	p := testPool(t)
	subjectLibrary(t, p)

	matches, err := p.MatchingSubjects(t.Context(), []string{"docker"})
	if err != nil {
		t.Fatalf("MatchingSubjects: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("a subject this library is full of matched nothing")
	}
	if matches[0].Slug != "docker" {
		t.Errorf("best match = %q, want docker", matches[0].Slug)
	}
}

// A subject it does not have, whose words happen to appear in one unrelated
// tag. This is the medieval case: "history" is in "mathematical-history", one
// book about the history of maths, and it came back as a reading list for
// medieval Europe.
//
// Relevance cannot catch it and points the wrong way - a word found in exactly
// one tag scores the HIGHEST relevance there is, because rarity is what
// relevance measures. What separates the cases is how much of the question the
// library can answer at all.
func TestASubjectTheLibraryLacksMatchesNothing(t *testing.T) {
	p := testPool(t)
	subjectLibrary(t, p)

	matches, err := p.MatchingSubjects(t.Context(),
		[]string{"medieval history", "european history", "middle ages"})
	if err != nil {
		t.Fatalf("MatchingSubjects: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("a library with nothing medieval offered %+v", matches)
	}
}

// And a question the library only PARTLY covers still answers, or the rule
// would be a refusal to answer anything imprecise.
func TestAPartlyCoveredSubjectStillMatches(t *testing.T) {
	p := testPool(t)
	subjectLibrary(t, p)

	// Two words, one of which this library knows well.
	matches, err := p.MatchingSubjects(t.Context(), []string{"docker containers"})
	if err != nil {
		t.Fatalf("MatchingSubjects: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("a question half of which the library knows matched nothing")
	}
}

func TestNoSubjectsMatchNothing(t *testing.T) {
	p := testPool(t)
	subjectLibrary(t, p)

	matches, err := p.MatchingSubjects(t.Context(), nil)
	if err != nil {
		t.Fatalf("MatchingSubjects: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("asking for no subjects returned %+v", matches)
	}
}
