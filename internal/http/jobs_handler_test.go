// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

var errJobsDown = errors.New("job store unavailable")

// jobScript answers each read of a job with the next scripted result, a job
// or an error, and holds on the last.
type jobScript struct {
	mu    sync.Mutex
	steps []jobStep
	calls int
}

type jobStep struct {
	job postgres.Job
	err error
}

func (q *jobScript) Enqueue(context.Context, postgres.EnqueueOptions) (uuid.UUID, bool, error) {
	return uuid.Nil, false, errJobsDown
}
func (q *jobScript) GetJobFor(context.Context, uuid.UUID, uuid.UUID) (postgres.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	i := min(q.calls, len(q.steps)-1)
	q.calls++
	return q.steps[i].job, q.steps[i].err
}
func (q *jobScript) Cancel(context.Context, uuid.UUID) error { return errJobsDown }

// jobAdminLookupFails is an account store that cannot be reached.
type jobAdminLookupFails struct{}

func (jobAdminLookupFails) User(context.Context, uuid.UUID) (postgres.UserRecord, error) {
	return postgres.UserRecord{}, errJobsDown
}

func jobServe(t *testing.T, h *JobHandler, method, target string, signedIn bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if signedIn {
		req = asUser(req)
	}
	rec := httptest.NewRecorder()
	(&Router{Health: &HealthHandler{}, Jobs: h}).Handler().ServeHTTP(rec, req)
	return rec
}

// A scan's books belong to whoever asked for it, and so does the job.
func TestAScanBelongsToWhoeverAskedForIt(t *testing.T) {
	q := &fakeQueue{enqueueID: uuid.New(), enqueueCreated: true}
	admin := uuid.New()
	rec := httptest.NewRecorder()
	(&JobHandler{Queue: q, Accounts: anAdmin}).StartScan(rec,
		asUserID(httptest.NewRequest(http.MethodPost, "/api/v1/scan", nil), admin))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	payload, ok := q.lastEnqueue.Payload.(worker.ScanPayload)
	if q.lastEnqueue.Type != domain.JobScan || q.lastEnqueue.RequestedBy != admin || !ok || payload.OwnerID != admin {
		t.Errorf("enqueued %+v, want a scan owned by and requested by %v", q.lastEnqueue, admin)
	}
}

func TestJobRefusalsAndFailures(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct {
		name     string
		h        *JobHandler
		method   string
		target   string
		signedIn bool
		status   int
	}{
		{"scan signed out", &JobHandler{Queue: &fakeQueue{}, Accounts: anAdmin}, http.MethodPost, "/api/v1/scan", false,
			http.StatusUnauthorized},
		{"scan when the queue fails", &JobHandler{Queue: &fakeQueue{enqueueErr: errJobsDown}, Accounts: anAdmin},
			http.MethodPost, "/api/v1/scan", true, http.StatusInternalServerError},
		{"scan when the admin check fails", &JobHandler{Queue: &fakeQueue{}, Accounts: jobAdminLookupFails{}},
			http.MethodPost, "/api/v1/scan", true, http.StatusInternalServerError},
		{"read signed out", &JobHandler{Queue: &fakeQueue{}}, http.MethodGet, "/api/v1/jobs/" + id, false,
			http.StatusUnauthorized},
		{"read a bad id", &JobHandler{Queue: &fakeQueue{}}, http.MethodGet, "/api/v1/jobs/nope", true,
			http.StatusBadRequest},
		{"read when the store fails", &JobHandler{Queue: &fakeQueue{getErr: errJobsDown}}, http.MethodGet,
			"/api/v1/jobs/" + id, true, http.StatusInternalServerError},
		{"cancel a bad id", &JobHandler{Queue: &fakeQueue{}, Accounts: anAdmin}, http.MethodDelete, "/api/v1/jobs/nope", true,
			http.StatusBadRequest},
		{"cancel when the store fails", &JobHandler{Queue: &fakeQueue{cancelErr: errJobsDown}, Accounts: anAdmin},
			http.MethodDelete, "/api/v1/jobs/" + id, true, http.StatusInternalServerError},
		{"follow signed out", &JobHandler{Queue: &fakeQueue{}}, http.MethodGet, "/api/v1/jobs/" + id + "/events", false,
			http.StatusUnauthorized},
		{"follow when the store fails", &JobHandler{Queue: &fakeQueue{getErr: errJobsDown}}, http.MethodGet,
			"/api/v1/jobs/" + id + "/events", true, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := jobServe(t, tc.h, tc.method, tc.target, tc.signedIn)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); ct == "text/event-stream" {
				t.Error("a refused request opened a stream")
			}
		})
	}
}

