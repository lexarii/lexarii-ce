// SPDX-License-Identifier: AGPL-3.0-or-later

// Package migrations carries the schema so a binary can apply it to an empty
// database on its own.
//
// A Go file in a directory of SQL looks odd until you try the install this
// project promises: one binary and a DATABASE_URL. Reading migrations from
// disk means shipping a directory alongside the binary and getting its path
// right in every deployment, and the container image that shipped before this
// did not carry them at all - it started a server against an empty schema and
// failed every query.
//
// The embed directive cannot reach outside its own package directory, which
// is why this file lives here rather than beside the code that uses it.
package migrations

import "embed"

// FS is every migration, in order.
//
//go:embed *.sql
var FS embed.FS
