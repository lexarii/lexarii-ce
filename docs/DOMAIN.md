# Lexarii — Domain Model

The shared vocabulary. Every name here means exactly one thing, in Go, in SQL, in TypeScript, and in the UI. When code and this document disagree, one of them is a bug.

---

## Why the model is shaped this way

A naive ebook app has one table: `books`. That collapses the moment a real library shows up, because three genuinely different things get conflated:

- the *work* the user thinks about ("Gödel, Escher, Bach")
- the *edition* that has an ISBN and a publisher
- the *file* on disk that has a hash and a format

The user has two copies of the same book (an EPUB and a scanned PDF). They must appear as one thing on the shelf, but the reader, the annotations, and the chunk positions belong to the *file*. The Zotero-style features later depend on the *work* identity, because a citation cites a work, not a file.

Splitting this now costs one extra table. Retrofitting it later costs a migration of every annotation and every chunk.

---

## Core entities

### Work

A book as an idea. "Gödel, Escher, Bach", regardless of edition or format.

- Holds: canonical title, subtitle, authors, series and series index, original language, description, subjects/tags.
- Holds the enrichment output that is about the *content*: subject classification, generated summary, topic embedding.
- Has one or more Editions.

### Edition

A specific published version of a Work. First edition hardback, 20th anniversary edition, the 2007 Penguin reprint.

- Holds: ISBN-10/13, publisher, publication date, page count, edition language, cover image.
- Belongs to one Work. Has one or more BookFiles.
- For most self-hosted libraries there will be exactly one Edition per Work. That is fine — it stays cheap and it is there when it is needed.

### BookFile

An actual file in the library directory.

- Holds: absolute path, `content_hash` (SHA-256, **unique** — this is file identity), format (`epub`, `pdf`, `cbz`, `mobi`, `azw3`), byte size, ingest status, ingest error, `extracted_at`.
- Belongs to one Edition.
- **The library directory is read-only.** Lexarii never writes to a BookFile's path. Derived artifacts (covers, extracted text, thumbnails) live in the separate data directory.
- A moved or renamed file with the same hash is the *same* BookFile, updated — never a duplicate.
- Has one or more **holders**: the people who added it. Two people adding the same bytes is one BookFile with two holders, never two BookFiles — see "Who may do what".

### Author

A person. Separate table, many-to-many with Work, because "Kernighan & Ritchie" must be two browsable people, not one string.

- Holds: canonical name, sort name (`Ritchie, Dennis`), aliases, optional external identifiers for later Zotero-style work.

### Locator

**Not a table — a value type, and the most important type in the system.**

A stable pointer to a position inside a BookFile. Everything that refers to a place in a book uses one: reading position, highlight anchors, chunk positions, chat citations.

```
EPUB: { spineIndex: int, elementPath: string, charOffset: int }
PDF:  { page: int, charOffset: int }
```

Invariants, which are not negotiable:

- **Stable across re-ingest.** The same file always produces the same locators. Extraction must be deterministic.
- **Independent of presentation.** Font size, window width, device, and theme cannot change a locator. This is why reading position is never a scroll percentage.
- **Resolvable to a rendered position** by the reader, and to a character range by the indexer.

If locators are unstable, every highlight orphans and every chat citation rots. Changing the locator format is a coordinated change across `ebook-ingest`, `reader-ux`, and `rag-engine`, never a local one.

### Chunk

A retrieval unit: a passage of text from a BookFile, embedded for search.

- Holds: text, `startLocator`, `endLocator`, chapter title, token count, `chunk_strategy_version`.
- Has zero or more Embeddings.
- A Chunk the reader cannot navigate to is worthless — the locators are part of the definition, not metadata.

### Embedding

A vector for a Chunk, under a specific model.

- Holds: `vector`, `model_id`, `dimensions`, `created_at`.
- **The `model_id` is essential.** Swapping embedding models invalidates existing vectors; without the model id there is no way to tell which rows need recomputing, and mixed-model vector search silently returns nonsense.

