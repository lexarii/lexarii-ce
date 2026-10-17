// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// A read that could not happen must say so. Returning an empty list would
// tell the reader "nothing in your library" or "no such book" about a library
// that was simply unreachable, and mapping the failure to ErrNotFound would
// turn an aborted request into a 404 for a book that exists.
//
// A cancelled context is the cheapest real failure to produce: it is what a
// client hanging up mid-request looks like to the store.
func TestStoreCallsReportACancelledRequest(t *testing.T) {
	p := testPool(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	viewer, other := uuid.New(), uuid.New()
	parent := uuid.New()

	calls := map[string]func() error{
		"Search": func() error {
			_, err := p.Search(ctx, SearchQuery{Text: "x", ViewerID: viewer})
			return err
		},
		"WorksMatchingSubjects": func() error {
			_, err := p.WorksMatchingSubjects(ctx, []string{"docker"}, 5)
			return err
		},
		"MatchingSubjects": func() error {
			_, err := p.MatchingSubjects(ctx, []string{"docker"})
			return err
		},
		"LibraryFacts": func() error {
			_, err := p.LibraryFacts(ctx, viewer)
			return err
		},
		"Subjects": func() error {
			_, err := p.Subjects(ctx, viewer)
			return err
		},
		"Mentions": func() error {
			_, err := p.Mentions(ctx, other, viewer, 5)
			return err
		},
		"Collections": func() error {
			_, err := p.Collections(ctx, viewer)
			return err
		},
		"CreateCollection": func() error {
			_, err := p.CreateCollection(ctx, viewer, "x", "", nil)
			return err
		},
		"CreateCollection with a parent": func() error {
			_, err := p.CreateCollection(ctx, viewer, "x", "", &parent)
			return err
		},
		"UpdateCollection": func() error {
			_, err := p.UpdateCollection(ctx, viewer, other, "x", "")
			return err
		},
		"DeleteCollection": func() error { return p.DeleteCollection(ctx, viewer, other) },
		"AddToCollection":  func() error { return p.AddToCollection(ctx, viewer, other, other) },
		"RemoveFromCollection": func() error {
			return p.RemoveFromCollection(ctx, viewer, other, other)
		},
		"Collection": func() error {
			_, err := p.Collection(ctx, viewer, other)
			return err
		},
		"CollectionsHolding": func() error {
			_, err := p.CollectionsHolding(ctx, viewer, other)
			return err
		},
		"SetCover": func() error {
			_, err := p.SetCover(ctx, other, viewer, "x.jpg")
			return err
		},
		"Cover": func() error {
			_, err := p.Cover(ctx, other, viewer)
			return err
		},
		"FilesNeedingCovers": func() error {
			_, err := p.FilesNeedingCovers(ctx, viewer, 5)
			return err
		},
		"CountFilesNeedingCovers": func() error {
			_, err := p.CountFilesNeedingCovers(ctx, viewer)
			return err
		},
		"NeedsExternalMetadata": func() error {
			_, err := p.NeedsExternalMetadata(ctx, viewer, 5)
			return err
		},
		"ApplyExternalMetadata": func() error {
			_, err := p.ApplyExternalMetadata(ctx, ExternalMetadata{WorkID: other})
			return err
		},
		"SaveChunks": func() error {
			_, err := p.SaveChunks(ctx, other, nil, 1)
			return err
		},
		"BookFilesNeedingChunks": func() error {
			_, err := p.BookFilesNeedingChunks(ctx, 1, 5)
			return err
		},
		"EnqueueIfNeedsChunks": func() error {
			_, err := p.EnqueueIfNeedsChunks(ctx, EnqueueOptions{Type: domain.JobChunk, TargetID: &other}, 1)
			return err
		},
		"ChunksNeedingEmbedding": func() error {
			_, err := p.ChunksNeedingEmbedding(ctx, searchModel, 5)
			return err
		},
		"SaveEmbeddings": func() error {
			_, err := p.SaveEmbeddings(ctx, searchModel, 1, []EmbeddingInput{{ChunkID: other, Vector: []float32{1}}})
			return err
		},
		"RetrievalStats": func() error {
			_, err := p.RetrievalStats(ctx, searchModel)
			return err
		},
		"CreateConversation": func() error {
			_, err := p.CreateConversation(ctx, viewer, "x", nil)
			return err
		},
		"AppendMessage": func() error {
			_, err := p.AppendMessage(ctx, other, Message{Role: "user", Text: "x"})
			return err
		},
		"Conversation": func() error {
			_, err := p.Conversation(ctx, viewer, other)
			return err
		},
		"Conversations": func() error {
			_, err := p.Conversations(ctx, viewer, 5)
			return err
		},
		"DeleteConversation":   func() error { return p.DeleteConversation(ctx, viewer, other) },
		"SetConversationTitle": func() error { return p.SetConversationTitle(ctx, viewer, other, "x") },
		"VectorDimensions": func() error {
			_, err := p.VectorDimensions(ctx)
			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want it to carry context.Canceled", err)
			}
			if errors.Is(err, store.ErrNotFound) {
				t.Fatalf("an aborted request was reported as not found: %v", err)
			}
		})
	}
}
