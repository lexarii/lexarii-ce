// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// readerAudit is one RecordAccess call, as the fake received it.
type readerAudit struct {
	file, viewer uuid.UUID
	kind         postgres.AccessKind
	unit         int
}

// readerStoreFake serves a fixed set of files and records what it was asked.
type readerStoreFake struct {
	files map[uuid.UUID]domain.BookFile
	// lookupErr, when set, is what every lookup fails with.
	lookupErr   error
	download    bool
	downloadErr error
	recordErr   error

	mu      sync.Mutex
	viewers []uuid.UUID
	audits  []readerAudit
}

func newReaderStoreFake(files ...domain.BookFile) *readerStoreFake {
	f := &readerStoreFake{files: map[uuid.UUID]domain.BookFile{}}
	for _, file := range files {
		f.files[file.ID] = file
	}
	return f
}

func (f *readerStoreFake) ReadableBookFile(_ context.Context, id, viewer uuid.UUID) (domain.BookFile, error) {
	f.mu.Lock()
	f.viewers = append(f.viewers, viewer)
	f.mu.Unlock()
	if f.lookupErr != nil {
		return domain.BookFile{}, f.lookupErr
	}
	file, ok := f.files[id]
	if !ok {
		return domain.BookFile{}, store.ErrNotFound
	}
	return file, nil
}

func (f *readerStoreFake) ReadableBookWithWork(ctx context.Context, id, viewer uuid.UUID) (postgres.ReadableBook, error) {
	file, err := f.ReadableBookFile(ctx, id, viewer)
	if err != nil {
		return postgres.ReadableBook{}, err
	}
	return postgres.ReadableBook{File: file, WorkID: readerWorkID, Title: "Untrusted Markup"}, nil
}

func (f *readerStoreFake) CanDownload(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return f.download, f.downloadErr
}

func (f *readerStoreFake) RecordAccess(_ context.Context, file, viewer uuid.UUID, kind postgres.AccessKind, unit int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audits = append(f.audits, readerAudit{file: file, viewer: viewer, kind: kind, unit: unit})
	return f.recordErr
}

func (f *readerStoreFake) recorded() []readerAudit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]readerAudit(nil), f.audits...)
}

var readerWorkID = uuid.MustParse("6f1d1f2e-8a5b-4c3d-9e7f-0a1b2c3d4e5f")

// hostileChapter is a chapter written by somebody who wants to run code in
// the reader's page. Every one of these must be gone by the time it is served.
const hostileChapter = `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>One</title>
<script>document.cookie</script></head>
<body>
<h1 id="top">Chapter One</h1>
<p onclick="steal()" style="color:red">Opening words.</p>
<script>fetch('/api/v1/auth/me')</script>
<p><a href="javascript:alert(1)">a trap</a> and <a href="ch2.xhtml#later">onwards</a></p>
<img src="images/plate 1.png" alt="The first plate"/>
<img src="http://tracker.example/pixel.png" alt="remote"/>
<iframe src="https://evil.example/"></iframe>
<object data="x.swf"></object>
<form action="https://evil.example/"><input name="pw"/></form>
</body></html>`