// With no account store there are no accounts to check against; the signed-in
// user is let through.
func TestTheAdminGuardWithNoAccountStore(t *testing.T) {
	q := &fakeQueue{enqueueID: uuid.New(), enqueueCreated: true}
	rec := jobServe(t, &JobHandler{Queue: q}, http.MethodPost, "/api/v1/scan", true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
}

// A job waiting out a backoff says when it will run again; one that is not
// waiting does not.
func TestAJobInBackoffSaysWhenItRunsNext(t *testing.T) {
	later := time.Now().Add(time.Hour)
	waiting := postgres.Job{ID: uuid.New(), Type: domain.JobExtract, Status: domain.JobPending, RunAfter: later}
	rec := jobServe(t, &JobHandler{Queue: &fakeQueue{states: []postgres.Job{waiting}}}, http.MethodGet,
		"/api/v1/jobs/"+waiting.ID.String(), true)
	var out dto.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.RunAfter == nil || !out.RunAfter.Equal(later) {
		t.Errorf("runAfter = %v, want %v", out.RunAfter, later)
	}

	past := postgres.Job{ID: uuid.New(), Status: domain.JobPending, RunAfter: time.Now().Add(-time.Hour)}
	rec = jobServe(t, &JobHandler{Queue: &fakeQueue{states: []postgres.Job{past}}}, http.MethodGet,
		"/api/v1/jobs/"+past.ID.String(), true)
	var notWaiting dto.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &notWaiting); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if notWaiting.RunAfter != nil {
		t.Errorf("runAfter = %v for a job not waiting", notWaiting.RunAfter)
	}
}

// A stream that loses track of its job ends with an error event rather than
// hanging, so the client stops waiting.
func TestAStreamThatLosesItsJobEndsWithAnError(t *testing.T) {
	id := uuid.New()
	q := &jobScript{steps: []jobStep{
		{job: postgres.Job{ID: id, Type: domain.JobScan, Status: domain.JobRunning}},
		{err: errJobsDown},
	}}
	rec := jobServe(t, &JobHandler{Queue: q, PollInterval: time.Millisecond}, http.MethodGet,
		"/api/v1/jobs/"+id.String()+"/events", true)

	got := parseSSE(t, rec.Body.String())
	if len(got) != 2 || got[0][0] != "progress" || got[1][0] != "error" {
		t.Fatalf("events = %v, want progress then error", got)
	}
	var e map[string]string
	if err := json.Unmarshal([]byte(got[1][1]), &e); err != nil || e["message"] == "" {
		t.Errorf("error event = %s", got[1][1])
	}
}

// A finished job's stream ends at once, whatever the poll interval - here the
// default.
func TestAFinishedJobsStreamUsesNoPolling(t *testing.T) {
	done := postgres.Job{ID: uuid.New(), Status: domain.JobSucceeded, Progress: 1}
	q := &fakeQueue{states: []postgres.Job{done}}
	rec := jobServe(t, &JobHandler{Queue: q}, http.MethodGet, "/api/v1/jobs/"+done.ID.String()+"/events", true)
	got := parseSSE(t, rec.Body.String())
	if len(got) != 2 || got[1][0] != "done" {
		t.Fatalf("events = %v, want progress then done", got)
	}
	if q.calls != 1 {
		t.Errorf("read the job %d times, want once", q.calls)
	}
}
