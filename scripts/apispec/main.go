// SPDX-License-Identifier: AGPL-3.0-or-later

// Command apispec generates the component schemas of api/openapi.yaml from the
// Go wire types in internal/http/dto.
//
// The spec is supposed to be the source of truth for the HTTP contract, and for
// most of this project's life it described one endpoint out of forty-six while
// fifty hand-written TypeScript interfaces drifted alongside it. Hand-copying
// Go structs into YAML is what caused that: it is dull, it is invisible when
// wrong, and nothing fails when someone forgets.
//
// So the schemas are not written by hand. They are read out of the Go source -
// field names from the json tags, optionality from omitempty and pointers,
// descriptions from the doc comments, which is why this parses the AST rather
// than using reflection. The PATHS are still hand-written, in api/paths.yaml,
// because what an endpoint means is not derivable from a struct.
//
//	make api-spec     # regenerate api/openapi.yaml
//	make api-types    # regenerate the TypeScript from it
//
// Run both after changing anything in internal/http/dto.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"os"
	"sort"
	"strings"

	"scm.poniatowski.dev/adam/lexarii/scripts/wiretypes"
)

func main() {
	var (
		dtoDir = flag.String("dto", "internal/http/dto", "package holding the wire types")
		paths  = flag.String("paths", "api/paths.yaml", "hand-written paths document")
		out    = flag.String("out", "api/openapi.yaml", "spec to write")
		check  = flag.Bool("check", false, "fail if the spec on disk is out of date")
	)
	flag.Parse()

	generated, err := generate(*dtoDir, *paths)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apispec:", err)
		os.Exit(1)
	}

	if *check {
		current, err := os.ReadFile(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "apispec:", err)
			os.Exit(1)
		}
		if !bytes.Equal(current, generated) {
			fmt.Fprintf(os.Stderr,
				"apispec: %s is out of date; run `make api-spec`\n", *out)
			os.Exit(1)
		}
		fmt.Printf("%s is up to date\n", *out)
		return
	}

	if err := os.WriteFile(*out, generated, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "apispec:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", *out)
}

func generate(dtoDir, pathsFile string) ([]byte, error) {
	types, err := wiretypes.Read(dtoDir)
	if err != nil {
		return nil, err
	}
	if len(types) == 0 {
		return nil, fmt.Errorf("no exported struct types found in %s", dtoDir)
	}

	head, err := os.ReadFile(pathsFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", pathsFile, err)
	}
	if err := checkForRepeatedPaths(head); err != nil {
		return nil, err
	}

	var b bytes.Buffer
	b.WriteString("# GENERATED FILE - DO NOT EDIT.\n")
	b.WriteString("#\n")
	b.WriteString("# Paths come from api/paths.yaml; component schemas are read out of the Go\n")
	b.WriteString("# types in internal/http/dto. Edit one of those and run `make api-spec`.\n")
	b.Write(head)
	if !bytes.HasSuffix(head, []byte("\n")) {
		b.WriteString("\n")
	}

	b.WriteString("\ncomponents:\n  schemas:\n")
	names := make([]string, 0, len(types))
	for name := range types {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writeSchema(&b, types[name], types)
	}

	b.WriteString(`
  securitySchemes:
    sessionCookie:
      type: apiKey
      in: cookie
      name: lexarii_session
      description: |
        Set by /auth/login, /auth/setup, or a federated callback. Every endpoint
        requires it except /health and the /auth endpoints that establish it.
        This is how a BROWSER authenticates: the cookie is HttpOnly, so no
        script on the page can read the token.
    bearerToken:
      type: http
      scheme: bearer
      description: |
        How a client that is not a browser authenticates. /auth/token returns
        a token, the client stores it and sends it as an Authorization header
        of the form "Bearer <token>" on every request.

        It is the same kind of token against the same sessions as the cookie -
        one scheme with two ways of carrying it, not two authentication
        systems - so a token can be listed and revoked beside browser sessions.
        When a request carries both, the header wins: a header is a deliberate
        act by the caller and a cookie is attached by the browser whether or
        not anybody meant it.

  responses:
    NotFound:
      description: No such resource
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/Error'
    Unauthorized:
      description: Not signed in
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/Error'
    Forbidden:
      description: |
        Signed in, but not allowed. Returned with the error code not_an_admin
        by the endpoints that change the shared library.
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/Error'
    BadRequest:
      description: The request was malformed or failed validation
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/Error'
`)
	return b.Bytes(), nil
}

