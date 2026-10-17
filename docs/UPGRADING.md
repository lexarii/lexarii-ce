# Upgrading Lexarii

What to know before moving an existing installation to a newer version, newest
first. Lexarii migrates its own database at startup (`LEXARII_AUTO_MIGRATE`,
on by default), so most of this happens the moment the new binary or image
starts.

**Always take a backup first.** The schema changes below are reversible, but
a backup is the only undo that does not depend on reading this page carefully:

```bash
pg_dump --format=custom --file=lexarii-before-upgrade.dump "$DATABASE_URL"
```

Back up the data directory too (`LEXARII_DATA_DIR`: uploads, covers, the
archive). The library directory is never written to and needs nothing.

---

## Sharing by username and erasing books, tightened (no migration)

- **Lending a book or sharing notes by username checks the book first.** A
  book the caller may not lend (or, for notes, read) is the plain
  `404 not_found` whatever name came with it, and the name is not looked up,
  so a made-up book id no longer answers whether a username exists. Names
  that match nobody are counted per person: after 8 in 15 minutes every
  name is `429 too_many_unknown_names` for 15 minutes, with `Retry-After`.
  The web client shows the message; nothing to do.
- **`POST /api/v1/admin/users/{id}/erase-files` also withdraws the person's
  uploads not extracted yet**: their claim on the bytes goes, a queued
  extraction is cancelled and its stored copy deleted, and the release is
  remembered, so a later extraction of the same bytes does not put the book
  on their shelf. Before, those uploads were left to arrive. And an
  extraction no longer turns a claim into a holding for somebody who has let
  those bytes go since.
- **Finished jobs no longer keep uploaders' file names.** Deleting an account
  (archive or erase) and erasing somebody's books remove `originalFilename`
  and `ownerId` from that person's finished jobs and clear who asked for them.
  And finished jobs are now deleted after **`LEXARII_JOB_RETENTION`** (default
  30 days, checked at start and hourly; at least `168h`, and a shorter value
  stops startup naming the variable). The first sweep after upgrading deletes
  every job that finished more than 30 days ago - set a longer retention
  first if you want to keep them. Finished jobs of accounts deleted before
  the upgrade keep their payloads until the sweep reaches them.

## Settings for larger installations, and a sign-in fix (migration 39)

- **Migration 39 adds `pending_discards`**: the stored copies an erasure owes
  the disk. It starts empty and stays empty unless the installation erases
  (below). **Downgrading**: step the schema down with this release's
  migrations (the `goose ... down-to` command further down, with `38`) before
  starting an older binary; copies still owed then are not deleted, and are
  listed by `SELECT * FROM pending_discards` beforehand if you want them.

- **Federated sign-in now checks the ID token's nonce.** Lexarii sends a
  random nonce of its own (it used to send the state) and refuses an identity
  that does not carry it back. Every OpenID Connect provider supports this.
  A sign-in started before the upgrade and finished after it fails once with
  "the sign-in attempt expired"; starting again works.
- **`LEXARII_PEOPLE_DIRECTORY`** (default `all`) decides whether every
  signed-in account may list every other one, which is what the share picker
  offers. `all` changes nothing. `none` lists accounts to administrators only:
  `GET /api/v1/people` answers everybody else an empty list, and they lend a
  book or share their notes by typing the recipient's exact username
  (`POST /api/v1/books/{id}/shares`, `POST /api/v1/books/{id}/annotations/shares`).
  A name nobody has is `404 user_not_found`. A value other than `all` or
  `none` stops startup naming the variable.
