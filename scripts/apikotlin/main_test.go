// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes a throwaway dto package and returns the Kotlin generated from
// it, so the mapping rules are tested against source written for the purpose
// rather than against whatever the real dto package happens to contain today.
func fixture(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dto.go"), []byte(source), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	out, err := generate(dir, "com.lexarii.app.api")
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	return string(out)
}

func mustContain(t *testing.T, kotlin string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(kotlin, want) {
			t.Errorf("generated Kotlin is missing:\n  %s\n--- got ---\n%s", want, kotlin)
		}
	}
}

// Optionality is the whole reason this reads the Go rather than the spec: a
// pointer or omitempty may be absent, and everything else is always there.
// Getting it backwards gives a client that either crashes on a missing field
// or makes every field nullable and useless.
func TestOptionalityComesFromPointersAndOmitempty(t *testing.T) {
	kotlin := fixture(t, `package dto

type Thing struct {
	Always   string  `+"`json:\"always\"`"+`
	Pointer  *string `+"`json:\"pointer\"`"+`
	Omitted  string  `+"`json:\"omitted,omitempty\"`"+`
}
`)
	mustContain(t, kotlin,
		"val always: String,",
		"val pointer: String? = null,",
		"val omitted: String? = null",
	)
}

// A pointer inside a list is still a list of present things; the nullability
// belongs to the field, not to the element.
func TestAPointerElementDoesNotMakeTheListNullable(t *testing.T) {
	kotlin := fixture(t, `package dto

type Thing struct {
	Items []*Other `+"`json:\"items\"`"+`
}

type Other struct {
	Name string `+"`json:\"name\"`"+`
}
`)
	mustContain(t, kotlin, "val items: List<Other>")
	if strings.Contains(kotlin, "List<Other?>") {
		t.Error("a pointer element produced List<Other?>")
	}
}

// Go embeds its fields into the outer object when it marshals, so the Kotlin
// has to declare them. Skipping one silently is how a client comes to believe
// a response holds less than it does.
func TestEmbeddedStructsAreSplicedIn(t *testing.T) {
	kotlin := fixture(t, `package dto

type Page struct {
	Cursor string `+"`json:\"cursor\"`"+`
}

type Listing struct {
	Page
	Items []string `+"`json:\"items\"`"+`
}
`)
	mustContain(t, kotlin, "data class Listing(", "val cursor: String,", "val items: List<String>")
}

// A named string type with constants is an enum on both sides. The wire value
// is preserved with @SerialName, because the Kotlin constant is spelled
// differently.
func TestNamedStringTypesBecomeEnums(t *testing.T) {
	kotlin := fixture(t, `package dto

// Status is how an upload ended.
type Status string

const (
	StatusStored   Status = "stored"
	StatusRejected Status = "rejected"
)
`)
	mustContain(t, kotlin,
		"enum class Status {",
		`@SerialName("stored")`,
		"STORED,",
		`@SerialName("rejected")`,
		"REJECTED",
	)
}

// A named string type with no constants is not an enum - it is a string, and
// pretending otherwise would reject values the server legitimately sends.
func TestANamedStringWithNoConstantsIsAnAlias(t *testing.T) {
	kotlin := fixture(t, `package dto

// Locator addresses a position in a book.
type Locator string
`)
	mustContain(t, kotlin, "typealias Locator = String")
}

// Sizes and counts of bytes genuinely exceed 2^31 on a real library, so int64
// must not land as Int.
func TestNumbersMapToTypesThatCanHoldThem(t *testing.T) {
	kotlin := fixture(t, `package dto

type Thing struct {
	Count int     `+"`json:\"count\"`"+`
	Size  int64   `+"`json:\"size\"`"+`
	Ratio float64 `+"`json:\"ratio\"`"+`
	Ok    bool    `+"`json:\"ok\"`"+`
}
`)
	mustContain(t, kotlin,
		"val count: Int,",
		"val size: Long,",
		"val ratio: Double,",
		"val ok: Boolean",
	)
}

