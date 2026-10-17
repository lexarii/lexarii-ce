// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
)

// brandingDir lays out a branding directory with something of every kind the
// rules have to tell apart, next to a file outside it that must stay out of
// reach.
func brandingDir(t *testing.T) (dir, outside string) {
	t.Helper()
	parent := t.TempDir()
	dir = filepath.Join(parent, "branding")
	write := func(name string, body []byte) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("branding.css", []byte(":root { --accent: #123456; }"))
	write("logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	write("bg.png", []byte("\x89PNG\r\n\x1a\nnot really"))
	write("bg.webp", []byte("RIFF....WEBP"))
	write("bg.avif", []byte("....ftypavif"))
	write("fonts/Brand.woff2", []byte("wOF2"))
	write("favicon.ico", []byte{0, 0, 1, 0})
	write("LOUD.PNG", []byte("\x89PNG\r\n\x1a\n"))
	write("index.html", []byte("<script>alert(1)</script>"))
	write("app.js", []byte("alert(1)"))
	write("notes.txt", []byte("private"))
	write(".hidden.css", []byte("private"))
	write(".git/config.css", []byte("private"))
	write("huge.png", bytes.Repeat([]byte{0}, BrandingMaxFileBytes+1))
	if err := os.MkdirAll(filepath.Join(dir, "folder.css"), 0o755); err != nil {
		t.Fatal(err)
	}

	outside = filepath.Join(parent, "secret.css")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.css")); err != nil {
		t.Fatal(err)
	}
	return dir, outside
}

func brandingRouter(t *testing.T, dir string) *Router {
	t.Helper()
	files, err := NewBrandingFiles(dir, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	return &Router{Branding: files}
}

func TestBrandingFilesAreServedWithTheirOwnType(t *testing.T) {
	dir, _ := brandingDir(t)
	h := brandingRouter(t, dir).Handler()

	for name, want := range map[string]string{
		"branding.css":      "text/css; charset=utf-8",
		"logo.svg":          "image/svg+xml",
		"bg.png":            "image/png",
		"bg.webp":           "image/webp",
		"bg.avif":           "image/avif",
		"fonts/Brand.woff2": "font/woff2",
		"favicon.ico":       "image/x-icon",
		"LOUD.PNG":          "image/png",
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/branding/"+name, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
			}
			body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rec.Body.Bytes(), body) {
				t.Errorf("body = %q, want the file", rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != want {
				t.Errorf("Content-Type = %q, want %q", got, want)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			// Opened directly, an SVG is a document on this origin; the
			// sandbox is what stops one running script here.
			if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") ||
				!strings.Contains(csp, "default-src 'none'") {
				t.Errorf("Content-Security-Policy = %q, want a sandboxed default-src 'none'", csp)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
				t.Errorf("Cache-Control = %q, want no-cache", got)
			}
		})
	}

	t.Run("HEAD", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/branding/branding.css", nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/css; charset=utf-8" {
			t.Errorf("HEAD = %d %q", rec.Code, rec.Header().Get("Content-Type"))
		}
	})

	t.Run("an unchanged file is a 304", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/branding/branding.css", nil))
		req := httptest.NewRequest(http.MethodGet, "/branding/branding.css", nil)
		req.Header.Set("If-Modified-Since", rec.Header().Get("Last-Modified"))
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Errorf("status = %d, want 304", rec.Code)
		}
	})

	t.Run("writing is not a method it has", func(t *testing.T) {
		for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, "/branding/branding.css", strings.NewReader("x")))
			if rec.Code == http.StatusOK || rec.Code == http.StatusNoContent || rec.Code == http.StatusCreated {
				t.Errorf("%s = %d", method, rec.Code)
			}
		}
	})
}