### Annotation

A user mark inside a book: highlight, note, or bookmark.

- Holds: type, `startLocator`, `endLocator`, selected text snapshot, note body, colour, timestamps.
- The **selected text snapshot matters**: if the anchor fails to resolve after a re-ingest, the annotation becomes *orphaned* and is shown to the user with its original text. It is never silently deleted.

### ReadingProgress

Where a user is in a BookFile. One row per user per file. Holds a Locator, a derived percentage for display only, and `last_read_at`. The Locator is the truth; the percentage is a rendering.

### Collection

A user-made grouping — "Maths", "To read", "PhD sources". Nestable. Many-to-many with Work.

Distinct from **Tag**, which is a flat label, frequently machine-generated, and normalised against a controlled vocabulary. Collections are hand-curated and hierarchical; tags are cheap and numerous. Do not merge them.

### EnrichmentRecord

The provenance of anything the LLM wrote.

- Holds: target (work/edition), field, value, `source` (`embedded` | `llm` | `user` | `filename`), `model_id`, `prompt_version`, `confidence`, `generated_at`, `accepted_at`.
- **Rule: publisher metadata beats model output, always.** LLM values fill empty fields or land in a `suggested` state awaiting the user. The UI must always be able to answer "where did this come from?".

### Job

Asynchronous work: `scan`, `extract`, `enrich`, `chunk`, `embed`.

- Holds: type, target, status (`pending` | `running` | `succeeded` | `failed` | `cancelled`), progress, attempts, error, timestamps.
- Every Job is **idempotent and resumable**. A 5000-book library is hours of GPU time; a restart must not start over.
- Claimed with `SELECT ... FOR UPDATE SKIP LOCKED`.
- **A finished job is not a record of anybody.** An upload's extraction
  carries the uploaded file's name and whom it is for, because the worker runs
  long after the request. Once it has finished nothing reads them, so they go
  when they would outlive their purpose: deleting an account, either way, and
  an administrator erasing somebody's books strip them (and who asked) from
  that person's finished jobs at once, and every finished job is deleted after
  `LEXARII_JOB_RETENTION` (30 days by default; at least a week, which
  enrichment on ingest reads back).

### Conversation / Message / Citation

The chat surface.

- A **Conversation** scopes a thread, optionally to a Collection or a set of Works.
- A **Message** is a turn. An assistant Message holds the routed `intent`, the retrieved Chunk ids, and the token budget used.
- A **Citation** links a Message to a Chunk and therefore to a Locator, which is what makes "jump to this passage in the book" work. Citations are the trust mechanism of the whole feature — an assistant claim without one is a defect.

---

## Query intents

The router classifies every question before retrieving anything. This is what keeps the local model's context window survivable.

| Intent | Meaning | Behaviour |
|---|---|---|
| `factual-lookup` | "What does Knuth say about loop invariants?" | Scope by subject, hybrid retrieve, rerank, answer with citations |
| `find-book` | "Which of my books covers category theory?" | Search Work-level metadata and summaries; return titles, do **not** synthesise an answer |
| `summarise-book` | "Summarise chapter 3 of X" | Scoped to one BookFile; structural retrieval, not similarity search |
| `broad-topic` | "Tell me about physics" | Too wide to answer — return a reading list and offer to narrow |
| `metadata` | "How many unread books do I have?" | Pure SQL. Retrieve nothing, embed nothing |

Routing is cheap and saves more tokens than any other optimisation.

---

## Naming rules

- Go: `Work`, `BookFile`, `Locator`, `Chunk`. SQL: `works`, `book_files`, `chunks`. Wire JSON: `camelCase`.
- Never use the bare word **"book"** in code. It is ambiguous between Work, Edition, and BookFile — that ambiguity is precisely the bug this model exists to prevent. In *user-facing copy*, "book" means Work, and that is the only place the word is allowed.
- "Library" means the user's whole collection. "Library directory" means the read-only folder of files on disk. Never shorten the second to "library".

