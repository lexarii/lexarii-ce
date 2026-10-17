// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// staticFiles holds the built web client.
//
// `make build` compiles the SvelteKit app and copies it here before the Go
// build runs, which is what makes the single-binary install possible: one file
// plus a DATABASE_URL and the whole application is running.
//
// The placeholder index.html in the repository exists so that a plain
// `go build` still compiles; go:embed fails outright on an empty directory.
//
//go:embed all:static
var staticFiles embed.FS

// clientHandler serves the embedded client, falling back to index.html so that
// client-side routes like /works/<id> resolve on a hard refresh.
func clientHandler() (http.Handler, error) {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	files := http.FS(sub)
	server := http.FileServer(files)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never let the client shadow the API: an unknown /api path is a 404
		// from the API, not the app shell, or a typo'd endpoint would return
		// HTML and a 200 and look like it worked.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			respondError(w, NotFound("endpoint"))
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		if f, err := files.Open(path); err == nil {
			_ = f.Close()
			// Immutable build assets are content-hashed by Vite, so they can be
			// cached hard. Everything else must not be.
			if strings.HasPrefix(path, "_app/immutable/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			server.ServeHTTP(w, r)
			return
		}

		// A client-side route. Serve the shell and let the app router handle it.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		server.ServeHTTP(w, r2)
	}), nil
}
