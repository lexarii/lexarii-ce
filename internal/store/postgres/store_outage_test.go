// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// When the database cannot be asked, every store method says so, with the
// cause intact - it never answers as though the question had been asked.
//
// The difference matters most where a negative answer is itself a decision:
// "nobody can sign in yet" reopens first-run setup, "this is not blocked" lets
// removed bytes back in, "not an administrator" and "not found" send the
// caller down a refusal path and hide the outage. A request whose context has
// been cancelled - a client gone away, a shutdown - is the everyday way this
// happens, and the cause has to survive every layer of wrapping so the HTTP
// layer can tell a cancelled request from a fault.
func TestAnUnreachableDatabaseIsAnErrorNeverAnAnswer(t *testing.T) {
	p := testPool(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	id := uuid.New()
	other := uuid.New()
	hash := hashOf("9")
	epub := domain.Locator{Format: domain.FormatEPUB, SpineIndex: 1, ElementPath: "p[1]"}
	mark := domain.Annotation{
		UserID: id, BookFileID: other, Type: domain.AnnotationHighlight,
		Range: domain.Range{Start: epub, End: epub}, SelectedText: "x",
	}
	ignore := func(string) (string, error) { return "", nil }

	calls := map[string]func() error{
		// auth.go
		"LocalIdentity": func() error { _, _, err := p.LocalIdentity(ctx, "someone"); return err },
		"UpsertIdentity": func() error {
			_, _, err := p.UpsertIdentity(ctx, auth.Identity{Provider: "oidc", Subject: "s"}, true)
			return err
		},
		"CreateLocalUser": func() error {
			_, err := p.CreateLocalUser(ctx, "someone", "", localUserPassword, false)
			return err
		},
		"SetLocalPassword": func() error { return p.SetLocalPassword(ctx, id, localUserPassword) },
		"HasAnyUser":       func() error { _, err := p.HasAnyUser(ctx); return err },
		"AdoptOrCreateLocalUser": func() error {
			_, err := p.AdoptOrCreateLocalUser(ctx, "someone", "", localUserPassword, true)
			return err
		},
		"User": func() error { _, err := p.User(ctx, id); return err },
		"CreateSession": func() error {
			_, err := p.CreateSession(ctx, "h", id.String(), "", "", "", "", time.Now().Add(time.Hour))
			return err
		},
		"RevokeSession":        func() error { return p.RevokeSession(ctx, "h") },
		"RevokeAllSessions":    func() error { return p.RevokeAllSessions(ctx, id.String()) },
		"UserSessions":         func() error { _, err := p.UserSessions(ctx, id.String()); return err },
		"RevokeSessionByID":    func() error { return p.RevokeSessionByID(ctx, id.String(), other.String()) },
		"PurgeExpiredSessions": func() error { _, err := p.PurgeExpiredSessions(ctx, time.Hour); return err },

		// library.go and library_query.go
		"UpsertBookFile": func() error {
			_, err := p.UpsertBookFile(ctx, owned(sampleInput(hash, "/l/x.epub", "T", "A"), id))
			return err
		},
		"MarkMissing":    func() error { _, err := p.MarkMissing(ctx, time.Now()); return err },
		"TouchSeen":      func() error { return p.TouchSeen(ctx, hash, "/l/x.epub", id) },
		"BookFileStatus": func() error { _, err := p.BookFileStatus(ctx, hash); return err },
		"BookFilePath":   func() error { _, err := p.BookFilePath(ctx, hash); return err },
		"Library":        func() error { _, err := p.Library(ctx, LibraryQuery{ViewerID: id}); return err },
		"Stats":          func() error { _, err := p.Stats(ctx, id); return err },
		"Work":           func() error { _, err := p.Work(ctx, other, id); return err },
		"ReadableBookWithWork": func() error {
			_, err := p.ReadableBookWithWork(ctx, other, id)
			return err
		},
		"ReadableBookFile": func() error { _, err := p.ReadableBookFile(ctx, other, id); return err },
		"CanDownload":      func() error { _, err := p.CanDownload(ctx, other, id); return err },
		"BookFile":         func() error { _, err := p.BookFile(ctx, other); return err },

		// sync.go
		"Changes": func() error { _, err := p.Changes(ctx, id, time.Now(), "", 10); return err },
		"ApplyMarks": func() error {
			_, err := p.ApplyMarks(ctx, id, []PushedMark{{ID: other, BookFileID: other}})
			return err
		},
		"MergePositions": func() error { return p.MergePositions(ctx, id, []PushedPosition{{BookFileID: other, Position: epub}}) },

		// enrichment.go
		"SaveEnrichment":     func() error { _, err := p.SaveEnrichment(ctx, EnrichmentInput{WorkID: other}); return err },
		"EnrichmentSubject":  func() error { _, err := p.EnrichmentSubject(ctx, other); return err },
		"PendingSuggestions": func() error { _, err := p.PendingSuggestions(ctx, 10); return err },
		"WorksNeedingEnrichment": func() error {
			_, err := p.WorksNeedingEnrichment(ctx, "classify/v1", 10)
			return err
		},
		"EnqueueIfNeedsEnrichment": func() error {
			_, err := p.EnqueueIfNeedsEnrichment(ctx, EnqueueOptions{Type: domain.JobEnrich, TargetID: &other}, "classify/v1")
			return err
		},
		"ResolveSuggestion": func() error { return p.ResolveSuggestion(ctx, other, true) },

		// moderation.go
		"IsBlocked": func() error { _, err := p.IsBlocked(ctx, hash); return err },
		"RemoveBookFileForEveryone": func() error {
			_, err := p.RemoveBookFileForEveryone(ctx, other, id, "a reason", RemovalOptions{})
			return err
		},
		"DiscardBlockedCopies": func() error { return p.DiscardBlockedCopies(ctx, hash, func(StoredCopies) error { return nil }) },
		"DiscardBlockedUpload": func() error { return p.DiscardBlockedUpload(ctx, hash, func() error { return nil }) },
		"BlocksWithCopies":     func() error { _, err := p.BlocksWithCopies(ctx); return err },
		"BlockedFiles":         func() error { _, err := p.BlockedFiles(ctx, id); return err },
		"Unblock":              func() error { return p.Unblock(ctx, id, hash) },

		// sharing.go
		"People":             func() error { _, err := p.People(ctx, id); return err },
		"BookShares":         func() error { _, err := p.BookShares(ctx, other, id); return err },
		"ShareBook":          func() error { return p.ShareBook(ctx, other, id, uuid.New(), false, false) },
		"UnshareBook":        func() error { return p.UnshareBook(ctx, other, id, uuid.New()) },
		"RecordAccess":       func() error { return p.RecordAccess(ctx, other, id, AccessChapter, 1) },
		"BookAccessLog":      func() error { _, err := p.BookAccessLog(ctx, other, id); return err },
		"SharingActivity":    func() error { _, err := p.SharingActivity(ctx, id, time.Now(), 10); return err },
		"ShareAnnotations":   func() error { return p.ShareAnnotations(ctx, other, id, uuid.New()) },
		"UnshareAnnotations": func() error { return p.UnshareAnnotations(ctx, other, id, uuid.New()) },
		"AnnotationShares":   func() error { _, err := p.AnnotationShares(ctx, other, id); return err },
		"PersonNamed":        func() error { _, err := p.PersonNamed(ctx, "somebody"); return err },

		// accounts.go
		"DeleteAccount":  func() error { _, err := p.DeleteAccount(ctx, id, id); return err },
		"ReleaseHolding": func() error { _, err := p.ReleaseHolding(ctx, other, id); return err },
		"MoveArchived":   func() error { _, err := p.MoveArchived(ctx, other, "/l/x.epub", ignore); return err },
		"ArchivedBooks":  func() error { _, err := p.ArchivedBooks(ctx, id); return err },
		"ClaimArchived":  func() error { return p.ClaimArchived(ctx, id, other, id) },
		"ExpireArchived": func() error { _, err := p.ExpireArchived(ctx, time.Now(), false); return err },

		// duplicates.go
		"Duplicates":    func() error { _, err := p.Duplicates(ctx, id); return err },
		"MergeWorks":    func() error { _, err := p.MergeWorks(ctx, id, other, []uuid.UUID{uuid.New()}); return err },
		"FilesForWorks": func() error { _, err := p.FilesForWorks(ctx, id, []uuid.UUID{other}); return err },

		// annotations.go
		"CreateAnnotation": func() error { _, err := p.CreateAnnotation(ctx, mark); return err },
		"Annotations":      func() error { _, err := p.Annotations(ctx, id, other); return err },
		"UpdateAnnotation": func() error { _, err := p.UpdateAnnotation(ctx, id, other, "n", ""); return err },
		"DeleteAnnotation": func() error { return p.DeleteAnnotation(ctx, id, other) },
		"MarkAnnotationsOrphaned": func() error {
			_, err := p.MarkAnnotationsOrphaned(ctx, id, []uuid.UUID{other})
			return err
		},
		"ClearAnnotationOrphan": func() error { return p.ClearAnnotationOrphan(ctx, id, []uuid.UUID{other}) },
		"RecentAnnotations":     func() error { _, err := p.RecentAnnotations(ctx, id, 10, ""); return err },
		"SearchNotes":           func() error { _, err := p.SearchNotes(ctx, id, "monads", 3); return err },
		"ImportMarks":           func() error { _, err := p.ImportMarks(ctx, id, other, nil); return err },

		// authors.go
		"CountAuthors":     func() error { _, err := p.CountAuthors(ctx); return err },
		"AuthorCandidates": func() error { _, _, err := p.AuthorCandidates(ctx, 0.5); return err },
		"MergeAuthors":     func() error { return p.MergeAuthors(ctx, AuthorPair{KeepID: id, DropID: other}) },
		"AuthorImposters":  func() error { _, err := p.AuthorImposters(ctx); return err },
		"PruneAuthor":      func() error { return p.PruneAuthor(ctx, other) },
		"NormaliseAuthors": func() error { _, err := p.NormaliseAuthors(ctx, 0.5, true); return err },

		// holdings.go
		"HoldUpload":           func() error { _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: id}); return err },
		"RemoveIfUnreferenced": func() error { return p.RemoveIfUnreferenced(ctx, hash, "/l/x.epub", func(string) error { return nil }) },
		"HeldStorage":          func() error { _, err := p.HeldStorage(ctx, id); return err },

		// edit.go
		"UpdateWork": func() error { _, err := p.UpdateWork(ctx, other, id, WorkEdit{Title: str("T")}); return err },

		// jobs.go
		"Enqueue":           func() error { _, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobScan}); return err },
		"Claim":             func() error { _, err := p.Claim(ctx, "w"); return err },
		"Progress":          func() error { return p.Progress(ctx, other, "w", 0.5, "") },
		"Succeed":           func() error { return p.Succeed(ctx, other, "w") },
		"Fail":              func() error { return p.Fail(ctx, other, "w", errors.New("x")) },
		"RescheduleSkipped": func() error { return p.RescheduleSkipped(ctx, other, "w", time.Now(), "", 3) },
		"Defer":             func() error { return p.Defer(ctx, other, "w", time.Now(), "") },
		"Cancel":            func() error { return p.Cancel(ctx, other) },
		"ReclaimStale":      func() error { _, err := p.ReclaimStale(ctx, time.Minute); return err },
		"GetJob":            func() error { _, err := p.GetJob(ctx, other); return err },
		"GetJobFor":         func() error { _, err := p.GetJobFor(ctx, other, id); return err },
		"QueueDepth":        func() error { _, err := p.QueueDepth(ctx); return err },

		// reading.go
		"EnsureDefaultUser": func() error { _, err := p.EnsureDefaultUser(ctx); return err },
		"SaveProgress": func() error {
			_, err := p.SaveProgress(ctx, domain.ReadingProgress{UserID: id, BookFileID: other, Position: epub})
			return err
		},
		"ReadingProgress": func() error { _, err := p.ReadingProgress(ctx, id, other); return err },
		"ContinueReading": func() error { _, err := p.ContinueReading(ctx, id, 10); return err },
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, context.Canceled) {
				t.Errorf("%s = %v, want an error carrying context.Canceled", name, err)
			}
		})
	}

	// A session lookup is the one deliberate exception: every failure is the
	// same answer, "not a valid session", so nothing above it is tempted to
	// say which.
	if _, err := p.SessionByTokenHash(ctx, "h"); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Errorf("SessionByTokenHash = %v, want auth.ErrSessionInvalid", err)
	}
}
