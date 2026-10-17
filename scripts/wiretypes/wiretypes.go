// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wiretypes reads the Go wire types in internal/http/dto.
//
// It exists because there is now more than one thing generated from those
// structs - the OpenAPI schemas, and the Kotlin the Android client is built
// from - and the reading of them is the half that must not differ. Two copies
// of "which fields are optional" is two answers to that question, and the one
// that is wrong is silent: a client that thinks a field is always present, on
// a server that omits it.
//
// What each generator does with the result is its own business, so only the
// PARSING lives here. Mapping a Go type onto a JSON Schema or onto a Kotlin
// declaration is a different job in each case and stays with its generator.
package wiretypes

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// Struct is one exported wire type.
type Struct struct {
	Name   string
	Doc    string
	Fields []Field
	// Values are the declared constants of a named string type, which become a
	// JSON Schema enum and a Kotlin enum. Without this a field typed
	// UploadStatus generates a dangling $ref and the whole spec fails to
	// resolve.
	Values []string
}

// Field is one member of a wire type.
type Field struct {
	// Embeds names a struct whose fields belong here, from a Go embedded
	// field. Set on its own: such a field has no JSON name, because its
	// contents are spliced in where it stands.
	Embeds string

	JSON     string
	Doc      string
	Type     ast.Expr
	Required bool
}

// Read parses the package and collects every exported struct.
func Read(dir string) (map[string]Struct, error) {
	// Files are walked and parsed individually rather than with
	// parser.ParseDir, which is deprecated, and whose replacement
	// (golang.org/x/tools/go/packages) is a dependency this does not need: one
	// flat directory of plain structs has no build tags to reason about.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		files = append(files, f)
	}

	out := map[string]Struct{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				doc := ts.Doc
				if doc == nil {
					doc = gen.Doc
				}

				switch t := ts.Type.(type) {
				case *ast.StructType:
					out[ts.Name.Name] = Struct{
						Name:   ts.Name.Name,
						Doc:    Text(doc),
						Fields: readFields(t),
					}
				case *ast.Ident:
					if t.Name == "string" {
						out[ts.Name.Name] = Struct{
							Name: ts.Name.Name,
							Doc:  Text(doc),
						}
					}
				}
			}
		}
	}
	// A second pass for the constants, which are declared after the type they
	// belong to and often in a different block.
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || vs.Type == nil {
					continue
				}
				ident, ok := vs.Type.(*ast.Ident)
				if !ok {
					continue
				}
				named, ok := out[ident.Name]
				if !ok || len(named.Fields) > 0 {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					named.Values = append(named.Values, strings.Trim(lit.Value, `"`))
				}
				out[ident.Name] = named
			}
		}
	}
	return out, nil
}

func readFields(st *ast.StructType) []Field {
	var out []Field
	for _, f := range st.Fields.List {
		// An EMBEDDED struct: no name of its own and no tag. Go marshals its
		// fields as though they were written out here, so the schema has to
		// say the same - and skipping it silently is how CollectionDetail came
		// to describe itself as holding nothing but a list of books, with the
		// generated TypeScript agreeing and the server sending six more
		// fields.
		if f.Tag == nil {
			if len(f.Names) == 0 {
				if ident, ok := f.Type.(*ast.Ident); ok {
					out = append(out, Field{Embeds: ident.Name})
				}
			}
			continue
		}
		tag := strings.Trim(f.Tag.Value, "`")
		name, opts := jsonTag(tag)
		if name == "" || name == "-" {
			continue
		}

		_, isPointer := f.Type.(*ast.StarExpr)
		out = append(out, Field{
			JSON: name,
			Doc:  Text(f.Doc),
			Type: f.Type,
			// A field is required unless it can legitimately be absent: a
			// pointer is nullable by construction and omitempty says the server
			// will drop it. Everything else is always present, and saying so is
			// what makes the generated TypeScript useful rather than a wall of
			// optional properties.
			Required: !isPointer && !opts["omitempty"],
		})
	}
	return out
}

// jsonTag pulls the name and options out of a struct tag.
func jsonTag(tag string) (string, map[string]bool) {
	idx := strings.Index(tag, `json:"`)
	if idx < 0 {
		return "", nil
	}
	rest := tag[idx+len(`json:"`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return "", nil
	}
	parts := strings.Split(rest[:end], ",")
	opts := map[string]bool{}
	for _, o := range parts[1:] {
		opts[o] = true
	}
	return parts[0], opts
}

// Text flattens a doc comment to one paragraph.
func Text(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	var lines []string
	for _, c := range g.List {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(c.Text, "//"), "/*"))
		line = strings.TrimSuffix(line, "*/")
		lines = append(lines, strings.TrimSpace(line))
	}
	joined := strings.Join(lines, " ")
	// Doc comments here are written as prose over several paragraphs. Only the
	// first sentence belongs in a schema description; the rest is reasoning for
	// whoever is reading the Go.
	if idx := strings.Index(joined, ".  "); idx > 0 {
		joined = joined[:idx+1]
	}
	joined = strings.Join(strings.Fields(joined), " ")
	if len(joined) > 400 {
		joined = strings.TrimSpace(joined[:400]) + "…"
	}
	return joined
}

// Flatten splices embedded structs into the field list where they stand, the
// way Go marshals them.
//
// onMissing is called for an embedded type the caller does not know about,
// because a generated file missing whatever that type contributes is exactly
// the silence this is here to prevent.
func Flatten(fields []Field, types map[string]Struct, seen map[string]bool, onMissing func(name string)) []Field {
	var out []Field
	for _, f := range fields {
		if f.Embeds == "" {
			out = append(out, f)
			continue
		}
		if seen[f.Embeds] {
			continue
		}
		embedded, ok := types[f.Embeds]
		if !ok {
			onMissing(f.Embeds)
			continue
		}
		seen[f.Embeds] = true
		out = append(out, Flatten(embedded.Fields, types, seen, onMissing)...)
	}
	return out
}
