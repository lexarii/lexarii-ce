// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// statementFault fails exactly one statement - the n-th the pool runs once
// armed - by handing pgx a context that is already cancelled for it. Nothing
// reaches the server for that statement, and the connection stays usable, so
// the code under test sees what it would see if the database went away at
// that precise point.
type statementFault struct {
	mu       sync.Mutex
	failAt   int
	seen     int
	fired    bool
	commits  int
	beforeFi int
}

func (f *statementFault) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAt == 0 || f.fired {
		return ctx
	}
	f.seen++
	if f.seen == f.failAt {
		f.fired = true
		f.beforeFi = f.commits
		failed, cancel := context.WithCancel(ctx)
		cancel()
		return failed
	}
	if strings.EqualFold(strings.TrimSpace(data.SQL), "commit") {
		f.commits++
	}
	return ctx
}

func (f *statementFault) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (f *statementFault) arm(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAt, f.seen, f.fired, f.commits, f.beforeFi = n, 0, false, 0, 0
}

// disarm reports whether the fault fired, and whether anything had been
// committed before it did.
func (f *statementFault) disarm() (fired, committedFirst bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fired, committedFirst = f.fired, f.beforeFi > 0
	f.failAt = 0
	return fired, committedFirst
}

// storeFaultPool is a second pool on the test database whose statements can be
// failed one at a time.
func storeFaultPool(t *testing.T) (*Pool, *statementFault) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parsing the test URL: %v", err)
	}
	fault := &statementFault{}
	cfg.ConnConfig.Tracer = fault
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("opening the faulty pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Pool{Pool: pool}, fault
}

// storeFaultFingerprint is a digest of every row in every table, so "nothing changed"
// can be checked without knowing what a call might have touched.
func storeFaultFingerprint(t *testing.T, p *Pool) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := p.Query(ctx, `
		SELECT tablename FROM pg_tables
		 WHERE schemaname = 'public' AND tablename <> 'goose_db_version'`)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	out := map[string]string{}
	for _, table := range tables {
		var digest string
		if err := p.QueryRow(ctx, fmt.Sprintf(
			`SELECT COALESCE(md5(string_agg(x::text, '|' ORDER BY x::text)), '') FROM %s x`,
			pgx.Identifier{table}.Sanitize())).Scan(&digest); err != nil {
			t.Fatalf("fingerprinting %s: %v", table, err)
		}
		out[table] = digest
	}
	return out
}

func storeFaultChangedTables(before, after map[string]string) []string {
	var out []string
	for table, digest := range before {
		if after[table] != digest {
			out = append(out, table)
		}
	}
	sort.Strings(out)
	return out
}

// faultWorld is a small installation with something of everything: a scanned
// book and an upload held by Alice, the scanned one lent to Bob with her notes,
// a mark and a reading position, a pending suggestion, an archived book a
// leaver left behind, and blocked bytes.
type faultWorld struct {
	admin, alice, bob    uuid.UUID
	scanned, upload      uuid.UUID
	scannedHash          string
	uploadHash           string
	work, uploadWork     uuid.UUID
	mark                 uuid.UUID
	suggestion           uuid.UUID
	archived             uuid.UUID
	blockedHash          string
	author, spareAuthor  uuid.UUID
	freshHash, freshName string
	identitySubject      string
	knownIdentitySubject string
	people               []uuid.UUID
}