// writeReaderEPUB writes a small but complete EPUB 3 with a navigation
// document, two chapters, an image and a stylesheet, and returns its path.
func writeReaderEPUB(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range []struct{ name, body string }{
		{"mimetype", "application/epub+zip"},
		{"META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`},
		{"OEBPS/content.opf", `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="id">urn:uuid:reader-test</dc:identifier>
    <dc:title>Untrusted Markup</dc:title><dc:language>en</dc:language>
  </metadata>
  <manifest>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    <item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="c2" href="ch2.xhtml" media-type="application/xhtml+xml"/>
    <item id="plate" href="images/plate 1.png" media-type="image/png"/>
    <item id="css" href="style.css" media-type="text/css"/>
  </manifest>
  <spine><itemref idref="c1"/><itemref idref="c2" linear="no"/></spine>
</package>`},
		{"OEBPS/nav.xhtml", `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops"><body>
<nav epub:type="toc"><ol>
  <li><a href="ch1.xhtml">Chapter One</a></li>
  <li><a href="ch2.xhtml#later">Chapter Two</a>
    <ol><li><a href="ch2.xhtml#deeper">A Section</a></li></ol></li>
</ol></nav></body></html>`},
		{"OEBPS/ch1.xhtml", hostileChapter},
		{"OEBPS/ch2.xhtml", `<html><body><h2 id="later">Chapter Two</h2><p id="deeper">The end.</p></body></html>`},
		{"OEBPS/images/plate 1.png", string(pngBytes)},
		{"OEBPS/style.css", "body { background: url(javascript:alert(1)) }"},
	} {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatalf("creating %s: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("writing %s: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the archive: %v", err)
	}
	path := filepath.Join(t.TempDir(), "untrusted.epub")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing the EPUB: %v", err)
	}
	return path
}

// readerPDFBytes builds a valid PDF of the given number of blank pages with a
// two-entry outline pointing at pages 1 and 2.
func readerPDFBytes(t *testing.T, pages int) []byte {
	t.Helper()
	firstPage := 3
	outlineRoot := firstPage + pages
	kids := make([]string, 0, pages)
	for i := 0; i < pages; i++ {
		kids = append(kids, fmt.Sprintf("%d 0 R", firstPage+i))
	}
	objects := []string{
		fmt.Sprintf("<< /Type /Catalog /Pages 2 0 R /Outlines %d 0 R >>", outlineRoot),
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages),
	}
	for i := 0; i < pages; i++ {
		objects = append(objects, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /UserUnit %d >>", i+1))
	}
	first, second := outlineRoot+1, outlineRoot+2
	objects = append(objects,
		fmt.Sprintf("<< /Type /Outlines /First %d 0 R /Last %d 0 R /Count 2 >>", first, second),
		fmt.Sprintf("<< /Title (Preface) /Parent %d 0 R /Next %d 0 R /Dest [%d 0 R /Fit] >>",
			outlineRoot, second, firstPage),
		fmt.Sprintf("<< /Title (Methods) /Parent %d 0 R /Prev %d 0 R /Dest [%d 0 R /Fit] >>",
			outlineRoot, first, firstPage+1),
	)

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")
	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return buf.Bytes()
}

func writeReaderPDF(t *testing.T, pages int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reference.pdf")
	if err := os.WriteFile(path, readerPDFBytes(t, pages), 0o600); err != nil {
		t.Fatalf("writing the PDF: %v", err)
	}
	return path
}

// readerGet sends a request through the router, as the given viewer (none
// when uuid.Nil), so path values are extracted exactly as in production.
func readerGet(t *testing.T, h *ReaderHandler, viewer uuid.UUID, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	if viewer != uuid.Nil {
		req = asUserID(req, viewer)
	}
	rec := httptest.NewRecorder()
	(&Router{Reader: h}).Handler().ServeHTTP(rec, req)
	return rec
}

// wireCodeOf reads the machine-readable code out of the standard error shape.
func wireCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body dto.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not the error shape: %v; body = %s", err, rec.Body)
	}
	return body.Error.Code
}

type readerFixture struct {
	epub, pdf, cbz, missing, corrupt domain.BookFile
	store                            *readerStoreFake
	handler                          *ReaderHandler
	viewer                           uuid.UUID
}

func newReaderFixture(t *testing.T) *readerFixture {
	t.Helper()
	dir := t.TempDir()
	corruptPath := filepath.Join(dir, "corrupt.epub")
	if err := os.WriteFile(corruptPath, []byte("this is not a zip archive"), 0o600); err != nil {
		t.Fatalf("writing the corrupt file: %v", err)
	}
	f := &readerFixture{
		epub: domain.BookFile{ID: uuid.New(), Path: writeReaderEPUB(t), Format: domain.FormatEPUB,
			Status: domain.IngestReady, SizeBytes: 4096, ContentHash: "epubhash"},
		pdf: domain.BookFile{ID: uuid.New(), Path: writeReaderPDF(t, 3), Format: domain.FormatPDF,
			Status: domain.IngestReady, ContentHash: "pdfhash"},
		cbz:     domain.BookFile{ID: uuid.New(), Path: corruptPath, Format: domain.FormatCBZ, ContentHash: "cbzhash"},
		missing: domain.BookFile{ID: uuid.New(), Path: filepath.Join(dir, "gone.epub"), Format: domain.FormatEPUB},
		corrupt: domain.BookFile{ID: uuid.New(), Path: corruptPath, Format: domain.FormatEPUB},
		viewer:  uuid.New(),
	}
	f.store = newReaderStoreFake(f.epub, f.pdf, f.cbz, f.missing, f.corrupt)
	f.handler = &ReaderHandler{Store: f.store}
	return f
}