## Who may do what

**Books belong to people.** Every `book_file` is held by whoever added it -
uploaded it, or scanned it out of the library directory. It is visible to its
holders, and to anyone a holder grants it to through `book_access`, and to
nobody else. A *work* is visible when at least one of its files is, so two
people can hold different formats of the same book and each see it through
their own copy.

This replaced a shared library. Until migration 17 the schema had no owner on
any book and everything with an account could read everything - a reasonable
design for one household sharing one shelf, and the wrong one here.

**A file can have several holders.** `book_file_holders (book_file_id, user_id,
original_filename)` is the single definition of who holds what. Two people in
one household who both add the same EPUB each hold it: one file on disk, one
set of extracted text, chunks, embeddings and enrichment - all of that is about
the *content* and is never duplicated - and two holdings. Until migration 33 a
file had exactly one `owner_id`, so the second person's upload was answered
"duplicate", never reached their shelf, and told them somebody else here had
that exact file.

Every holder has full owner rights to the FILE: reading, downloading, lending.
What is per holder:

- **The name.** `original_filename` is on the holding; the shelf shows each
  person the name they gave it. The file keeps the name it first arrived with,
  which only background work (merging formats by stem, the enrichment prompt)
  reads, so one person's upload never renames another's book.
- **The lending.** A grant is made BY a holding - `book_access` is keyed on
  `granted_by` and references the holding that made it - so each holder lends
  independently, sees and revokes only their own grants, and a holder's grants
  end with their holding. The access log records who lent the book at the time
  it was read (`access_log.lent_by`), so each holder sees the reading done
  through their own grants and nothing about another holder's lending.

**An upload never says what anybody else owns.** `duplicate` means *this person*
already holds the file. Somebody adding bytes another account already added -
settled, or still being extracted for them - gets exactly the reply a new file
gets, with a job of their own. While an extraction is in flight there is no row
to hold yet, so the upload leaves a claim on the content hash
(`holding_claims`), and the ingest that creates the file turns every claim into
a holding in the same transaction.

Be precise about what that buys. The REPLY is the same; the book is not new. A
file somebody else already holds is one file, so it reaches the second person's
shelf already extracted, carrying whatever title its other holders gave it, and
somebody watching closely can tell. Hiding that would mean extracting, indexing
and enriching every copy once per person, which is the duplication holdings
exist to avoid. What is not given away is anything beyond the bytes they
themselves supplied: no note, no other person's job, no other person's
filename, lending or reading.

**An upload only joins its uploader's own books.** A new file normally joins an
existing edition with the same ISBN, or with the same filename stem in another
format. For an upload, only an edition the uploader already holds a file of is
a candidate - otherwise anybody could attach a file to somebody else's book by
giving it that book's ISBN, see it appear on their shelf, and become one of its
editors. An edition that is not theirs is simply not found, exactly as if it
did not exist, and ISBNs are not unique (migration 35): two people's editions
of one book each carry its ISBN, so adding or editing one never fails in a way
that says somebody else has it. A scan is scoped the same way, to the scanning
administrator's own books: joined to somebody else's, a scanned PDF and an
uploaded EPUB would make a work nobody holds every file of, which nobody could
then edit or merge.

**Who may edit a work** is the view `work_editors (work_id, user_id)`: somebody
who holds EVERY file of it. Editing and merging both read it, so they cannot
come to disagree. A work's title is what everybody holding any of its files
sees, and merging moves all of its files, so holding one copy is not enough:
somebody lent an EPUB who uploads the same bytes holds that EPUB, and must not
thereby retitle - or merge away - a book whose other files, including ones
nobody lent them, are somebody else's. People who hold exactly the same files
of a work share it completely, and any of them may change it for all of them.
The duplicates page offers only works the viewer may merge.

**Two known, bounded leaks, kept on purpose.** Both say that a work has more to
it than the files the viewer can open; neither says whose, or anything about
who holds or lends what.