- **Sharing a book or notes with yourself is now a 400**, not a 500.
- **`LEXARII_DELETED_ACCOUNT_FILES`** (default `archive`) decides what happens
  to the books only a deleted account held. `archive` changes nothing. `erase`
  deletes them with the account - record, passages, cover, everybody's notes
  on them - and then the copies Lexarii stored in its upload directory and
  archive; never anything in the library directory. The copies are deleted
  right after the deletion and, if that cannot happen yet, at every start and
  every hour until it has (`copiesPending` in the reply counts them). In erase
  mode an archive record that expires (`LEXARII_ARCHIVE_RETENTION`) takes its
  stored copy with it too - including records archived before the setting was
  turned on. Back up the data directory before turning it on if you may want
  a leaver's uploads back. A value other than `archive` or `erase` stops
  startup naming the variable.
- **`POST /api/v1/admin/users/{id}/erase-files`** erases one person's books
  the same way without deleting the account, in batches of 1,000 files.
  Administrators only. Deleting an account takes a lock per file the person
  holds in one transaction, as it always did; for a shelf of tens of
  thousands, erase their books first or raise `max_locks_per_transaction`.
- **An upload keeps its own copy aside until it is recorded** (in
  `<LEXARII_UPLOAD_DIR>/incoming`, as a second link to the same file where the
  filesystem allows, a copy otherwise), so an erasure of the same bytes at
  that moment cannot leave it pointing at nothing. Nothing to do; a crash at
  that instant can leave a stray file there, as an interrupted upload always
  could.
- **A book's page shows where its file sits on the server's disk only to
  administrators.** It used to show it to whoever scanned the file; somebody
  whose administrator rights were taken away no longer sees it.

## Enrichment on ingest, deferred jobs, and the queue's order (migration 38)

- **Migration 38 renumbers the jobs already waiting** to the queue's new
  order - scan, extract, chunk, embed, then enrichment last. Enrichment that
  used to run ahead of extraction now waits for it.
- **Enrichment on ingest does not ask about a work again for a week** when
  the model was asked and nothing came of it (no text, an empty answer, a
  failure). The administrator's "describe more books" is not held back.
  Comics are no longer queued for enrichment at all.
- **Downgrading**: step the schema down with this release's migrations (the
  `goose ... down-to` command below, with `37`, or lower for the sections
  further down). Waiting jobs get their old numbers back.

- **`LEXARII_ENRICH_ON_INGEST`** (default `false`) has the model describe each
  new book as it is uploaded or scanned, as `LEXARII_INDEX_ON_INGEST` does for
  indexing. Off, nothing changes. On, every extraction of a book not yet
  described queues one enrichment job for its work, belonging to whoever added
  the book; it is ignored with no chat model configured. A value other than
  true or false stops startup naming the variable.
- **Background work can be deferred** by an installation that wraps the job
  handlers (`worker.Defer`): the job waits until a given time without using an
  attempt. Nothing in Lexarii itself defers anything.

## Administrators can remove a file for everybody (migration 37)

An administrator can now take a file off the installation entirely and keep
it off: `POST /api/v1/admin/books/{id}/remove`, or "Remove for everyone" on a
book's page. See "An administrator can take a file off the installation" in
[DOMAIN.md](DOMAIN.md).

- **Migration 37 adds `blocked_content`**, the blocklist. It starts empty and
  changes nothing until an administrator removes something.
- **The library directory may no longer overlap what Lexarii deletes from.**
  Startup now refuses a `LEXARII_LIBRARY_DIR` inside `LEXARII_UPLOAD_DIR`, and
  one that contains or is contained in the archive (`<LEXARII_DATA_DIR>/archive`),
  naming the variables. Such a layout let Lexarii move or delete files inside
  the read-only library directory. Move the data directory out of the library
  directory before upgrading if yours was inside it.
- **Removing deletes files** Lexarii itself stored - uploads, archive copies,
  covers - for the removed file. Files in the library directory are never
  deleted; a scan passes over them instead. Back up the data directory if you
  may want a removed upload back.
- **An upload can now be refused with 422 `content_blocked`**, or, in a batch
  with other files, reported as `rejected` with `code: content_blocked`. A
  client that treats every 4xx from `/uploads` as a malformed request should
  show the message instead.