func (f *readerFixture) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	return readerGet(t, f.handler, f.viewer, target, nil)
}

func bookFileURL(id uuid.UUID, rest string) string {
	return "/api/v1/books/" + id.String() + rest
}

// Every reader endpoint answers the same three ways before it reaches a file:
// a malformed id is the client's mistake, an unknown or unreadable book is
// "not found" - never "forbidden", which would say the book exists - and a
// store failure is a server fault that says nothing about why.
func TestReaderEndpointsRefuseBeforeTouchingAFile(t *testing.T) {
	stranger := uuid.New().String()
	for _, endpoint := range []string{"/info", "/file", "/manifest", "/spine/0", "/assets/OEBPS/images/x.png", "/page/1"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newReaderFixture(t)

			rec := f.get(t, "/api/v1/books/not-a-uuid"+endpoint)
			if rec.Code != http.StatusBadRequest || wireCodeOf(t, rec) != string(CodeInvalid) {
				t.Errorf("malformed id: status %d, body %s; want 400 invalid_request", rec.Code, rec.Body)
			}

			rec = f.get(t, "/api/v1/books/"+stranger+endpoint)
			if rec.Code != http.StatusNotFound || wireCodeOf(t, rec) != string(CodeNotFound) {
				t.Errorf("unknown book: status %d, body %s; want 404 not_found", rec.Code, rec.Body)
			}

			f.store.lookupErr = errors.New("connection reset")
			rec = f.get(t, bookFileURL(f.epub.ID, endpoint))
			if rec.Code != http.StatusInternalServerError || wireCodeOf(t, rec) != string(CodeInternal) {
				t.Errorf("store failure: status %d, body %s; want 500 internal_error", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "connection reset") {
				t.Error("an internal error's detail reached the client")
			}
			if len(f.store.recorded()) != 0 {
				t.Errorf("a refused request was audited as reading: %v", f.store.recorded())
			}
		})
	}
}

// The store is always asked with the signed-in viewer, never a fixed id or
// nobody: the viewer is what decides whether the book exists for them.
func TestReaderAsksTheStoreAsTheViewer(t *testing.T) {
	f := newReaderFixture(t)
	for _, endpoint := range []string{"/info", "/file", "/manifest", "/spine/0", "/page/1"} {
		f.get(t, bookFileURL(f.epub.ID, endpoint))
	}
	if len(f.store.viewers) == 0 {
		t.Fatal("the store was never asked")
	}
	for _, v := range f.store.viewers {
		if v != f.viewer {
			t.Fatalf("the store was asked as %s, want the signed-in viewer %s", v, f.viewer)
		}
	}
}

