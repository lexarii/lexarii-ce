// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"regexp"
	"strconv"
)

// citationPattern matches a bracketed passage number: [1], [12].
//
// Deliberately strict. Books are full of brackets - "[sic]", "[1]" as the
// book's own reference, "[0..n]" in a code sample - and a loose pattern would
// find citations in the quoted material rather than in the answer about it.
var citationPattern = regexp.MustCompile(`\[(\d{1,3})\]`)

// CitedNumbers returns the passage numbers an answer referred to, in the order
// they first appear, ignoring anything outside the range that was offered.
//
// Out-of-range markers are dropped rather than reported: a model citing [17]
// when it was given ten passages has made something up, and the honest handling
// is for that citation not to exist. A number that pointed at nothing would be
// worse than none, because the reader would follow it.
func CitedNumbers(answer string, available int) []int {
	if available <= 0 {
		return nil
	}

	var out []int
	seen := make(map[int]bool)

	for _, match := range citationPattern.FindAllStringSubmatch(answer, -1) {
		n, err := strconv.Atoi(match[1])
		if err != nil || n < 1 || n > available {
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// InventedCitations returns markers that point at passages which were never
// offered.
//
// Worth surfacing rather than only discarding: a model inventing citations is a
// sign the answer is drifting away from its sources, and that is exactly what
// this whole design exists to make visible.
func InventedCitations(answer string, available int) []int {
	var out []int
	seen := make(map[int]bool)

	for _, match := range citationPattern.FindAllStringSubmatch(answer, -1) {
		n, err := strconv.Atoi(match[1])
		if err != nil || (n >= 1 && n <= available) {
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