// Everything that is not an allowed file in the directory is a 404, never the
// file, never a listing, and never the app shell pretending to be a stylesheet.
func TestBrandingFilesServeNothingElse(t *testing.T) {
	dir, outside := brandingDir(t)
	h := brandingRouter(t, dir).Handler()
	secret, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{
		"/branding/",                // a listing
		"/branding/fonts/",          // a listing of a subdirectory
		"/branding/folder.css",      // a directory with an allowed name
		"/branding/missing.css",     // nothing there
		"/branding/index.html",      // a type outside the allowlist
		"/branding/app.js",          // nor a script
		"/branding/notes.txt",       // nor anything else
		"/branding/.hidden.css",     // a dotfile
		"/branding/.git/config.css", // inside a dot directory
		"/branding/huge.png",        // over the size limit
		"/branding/escape.css",      // a symbolic link out of the directory
		"/branding/..%2Fsecret.css", // an escaped traversal
		"/branding/fonts%2F..%2F..%2Fsecret.css",
		"/branding/%2E%2E/secret.css",         // an escaped dot-dot segment
		`/branding/..\secret.css`,             // a Windows separator
		"/branding/branding.css%00.png",       // a NUL
		"/branding/fonts/%2E%2E/branding.css", // the same, landing inside
	} {
		t.Run(target, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, rawRequest(t, http.MethodGet, target))
			if bytes.Contains(rec.Body.Bytes(), secret) {
				t.Fatalf("%s served the file outside the directory", target)
			}
			if rec.Code == http.StatusOK {
				t.Errorf("status = 200, body %.80q", rec.Body.String())
			}
			if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
				t.Errorf("answered with HTML: %.80q", rec.Body.String())
			}
		})
	}

	// The router cleans a plain ".." away before any handler sees it; what
	// it redirects to must still be inside /branding/ or a 404.
	t.Run("a plain traversal", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, rawRequest(t, http.MethodGet, "/branding/../secret.css"))
		if bytes.Contains(rec.Body.Bytes(), secret) || rec.Code == http.StatusOK {
			t.Errorf("status = %d, Location %q", rec.Code, rec.Header().Get("Location"))
		}
	})
}

func TestBrandingNeedsADirectory(t *testing.T) {
	if _, err := NewBrandingFiles(filepath.Join(t.TempDir(), "missing"), nil); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing directory: err = %v, want ErrNotExist", err)
	}
	file := filepath.Join(t.TempDir(), "branding.css")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBrandingFiles(file, nil); !errors.Is(err, ErrNotADirectory) {
		t.Errorf("a file: err = %v, want ErrNotADirectory", err)
	}
}

// Branding is for the sign-in page, so a visitor with no session gets it -
// and the gate still refuses an escaped path rather than passing it on.
func TestBrandingIsServedToASignedOutVisitor(t *testing.T) {
	dir, _ := brandingDir(t)
	files, err := NewBrandingFiles(dir, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	authHandler, _, _ := tokenHandler(t)
	h := (&Router{Auth: authHandler, Sessions: authHandler.Sessions, Branding: files}).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/branding/branding.css", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "--accent") {
		t.Errorf("signed out: %d %.80q, want the stylesheet", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, rawRequest(t, http.MethodGet, "/branding/..%2Fsecret.css"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out, escaped path: %d, want 401", rec.Code)
	}
}

// With no branding configured nothing is mounted at /branding/: the router
// behaves exactly as before.
func TestNoBrandingMountsNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Router{}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/branding/branding.css", nil))
	if !routerFallback(rec) {
		t.Errorf("an unconfigured router answered /branding/ itself: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

// The client learns from the open providers request whether to load the
// stylesheet, and the field is always present so it has one shape to read.
func TestTheProvidersListSaysWhetherThereIsBranding(t *testing.T) {
	h, _, _ := tokenHandler(t)
	get := func() (dto.AuthOptions, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ProvidersList(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/providers", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
		}
		var out dto.AuthOptions
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out, rec.Body.String()
	}

	if out, body := get(); out.Branding || !strings.Contains(body, `"branding":false`) {
		t.Errorf("unbranded: %s, want \"branding\":false", body)
	}
	h.Branding = true
	if out, _ := get(); !out.Branding {
		t.Error("AuthHandler.Branding = true did not reach the response")
	}
}