func TestReaderInfo(t *testing.T) {
	f := newReaderFixture(t)
	for _, tc := range []struct {
		name       string
		file       domain.BookFile
		download   bool
		reflowable bool
	}{
		{"an EPUB that may be downloaded", f.epub, true, true},
		{"a PDF shared to be read only", f.pdf, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.store.download = tc.download
			rec := f.get(t, bookFileURL(tc.file.ID, "/info"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
			}
			var got dto.BookInfo
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			want := dto.BookInfo{
				CanDownload: tc.download, BookFileID: tc.file.ID.String(), Title: "Untrusted Markup",
				WorkID: readerWorkID.String(), Format: string(tc.file.Format), SizeBytes: tc.file.SizeBytes,
				Status: string(tc.file.Status), Reflowable: tc.reflowable,
			}
			if got.CanDownload != want.CanDownload || got.BookFileID != want.BookFileID ||
				got.Title != want.Title || got.WorkID != want.WorkID || got.Format != want.Format ||
				got.SizeBytes != want.SizeBytes || got.Status != want.Status || got.Reflowable != want.Reflowable {
				t.Errorf("info = %+v\nwant   %+v", got, want)
			}
		})
	}

	t.Run("a failure reading the download permission is a server fault", func(t *testing.T) {
		f.store.downloadErr = errors.New("permission lookup failed")
		defer func() { f.store.downloadErr = nil }()
		rec := f.get(t, bookFileURL(f.epub.ID, "/info"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

// The original file is handed over only with the download permission, with
// headers that keep it from ever being treated as a page, and with HTTP's own
// range and conditional handling.
func TestReaderFile(t *testing.T) {
	t.Run("without the download permission the file is refused", func(t *testing.T) {
		f := newReaderFixture(t)
		f.store.download = false
		rec := f.get(t, bookFileURL(f.epub.ID, "/file"))
		if rec.Code != http.StatusForbidden || wireCodeOf(t, rec) != "download_not_permitted" {
			t.Fatalf("status = %d, body = %s; want 403 download_not_permitted", rec.Code, rec.Body)
		}
		if bytes.Contains(rec.Body.Bytes(), []byte("PK")) {
			t.Fatal("archive bytes leaked into a refusal")
		}
		if len(f.store.recorded()) != 0 {
			t.Errorf("a refused download was audited: %v", f.store.recorded())
		}
	})

	t.Run("a failure reading the permission is a server fault, not a download", func(t *testing.T) {
		f := newReaderFixture(t)
		f.store.download, f.store.downloadErr = true, errors.New("lookup failed")
		rec := f.get(t, bookFileURL(f.epub.ID, "/file"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("a file gone from disk is 410", func(t *testing.T) {
		f := newReaderFixture(t)
		f.store.download = true
		rec := f.get(t, bookFileURL(f.missing.ID, "/file"))
		if rec.Code != http.StatusGone || wireCodeOf(t, rec) != "file_missing" {
			t.Fatalf("status = %d, body = %s; want 410 file_missing", rec.Code, rec.Body)
		}
	})

	for _, tc := range []struct {
		name        string
		pick        func(f *readerFixture) domain.BookFile
		contentType string
	}{
		{"an EPUB", func(f *readerFixture) domain.BookFile { return f.epub }, "application/epub+zip"},
		{"a PDF", func(f *readerFixture) domain.BookFile { return f.pdf }, "application/pdf"},
		{"any other format", func(f *readerFixture) domain.BookFile { return f.cbz }, "application/octet-stream"},
	} {
		t.Run("the whole of "+tc.name, func(t *testing.T) {
			f := newReaderFixture(t)
			f.store.download = true
			file := tc.pick(f)
			want, err := os.ReadFile(file.Path)
			if err != nil {
				t.Fatalf("reading the fixture: %v", err)
			}

			rec := f.get(t, bookFileURL(file.ID, "/file"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
			}
			if !bytes.Equal(rec.Body.Bytes(), want) {
				t.Fatalf("served %d bytes that are not the file (%d bytes)", rec.Body.Len(), len(want))
			}
			for header, value := range map[string]string{
				"Content-Type":            tc.contentType,
				"Cache-Control":           "private, max-age=86400",
				"X-Content-Type-Options":  "nosniff",
				"Content-Security-Policy": "default-src 'none'; sandbox",
				"Accept-Ranges":           "bytes",
			} {
				if got := rec.Header().Get(header); got != value {
					t.Errorf("%s = %q, want %q", header, got, value)
				}
			}
			audits := f.store.recorded()
			if len(audits) != 1 || audits[0] != (readerAudit{file: file.ID, viewer: f.viewer, kind: postgres.AccessFile}) {
				t.Errorf("audits = %+v, want one whole-file read by the viewer", audits)
			}
		})
	}

	t.Run("a range is served partially and is not counted as a download", func(t *testing.T) {
		f := newReaderFixture(t)
		f.store.download = true
		whole, err := os.ReadFile(f.pdf.Path)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		rec := readerGet(t, f.handler, f.viewer, bookFileURL(f.pdf.ID, "/file"),
			http.Header{"Range": {"bytes=0-7"}})
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", rec.Code)
		}
		if !bytes.Equal(rec.Body.Bytes(), whole[:8]) {
			t.Errorf("range body = %q, want %q", rec.Body.Bytes(), whole[:8])
		}
		if got := rec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes 0-7/%d", len(whole)) {
			t.Errorf("Content-Range = %q", got)
		}
		if audits := f.store.recorded(); len(audits) != 0 {
			t.Errorf("a range request was audited as a download: %+v", audits)
		}
	})

	t.Run("an unchanged file is not sent again", func(t *testing.T) {
		f := newReaderFixture(t)
		f.store.download = true
		info, err := os.Stat(f.pdf.Path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		since := info.ModTime().Add(time.Second).UTC().Format(http.TimeFormat)
		rec := readerGet(t, f.handler, f.viewer, bookFileURL(f.pdf.ID, "/file"),
			http.Header{"If-Modified-Since": {since}})
		if rec.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("a 304 carried %d bytes", rec.Body.Len())
		}
	})

	t.Run("an anonymous read is served without an audit row", func(t *testing.T) {
		// Only reachable when the router runs without sessions, which is a
		// test configuration; with nobody to attribute it to, nothing is logged.
		f := newReaderFixture(t)
		f.store.download = true
		rec := readerGet(t, f.handler, uuid.Nil, bookFileURL(f.epub.ID, "/file"), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if audits := f.store.recorded(); len(audits) != 0 {
			t.Errorf("an anonymous read was attributed: %+v", audits)
		}
	})
}

// Reading and downloading are different permissions. A grant without the
// download permission still reads the EPUB, as sanitised chapters, and does
// not get the file: refusing one is only defensible because the other works.
func TestAReadOnlyGrantReadsChaptersButNotTheFile(t *testing.T) {
	f := newReaderFixture(t)
	f.store.download = false

	if rec := f.get(t, bookFileURL(f.epub.ID, "/file")); rec.Code != http.StatusForbidden {
		t.Fatalf("file: status = %d, want 403", rec.Code)
	}
	for _, endpoint := range []string{"/manifest", "/spine/0", "/spine/1", "/assets/OEBPS/images/plate%201.png"} {
		if rec := f.get(t, bookFileURL(f.epub.ID, endpoint)); rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200; body = %s", endpoint, rec.Code, rec.Body)
		}
	}
}

func TestReaderManifestOfAnEPUB(t *testing.T) {
	f := newReaderFixture(t)
	rec := f.get(t, bookFileURL(f.epub.ID, "/manifest"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q", ct)
	}
	var got dto.ReaderManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.BookFileID != f.epub.ID.String() || got.Format != "epub" {
		t.Errorf("manifest identifies %s/%s", got.BookFileID, got.Format)
	}
	wantSpine := []dto.SpineItem{{Index: 0, ID: "c1", Linear: true}, {Index: 1, ID: "c2", Linear: false}}
	if len(got.Spine) != len(wantSpine) {
		t.Fatalf("spine = %+v, want %+v", got.Spine, wantSpine)
	}
	for i := range wantSpine {
		if got.Spine[i] != wantSpine[i] {
			t.Errorf("spine[%d] = %+v, want %+v", i, got.Spine[i], wantSpine[i])
		}
	}
	if len(got.TOC) != 2 {
		t.Fatalf("toc = %+v, want two top-level entries", got.TOC)
	}
	if got.TOC[0].Title != "Chapter One" || got.TOC[0].SpineIndex != 0 {
		t.Errorf("toc[0] = %+v", got.TOC[0])
	}
	second := got.TOC[1]
	if second.Title != "Chapter Two" || second.SpineIndex != 1 || second.Fragment != "later" {
		t.Errorf("toc[1] = %+v", second)
	}
	if len(second.Children) != 1 || second.Children[0].Title != "A Section" || second.Children[0].Fragment != "deeper" {
		t.Errorf("toc[1] children = %+v; nesting was lost", second.Children)
	}
}

// A PDF has no spine, but it does have a contents list, and failing to read
// one is never a reason to fail the manifest.
func TestReaderManifestOfAPDF(t *testing.T) {
	garbage := filepath.Join(t.TempDir(), "garbage.pdf")
	if err := os.WriteFile(garbage, []byte("%PDF-1.4 and then nothing that parses"), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	f := newReaderFixture(t)
	gone := domain.BookFile{ID: uuid.New(), Path: filepath.Join(t.TempDir(), "gone.pdf"), Format: domain.FormatPDF}
	broken := domain.BookFile{ID: uuid.New(), Path: garbage, Format: domain.FormatPDF}
	f.store.files[gone.ID], f.store.files[broken.ID] = gone, broken

	for _, tc := range []struct {
		name   string
		file   domain.BookFile
		titles []string
		pages  []int
	}{
		{"with an outline", f.pdf, []string{"Preface", "Methods"}, []int{1, 2}},
		{"missing from disk", gone, nil, nil},
		{"with no readable outline", broken, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.get(t, bookFileURL(tc.file.ID, "/manifest"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
			}
			var got dto.ReaderManifest
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if got.Format != "pdf" || got.BookFileID != tc.file.ID.String() {
				t.Errorf("manifest identifies %s/%s", got.BookFileID, got.Format)
			}
			// Empty arrays rather than null: the client iterates them.
			if !bytes.Contains(rec.Body.Bytes(), []byte(`"spine":[]`)) {
				t.Errorf("a PDF's spine is not an empty list: %s", rec.Body)
			}
			if len(got.TOC) != len(tc.titles) {
				t.Fatalf("toc = %+v, want titles %v", got.TOC, tc.titles)
			}
			for i, title := range tc.titles {
				if got.TOC[i].Title != title || got.TOC[i].SpineIndex != tc.pages[i] {
					t.Errorf("toc[%d] = %+v, want %q at page %d", i, got.TOC[i], title, tc.pages[i])
				}
			}
		})
	}
}

// Only an EPUB is reflowed, and a file that cannot be opened says why with a
// status of its own rather than a generic failure.
func TestReaderRefusesWhatItCannotReflow(t *testing.T) {
	f := newReaderFixture(t)
	for _, tc := range []struct {
		name   string
		file   domain.BookFile
		status int
		code   string
	}{
		{"a comic archive", f.cbz, http.StatusNotImplemented, "format_not_reflowable"},
		{"an EPUB gone from disk", f.missing, http.StatusGone, "file_missing"},
		{"an EPUB that is not a zip", f.corrupt, http.StatusUnprocessableEntity, "unreadable_file"},
	} {
		for _, endpoint := range []string{"/manifest", "/spine/0", "/assets/OEBPS/images/plate%201.png"} {
			t.Run(tc.name+endpoint, func(t *testing.T) {
				rec := f.get(t, bookFileURL(tc.file.ID, endpoint))
				if rec.Code != tc.status || wireCodeOf(t, rec) != tc.code {
					t.Fatalf("status = %d, body = %s; want %d %s", rec.Code, rec.Body, tc.status, tc.code)
				}
			})
		}
	}

	t.Run("a PDF has no spine to serve", func(t *testing.T) {
		rec := f.get(t, bookFileURL(f.pdf.ID, "/spine/0"))
		if rec.Code != http.StatusNotImplemented || wireCodeOf(t, rec) != "format_not_reflowable" {
			t.Fatalf("status = %d, body = %s; want 501 format_not_reflowable", rec.Code, rec.Body)
		}
	})
}

// Invariant 6: book HTML is sanitised before it reaches the DOM, and the
// server is where that happens.
func TestReaderContentIsSanitised(t *testing.T) {
	f := newReaderFixture(t)
	rec := f.get(t, bookFileURL(f.epub.ID, "/spine/0"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q; the chapter must arrive as data, not as a document", ct)
	}
	var got dto.SpineContent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Index != 0 {
		t.Errorf("index = %d", got.Index)
	}
	html := strings.ToLower(got.HTML)

	for _, hostile := range []string{
		"<script", "document.cookie", "fetch(", "onclick", "steal()", "javascript:",
		"<iframe", "evil.example", "<object", "<form", "<input", "tracker.example",
		"<head", "<title",
	} {
		if strings.Contains(html, hostile) {
			t.Errorf("sanitised chapter still contains %q:\n%s", hostile, got.HTML)
		}
	}
	for _, kept := range []string{"Chapter One", "Opening words.", "a trap", "The first plate"} {
		if !strings.Contains(got.HTML, kept) {
			t.Errorf("sanitising lost the text %q:\n%s", kept, got.HTML)
		}
	}

	// The image is rewritten to this book's asset endpoint, each segment
	// escaped, and a link to another chapter becomes reader coordinates.
	wantImg := `src="/api/v1/books/` + f.epub.ID.String() + `/assets/OEBPS/images/plate%201.png"`
	if !strings.Contains(got.HTML, wantImg) {
		t.Errorf("the image was not rewritten to %s:\n%s", wantImg, got.HTML)
	}
	if !strings.Contains(got.HTML, `href="?spine=1&amp;frag=later"`) {
		t.Errorf("the cross-chapter link was not rewritten:\n%s", got.HTML)
	}

	audits := f.store.recorded()
	if len(audits) != 1 || audits[0] != (readerAudit{file: f.epub.ID, viewer: f.viewer, kind: postgres.AccessChapter, unit: 1}) {
		t.Errorf("audits = %+v, want chapter 1 recorded against the viewer", audits)
	}
}

func TestReaderContentRefusesABadIndex(t *testing.T) {
	f := newReaderFixture(t)
	for _, tc := range []struct {
		index  string
		status int
	}{
		{"one", http.StatusBadRequest},
		{"2", http.StatusNotFound},
		{"-1", http.StatusNotFound},
	} {
		t.Run(tc.index, func(t *testing.T) {
			rec := f.get(t, bookFileURL(f.epub.ID, "/spine/"+tc.index))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
		})
	}
	if audits := f.store.recorded(); len(audits) != 0 {
		t.Errorf("a refused chapter was audited: %+v", audits)
	}
}

// An asset is served as an image and nothing else: never a document, never a
// file outside the archive, never a stylesheet or script.
func TestReaderAsset(t *testing.T) {
	f := newReaderFixture(t)

	t.Run("an image", func(t *testing.T) {
		rec := f.get(t, bookFileURL(f.epub.ID, "/assets/OEBPS/images/plate%201.png"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
		}
		if !bytes.Equal(rec.Body.Bytes(), pngBytes) {
			t.Error("the image bytes were not served as stored")
		}
		for header, value := range map[string]string{
			"Content-Type":            "image/png",
			"Cache-Control":           "private, max-age=86400",
			"X-Content-Type-Options":  "nosniff",
			"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; sandbox",
		} {
			if got := rec.Header().Get(header); got != value {
				t.Errorf("%s = %q, want %q", header, got, value)
			}
		}
	})

	for _, tc := range []struct {
		name, path string
		status     int
	}{
		{"a stylesheet", "OEBPS/style.css", http.StatusNotFound},
		{"a chapter", "OEBPS/ch1.xhtml", http.StatusNotFound},
		{"an entry that does not exist", "OEBPS/images/none.png", http.StatusNotFound},
		{"a traversal", "OEBPS/..%252F..%252F..%252Fetc%252Fpasswd.png", http.StatusBadRequest},
		{"a malformed escape", "OEBPS/%25zz.png", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.get(t, bookFileURL(f.epub.ID, "/assets/"+tc.path))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "javascript") || strings.Contains(rec.Body.String(), "root:") {
				t.Fatalf("refused content leaked: %s", rec.Body)
			}
		})
	}
}

// One page of a PDF, as a one-page PDF: what a grant without the download
// permission reads with.
func TestReaderPage(t *testing.T) {
	f := newReaderFixture(t)
	f.store.download = false

	t.Run("a page is a standalone PDF, privately cached", func(t *testing.T) {
		rec := f.get(t, bookFileURL(f.pdf.ID, "/page/2"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
			t.Errorf("content type = %q", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "private, max-age=300" {
			t.Errorf("cache control = %q; a shared page must never sit in a shared cache", cc)
		}
		if !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF-")) {
			t.Fatalf("not a PDF: %q", rec.Body.Bytes()[:min(16, rec.Body.Len())])
		}
	})

	t.Run("a prefetched page is traffic, not a page read", func(t *testing.T) {
		if rec := f.get(t, bookFileURL(f.pdf.ID, "/page/3?prefetch=1")); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	audits := f.store.recorded()
	want := []readerAudit{
		{file: f.pdf.ID, viewer: f.viewer, kind: postgres.AccessPage, unit: 2},
		{file: f.pdf.ID, viewer: f.viewer, kind: postgres.AccessPage, unit: 0},
	}
	if len(audits) != len(want) || audits[0] != want[0] || audits[1] != want[1] {
		t.Errorf("audits = %+v, want %+v", audits, want)
	}

	for _, tc := range []struct {
		name   string
		target string
		status int
		code   string
	}{
		{"a page number that is not a number", bookFileURL(f.pdf.ID, "/page/two"), http.StatusBadRequest, string(CodeInvalid)},
		{"page zero", bookFileURL(f.pdf.ID, "/page/0"), http.StatusBadRequest, string(CodeInvalid)},
		{"a page past the end", bookFileURL(f.pdf.ID, "/page/4"), http.StatusBadRequest, string(CodeInvalid)},
		{"an EPUB", bookFileURL(f.epub.ID, "/page/1"), http.StatusBadRequest, string(CodeInvalid)},
		{"a PDF that will not parse", bookFileURL(f.corruptPDF(t), "/page/1"), http.StatusGone, "file_unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.get(t, tc.target)
			if rec.Code != tc.status || wireCodeOf(t, rec) != tc.code {
				t.Fatalf("status = %d, body = %s; want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
		})
	}
}

// corruptPDF registers a file that claims to be a PDF and is not one.
func (f *readerFixture) corruptPDF(t *testing.T) uuid.UUID {
	t.Helper()
	file := domain.BookFile{ID: uuid.New(), Path: f.corrupt.Path, Format: domain.FormatPDF}
	f.store.files[file.ID] = file
	return file.ID
}

// Auditing is a side effect of reading. A log that cannot be written must not
// turn into a reader that cannot read.
func TestAFailedAuditDoesNotStopReading(t *testing.T) {
	f := newReaderFixture(t)
	f.store.recordErr = errors.New("access log unavailable")
	f.store.download = true

	for _, endpoint := range []string{"/spine/0", "/page/1"} {
		file := f.epub.ID
		if strings.HasPrefix(endpoint, "/page") {
			file = f.pdf.ID
		}
		if rec := f.get(t, bookFileURL(file, endpoint)); rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 despite the audit failing", endpoint, rec.Code)
		}
	}
	if rec := f.get(t, bookFileURL(f.epub.ID, "/file")); rec.Code != http.StatusOK {
		t.Errorf("file: status = %d, want 200 despite the audit failing", rec.Code)
	}
	if got := len(f.store.recorded()); got != 3 {
		t.Errorf("%d audit attempts, want 3", got)
	}
}

// A file that exists but cannot be opened is the server's problem, not the
// reader's, and is not reported as missing.
func TestAnUnreadableEPUBIsAServerFault(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	f := newReaderFixture(t)
	locked := domain.BookFile{ID: uuid.New(), Path: writeReaderEPUB(t), Format: domain.FormatEPUB}
	if err := os.Chmod(locked.Path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked.Path, 0o600) })
	f.store.files[locked.ID] = locked

	rec := f.get(t, bookFileURL(locked.ID, "/spine/0"))
	if rec.Code != http.StatusInternalServerError || wireCodeOf(t, rec) != string(CodeInternal) {
		t.Fatalf("status = %d, body = %s; want 500 internal_error", rec.Code, rec.Body)
	}
}
