// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// JobQueue is the slice of the job store the HTTP layer needs.
type JobQueue interface {
	Enqueue(ctx context.Context, opts postgres.EnqueueOptions) (uuid.UUID, bool, error)
	// GetJobFor reads a job only for the person it was queued for or an
	// administrator, and reports store.ErrNotFound to anybody else.
	GetJobFor(ctx context.Context, id, viewer uuid.UUID) (postgres.Job, error)
	Cancel(ctx context.Context, id uuid.UUID) error
}

// JobHandler starts background work and reports on it.
type JobHandler struct {
	Queue JobQueue
	// Accounts is the admin check. Scanning and cancelling change the shared
	// library, so they are not a reader's to do.
	Accounts AdminStore

	// PollInterval is how often an open SSE stream re-reads the job. Defaults to
	// 500ms, which is frequent enough to feel live and cheap enough that a few
	// open tabs do not matter.
	PollInterval time.Duration
}

// StartScan serves POST /api/v1/scan.
//
// Scanning a library of thousands of books takes minutes, so the request
// returns 202 with a job id immediately and progress is followed over SSE.
func (h *JobHandler) StartScan(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminOrForbidden(w, r, h.Accounts)
	if !ok {
		return
	}
	// A scan has no target id, so the queue's per-target deduplication does not
	// apply to it. That is deliberate: a user who adds books and asks for a
	// rescan should get one.
	id, created, err := h.Queue.Enqueue(r.Context(), postgres.EnqueueOptions{
		// Ahead of everything else in the queue: see domain.JobType.Priority.
		Type: domain.JobScan,
		// Books found on disk belong to whoever asked for the scan. Nothing
		// else has a claim on them, and a file with no holder is visible to
		// nobody.
		Payload:     worker.ScanPayload{OwnerID: admin},
		RequestedBy: admin,
	})
	if err != nil {
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusAccepted, dto.JobAccepted{JobID: id.String(), Created: created})
}

// Get serves GET /api/v1/jobs/{id}.
//
// For the person the job was queued for, and for administrators. Anybody else
// gets the same 404 as for a job that does not exist.
func (h *JobHandler) Get(w http.ResponseWriter, r *http.Request) {
	viewer, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("job id must be a UUID", err))
		return
	}

	job, err := h.Queue.GetJobFor(r.Context(), id, viewer)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("job"))
			return
		}
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, jobDTO(job))
}

// Cancel serves DELETE /api/v1/jobs/{id}.
func (h *JobHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminOrForbidden(w, r, h.Accounts); !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("job id must be a UUID", err))
		return
	}

	if err := h.Queue.Cancel(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Either the job does not exist or it has already finished. Both are
			// "there is nothing to cancel".
			respondError(w, NotFound("running job"))
			return
		}
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusNoContent, nil)
}

// Events serves GET /api/v1/jobs/{id}/events as Server-Sent Events.
//
// SSE rather than WebSockets: progress is one-directional, proxies handle it
// without special configuration, and browsers reconnect on their own.
//
// The stream always ends with a terminal event. A stream that can hang with no
// end signal is a client bug generator - the UI would spin forever on a job
// that finished while the connection was briefly wedged.
func (h *JobHandler) Events(w http.ResponseWriter, r *http.Request) {
	viewer, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("job id must be a UUID", err))
		return
	}

	// ResponseController rather than a w.(http.Flusher) assertion: middleware
	// wraps the writer, and a type assertion sees only the outermost wrapper.
	rc := http.NewResponseController(w)

	// Confirm the job exists, and is this person's to follow, before switching
	// to an event stream: once headers are sent, a 404 can no longer be
	// reported.
	job, err := h.Queue.GetJobFor(r.Context(), id, viewer)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("job"))
			return
		}
		respondError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Nginx buffers proxied responses by default, which would hold every event
	// until the stream closed.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	interval := h.PollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}

	writeEvent(w, rc, "progress", jobDTO(job))
	if job.Status.Terminal() {
		writeEvent(w, rc, "done", jobDTO(job))
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// A heartbeat keeps intermediaries from closing an idle connection during a
	// long, quiet job.
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	last := job
	for {
		select {
		case <-r.Context().Done():
			// The client went away. Nothing to clean up: the job runs on.
			return

		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			_ = rc.Flush()

		case <-ticker.C:
			job, err := h.Queue.GetJobFor(r.Context(), id, viewer)
			if err != nil {
				if r.Context().Err() != nil {
					return
				}
				writeEvent(w, rc, "error", map[string]string{
					"message": "lost track of this job: " + err.Error(),
				})
				return
			}

			if changed(last, job) {
				writeEvent(w, rc, "progress", jobDTO(job))
				last = job
			}

			if job.Status.Terminal() {
				writeEvent(w, rc, "done", jobDTO(job))
				return
			}
		}
	}
}

// changed reports whether anything worth sending has moved.
func changed(a, b postgres.Job) bool {
	return a.Status != b.Status ||
		a.Progress != b.Progress ||
		a.ProgressNote != b.ProgressNote ||
		a.Attempts != b.Attempts ||
		a.LastError != b.LastError
}

// writeEvent emits one SSE event. Write errors mean the client is gone, which
// the caller detects on its next write.
func writeEvent(w http.ResponseWriter, rc *http.ResponseController, event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return
	}
	// A flush failure means the client is gone; the caller notices on its next
	// write, and there is nothing useful to do here.
	_ = rc.Flush()
}

func jobDTO(j postgres.Job) dto.Job {
	out := dto.Job{
		ID:           j.ID.String(),
		Type:         string(j.Type),
		Status:       string(j.Status),
		Progress:     j.Progress,
		ProgressNote: j.ProgressNote,
		Attempts:     j.Attempts,
		MaxAttempts:  j.MaxAttempts,
		LastError:    j.LastError,
		CreatedAt:    j.CreatedAt,
	}
	if !j.RunAfter.IsZero() && j.RunAfter.After(time.Now()) {
		// Only meaningful while the job is waiting out a backoff.
		runAfter := j.RunAfter
		out.RunAfter = &runAfter
	}
	return out
}
