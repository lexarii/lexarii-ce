// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// Storer accepts one uploaded file.
type Storer interface {
	Store(ctx context.Context, filename string, body io.Reader) (ingest.StoredFile, error)
	// Settle finishes a stored file once it has been recorded (keep) or
	// refused: see ingest.UploadStore.Settle.
	Settle(f ingest.StoredFile, keep bool) error
}

// UploadEnqueuer records an uploaded file and queues it for extraction.
type UploadEnqueuer interface {
	Enqueue(ctx context.Context, opts postgres.EnqueueOptions) (uuid.UUID, bool, error)
	// HoldUpload puts the file on the uploader's shelf, whoever else has it.
	HoldUpload(ctx context.Context, in postgres.HoldInput) (postgres.HoldOutcome, error)
	// DiscardBlockedUpload deletes, under the file's lock, the copy just
	// stored of bytes an administrator has blocked.
	DiscardBlockedUpload(ctx context.Context, contentHash string, remove func() error) error
}

// UploadHandler accepts bulk uploads.
type UploadHandler struct {
	Store Storer
	Queue UploadEnqueuer
	Log   *slog.Logger
	// Accounts is the admin check, for an installation where only
	// administrators add books.
	Accounts AdminStore
	// AnyoneMayUpload lets every signed-in account add books to its own shelf
	// (LEXARII_UPLOAD_POLICY=users). Off, uploading is for administrators,
	// which is what every installation did before the setting existed.
	//
	// Safe to turn on because an upload never reaches another person's book.
	// The file is the uploader's, visible to nobody else until they lend it.
	// It joins an existing edition only if the uploader already holds a file
	// of it. Uploading bytes somebody else already holds makes the uploader a
	// co-holder of that one file and nothing more: changing or merging a work
	// takes holding every file of it (work_editors), so a co-holder of one
	// copy cannot retitle, or merge away, a book whose other copies are
	// somebody else's. Co-holders of every file of a work do share it, and may
	// each change it - it is the same book on both their shelves.
	AnyoneMayUpload bool
}

// Create serves POST /api/v1/uploads.
//
// The request is read as a multipart stream and each part is written straight
// to disk. Nothing calls ParseMultipartForm: that buffers the whole request,
// which for a bulk upload of a few hundred books is gigabytes of server memory.
//
// Every file is handled independently and reported individually. A bulk upload
// over a flaky connection will lose files partway through, and the response has
// to say exactly which ones landed - the user's recovery is to re-upload the
// batch, and deduplication makes that cheap.
func (h *UploadHandler) Create(w http.ResponseWriter, r *http.Request) {
	var uploader uuid.UUID
	var ok bool
	if h.AnyoneMayUpload {
		uploader, ok = userOrUnauthorised(w, r)
	} else {
		uploader, ok = adminOrForbidden(w, r, h.Accounts)
	}
	if !ok {
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		respondError(w, Invalid("request must be multipart/form-data", err))
		return
	}

	out := dto.UploadResult{Files: []dto.UploadedFile{}}

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// The connection broke partway through the batch. Everything already
			// accepted is kept and reported: the files that made it are real
			// books now, and re-uploading the rest costs only the transfer.
			out.Interrupted = true
			out.Message = "the upload was interrupted; the files listed were stored, and re-uploading the batch will skip them"
			h.logger().Warn("upload interrupted", "error", err, "storedSoFar", len(out.Files))
			break
		}

		if part.FormName() != "files" && part.FileName() == "" {
			_ = part.Close()
			continue
		}

		result, interrupted, err := h.storeOne(r.Context(), part, uploader)
		_ = part.Close()
		if err != nil {
			// The file could not be put on the uploader's shelf. Answering
			// "stored" would be untrue - nothing else will put it there - so
			// the request fails. What came before it in the batch is stored
			// and theirs, and uploading the batch again finishes it.
			h.logger().Error("recording an upload's holding failed",
				"file", result.Filename, "storedSoFar", len(out.Files), "error", err)
			respondError(w, &httpError{
				status: http.StatusInternalServerError,
				code:   CodeInternal,
				message: "a file could not be added to your library; the files before it were, " +
					"and uploading the batch again will add the rest",
				cause: err,
			})
			return
		}
		out.Files = append(out.Files, result)

		// A truncated transfer surfaces as a failed READ of a part, not as a
		// failure of NextPart. Without this it would be reported as a rejected
		// file, telling the user their book was bad when the connection was.
		if interrupted {
			out.Interrupted = true
			out.Message = "the transfer was cut short; the files listed as stored are safe, and re-uploading the batch will skip them"
			h.logger().Warn("upload truncated", "storedSoFar", len(out.Files))
			break
		}

		if r.Context().Err() != nil {
			out.Interrupted = true
			out.Message = "the client disconnected; the files listed as stored are safe"
			break
		}
	}

	blocked := 0
	for _, f := range out.Files {
		switch f.Status {
		case dto.UploadStored:
			out.Summary.Stored++
		case dto.UploadDuplicate:
			out.Summary.Duplicate++
		default:
			out.Summary.Rejected++
		}
		if f.Code == string(codeContentBlocked) {
			blocked++
		}
	}

	// Nothing but blocked files: refused as a whole, in the standard error
	// shape, so a client uploading one file need not read a batch report to
	// learn it was turned away. A batch with anything else in it is reported
	// file by file as ever, the blocked ones as rejected with this code.
	if len(out.Files) > 0 && blocked == len(out.Files) {
		respondError(w, &httpError{
			status:  http.StatusUnprocessableEntity,
			code:    codeContentBlocked,
			message: blockedMessage,
		})
		return
	}

	status := http.StatusAccepted
	if len(out.Files) == 0 {
		status = http.StatusBadRequest
		out.Message = "the request contained no files"
	}
	respondJSON(w, status, out)
}

