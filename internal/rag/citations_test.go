// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

func TestCitedNumbers(t *testing.T) {
	tests := []struct {
		name      string
		answer    string
		available int
		want      []int
	}{
		{"one", "A monoid is a set with an associative operation [1].", 5, []int{1}},
		{"several", "It is defined here [1] and used there [3].", 5, []int{1, 3}},
		{"adjacent", "Both books agree [1][3].", 5, []int{1, 3}},
		{"in order of first appearance", "See [3], then [1], then [3] again.", 5, []int{3, 1}},
		{"two digits", "As set out in [12].", 20, []int{12}},
		{"none", "The passages I found do not answer this.", 5, nil},

		// A model citing [17] when it was given ten passages has made something
		// up. A citation that points at nothing is worse than none, because the
		// reader would follow it.
		{"out of range", "As shown in [17].", 10, nil},
		{"zero", "As shown in [0].", 10, nil},
		{"mixed", "Real [2] and invented [99].", 10, []int{2}},

		// Books are full of brackets of their own.
		{"not a citation", "The array is indexed [i] and the note says [sic].", 5, nil},
		{"no passages offered", "Something [1].", 0, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CitedNumbers(tt.answer, tt.available); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CitedNumbers(%q, %d) = %v, want %v", tt.answer, tt.available, got, tt.want)
			}
		})
	}
}

// A model inventing citations is a sign the answer is drifting from its
// sources, which is exactly what this design exists to make visible.
func TestInventedCitations(t *testing.T) {
	got := InventedCitations("Real [2], invented [99], invented again [99], and [12].", 10)
	want := []int{99, 12}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InventedCitations = %v, want %v", got, want)
	}

	if got := InventedCitations("All real [1][2].", 5); got != nil {
		t.Errorf("InventedCitations on a clean answer = %v", got)
	}
}

// Silence is not an answer. A personal library is finite, and "you have nothing
// on this" is a real answer to a real question.
func TestNothingFoundIsSaidPlainly(t *testing.T) {
	if NothingFound == "" {
		t.Fatal("the empty-result answer is empty")
	}
	for _, word := range []string{"library", "nothing"} {
		if !strings.Contains(strings.ToLower(NothingFound), word) {
			t.Errorf("the empty-result answer does not mention %q: %q", word, NothingFound)
		}
	}
	// It must not read as an error. A person seeing it should understand the
	// library was searched and came up empty.
	for _, word := range []string{"error", "failed", "sorry"} {
		if strings.Contains(strings.ToLower(NothingFound), word) {
			t.Errorf("the empty-result answer reads as a failure: %q", NothingFound)
		}
	}
}

