// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest/extract"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// LibraryStore is the slice of the store the ingest handlers need.
type LibraryStore interface {
	UpsertBookFile(ctx context.Context, in postgres.IngestInput) (postgres.IngestOutcome, error)
	Enqueue(ctx context.Context, opts postgres.EnqueueOptions) (uuid.UUID, bool, error)
	MarkMissing(ctx context.Context, before time.Time) (int64, error)
	TouchSeen(ctx context.Context, contentHash, path string, holder uuid.UUID) error
	BookFileStatus(ctx context.Context, contentHash string) (domain.IngestStatus, error)
	BookFilePath(ctx context.Context, contentHash string) (string, error)
	RemoveIfUnreferenced(ctx context.Context, contentHash, path string, remove func(string) error) error
	IsBlocked(ctx context.Context, contentHash string) (bool, error)
	DiscardBlockedUpload(ctx context.Context, contentHash string, remove func() error) error
}

// ExtractPayload is the work an extract job carries.
//
// The scan job has already opened, hashed, and identified the file, so the
// extract job does not repeat any of that.
type ExtractPayload struct {
	Path        string        `json:"path"`
	ContentHash string        `json:"contentHash"`
	Format      domain.Format `json:"format"`
	SizeBytes   int64         `json:"sizeBytes"`
	// OriginalFilename and Uploaded are set for files that arrived through the
	// upload endpoint. Uploads are stored content-addressed, so the path is a
	// hash and the name has to travel separately or same-book merging breaks.
	OriginalFilename string `json:"originalFilename,omitempty"`
	Uploaded         bool   `json:"uploaded,omitempty"`
	// OwnerID is who the book is for, and becomes a holder of it. It travels
	// in the payload because the worker runs long after the request that
	// queued it, and the people who added a file are the only ones who can see
	// it until they share it. Anybody else who uploaded the same bytes left a
	// claim on the hash, which the same ingest turns into a holding.
	OwnerID uuid.UUID `json:"ownerId,omitempty"`
}

// ScanPayload optionally overrides the library directory for one scan.
type ScanPayload struct {
	Root string `json:"root,omitempty"`
	// OwnerID is who the scanned books belong to: the administrator who asked
	// for the scan. Books found on disk have no other claimant.
	OwnerID uuid.UUID `json:"ownerId,omitempty"`
}

// Ingest holds the scan and extract handlers.
type Ingest struct {
	Store      LibraryStore
	LibraryDir string
	Logger     *slog.Logger

	// CoverDir is where extracted cover images are written: under the DATA
	// directory, never the library directory, which is read-only.
	CoverDir string

	// FollowSymlinks is passed through to the scanner. Off by default, because a
	// symlink in a library directory can point anywhere on the filesystem.
	FollowSymlinks bool

	// ArchiveDir is where the books of departed holders are kept. A copy
	// there that a fresh upload has replaced is removed rather than left on
	// disk with nothing pointing at it. Lexarii's own directory: nothing
	// outside it is ever removed.
	ArchiveDir string

	// UploadDir is where uploads are stored. An extraction of an upload whose
	// bytes an administrator has since removed from the installation deletes
	// its copy there, and nothing outside it.
	UploadDir string

	// Indexing, when set, queues chunking for each file an extraction leaves
	// readable, so a new book - uploaded or found by a scan - becomes
	// searchable without anybody asking. Embedding follows chunking as it
	// always does. Nil leaves indexing to POST /api/v1/index, which is the
	// default; LEXARII_INDEX_ON_INGEST turns it on.
	Indexing ChunkScheduler

	// Enriching, when set, queues enrichment for the work of each file an
	// extraction leaves readable, so a new book is described by the model
	// without anybody asking. The job is for whoever the extraction was for -
	// the uploader, or the administrator who scanned - who may then follow
	// it. Nil leaves enrichment to POST /api/v1/enrich, which is the default;
	// LEXARII_ENRICH_ON_INGEST turns it on, and it stays off with no model
	// configured, since every job would only wait for one.
	Enriching EnrichScheduler

	// afterParse, when set, runs between parsing a file and storing it, so a
	// test can make something happen in exactly that gap. Nil outside tests.
	afterParse func()
}

