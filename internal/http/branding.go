// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
)

// BrandingMaxFileBytes is the largest file BrandingFiles will serve. A logo,
// a stylesheet, a font or a background image all fit comfortably; anything
// larger is far more likely a mistake than a sign-in page.
const BrandingMaxFileBytes = 4 << 20

// brandingTypes is every kind of file the branding directory may serve, by
// extension, and the type it is served as. An allowlist: a branding directory
// is not a place to publish HTML or scripts from this server's origin.
var brandingTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".webp":  "image/webp",
	".avif":  "image/avif",
	".woff2": "font/woff2",
	".ico":   "image/x-icon",
}

// brandingCSP is the policy on every branding response, replacing the
// router's. A stylesheet or an image loaded by the app is unaffected by it;
// what it governs is a branding file opened directly, which for an SVG would
// otherwise be a document that can run script on this origin.
const brandingCSP = "default-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; " +
	"font-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; sandbox"

// BrandingFiles serves an installation's own branding - a stylesheet and the
// images and fonts it uses - read-only from a directory, at /branding/.
//
// Mounted as Router.Branding, under GET /branding/{path...}. Only regular
// files with an extension in the allowlist are served, nothing whose name or
// any directory above it starts with a dot, nothing larger than
// BrandingMaxFileBytes, and never a directory listing. Files are opened
// through os.OpenInRoot, so neither ".." nor a symbolic link can reach
// anything outside the directory.
//
// The files are read on every request (with conditional requests answered
// from the modification time), so changing them needs no restart.
type BrandingFiles struct {
	dir string
	log *slog.Logger
}

// NewBrandingFiles serves dir, which must exist and be a directory. A nil log
// uses slog.Default.
func NewBrandingFiles(dir string, log *slog.Logger) (*BrandingFiles, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("branding directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("branding directory %s: %w", dir, ErrNotADirectory)
	}
	if log == nil {
		log = slog.Default()
	}
	return &BrandingFiles{dir: dir, log: log}, nil
}

// ErrNotADirectory is returned by NewBrandingFiles for a path that exists but
// is not a directory.
var ErrNotADirectory = errors.New("not a directory")

// ServeHTTP serves one file. The path below /branding/ comes from the
// router's {path...} wildcard.
func (b *BrandingFiles) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("path")
	contentType, ok := brandingContentType(name)
	if !ok {
		respondError(w, NotFound("branding file"))
		return
	}

	f, err := os.OpenInRoot(b.dir, name)
	if err != nil {
		respondError(w, NotFound("branding file"))
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		respondError(w, NotFound("branding file"))
		return
	}
	if info.Size() > BrandingMaxFileBytes {
		// The administrator's mistake, so it is said in the log, where they
		// will look, rather than to a visitor.
		b.log.Warn("branding file too large to serve",
			"file", name, "bytes", info.Size(), "limit", BrandingMaxFileBytes)
		respondError(w, NotFound("branding file"))
		return
	}

	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", brandingCSP)
	// Not content-hashed, so not cacheable for long: revalidated each time,
	// which ServeContent answers with a 304 when nothing changed.
	h.Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, path.Base(name), info.ModTime(), f)
}

// brandingContentType reports the type a branding file is served as, and
// false for a name that may not be served at all: empty, with an empty, "."
// or ".." segment or any segment starting with a dot, a backslash or a control
// character, or an extension outside the allowlist.
//
// os.OpenInRoot already refuses to leave the directory; this is the second
// check, and the one that keeps dotfiles and unlisted types private.
func brandingContentType(name string) (string, bool) {
	if name == "" || len(name) > 512 {
		return "", false
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "", false
		}
	}
	for segment := range strings.SplitSeq(name, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") {
			return "", false
		}
	}
	contentType, ok := brandingTypes[strings.ToLower(path.Ext(name))]
	return contentType, ok
}