// Every notation a model has been seen to cite in becomes the one the rest of
// the system reads, and nothing else changes.
func TestNormaliseCitations(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain markers are left alone", "As shown [1] and [12].", "As shown [1] and [12]."},
		{"lenticular brackets", "Victor flees 【1】.", "Victor flees [1]."},
		{"lenticular with a dagger and a line range", "He abandons it 【3†L10-L20】.", "He abandons it [3]."},
		{"lenticular with a dagger and a word", "See 【2†source】.", "See [2]."},
		{"lenticular with spaces inside", "See 【 4 】 and 【5 †L1】.", "See [4] and [5]."},
		{"three digits", "See 【120†L1】.", "See [120]."},
		{"ASCII with a dagger", "See [1†L10-L20].", "See [1]."},
		{"adjacent lenticular", "Both say so 【1】【2】.", "Both say so [1][2]."},
		{"adjacent with daggers", "Both 【1†L1-L2】【2†L8】 and [3†a][4†b].", "Both [1][2] and [3][4]."},
		{"adjacent plain stays as it is", "Both [1][2].", "Both [1][2]."},
		{"mixed in one answer", "One [1], two 【2】, three [3†L4].", "One [1], two [2], three [3]."},
		{"at the very start and end", "【1】 begins, ends 【2†x】", "[1] begins, ends [2]"},
		{"ASCII marker inside lenticular brackets", "urges him to begone【[1]】.", "urges him to begone[1]."},
		{"wrapped markers side by side", "Both 【[1]】【[2]】 and 【 [3] †L4】.", "Both [1][2] and [3]."},

		// Not markers, and so not touched.
		{"lenticular around words", "The heading 【Note】 stays.", "The heading 【Note】 stays."},
		{"lenticular with four digits", "A year 【1818】.", "A year 【1818】."},
		{"ASCII without a dagger", "An index [i] and [sic].", "An index [i] and [sic]."},
		{"a dagger in prose", "The obituary marks him † 1851 [2].", "The obituary marks him † 1851 [2]."},
		{"an unclosed bracket", "Text 【1†L10 and on", "Text 【1†L10 and on"},
		{"a marker cannot span a line", "【1†L1\nL2】", "【1†L1\nL2】"},
		{"a suffix too long to be a locator", "【1†" + strings.Repeat("x", 41) + "】", "【1†" + strings.Repeat("x", 41) + "】"},
		{"a suffix exactly as long as allowed", "【1†" + strings.Repeat("x", 40) + "】", "[1]"},
		{"mismatched brackets", "【1] and [2】", "【1] and [2】"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormaliseCitations(tt.in); got != tt.want {
				t.Errorf("NormaliseCitations(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Normalised markers are what CitedNumbers reads, so the sources are linked.
func TestNormalisedCitationsAreCited(t *testing.T) {
	answer := NormaliseCitations("He flees 【2†L10-L20】, then returns 【1】【2】 and [3†L4].")
	if got, want := CitedNumbers(answer, 5), []int{2, 1, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("CitedNumbers = %v, want %v", got, want)
	}
}

// However a streamed answer is cut into pieces - including through the middle
// of a marker, and through the middle of a multi-byte bracket - the pieces it
// emits join to exactly the normalised whole.
func TestCitationStreamAgreesWithTheWholeAnswer(t *testing.T) {
	answers := []string{
		"Victor flees 【1†L10-L20】 and the creature 【2】【3】 is left [4†L1]. End [5].",
		"An array [i] is not a citation, nor is 【Note】, but 【12】 is.",
		"Wrapped 【[1]】 and 【[2]】【[3]】, then plain [4] and 【[5】 too.",
		"Unclosed at the end 【1†L4",
		"A line break 【1\n】 and a long one 【2†" + strings.Repeat("y", 60) + "】 then 【3】",
		"",
	}
	for _, answer := range answers {
		want := NormaliseCitations(answer)

		// Every two-piece split, at every byte.
		for cut := 0; cut <= len(answer); cut++ {
			if got := streamed(answer[:cut], answer[cut:]); got != want {
				t.Fatalf("split at byte %d of %q:\n got %q\nwant %q", cut, answer, got, want)
			}
		}
		// One byte at a time, the worst a server could do.
		pieces := make([]string, 0, len(answer))
		for i := range len(answer) {
			pieces = append(pieces, answer[i:i+1])
		}
		if got := streamed(pieces...); got != want {
			t.Fatalf("byte by byte %q:\n got %q\nwant %q", answer, got, want)
		}
	}
}

// Text with no brackets in it is passed on as it arrives, not held.
func TestCitationStreamDoesNotHoldPlainText(t *testing.T) {
	var got []string
	s := newCitationStream(func(p string) { got = append(got, p) })
	s.write("Closures ")
	s.write("capture [1]. ")
	s.write("Then 【")
	if want := []string{"Closures ", "capture [1]. ", "Then "}; !reflect.DeepEqual(got, want) {
		t.Fatalf("emitted %q, want %q", got, want)
	}
	s.write("2†L4】.")
	s.flush()
	if want := []string{"Closures ", "capture [1]. ", "Then ", "[2]."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("emitted %q, want %q", got, want)
	}
}

func streamed(pieces ...string) string {
	var b strings.Builder
	s := newCitationStream(func(p string) {
		if p == "" {
			panic("an empty piece was emitted")
		}
		b.WriteString(p)
	})
	for _, p := range pieces {
		s.write(p)
	}
	s.flush()
	return b.String()
}

// Random answers built from the pieces markers are made of, split at random,
// still stream to exactly the normalised whole. Seeded, so a failure repeats.
func TestCitationStreamRandomSplits(t *testing.T) {
	parts := []string{"【", "】", "[", "]", "†", "1", "23", " ", "\n", "L4", "x", "é", "【[1]】", "[2†L1]", "【3】"}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		var b strings.Builder
		for range rng.IntN(12) {
			b.WriteString(parts[rng.IntN(len(parts))])
		}
		answer := b.String()
		var pieces []string
		for rest := answer; rest != ""; {
			n := min(1+rng.IntN(6), len(rest))
			pieces = append(pieces, rest[:n])
			rest = rest[n:]
		}
		if got, want := streamed(pieces...), NormaliseCitations(answer); got != want {
			t.Fatalf("pieces %q:\n got %q\nwant %q", pieces, got, want)
		}
	}
}