// writeSchema writes one schema. A named string type with constants - the
// project's enums - arrives here too, with Values set and Fields empty.
func writeSchema(b *bytes.Buffer, t wiretypes.Struct, types map[string]wiretypes.Struct) {
	fmt.Fprintf(b, "    %s:\n", t.Name)

	// A named string type is an enum, not an object.
	if len(t.Fields) == 0 {
		fmt.Fprintf(b, "      type: string\n")
		if len(t.Values) > 0 {
			fmt.Fprintf(b, "      enum: [%s]\n", strings.Join(t.Values, ", "))
		}
		if t.Doc != "" {
			fmt.Fprintf(b, "      description: %s\n", quote(t.Doc))
		}
		return
	}

	fmt.Fprintf(b, "      type: object\n")
	if t.Doc != "" {
		fmt.Fprintf(b, "      description: %s\n", quote(t.Doc))
	}

	// Embedded structs are spliced in where they stand, because that is what
	// Go does when it marshals one.
	fields := wiretypes.Flatten(t.Fields, types, map[string]bool{t.Name: true}, func(name string) {
		// Embedding a type the spec does not describe would produce a schema
		// missing whatever that type contributes - which is exactly the
		// silence this is here to prevent.
		fmt.Fprintf(os.Stderr, "apispec: a wire type embeds %s, which is not one\n", name)
		os.Exit(1)
	})

	var required []string
	for _, f := range fields {
		if f.Required {
			required = append(required, f.JSON)
		}
	}
	if len(required) > 0 {
		fmt.Fprintf(b, "      required: [%s]\n", strings.Join(required, ", "))
	}

	fmt.Fprintf(b, "      properties:\n")
	for _, f := range fields {
		var shape bytes.Buffer
		writeType(&shape, f.Type, "          ")

		// A field whose Go type is `any` or json.RawMessage has no shape to
		// describe, and a property with neither a type nor a description is not
		// valid YAML. An empty schema says "anything", which is the truth.
		if shape.Len() == 0 && f.Doc == "" {
			fmt.Fprintf(b, "        %s: {}\n", f.JSON)
			continue
		}

		fmt.Fprintf(b, "        %s:\n", f.JSON)
		b.Write(shape.Bytes())
		if f.Doc != "" {
			fmt.Fprintf(b, "          description: %s\n", quote(f.Doc))
		}
	}
}

// flatten splices an embedded struct's fields in where it stands.
//
// writeType maps a Go type onto its JSON Schema shape.
func writeType(b *bytes.Buffer, expr ast.Expr, indent string) {
	switch t := expr.(type) {
	case *ast.StarExpr:
		// A pointer is the same shape, allowed to be null.
		writeType(b, t.X, indent)

	case *ast.ArrayType:
		fmt.Fprintf(b, "%stype: array\n%sitems:\n", indent, indent)
		writeType(b, t.Elt, indent+"  ")

	case *ast.MapType:
		fmt.Fprintf(b, "%stype: object\n%sadditionalProperties:\n", indent, indent)
		writeType(b, t.Value, indent+"  ")

	case *ast.SelectorExpr:
		// The only qualified types on the wire are time.Time and
		// json.RawMessage, and both have a settled representation.
		switch t.Sel.Name {
		case "Time":
			fmt.Fprintf(b, "%stype: string\n%sformat: date-time\n", indent, indent)
		case "RawMessage":
			// Arbitrary JSON: no shape to state.
		default:
			fmt.Fprintf(b, "%s$ref: '#/components/schemas/%s'\n", indent, t.Sel.Name)
		}

	case *ast.InterfaceType:
		// Arbitrary JSON.

	case *ast.Ident:
		switch t.Name {
		case "string":
			fmt.Fprintf(b, "%stype: string\n", indent)
		case "bool":
			fmt.Fprintf(b, "%stype: boolean\n", indent)
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64":
			fmt.Fprintf(b, "%stype: integer\n", indent)
		case "float32", "float64":
			fmt.Fprintf(b, "%stype: number\n", indent)
		case "any":
			// Arbitrary JSON.
		default:
			// Another type in the same package.
			fmt.Fprintf(b, "%s$ref: '#/components/schemas/%s'\n", indent, t.Name)
		}

	default:
		fmt.Fprintf(b, "%s# unmapped Go type\n", indent)
	}
}

// quote renders a description as a YAML double-quoted scalar.
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// checkForRepeatedPaths refuses a document that names one path twice.
//
// YAML has no opinion about a repeated key: the last one silently wins and the
// first disappears. Adding PATCH /works/{id} as a second entry did exactly
// that to the GET that had documented it since M1, and nothing complained -
// the spec regenerated, the linter passed, and one endpoint had quietly
// stopped existing on paper.
//
// Matched by line rather than by parsing, because a YAML library is a
// dependency and this is the only thing that would be asked of it.
func checkForRepeatedPaths(doc []byte) error {
	seen := map[string]int{}
	for i, line := range strings.Split(string(doc), "\n") {
		// A path entry is indented exactly two spaces and starts with a slash.
		if !strings.HasPrefix(line, "  /") || strings.HasPrefix(line, "   ") {
			continue
		}
		key, _, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		if first, repeated := seen[key]; repeated {
			return fmt.Errorf("%s is documented twice, on lines %d and %d; "+
				"the second silently replaces the first - put both methods under one key",
				key, first, i+1)
		}
		seen[key] = i + 1
	}
	return nil
}