- Somebody who holds one copy and is refused an edit or a merge learns that the
  work has a file they cannot see. Deciding by the files they can see instead
  would undo the rule above: somebody lent one EPUB, who uploads it back, holds
  every file *they* can see, and could retitle the owner's book again.
- A work's page lists all of its editions (publisher, ISBN, year), including
  editions whose files the viewer cannot open. Its FILES are filtered to the
  ones the viewer may open; its editions are not yet.
Administrators have no wider right to edit or merge by hand than anybody else.
What they can do on top is the existing, separate power to accept or reject a
metadata suggestion.

Two views are the single definition of access, so no query can quietly
disagree with another: `readable_book_files (user_id, book_file_id,
can_download)` and `readable_works (user_id, work_id)`. Everything that reads a
book joins one of them. With `book_file_holders` and `work_editors` beside them
that is the whole of the access model: who holds, who may read, who may edit.

**What a shelf costs on disk** is a sum over holdings: `book_file_holders` joined
to `book_files.size_bytes` for one person. Each holder is counted for the whole
file - it is what they added - while the installation stores it once.

**Who else is here is listed, unless the installation says not.** Lending
needs a name to point at, so by default anybody signed in may list every
account's username (`GET /people`) - in a household that list is the point.
On a larger installation it is every member, handed to each of them, so
`LEXARII_PEOPLE_DIRECTORY=none` lists accounts to administrators only and
everybody else names a recipient by their exact username instead. A name
nobody has is answered plainly as not found. Sign-in deliberately does not say
whether a username exists, but lending by name cannot help it - a reply that
hid it would leave somebody believing they had lent a book to nobody - so the
setting stops handing out the list, not guessing; usernames name people here,
they are not credentials. The answer says nothing about any book. The book is
checked first: a request about a book the caller may not lend (or, for
notes, read) is the book's plain not-found and the name is never looked up,
so only somebody with a book can guess at all - and guesses that match nobody
are counted per person, and refused for a while past a handful.

**Reading and downloading are different permissions.** A grant carries
`can_download`, false by default. A holder always may. Withholding it means the
original file is never sent: `GET /books/{id}/file` refuses, and an EPUB reader
receives sanitised per-chapter HTML instead, which it already did.

Be precise about what that achieves. It does NOT stop a determined person
keeping what they can read - screenshots and OCR exist, and no arrangement of
this code changes that. It raises the cost from one click to several hundred
requests, and it leaves a record of who was given what. That is worth having
and it is not the same as prevention, and anyone relying on it for a legal
obligation should know the difference.

**Background work has no viewer.** Chunking, embedding and enrichment act as
the installation rather than as a person: they run long after the request that
queued them, and must index a book whose holders are not signed in. `BookFile` has
no viewer for that reason and `ReadableBookFile` exists separately for the
serving paths. The distinction is deliberate and load-bearing.

**Reading somebody else's book is recorded; reading your own is not.**
`book_access` holds the permission, `access_log` holds the fact. The holder
check is inside the statement that writes, so a person's own library - any file
they hold, whoever else holds it - is never logged and the rule cannot be
forgotten by a later caller. An audit trail over
someone's own reading is not an audit trail, and it would bury the entries that
matter under everything else they read that week.

The log aggregates: one row per person, book, day and kind, carrying `views`
(requests) and the DISTINCT units seen. `cardinality(units)` against the book's
page count answers the question a lender actually has - how much of it did they
take - and distinguishes three pages read four times from three pages taken.
A row per page view would have meant 558 rows for one reading of one book.

**A book can be taken off a shelf.** `DELETE /books/{id}/holding` ends the
caller's holding of one file, and only ever their own - administrators
included. (Taking a file off the installation for everybody is a different
act, below.) It is account deletion's rule for that one file, through the same
code: a file somebody else holds simply loses a holder, and one nobody else
holds is archived against the caller's name and moved if Lexarii stores it.
The reply is the same empty 204 either way, because which of the two happened
would say whether anybody else holds the file.