// storeOne handles a single part, converting every failure of the FILE into a
// reported outcome rather than an error that would abandon the rest of the
// batch. The error it does return is the installation's, not the file's: the
// bytes are stored but could not be put on the uploader's shelf.
func (h *UploadHandler) storeOne(ctx context.Context, part *multipart.Part, owner uuid.UUID) (dto.UploadedFile, bool, error) {
	name := part.FileName()
	result := dto.UploadedFile{Filename: name}

	stored, err := h.Store.Store(ctx, name, part)
	if err != nil {
		result.Status = dto.UploadRejected
		result.Error = uploadErrorMessage(err)
		// A transport failure is not a bad file. Telling the user their book was
		// rejected when the connection dropped sends them looking for a problem
		// with the book.
		return result, isTransportFailure(err), nil
	}

	result.Filename = stored.OriginalFilename
	result.ContentHash = stored.ContentHash
	result.Format = string(stored.Format)
	result.SizeBytes = stored.SizeBytes

	// The file goes on this person's shelf whatever else is true of it. Who
	// else holds the same bytes, and whether their extraction is still running,
	// must not change the answer: "duplicate" is only ever about the person
	// asking. Anything else would tell them what somebody else here owns.
	//
	// Recording the holding is not optional. The extraction queued below
	// respects a release the person made earlier - only this deliberate
	// upload forgets it - so a job alone would never put back a book they let
	// go and are now adding again.
	hold, herr := h.Queue.HoldUpload(ctx, postgres.HoldInput{
		ContentHash:      stored.ContentHash,
		UserID:           owner,
		OriginalFilename: stored.OriginalFilename,
		// Under the bytes' lock, as the upload is recorded: the stored copy
		// may have been erased since it was written - with a record of the
		// same bytes - and this upload's own copy takes its place. Nothing
		// deleting copies of these bytes can run in between.
		Settle: func() error { return h.Store.Settle(stored, true) },
	})
	if herr != nil {
		// Not recorded: the copy kept aside goes, and is put in place of
		// nothing.
		if err := h.Store.Settle(stored, false); err != nil {
			h.logger().Warn("removing an upload's spare copy failed", "contentHash", stored.ContentHash, "error", err)
		}
	}
	if errors.Is(herr, store.ErrBlocked) {
		// An administrator removed these bytes from the installation. The
		// copy just written goes again, and the answer is about the bytes
		// alone: who else added them, and who removed them, is not in it.
		if err := h.Queue.DiscardBlockedUpload(ctx, stored.ContentHash, func() error {
			return removeIfPresent(stored.Path)
		}); err != nil {
			h.logger().Warn("removing a blocked upload failed", "path", stored.Path, "error", err)
		}
		result.Status = dto.UploadRejected
		result.Code = string(codeContentBlocked)
		result.Error = blockedMessage
		return result, false, nil
	}
	if herr != nil {
		return result, false, fmt.Errorf("holding %s: %w", stored.ContentHash, herr)
	}

	// Already theirs AND already ingested: nothing to do at all. Already theirs
	// but never ingested - an upload that landed before a crash - still needs
	// extracting, so the status is checked rather than assumed. So does one
	// whose recorded copy has gone missing: this upload is the copy that
	// exists now, and only an extraction points the file at it. And so does
	// one whose extraction failed - possibly because its bytes were not there
	// at the time - since uploading it again is how somebody says "try again".
	if hold.AlreadyHeld && !hold.Missing &&
		hold.Status != "" && hold.Status != domain.IngestPending &&
		hold.Status != domain.IngestFailed {
		result.Status = dto.UploadDuplicate
		return result, false, nil
	}

	// This person's own target, so the job id in the reply is always one they
	// may follow and never somebody else's.
	target := worker.TargetForUpload(stored.ContentHash, owner)
	jobID, _, err := h.Queue.Enqueue(ctx, postgres.EnqueueOptions{
		Type:        domain.JobExtract,
		TargetID:    &target,
		RequestedBy: owner,
		Payload: worker.ExtractPayload{
			Path:             stored.Path,
			ContentHash:      stored.ContentHash,
			Format:           stored.Format,
			SizeBytes:        stored.SizeBytes,
			OriginalFilename: stored.OriginalFilename,
			Uploaded:         true,
			OwnerID:          owner,
		},
	})
	if err != nil {
		// The bytes are safely stored; only the queueing failed. Say so
		// precisely, because the file is not lost and a rescan will pick it up.
		result.Status = dto.UploadStored
		result.Error = "stored, but could not be queued for processing: " + err.Error()
		h.logger().Error("queueing an upload failed", "file", stored.OriginalFilename, "error", err)
		return result, false, nil
	}

	result.Status = dto.UploadStored
	result.JobID = jobID.String()
	if hold.AlreadyHeld {
		// Only ever about this person's own earlier upload.
		result.Note = "already in your library; queued for processing again"
	}
	return result, false, nil
}

// codeContentBlocked refuses bytes an administrator has removed from the
// installation and blocked.
const codeContentBlocked ErrorCode = "content_blocked"

// blockedMessage says what happened to the file and nothing about anybody
// else: not who added these bytes before, nor who blocked them, nor why.
const blockedMessage = "this file cannot be added to this library"

// removeIfPresent removes a file, treating one already gone as removed.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// isTransportFailure reports whether an error means the transfer broke rather
// than the file being unacceptable.
func isTransportFailure(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, multipart.ErrMessageTooLarge)
}

// uploadErrorMessage turns a storage error into something worth showing a user.
func uploadErrorMessage(err error) string {
	switch {
	case errors.Is(err, ingest.ErrUnsupportedUpload):
		return "not a supported book format (epub, pdf, mobi, azw3, cbz, cbr)"
	case errors.Is(err, ingest.ErrEmptyUpload):
		return "the file is empty"
	case errors.Is(err, ingest.ErrUploadTooLarge):
		return "the file is too large"
	case errors.Is(err, ingest.ErrBadFilename):
		return "the filename is unusable"
	case errors.Is(err, context.Canceled):
		return "the transfer was interrupted"
	default:
		return "could not be stored: " + err.Error()
	}
}

func (h *UploadHandler) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}