func buildFaultWorld(t *testing.T, p *Pool) faultWorld {
	t.Helper()
	ctx := context.Background()
	cleanLibrary(t, p)
	cleanJobs(t, p)

	var w faultWorld
	w.admin = testOwner(t, p)
	if _, err := p.Exec(ctx, `UPDATE users SET is_admin = true WHERE id = $1`, w.admin); err != nil {
		t.Fatalf("making the owner an administrator: %v", err)
	}
	w.alice, w.bob = secondUser(t, p), secondUser(t, p)
	leaver := secondUser(t, p)
	w.people = []uuid.UUID{w.alice, w.bob}

	w.scannedHash = fmt.Sprintf("%064x", 0xfa1)
	scanned, err := p.UpsertBookFile(ctx, owned(sampleInput(w.scannedHash, "/library/fault-lines.epub",
		"Fault Lines", "Ada Faultless"), w.alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	w.scanned, w.work = scanned.BookFileID, scanned.WorkID

	w.uploadHash = fmt.Sprintf("%064x", 0xfa2)
	upload, err := p.UpsertBookFile(ctx, uploadInput(w.uploadHash, "faults.epub", w.alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	w.upload, w.uploadWork = upload.BookFileID, upload.WorkID

	if err := p.ShareBook(ctx, w.scanned, w.alice, w.bob, true, true); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	mark, err := p.CreateAnnotation(ctx, epubMark(w.alice, w.scanned, 2, "p[1]", 0, 9))
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	w.mark = mark.ID
	save(t, p, w.alice, w.scanned, epubPosition(2, "p[1]", 4), 0.2)

	in := classification()
	in.WorkID = w.work
	in.Confidence = 0.2
	if _, err := p.SaveEnrichment(ctx, in); err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}
	if err := p.QueryRow(ctx, `
		SELECT id FROM enrichment_records
		 WHERE work_id = $1 AND field = 'title' AND accepted_at IS NULL AND rejected_at IS NULL`, w.work).
		Scan(&w.suggestion); err != nil {
		t.Fatalf("finding the suggestion: %v", err)
	}

	left, err := p.UpsertBookFile(ctx, owned(sampleInput(fmt.Sprintf("%064x", 0xfa3), "/library/left.epub",
		"Left Here", "Somebody Else"), leaver))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.DeleteAccount(ctx, w.admin, leaver); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	w.archived = left.BookFileID

	w.blockedHash = fmt.Sprintf("%064x", 0xfa4)
	blocked, err := p.UpsertBookFile(ctx, uploadInput(w.blockedHash, "bad.epub", w.bob))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.RemoveBookFileForEveryone(ctx, blocked.BookFileID, w.admin, "not here", RemovalOptions{}); err != nil {
		t.Fatalf("RemoveBookFileForEveryone: %v", err)
	}

	w.author = authorIDNamedForQuery(t, p, "Ada Faultless")
	w.spareAuthor = authorIDNamedForQuery(t, p, "Somebody Else")

	w.freshHash = fmt.Sprintf("%064x", 0xfa5)
	w.freshName = "fault-" + uuid.NewString()[:8]
	w.identitySubject = "fault-sub-" + uuid.NewString()[:8]
	w.knownIdentitySubject = "fault-known-" + uuid.NewString()[:8]
	if _, err := p.Exec(ctx, `
		INSERT INTO user_identities (user_id, provider, provider_user_id) VALUES ($1, 'oidc', $2)`,
		w.bob, w.knownIdentitySubject); err != nil {
		t.Fatalf("seeding an identity: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO user_identities (user_id, provider, provider_user_id, secret) VALUES ($1, 'local', $2, 'x')`,
		w.alice, "fault-local-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("seeding a local identity: %v", err)
	}
	newSession(t, p, w.alice.String(), "fault-session-"+uuid.NewString(), "", "", time.Hour)
	// Somebody waiting for the fresh bytes, so an ingest of them turns the
	// claim into a holding.
	if _, err := p.Exec(ctx, `
		INSERT INTO holding_claims (content_hash, user_id, original_filename) VALUES ($1, $2, 'claimed.epub')`,
		fmt.Sprintf("%064x", 0xfa5), w.admin); err != nil {
		t.Fatalf("seeding a claim: %v", err)
	}
	if _, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobChunk, TargetID: &w.scanned}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return w
}

// dropFaultWorld removes the accounts a world made, so repeated builds do not
// pile people up.
func dropFaultWorld(p *Pool, w faultWorld) {
	ctx := context.Background()
	_, _ = p.Exec(ctx, `DELETE FROM users WHERE id = ANY($1) OR username = $2
	                     OR id IN (SELECT user_id FROM user_identities WHERE provider_user_id = $3)`,
		w.people, w.freshName, w.identitySubject)
}

// Every write that runs in a transaction is all or nothing. Failing each of
// its statements in turn - the database going away at that exact point - the
// call reports the failure with its cause, and unless it had already
// committed, the database is exactly as it was.
func TestEveryStatementOfAWriteCanFailWithoutLeavingHalfOfIt(t *testing.T) {
	base := testPool(t)
	faulty, fault := storeFaultPool(t)

	writes := map[string]func(ctx context.Context, p *Pool, w faultWorld) error{
		"DeleteAccount": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.DeleteAccount(ctx, w.admin, w.alice)
			return err
		},
		"ReleaseHolding": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.ReleaseHolding(ctx, w.upload, w.alice)
			return err
		},
		"RemoveBookFileForEveryone": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.RemoveBookFileForEveryone(ctx, w.scanned, w.admin, "gone", RemovalOptions{PurgeQuotes: true})
			return err
		},
		"UpsertBookFile new": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.UpsertBookFile(ctx, owned(sampleInput(w.freshHash, "/library/new.epub", "Brand New", "New Author"), w.bob))
			return err
		},
		"UpsertBookFile again": func(ctx context.Context, p *Pool, w faultWorld) error {
			in := owned(sampleInput(w.scannedHash, "/library/moved/fault-lines.epub", "Fault Lines", "Ada Faultless"), w.alice)
			in.CoverPath = "covers/fault.jpg"
			_, err := p.UpsertBookFile(ctx, in)
			return err
		},
		"TouchSeen": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.TouchSeen(ctx, w.scannedHash, "/library/elsewhere.epub", w.admin)
		},
		"HoldUpload held": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.HoldUpload(ctx, HoldInput{ContentHash: w.uploadHash, UserID: w.bob, OriginalFilename: "mine.epub"})
			return err
		},
		"HoldUpload claim": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.HoldUpload(ctx, HoldInput{ContentHash: w.freshHash, UserID: w.bob, OriginalFilename: "soon.epub"})
			return err
		},
		"SaveEnrichment": func(ctx context.Context, p *Pool, w faultWorld) error {
			in := classification()
			in.WorkID = w.uploadWork
			in.Authors = []string{"Model Author"}
			_, err := p.SaveEnrichment(ctx, in)
			return err
		},
		"ResolveSuggestion": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.ResolveSuggestion(ctx, w.suggestion, true)
		},
		"UpdateWork": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.UpdateWork(ctx, w.work, w.alice, WorkEdit{
				Title: str("Corrected"), Subtitle: str("Sub"), Series: str("Faults"), SeriesIndex: ptrFloatForReview(2),
				Authors: strs("Ada Faultless", "Grace Hopper"), Publisher: str("Press"), Year: intp(2001),
			})
			return err
		},
		"MergeWorks": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.MergeWorks(ctx, w.alice, w.work, []uuid.UUID{w.uploadWork})
			return err
		},
		"ClaimArchived": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.ClaimArchived(ctx, w.admin, w.archived, w.bob)
		},
		"MoveArchived": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.MoveArchived(ctx, w.archived, "/library/left.epub", func(string) (string, error) { return "/data/archive/left.epub", nil })
			return err
		},
		"ShareBook": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.ShareBook(ctx, w.upload, w.alice, w.bob, false, true)
		},
		"UnshareBook": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.UnshareBook(ctx, w.scanned, w.alice, w.bob)
		},
		"Unblock": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.Unblock(ctx, w.admin, w.blockedHash)
		},
		"DiscardBlockedCopies": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.DiscardBlockedCopies(ctx, w.blockedHash, func(StoredCopies) error { return nil })
		},
		"ApplyMarks deletion": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.ApplyMarks(ctx, w.alice, []PushedMark{{ID: w.mark, BookFileID: w.scanned, Deleted: true}})
			return err
		},
		"MergePositions": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.MergePositions(ctx, w.alice, []PushedPosition{{BookFileID: w.scanned, Position: epubPosition(3, "p[2]", 0), ReadAt: time.Now()}})
		},
		"SaveProgress": func(ctx context.Context, p *Pool, w faultWorld) error {
			pct := float32(0.5)
			_, err := p.SaveProgress(ctx, domain.ReadingProgress{UserID: w.alice, BookFileID: w.scanned, Position: epubPosition(4, "p[1]", 0), Percent: &pct})
			return err
		},
		"DeleteAnnotation": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.DeleteAnnotation(ctx, w.alice, w.mark)
		},
		"ImportMarks": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.ImportMarks(ctx, w.alice, w.upload, []ImportedMark{{Page: 1, Text: "imported", Note: "n"}})
			return err
		},
		"MergeAuthors": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.MergeAuthors(ctx, AuthorPair{KeepID: w.author, KeepName: "Ada Faultless", DropID: w.spareAuthor, DropName: "Somebody Else"})
		},
		"PruneAuthor": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.PruneAuthor(ctx, w.author)
		},
		"UpsertIdentity new": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, _, err := p.UpsertIdentity(ctx, auth.Identity{Provider: "oidc", Subject: w.identitySubject, DisplayName: w.freshName}, true)
			return err
		},
		"UpsertIdentity known": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, _, err := p.UpsertIdentity(ctx, auth.Identity{Provider: "oidc", Subject: w.knownIdentitySubject, Email: "b@example.org"}, false)
			return err
		},
		"CreateLocalUser": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.CreateLocalUser(ctx, w.freshName, "", localUserPassword, false)
			return err
		},
		"SetLocalPassword": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.SetLocalPassword(ctx, w.alice, localUserPassword)
		},
		"AdoptOrCreateLocalUser": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.AdoptOrCreateLocalUser(ctx, w.freshName, "", localUserPassword, false)
			return err
		},
		"ShareAnnotations": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.ShareAnnotations(ctx, w.scanned, w.bob, w.alice)
		},
		"Enqueue again": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobChunk, TargetID: &w.scanned})
			return err
		},
		"ApplyMarks edit": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.ApplyMarks(ctx, w.alice, []PushedMark{pushedHighlight(w.mark, w.scanned, "edited offline", time.Now().Add(time.Hour))})
			return err
		},
		"DiscardBlockedUpload": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.DiscardBlockedUpload(ctx, w.blockedHash, func() error { return nil })
		},
		"RemoveIfUnreferenced": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.RemoveIfUnreferenced(ctx, w.uploadHash, "/data/archive/stale.epub", func(string) error { return nil })
		},
		"RecordAccess": func(ctx context.Context, p *Pool, w faultWorld) error {
			return p.RecordAccess(ctx, w.scanned, w.bob, AccessChapter, 2)
		},
	}

	names := make([]string, 0, len(writes))
	for name := range writes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		call := writes[name]
		t.Run(name, func(t *testing.T) {
			// The world is rebuilt only when a run may have changed it. A
			// failed run that committed nothing left it exactly as it was -
			// which is what is asserted - so the next statement's turn can
			// start from it.
			var w faultWorld
			var before map[string]string
			fresh := false
			for n := 1; ; n++ {
				if n > 80 {
					t.Fatalf("still failing at statement %d; the call never completes", n)
				}
				if !fresh {
					w = buildFaultWorld(t, base)
					before = storeFaultFingerprint(t, base)
					fresh = true
				}

				fault.arm(n)
				err := call(t.Context(), faulty, w)
				fired, committedFirst := fault.disarm()

				if !fired {
					// Every statement has had its turn, and this run went
					// through untouched: it must have succeeded.
					dropFaultWorld(base, w)
					if err != nil {
						t.Fatalf("with every statement allowed through, the call failed: %v", err)
					}
					if n == 1 {
						t.Fatal("the call ran no statements at all")
					}
					return
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("statement %d failing: err = %v, want the failure reported with its cause", n, err)
				}
				if committedFirst {
					dropFaultWorld(base, w)
					fresh = false
					continue
				}
				if changed := storeFaultChangedTables(before, storeFaultFingerprint(t, base)); len(changed) > 0 {
					t.Errorf("statement %d failing left a partial write in %v", n, changed)
					dropFaultWorld(base, w)
					fresh = false
				}
			}
		})
	}
}