What goes with the holding is what the holding made: the grants they gave from
it, and the sharing of their notes on it unless they can still read the book
through somebody else's grant. An extraction of their own upload still in the
queue runs on - the file may be somebody else's too, and may be the copy that
brings their missing book back - but does not put it back on their shelf. What stays is what they WROTE: highlights, notes
and reading position. Losing a book is not a reason to lose your notes on it -
it is not when a lent book is withdrawn either - and because the file is the
same file, adding the same bytes again brings every mark back where it was.
Deleting notes is a separate act with its own controls.

**An account can be deleted; books only they held are archived.** Deleting a
person deletes the person: annotations, reading positions, conversations,
collections and their holdings go immediately, by cascade, and so do the grants
they made. Their books do not, because a book is a file rather than a fact about
somebody.

A book somebody else also holds simply loses a holder. It is not archived and
not moved, and nothing on the other holder's shelf changes - including what
they lend to whom.

A book nobody else holds is marked `archived_at` with the name that held it in
`archived_owner` - text, not a reference, since the row it would point at is the
one being deleted - and is visible to nobody meanwhile, because
`readable_book_files` requires a holder. An administrator can hand one to a new
holder, at which point it becomes an ordinary book again; nothing the leaver
lent comes back with it. Somebody uploading the same bytes gets it back the same
way, since their upload would otherwise vanish from their shelf. A rescan never
does: claiming an archived book is a decision, not a side effect.

