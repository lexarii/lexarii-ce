// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest_test

import (
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// The three strings the real model actually produced, cut off at the schema's
// length bound. Kept verbatim rather than shortened: a rule written against a
// tidied-up example is a rule that has not met the case it is for.
func TestRunawayModelTitlesAreRejected(t *testing.T) {
	runaway := []struct {
		name  string
		title string
	}{
		{
			"a repeated fragment of the last word",
			"Algebra Essentials" + strings.Repeat("s.", 60),
		},
		{
			"a repeated separator and word",
			"Algebra Essentials" + strings.Repeat("s_Essentials", 26) + "_10101",
		},
		{
			"a repeated phrase from the title page",
			"Bayesian Statistics the Fun Way: Understanding Statistics and Probability " +
				"with Star Wars, LEGO, and Rubber Ducks® , " +
				strings.Repeat("San Francisco® , LEGO® , and Rubber Ducks® by Will Kurt® ", 3),
		},
	}

	for _, c := range runaway {
		t.Run(c.name, func(t *testing.T) {
			if ingest.PlausibleTitle(c.title) {
				t.Errorf("accepted a runaway title: %.80s...", c.title)
			}
		})
	}
}

// Titles that repeat on purpose, and ordinary long ones. The longest real title
// in the library is 298 characters of publisher subtitle, so length alone says
// nothing.
func TestRealTitlesSurviveTheRepetitionCheck(t *testing.T) {
	real := []string{
		"Bye Bye Birdie",
		"New York, New York",
		"Sing, Sing, Sing",
		"Head First Go",
		"C# 8.0 in a Nutshell",
		"Algebra Essentials",
		"Bayesian Statistics the Fun Way: Understanding Statistics and Probability with Star Wars, LEGO, and Rubber Ducks",
		"Mastering Kubernetes, Third Edition, Level up your container orchestration skills with Kubernetes to " +
			"build, run, secure, and observe large-scale distributed apps",
		"The Lord of the Rings: The Fellowship of the Ring",
		"Tensor Analysis for Engineers: Transformations-Mathematics-Applications: Second Edition",
	}

	for _, title := range real {
		if !ingest.PlausibleTitle(title) {
			t.Errorf("rejected a real title: %q", title)
		}
	}
}
