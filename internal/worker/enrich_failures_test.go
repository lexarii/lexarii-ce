// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// unsaveableEnrichStore is fakeEnrichStore with a database that refuses the
// final write.
type unsaveableEnrichStore struct {
	fakeEnrichStore
	saveErr error
}

func (s *unsaveableEnrichStore) SaveEnrichment(context.Context, postgres.EnrichmentInput) (postgres.EnrichmentOutcome, error) {
	return postgres.EnrichmentOutcome{}, s.saveErr
}

func TestEnrichHandlersCoverEnrichment(t *testing.T) {
	handlers := (&Enrich{}).Handlers()
	if len(handlers) != 1 || handlers[domain.JobEnrich] == nil {
		t.Errorf("handlers = %v, want exactly enrich", handlers)
	}
}

func TestEnrichFailuresAreFailuresNotSkips(t *testing.T) {
	storeDown := errors.New("database unavailable")
	const reply = `{"subjects":["logic"],"summary":"A first course.","title":"Logic","confidence":0.9}`

	t.Run("payload is not JSON", func(t *testing.T) {
		client, calls, _ := modelSaying(t, reply)
		enricher := &Enrich{Store: &fakeEnrichStore{}, Client: client, Logger: quietLogger()}
		job := postgres.Job{ID: uuid.New(), Type: domain.JobEnrich, Payload: []byte("{")}

		err := enricher.Run(t.Context(), job, noProgress)
		if err == nil || errors.Is(err, ErrSkipped) {
			t.Fatalf("error = %v, want a failure", err)
		}
		if calls.Load() != 0 {
			t.Error("the model was asked about an unreadable job")
		}
	})

	t.Run("the work cannot be loaded", func(t *testing.T) {
		client, calls, _ := modelSaying(t, reply)
		enricher := &Enrich{Store: &fakeEnrichStore{err: storeDown}, Client: client, Logger: quietLogger()}

		err := enricher.Run(t.Context(), enrichJob(t, uuid.New()), noProgress)
		if !errors.Is(err, storeDown) || errors.Is(err, ErrSkipped) {
			t.Fatalf("error = %v, want the store's error and not a skip", err)
		}
		if calls.Load() != 0 {
			t.Error("the model was asked about a work that could not be loaded")
		}
	})

	t.Run("the description cannot be saved", func(t *testing.T) {
		client, calls, _ := modelSaying(t, reply)
		s := &unsaveableEnrichStore{saveErr: storeDown}
		s.subject = postgres.EnrichmentSubject{
			Path: writeEPUB(t, "<h1>Logic</h1><p>A first course in logic.</p>"), Format: domain.FormatEPUB,
		}
		enricher := &Enrich{Store: s, Client: client, Logger: quietLogger()}

		err := enricher.Run(t.Context(), enrichJob(t, uuid.New()), noProgress)
		if !errors.Is(err, storeDown) {
			t.Fatalf("error = %v, want the store's error", err)
		}
		if calls.Load() != 1 {
			t.Errorf("model calls = %d, want the one whose answer could not be kept", calls.Load())
		}
	})
}

// A comic has no text, and asking a model to describe one would produce a
// confident answer about nothing.
func TestEnrichAsksNothingAboutAFileWithNoText(t *testing.T) {
	client, calls, _ := modelSaying(t, `{"title":"x"}`)
	s := &fakeEnrichStore{subject: postgres.EnrichmentSubject{
		Path: writeEPUB(t, "<p>x</p>"), Format: domain.FormatCBZ,
	}}
	enricher := &Enrich{Store: s, Client: client, Logger: quietLogger()}

	if err := enricher.Run(t.Context(), enrichJob(t, uuid.New()), noProgress); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if calls.Load() != 0 || len(s.saved) != 0 {
		t.Errorf("model calls = %d, saved = %d; want neither", calls.Load(), len(s.saved))
	}
}