- **Downgrading**: with the new binary stopped, step the schema down with this
  release's migrations - the `goose ... down-to` command below, with `36` -
  before starting an older binary. The blocklist is dropped, so
  removed bytes can be uploaded or scanned again; the records of removed files
  stay archived and appear in the archive.

## Books can have several holders (migrations 33-36)

Until now every book file had exactly one owner. Now a file can be held by
everybody who added it, each with their own filename, their own lending and
their own access log; see "Who may do what" in [DOMAIN.md](DOMAIN.md).

### What the migration does

- **Migration 33 drops `book_files.owner_id`**, after copying every owner into
  the new `book_file_holders` table. Anything of your own that reads
  `owner_id` directly - a script, a report, a backup check - must read
  `book_file_holders` instead.
- **It runs in one transaction, at startup, holding exclusive locks** on
  `book_files`, `book_access` and `access_log` while it does. Expect tens of
  seconds per 100,000 book files, as measured on a test machine; larger
  libraries take proportionally longer. Lexarii does not serve requests until
  it finishes, so plan the restart for a quiet moment on a large library.
- **Some grants are dropped**, deliberately:
  - grants on books that were already archived (their owner's account had been
    deleted). Before, they would have come back for whoever claimed the book
    next; now nothing the departed owner lent comes back.
  - grants on books that had **no owner at all**. Those books are archived
    under the marker "nobody (no owner was recorded)", so an administrator can
    see them under the archive and hand them to somebody. Previously they were
    on nobody's shelf, readable only by whoever they had been lent to.
  - Every other grant whose recorded grantor is not the book's current owner
    (`granted_by IS DISTINCT FROM owner_id`: none was recorded, or the book
    changed hands after the grant was made) is kept and re-attributed to the
    book's owner, who could already see and revoke it.
- **Migration 34** records who each job was queued for. Following a job's
  progress is now limited to that person and administrators.
- **Migration 35** lets two editions carry the same ISBN, since two people's
  copies of one book are now separate editions.
- **Migration 36** remembers when somebody takes a book off their own shelf,
  so a later scan does not hand it back. It starts empty.

### Behaviour that changes

- **A new file only joins its adder's own books.** By ISBN or by filename, an
  upload or a scan now joins an existing edition only if the person adding it
  already holds a file of that edition. On an installation with several
  administrators who scan, a file one of them scans no longer joins a book
  another one holds; it becomes a separate book on the scanner's shelf, which
  the duplicates page can merge.
- **Editing or merging a book needs every one of its files.** Somebody who
  holds only some of a book's files - one format, while somebody else holds
  the PDF - can read and annotate it but no longer retitle or merge it.

### What does not change

- **`LEXARII_UPLOAD_POLICY` defaults to `admins`**, which is exactly who could
  upload before. Set it to `users` only if you want everybody to add books to
  their own shelf; there is no per-person storage quota yet.
- Reading, annotating, sharing and the reader itself behave as before for a
  library with one owner per book, which is every library that has not used
  the new features yet.

### Downgrading

An older binary **starts but then fails**: its queries read `owner_id`, which
no longer exists. It does not know migrations 33-36, so it cannot undo them
either. To go back, use **this** release's migrations to step the schema down
first, with the new binary stopped:

```bash
go run github.com/pressly/goose/v3/cmd/goose@latest \
  -dir migrations postgres "$DATABASE_URL" down-to 32
```

(from a checkout of this release), then start the older binary.

What a downgrade does to data:

- Each book's earliest holder becomes its owner again.
- **Every other holder becomes a downloadable grant from that owner.** They keep
  reading the book, but it is lent to them rather than theirs, and their own
  name for the file is gone.
- Grants are folded back to one per person per book, keeping download
  permission if any holder had given it.
- Of two editions sharing an ISBN, the later one loses it.
- The record of who took which book off their shelf is dropped, so a scan by
  an older binary may hand such a book back to an administrator.
- Books archived on the way up stay archived, and dropped grants stay dropped;
  restore the backup if you need them back.
