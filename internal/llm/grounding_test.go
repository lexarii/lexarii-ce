// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import "testing"

// The page a model is shown, roughly as an extractor hands it over: broken
// lines, a ligature lost, spacing that does not survive.
const titlePage = `
Kotlin Cookbook
A Problem-Focused Approach

Ken Kousen

O'Reilly Media, Inc.
Copyright © 2020
`

func TestAValueOnThePageIsGrounded(t *testing.T) {
	for _, value := range []string{
		"Kotlin Cookbook",
		// Title and subtitle joined, which is what a model usually answers.
		"Kotlin Cookbook: A Problem-Focused Approach",
		// Case and punctuation are not evidence of anything.
		"KOTLIN COOKBOOK",
		"kotlin cookbook - a problem focused approach",
		"Ken Kousen",
	} {
		if !Grounded(titlePage, value) {
			t.Errorf("Grounded(page, %q) = false; it is printed on the page", value)
		}
	}
}

func TestAValueFromNowhereIsNotGrounded(t *testing.T) {
	for _, value := range []string{
		"Java Cookbook",
		"Programming Rust",
		// The filename, which is where a model reaches when the page gives it
		// nothing.
		"kotlin_cookbook_2nd_ed_final_v3",
	} {
		if Grounded(titlePage, value) {
			t.Errorf("Grounded(page, %q) = true; none of that is on the page", value)
		}
	}
}

// A hyphen that became a line break, a ligature the extractor dropped: a true
// answer must survive an imperfect extraction, because the books with the
// worst extraction are the ones enrichment exists for.
func TestAnImperfectExtractionStillGrounds(t *testing.T) {
	broken := "E ective Modern C++\n42 SPECIFIC WAYS TO IMPROVE\nYOUR USE OF C++11 AND C++14\nScott Meyers"
	if !Grounded(broken, "Effective Modern C++: 42 Specific Ways to Improve Your Use of C++11 and C++14") {
		t.Error("a correct title was called invented because the extractor lost a ligature")
	}
}

func TestNothingIsNotGrounded(t *testing.T) {
	if Grounded(titlePage, "") {
		t.Error("an empty value was called grounded")
	}
	if Grounded("", "Kotlin Cookbook") {
		t.Error("a value was called grounded against no text at all")
	}
	if Grounded(titlePage, "   ") {
		t.Error("whitespace was called grounded")
	}
}

// Short words are in every book ever printed and carry no evidence.
func TestCommonWordsAreNotEvidence(t *testing.T) {
	page := "The Art of the And of the In of the On"
	if Grounded(page, "The Art of War") {
		t.Error("a title was grounded on nothing but its articles")
	}
}