// Arbitrary JSON has no shape to state, and JsonElement is the type that
// decodes anything rather than failing.
func TestArbitraryJSONBecomesJsonElement(t *testing.T) {
	kotlin := fixture(t, `package dto

import "encoding/json"

type Thing struct {
	Raw  json.RawMessage `+"`json:\"raw\"`"+`
	Free any             `+"`json:\"free\"`"+`
}
`)
	mustContain(t, kotlin, "val raw: JsonElement,", "val free: JsonElement")
}

// time.Time is RFC3339 text on the wire.
func TestTimestampsAreStrings(t *testing.T) {
	kotlin := fixture(t, `package dto

import "time"

type Thing struct {
	CreatedAt time.Time `+"`json:\"createdAt\"`"+`
}
`)
	mustContain(t, kotlin, "val createdAt: String")
}

// A json tag that happens to be a Kotlin keyword has to be escaped, or the
// generated file does not compile at all.
func TestKotlinKeywordsAreEscaped(t *testing.T) {
	kotlin := fixture(t, `package dto

type Thing struct {
	Object string `+"`json:\"object\"`"+`
	In     string `+"`json:\"in\"`"+`
	Normal string `+"`json:\"normal\"`"+`
}
`)
	mustContain(t, kotlin,
		"val `object`: String,",
		`@SerialName("object")`,
		"val `in`: String,",
		"val normal: String",
	)
	// The ordinary name needs no annotation; adding one everywhere would be
	// noise that hides the cases that matter.
	if strings.Contains(kotlin, `@SerialName("normal")`) {
		t.Error("a name that needs no @SerialName got one")
	}
}

// A field the server omits from JSON must not appear at all.
func TestSkippedFieldsAreNotGenerated(t *testing.T) {
	kotlin := fixture(t, `package dto

type Thing struct {
	Kept   string `+"`json:\"kept\"`"+`
	Hidden string `+"`json:\"-\"`"+`
}
`)
	mustContain(t, kotlin, "val kept: String")
	if strings.Contains(kotlin, "Hidden") || strings.Contains(kotlin, "hidden") {
		t.Error("a json:\"-\" field was generated")
	}
}

// F-Droid rebuilds a tagged commit and compares the bytes, so a generated file
// whose order came from map iteration would make the build unreproducible.
func TestOutputIsDeterministic(t *testing.T) {
	source := `package dto

type Alpha struct {
	A string ` + "`json:\"a\"`" + `
}

type Beta struct {
	B string ` + "`json:\"b\"`" + `
}

type Gamma struct {
	C string ` + "`json:\"c\"`" + `
}
`
	first := fixture(t, source)
	for i := 0; i < 8; i++ {
		if again := fixture(t, source); again != first {
			t.Fatal("two runs of the same source produced different Kotlin")
		}
	}
	// And in a stable order, which is what makes the diff of a real change
	// readable.
	alpha, beta, gamma := strings.Index(first, "class Alpha"), strings.Index(first, "class Beta"), strings.Index(first, "class Gamma")
	if alpha >= beta || beta >= gamma {
		t.Errorf("types are not in sorted order: Alpha@%d Beta@%d Gamma@%d", alpha, beta, gamma)
	}
}

// The real dto package is the one that ships. This is a smoke test that it
// generates at all and produces the header the file is recognised by.
func TestTheRealWireTypesGenerate(t *testing.T) {
	out, err := generate("../../internal/http/dto", "com.lexarii.app.api")
	if err != nil {
		t.Fatalf("generating from the real dto package: %v", err)
	}
	kotlin := string(out)
	mustContain(t, kotlin,
		"SPDX-License-Identifier: AGPL-3.0-or-later",
		"DO NOT EDIT",
		"package com.lexarii.app.api",
		"import kotlinx.serialization.Serializable",
	)
	if n := strings.Count(kotlin, "\ndata class "); n < 40 {
		t.Errorf("only %d data classes generated from the real wire types", n)
	}
}