// EnrichScheduler is what queuing enrichment after an extraction needs: queue
// the job only if the work is waiting for it, in one step.
type EnrichScheduler interface {
	EnqueueIfNeedsEnrichment(ctx context.Context, opts postgres.EnqueueOptions, promptID string) (bool, error)
}

// ChunkScheduler is what queuing chunking after an extraction needs: queue
// the job only if the file is waiting for it, in one step.
type ChunkScheduler interface {
	EnqueueIfNeedsChunks(ctx context.Context, opts postgres.EnqueueOptions, strategyVersion int) (bool, error)
}

// Handlers returns the job handlers this ingester provides.
func (i *Ingest) Handlers() map[domain.JobType]Handler {
	return map[domain.JobType]Handler{
		domain.JobScan:    i.Scan,
		domain.JobExtract: i.Extract,
	}
}

// Scan walks the library directory and enqueues extraction work.
//
// It deliberately does NOT parse anything itself. Parsing thousands of books
// inside one job would make the whole scan un-resumable and un-parallelisable,
// and a crash at book 4000 would start again from book 1. Discovery is cheap and
// sequential; extraction is expensive and parallel, so they are separate jobs.
//
// Files already ingested and unchanged are touched, not re-extracted, which is
// what makes a rescan of a large library nearly free.
func (i *Ingest) Scan(ctx context.Context, job postgres.Job, report ProgressFunc) error {
	log := i.logger().With("job", job.ID)

	root := i.LibraryDir
	var owner uuid.UUID
	if len(job.Payload) > 0 {
		var payload ScanPayload
		if err := json.Unmarshal(job.Payload, &payload); err == nil {
			if payload.Root != "" {
				root = payload.Root
			}
			owner = payload.OwnerID
		}
	}
	if root == "" {
		return errors.New("no library directory configured")
	}

	// Recorded before the walk: anything not touched during the scan was already
	// absent when the scan began, and using the start time avoids marking files
	// missing just because the scan was slow.
	startedAt := time.Now()

	scanner := &ingest.Scanner{
		Root:           root,
		FollowSymlinks: i.FollowSymlinks,
		Logger:         log,
	}

	var (
		seen      int
		enqueued  int
		unchanged int
		failed    int
		skipped   int
	)

	stats, err := scanner.Scan(ctx, func(f ingest.ScannedFile) error {
		seen++
		// The total is unknown until the walk finishes, so progress is reported
		// as a note rather than a fraction that would have to be invented.
		if seen%25 == 0 {
			report(0, fmt.Sprintf("scanned %d files", seen))
		}

		// Bytes an administrator removed from the installation are passed
		// over: not recorded, not held, not queued. The file itself is left
		// exactly where it is - the library directory is read-only - and the
		// block is what keeps it off every shelf.
		if f.ContentHash != "" {
			blocked, err := i.Store.IsBlocked(ctx, f.ContentHash)
			if err != nil {
				return fmt.Errorf("checking %s: %w", f.Path, err)
			}
			if blocked {
				skipped++
				return nil
			}
		}

		if f.Err != nil {
			failed++
			// The file is recorded with its error so the UI can show it rather
			// than silently omitting it.
			_, uerr := i.Store.UpsertBookFile(ctx, postgres.IngestInput{
				ContentHash: f.ContentHash,
				Path:        f.Path,
				Format:      f.Format,
				SizeBytes:   f.SizeBytes,
				Status:      domain.IngestFailed,
				IngestError: f.Err.Error(),
				OwnerID:     owner,
			})
			if uerr != nil && f.ContentHash != "" && !errors.Is(uerr, store.ErrBlocked) {
				log.Warn("recording unreadable file failed", "path", f.Path, "error", uerr)
			}
			return nil
		}

		status, err := i.Store.BookFileStatus(ctx, f.ContentHash)
		switch {
		case err == nil && isSettled(status):
			// Already ingested and unchanged: mark it seen so it is not flagged
			// missing, and move on without re-parsing.
			unchanged++
			// Passing the path matters: this is where a moved file's new
			// location gets recorded, since it skips extraction entirely. And
			// the owner, who holds what their scan finds unless they let it
			// go.
			if terr := i.Store.TouchSeen(ctx, f.ContentHash, f.Path, owner); terr != nil && !errors.Is(terr, store.ErrBlocked) {
				log.Warn("touching unchanged file failed", "path", f.Path, "error", terr)
			}
			return nil
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return fmt.Errorf("checking %s: %w", f.Path, err)
		}

		payload := ExtractPayload{
			Path:        f.Path,
			ContentHash: f.ContentHash,
			Format:      f.Format,
			SizeBytes:   f.SizeBytes,
			OwnerID:     owner,
		}
		// The target id is derived from the content hash, so enqueueing the same
		// file twice is deduplicated by the queue's partial unique index even
		// though no book_file row exists yet.
		target := TargetForHash(f.ContentHash)
		if _, created, err := i.Store.Enqueue(ctx, postgres.EnqueueOptions{
			Type:        domain.JobExtract,
			TargetID:    &target,
			Payload:     payload,
			RequestedBy: owner,
		}); err != nil {
			return fmt.Errorf("enqueuing extraction for %s: %w", f.Path, err)
		} else if created {
			enqueued++
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scanning %s: %w", root, err)
	}

	missing, err := i.Store.MarkMissing(ctx, startedAt)
	if err != nil {
		return fmt.Errorf("marking missing files: %w", err)
	}

	report(1, fmt.Sprintf("%d books found, %d queued, %d unchanged, %d unreadable, %d missing",
		stats.BooksFound, enqueued, unchanged, failed, missing))

	log.Info("scan finished",
		"filesSeen", stats.FilesSeen,
		"booksFound", stats.BooksFound,
		"queued", enqueued,
		"unchanged", unchanged,
		"unreadable", failed,
		"blocked", skipped,
		"markedMissing", missing)

	return nil
}

// Extract parses one book and persists it.
//
// Idempotent: the upsert is keyed on content hash, so a job interrupted and
// re-run produces the same rows rather than duplicates.
func (i *Ingest) Extract(ctx context.Context, job postgres.Job, report ProgressFunc) error {
	var payload ExtractPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decoding extract payload: %w", err)
	}
	if payload.Path == "" || payload.ContentHash == "" {
		return fmt.Errorf("extract payload is incomplete: %+v", payload)
	}

	// The bytes may no longer be where the job was told. An upload of a book
	// somebody was just letting go found it already stored and wrote nothing;
	// the archive then moved that copy. The book's row knows where it went,
	// and the file there has the same content hash, so it is the same file.
	if _, err := os.Stat(payload.Path); errors.Is(err, fs.ErrNotExist) {
		if current, perr := i.Store.BookFilePath(ctx, payload.ContentHash); perr == nil && current != payload.Path {
			if _, serr := os.Stat(current); serr == nil {
				payload.Path = current
			}
		}
	}

	report(0.1, "parsing "+payload.Path)

	result, parseErr := extract.File(ctx, payload.Path, payload.Format)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	in := postgres.IngestInput{
		ContentHash:      payload.ContentHash,
		Path:             payload.Path,
		Format:           payload.Format,
		SizeBytes:        payload.SizeBytes,
		Status:           extract.StatusFor(result, parseErr),
		Result:           result,
		OriginalFilename: payload.OriginalFilename,
		Uploaded:         payload.Uploaded,
		OwnerID:          payload.OwnerID,
		PathExists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
	}

	// An EPUB's cover comes out of the archive with the parse. A failure here
	// is never a reason to lose the book: the cover is a picture, and the
	// typographic fallback is a perfectly good one.
	if result != nil && result.Cover != nil {
		name, cerr := writeCover(i.CoverDir, result.Cover)
		if cerr != nil {
			i.logger().Warn("storing an extracted cover failed",
				"path", payload.Path, "error", cerr)
		}
		in.CoverPath = name
	}
	if parseErr != nil {
		in.IngestError = parseErr.Error()
	}

	if i.afterParse != nil {
		i.afterParse()
	}

	report(0.6, "storing "+payload.Path)

	out, err := i.Store.UpsertBookFile(ctx, in)
	if errors.Is(err, store.ErrBlocked) {
		// An administrator removed these bytes from the installation after
		// this job was queued. Nothing was recorded; the job is done.
		i.dropBlocked(ctx, payload, in.CoverPath)
		report(1, "")
		return nil
	}
	if err != nil {
		return fmt.Errorf("persisting %s: %w", payload.Path, err)
	}

	// An archived copy this upload replaced: the row points at the new copy,
	// so the old one would sit in the archive unreferenced forever. Only for
	// an upload, only once the store has confirmed the move - which it does
	// only onto a path that exists - and only under the file's lock, after
	// checking again that nothing points at the old copy: the archive may
	// have moved the book there since this upsert committed.
	if payload.Uploaded && out.Moved && within(i.ArchiveDir, out.PreviousPath) {
		if err := i.Store.RemoveIfUnreferenced(ctx, payload.ContentHash, out.PreviousPath, removeFile); err != nil {
			i.logger().Warn("removing a replaced archive copy failed",
				"path", out.PreviousPath, "error", err)
		}
	}

	// The cover this extraction displaced. Nothing references it now, and
	// leaving it would mean one orphan per book per re-extract.
	if out.ReplacedCover != "" && i.CoverDir != "" {
		if base := filepath.Base(out.ReplacedCover); base == out.ReplacedCover {
			_ = os.Remove(filepath.Join(i.CoverDir, base))
		}
	}

	// A file that could not be parsed is recorded with its error and the job
	// SUCCEEDS. Retrying will not make a corrupt EPUB parse - nor teach
	// Lexarii a format it does not implement - and a queue full of permanently
	// failing jobs buries the real failures.
	if parseErr != nil {
		i.logger().Info("file recorded as unreadable",
			"path", payload.Path, "error", parseErr, "bookFile", out.BookFileID)
	} else {
		i.queueChunking(ctx, out.BookFileID)
		i.queueEnrichment(ctx, out.WorkID, job.RequestedBy)
	}

	report(1, "")
	return nil
}

// dropBlocked deletes what an extraction of blocked bytes left on disk: the
// cover it has just written, which nothing references, and the upload's own
// copy, which the upload directory holds only for this extraction. Under the
// file's lock and only while the bytes are still blocked
// (DiscardBlockedUpload). A scanned file is in the library directory, which
// is read-only, and is never touched.
func (i *Ingest) dropBlocked(ctx context.Context, payload ExtractPayload, cover string) {
	if cover != "" && i.CoverDir != "" && filepath.Base(cover) == cover {
		_ = os.Remove(filepath.Join(i.CoverDir, cover))
	}
	i.logger().Info("extraction dropped: the file is blocked", "contentHash", payload.ContentHash)
	if !payload.Uploaded || !within(i.UploadDir, payload.Path) {
		return
	}
	if err := i.Store.DiscardBlockedUpload(ctx, payload.ContentHash, func() error {
		return removeFile(payload.Path)
	}); err != nil {
		i.logger().Warn("removing a blocked upload failed", "path", payload.Path, "error", err)
	}
}

// queueChunking asks for a file to be chunked, when indexing on ingest is on
// and the file is waiting for it.
//
// Decided by the database rather than by this extraction, so it is right
// however the job got here: a re-run of an extraction interrupted after the
// file was stored still queues it, while re-extracting a file that is already
// chunked does not throw its passages and their embeddings away to make them
// again. The check and the insert are one statement, and the job is keyed on
// the file, so two extractions racing still leave one waiting.
//
// Never a reason to fail the extraction. The book is stored and readable, and
// POST /api/v1/index will pick up anything this missed.
func (i *Ingest) queueChunking(ctx context.Context, bookFileID uuid.UUID) {
	if i.Indexing == nil {
		return
	}
	if _, err := i.Indexing.EnqueueIfNeedsChunks(ctx, ChunkJob(bookFileID), rag.StrategyVersion); err != nil {
		i.logger().Warn("could not queue indexing for a new file", "bookFile", bookFileID, "error", err)
	}
}

// queueEnrichment asks for a work to be described, when enrichment on ingest
// is on and the work is waiting for it - the same rule the administrator's
// sweep uses, checked in the same statement as the insert. The job is keyed
// on the work, so a book arriving in two formats is described once.
//
// Never a reason to fail the extraction: the book is stored and readable, and
// POST /api/v1/enrich will pick up anything this missed. Blocked bytes never
// get here - their extraction records nothing - and a work whose only file
// was removed has no readable file to describe.
func (i *Ingest) queueEnrichment(ctx context.Context, workID, requestedBy uuid.UUID) {
	if i.Enriching == nil || workID == uuid.Nil {
		return
	}
	if _, err := i.Enriching.EnqueueIfNeedsEnrichment(ctx, EnrichJob(workID, requestedBy), EnrichPromptID()); err != nil {
		i.logger().Warn("could not queue enrichment for a new book", "workId", workID, "error", err)
	}
}

// TargetForHash derives a stable job target id from a content hash.
//
// Extract jobs are enqueued before any book_file row exists, so there is no
// real id to deduplicate on. A UUIDv5 over the hash gives every distinct file a
// stable target, which lets the queue's partial unique index reject a duplicate
// enqueue rather than leaving it to a racy read-then-write in Go.
func TargetForHash(contentHash string) uuid.UUID {
	return uuid.NewSHA1(targetNamespace, []byte(contentHash))
}

// TargetForUpload derives the target of one person's upload of a file.
//
// Per person, not per file, and that is deliberate. With the file's own target
// a second person's upload was folded into the first person's job, and the
// reply handed them that job's id - which told them somebody else here had just
// added the same book, and pointed them at a job that is not theirs to follow.
// Two people uploading the same bytes in the same minute now cost one extra
// extraction, which is idempotent and keyed on the hash; the same person
// uploading twice is still one job.
func TargetForUpload(contentHash string, user uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(targetNamespace, []byte(contentHash+"/"+user.String()))
}

// targetNamespace is a fixed UUIDv5 namespace for Lexarii ingest targets.
var targetNamespace = uuid.MustParse("5f1e1f00-0000-4000-8000-5348454c464d")

// isSettled reports whether a file has been through extraction and does not
// need doing again.
//
// needs_ocr counts as settled: re-parsing a scan will produce the same absence
// of text every time, and OCR is a separate, explicit step.
// isSettled reports a file a scan can skip re-parsing.
//
// UNSUPPORTED is deliberately not settled, which it used to be. "Unsupported"
// is a statement about this version of Lexarii and not about the file: the
// day MOBI became readable, every MOBI already on the shelf stayed unreadable
// forever, because a scan looked at the status it had written itself and moved
// on. Nobody would have connected the two.
//
// The cost is one extraction attempt per unsupported file per scan, and for a
// format that really is unsupported that attempt is a switch statement and a
// stat. A comic costs microseconds; a format that has just started working
// costs one parse, once.
func isSettled(status domain.IngestStatus) bool {
	switch status {
	case domain.IngestReady, domain.IngestNeedsOCR:
		return true
	default:
		return false
	}
}

func (i *Ingest) logger() *slog.Logger {
	if i.Logger != nil {
		return i.Logger
	}
	return slog.Default()
}

// removeFile removes a file, treating one already gone as removed.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// within reports whether path lies strictly inside dir. An empty dir contains
// nothing, so a missing setting can never widen what may be removed.
func within(dir, path string) bool {
	if dir == "" || path == "" {
		return false
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	full, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, full)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