// Reads made of several statements report a failure at any of them, rather
// than answering from what they had read so far.
func TestEveryStatementOfARead(t *testing.T) {
	base := testPool(t)
	faulty, fault := storeFaultPool(t)

	reads := map[string]func(ctx context.Context, p *Pool, w faultWorld) error{
		"Work": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.Work(ctx, w.work, w.alice)
			return err
		},
		"Library": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.Library(ctx, LibraryQuery{ViewerID: w.alice})
			return err
		},
		"Stats": func(ctx context.Context, p *Pool, w faultWorld) error { _, err := p.Stats(ctx, w.alice); return err },
		"Changes": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.Changes(ctx, w.alice, time.Time{}, "", 10)
			return err
		},
		"EnrichmentSubject": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.EnrichmentSubject(ctx, w.work)
			return err
		},
		"Duplicates": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.Duplicates(ctx, w.alice)
			return err
		},
		"BookShares": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.BookShares(ctx, w.scanned, w.alice)
			return err
		},
		"BookAccessLog": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.BookAccessLog(ctx, w.scanned, w.alice)
			return err
		},
		"ArchivedBooks": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.ArchivedBooks(ctx, w.admin)
			return err
		},
		"BlockedFiles": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.BlockedFiles(ctx, w.admin)
			return err
		},
		"NormaliseAuthors": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.NormaliseAuthors(ctx, 0.3, false)
			return err
		},
		"SearchNotes": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.SearchNotes(ctx, w.alice, "a passage", 3)
			return err
		},
		"ApplyMarks edit": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.ApplyMarks(ctx, w.alice, []PushedMark{pushedHighlight(uuid.New(), w.scanned, "edit", time.Now())})
			return err
		},
		"CreateAnnotation": func(ctx context.Context, p *Pool, w faultWorld) error {
			_, err := p.CreateAnnotation(ctx, epubMark(w.alice, w.scanned, 5, "p[1]", 0, 3))
			return err
		},
	}

	names := make([]string, 0, len(reads))
	for name := range reads {
		names = append(names, name)
	}
	sort.Strings(names)

	w := buildFaultWorld(t, base)
	t.Cleanup(func() { dropFaultWorld(base, w) })
	for _, name := range names {
		call := reads[name]
		t.Run(name, func(t *testing.T) {
			for n := 1; n <= 40; n++ {
				fault.arm(n)
				err := call(t.Context(), faulty, w)
				if fired, _ := fault.disarm(); !fired {
					return
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("statement %d failing: err = %v, want the failure reported with its cause", n, err)
				}
			}
			t.Fatal("still failing after 40 statements")
		})
	}
}