**A scan holds what it finds, unless you let it go.** The administrator who
scans a file holds it, whoever else does: their library directory is theirs,
even where somebody uploaded the same file first, and leaving it off their
shelf would say who had. Letting a book go is remembered (`released_holdings`,
per person and per file's bytes), so neither the next scan nor an extraction
already queued for them puts it back; only a deliberate act - uploading it
again, or an administrator handing it to them - forgets it. The rule is the
same whether the file has one holder or several.

**Nothing is ever on nobody's shelf.** A file that ends up with no holder - the
person an upload was for deleted their account before it was extracted, say -
is archived, with no name recorded, so an administrator can see it and hand it
on. Migration 33 archived the files that had no owner for the same reason,
under the marker "nobody (no owner was recorded)".

**Moving an archived file waits for the file's lock.** Archiving commits first
and the file is moved afterwards; the move takes the same per-file lock as
every change of holder and happens only if the book is still archived and
still where it was. Somebody uploading the same bytes in the meantime brings
the book back and keeps it where it is. An extraction that finishes after the
book was archived never points it back at where its bytes were, and an
extraction never points a book at a path that does not exist - both checked
under the same lock. An extraction that finds its bytes gone
reads them from wherever the book's row says they now are, a re-upload of a
book whose extraction failed is tried again, and an archive copy a fresh upload
has replaced is removed rather than left on disk.

Books they had BORROWED need no handling: `book_access` cascades, so the access
ends and the book is simply its holders' again.

**An installation can erase them instead.** Some installations must not keep
what a person added once that person is gone - a larger one with a retention
rule, say. With `LEXARII_DELETED_ACCOUNT_FILES=erase` a book only the leaver
held is not archived but erased: its record, its passages and vectors, its
cover, its access log, the copies Lexarii stored in its upload directory or
archive, and everybody's notes and reading positions on it - what an archived
record takes with it when it expires, now rather than in six months. A copy in
the library directory is never touched, as ever; its record goes, and a later
scan finds the file as new. A book somebody else also holds still just loses
a holder.

The records go in one transaction with the account. The files cannot - a
rollback cannot bring a deleted file back - so that transaction writes down
what it owes the disk (`pending_discards`), and the copies are deleted after
it commits: at once, then at every start of the server and every hour, each
under its bytes' lock and only while nothing wants them. A failed disk or a
crash delays the deletion; nothing loses track of it.

Somebody adding the same bytes again wins. If a record of them exists again
by the time a debt is paid - an upload extracted, a scan that found the file -
the bytes are that record's holder's, nothing of them is the leaver's to
erase, and only the erased record's cover goes: no copy is deleted on a guess
about which ones the new record still needs. An upload on its way - recorded
and its extraction queued or running, or recorded within the last hour and
its extraction about to be queued - makes the debt wait, deleting meanwhile
only what no upload reads (the cover, the archive's copies); a claim whose
extraction failed for good holds nothing up. A debt that has waited a day is
logged, once. An upload that stored its copy before a debt was paid and is
recorded after puts its own copy back as it is recorded, under the same lock,
so it never points at nothing.

What erasing keeps, and until when, precisely:

- A book only the leaver held that somebody else is **waiting to hold** -
  their upload of the same bytes not extracted yet - is kept, archived with
  no name, and their extraction makes it theirs. If it never does, the record
  expires with the archive (`LEXARII_ARCHIVE_RETENTION`), and in erase mode an
  expiring record's stored copies are owed and deleted too.
- The leaver's own uploads **still waiting to be extracted** are cancelled and
  their bytes owed. One whose extraction is **already running** finishes, as a
  nameless archived record, and goes the same way when the archive expires it.
- A copy the disk refuses to delete stays owed and is retried every hour.
- Anything in the library directory stays: it is the operator's.

An administrator can do the same to one person's books without deleting the
account (`POST /admin/users/{id}/erase-files`), for a retention rule that
reaches the books first. It goes in batches of a thousand files, a
transaction each, so a very large shelf never holds more locks than the
server has room for. The release is remembered, so a scan does not hand a
library-directory file straight back. Their uploads still waiting to be
extracted are withdrawn as a deletion would withdraw them: the claim on the
bytes goes, a queued extraction is cancelled and its stored copy owed, and the
release is remembered too - so neither somebody else's extraction of the same
bytes nor their own already running puts the book on their shelf afterwards.
(One already running finishes as a nameless archived record, as above.) A
claim never outlives a release of the same bytes: the extraction that would
turn it into a holding refuses, exactly as it refuses its own uploader. Deleting an account stays one
transaction, as it always was, and takes a lock per file the person holds;
for a shelf of tens of thousands, erase their books first and then delete the
account, or raise the server's `max_locks_per_transaction`.

After six months an unclaimed archive RECORD is deleted and the FILE is kept.
The asymmetry is the point: a catalogue entry naming somebody who left is a
record about a person, while the book is bytes on a disk that cost nothing to
keep and cannot be recovered once gone.

The file is only ever moved when Lexarii stored it - an upload, in Lexarii's
own directory, with no other home - and only when its last holder goes. A book scanned from the library directory
stays exactly where it is. That directory is read-only to us, and moving
somebody's own file out of their own folder because an unrelated account was
deleted would not be archiving it.

**An administrator can take a file off the installation for everybody.**
Somebody running a library several people add to may need to make sure one
file is not kept here at all, and stays gone.
`POST /admin/books/{id}/remove` with a reason does that, and it is the one
change to a holding that is not the holder's own: it is about the
installation, not about a shelf.

It is letting go, for every holder at once, and two things more. The bytes are
**blocked** - `blocked_content`, by content hash, like `released_holdings` -
so an upload of them is refused (`422 content_blocked`), a scan passes over
them, and an extraction already queued records nothing; every one of those
checks under the file's lock, so a removal and an arrival cannot interleave.
And what was derived from them goes: the passages and their vectors, the
cover, and every copy Lexarii itself stored, in its upload directory or its
archive. Those are deleted after the removal commits, under the same lock, and
the deletion runs to the end whether or not whoever asked is still waiting. If
it fails, the blocklist says copies remain and offers to finish, and the next
start of the server tries again. A copy in the library directory is never
touched - that directory is read-only - and the block is what keeps it off
every shelf. Everybody who held the file, or was still waiting for their
upload of it to be extracted, loses it; the reply counts them and does not name
them. Every grant made from it ends with the holdings, and every share of
somebody's notes on it ends too.

Be precise about what the refusal says. A block is made from a file, so being
told these bytes are blocked does say they were here once, and that an
administrator decided against them. It says nothing more: not who added them,
not who removed them or why, and not whether anybody has them now - nobody
does.

**What people wrote on it stays theirs.** The rule for marks is the rule for
letting go: highlights, notes and reading positions are kept, because deleting
what somebody wrote is a separate act with its own controls, and an
administrator's decision about a file is not that act. So the file's record is
not deleted. It is archived, since nobody holds it, and marked missing, so no
sweep indexes, enriches or draws a cover for it; it is left out of the archive
while blocked, so it cannot be handed on; and it expires with the archive like
any other record, taking the marks with it then.

**Some of the file's words outlive it, unless the administrator erases them.**
A highlight keeps the text it marked. A chat citation keeps an excerpt of the
passage it cites - up to 400 characters, since migration 30, so an answer's
evidence survives re-chunking - and an assistant's answer may quote the book in
its own words. All of that stays by default, visible only to the person whose
highlight or conversation it is. Removing with **"also erase quoted
passages"** (`purgeQuotes`) blanks the quoted text of every highlight on the
file and the excerpt of every citation of it, in the same transaction. The
highlights, their notes and places, and the citations and their places stay:
nobody loses what they wrote, only the book's words. A highlight's edit time
moves on, so a device still holding the old quote cannot sync it back. An
answer's own text is never rewritten - it is the conversation, not a copy of
the book - so whatever it quoted stays; deleting the conversation is its
owner's act.

**Lifting a block brings nothing back by itself.** After
`DELETE /admin/blocked/{hash}` the bytes may be added again, and what that
means depends on where they lived. An uploaded file's bytes were deleted, so
only adding the same bytes again - uploading them - brings that book back, with
every mark on it, exactly as after letting go; its record stays out of the
archive meanwhile, since there would be nothing to hand on. A file in the
library directory was never deleted: the next scan that finds it marks it
present again, and it reappears in the archive for an administrator to hand
on, since a scan never puts an archived book on a shelf by itself.

**Not being allowed to see a book reports "not found", never "forbidden."**
Someone who may not see a book should not learn that it exists.

**Changing the shared half needs an administrator.** `users.is_admin` had
existed since the second migration and was checked by nothing, which meant every
reader could upload files, trigger a rescan, spend the household's model on
enrichment, and accept metadata suggestions that rewrite a book for everyone.
The flag is now enforced on exactly those endpoints:

| Action | Who |
|---|---|
| Browse, search, read, annotate, track position, ask questions | anyone signed in |
| Upload files | administrator, or anyone signed in with `LEXARII_UPLOAD_POLICY=users` |
| Scan the library | administrator |
| Start enrichment or indexing | administrator |
| Accept or reject a metadata suggestion | administrator |
| Take a book off a shelf | its holder, for their own holding only |
| Remove a file for everybody, see and lift the blocklist | administrator |
| Follow a job's progress | whoever it was queued for, and administrators |
| Cancel a job | administrator |

Uploading can be opened to everybody because, with holdings, an upload changes
only the uploader's shelf: the book is theirs, visible to nobody else until
they lend it, and adding a file somebody else already has is answered as
though it were new, and never attaches to anybody else's book.
The default stays `admins`, so an installation that upgrades changes nothing.

A job's progress note and error describe a file, and the file may be somebody
else's, so following one is not open to whoever holds its id. An upload's
extraction is queued per person for the same reason: folded into another
person's job, the reply would hand them that job's id.

Reading, annotating and asking are deliberately NOT restricted: they are the
point of having an account, and they affect nobody but the person doing them.

The check is a database read per request rather than a flag cached in the
session, so revoking someone's administrator rights takes effect immediately
rather than at their next sign-in. The endpoints behind it are rare and already
expensive, so the read costs nothing worth saving.
