# Lexarii — Architecture

Self-hosted ebook library, reader, and RAG research assistant. Go backend, SvelteKit client, Postgres with pgvector, and *your own* model over an OpenAI-compatible HTTP endpoint.

Read [DOMAIN.md](DOMAIN.md) first — it defines every noun used below.

---

## Design principles

1. **The reader is the product.** Everything else is plumbing that exists to get a book in front of the user. When a trade-off appears, the reading experience wins.
2. **The library directory is read-only.** Lexarii never writes to the user's book files. Derived data goes to its own directory. This is the promise that makes people willing to point it at a 20-year collection.
3. **The LLM is optional and remote.** It is a box in the user's house that will be off half the time. With it unreachable, Lexarii is still a complete library and reader; only enrichment and chat degrade.
4. **Retrieval quality is the feature.** Generation is cheap. A local model with well-chosen passages beats a large model with badly-chosen ones.
5. **Free software, end to end.** AGPL server, libre client, no proprietary dependencies anywhere — F-Droid's rules are the design constraint from day one, not a porting problem later.
6. **One command to run it.** A single static binary, or `docker compose up`.

---

## System shape

```
                        ┌───────────────────────────────┐
   browser / PWA  ────▶ │  SvelteKit client             │
   (later: Android)     │  library · reader · chat      │
                        └──────────────┬────────────────┘
                                       │ HTTP + SSE (JSON, camelCase)
                        ┌──────────────▼────────────────┐
                        │  Go server (single binary)    │
                        │  ┌─────────────────────────┐  │
                        │  │ internal/http           │  │  handlers, DTOs, SSE
                        │  ├─────────────────────────┤  │
                        │  │ internal/service        │  │  library, reading, chat, ingest
                        │  ├─────────────────────────┤  │
                        │  │ internal/store          │  │  pgx repositories
                        │  └─────────────────────────┘  │
                        │  internal/worker  (job pool)  │
                        └────┬────────────────┬─────────┘
                             │                │
            ┌────────────────▼──┐   ┌─────────▼──────────────┐
            │ Postgres 18       │   │ llama.cpp / Ollama     │
            │ + pgvector (HNSW) │   │ OpenAI-compatible HTTP │
            │ + FTS (GIN)       │   │ chat + embeddings      │
            │ + pg_trgm         │   │ (may be unreachable)   │
            └───────────────────┘   └────────────────────────┘
                     ▲
            ┌────────┴──────────┐
            │ library dir (ro)  │   user's ebook files, never written to
            │ data dir  (rw)    │   covers, extracted text, thumbnails
            └───────────────────┘
```

---

## Backend layering

Dependencies point inward. This is enforced in review, and violations compound fast.

| Package | Knows about | Must not know about |
|---|---|---|
| `cmd/lexarii` | everything, briefly — config, wiring, shutdown | any business logic of its own |
| `internal/http` | HTTP, DTOs, SSE, auth middleware | SQL, pgx, file formats |
| `internal/service` | domain, orchestration, transactions | HTTP status codes, SQL dialects |
| `internal/store` | SQL, pgx, migrations | HTTP, business rules |
| `internal/domain` | itself | everything else |
| `internal/worker` | services, job queue | HTTP |
| `internal/ingest` | file formats, archives, extraction | HTTP, SQL |
| `internal/llm` | the provider HTTP contract, prompts | SQL, HTTP handlers |
| `internal/rag` | chunking, retrieval, fusion, reranking | HTTP handlers |

Interfaces are declared by the **consumer**, not the implementer: `internal/service` defines the narrow interface it needs and `internal/store` happens to satisfy it. Services are then testable with 20-line hand-written fakes and no mocking framework.

---

## The ingest pipeline

Five stages, each a resumable job, each idempotent, each keyed on `content_hash` rather than path.

```
 scan ──▶ extract ──▶ enrich ──▶ chunk ──▶ embed
```

1. **scan** — walk the library directory, hash every file, create or match a `BookFile`. Unchanged hashes are skipped. A moved file is recognised and its path updated, never duplicated.
2. **extract** — parse the format. Produce canonical text with **Locators**, spine order, TOC, cover, and embedded metadata with `source: embedded`. No guessing happens here. Scanned PDFs with no text layer are marked `needs_ocr` rather than ingested empty.
3. **enrich** — the first LLM stage. Feed title, embedded metadata, TOC, and roughly the first 2000 tokens of body text; get back subject classification, tags, and a summary as schema-validated JSON. Written as `EnrichmentRecord`s with provenance. **Never overwrites embedded metadata.** Skipped entirely when the model server is down; the book is still fully usable.
4. **chunk** — structure-aware splitting (chapter → section → paragraph), ~512 tokens with ~15% overlap, never across a chapter boundary, never mid-sentence. Each chunk records its start and end Locator and the `chunk_strategy_version`.
5. **embed** — batch the chunks through the embedding endpoint, normalise, store with `model_id`. HNSW indexes are built *after* a bulk load, never before.

Failure of one file never fails the scan. The error is recorded against that `BookFile` and surfaced in the UI.

Chunking is not started by ingest unless asked for. By default an administrator queues it (`POST /api/v1/index`), because the embedding that follows is GPU time and a first scan of a large library is a great deal of it. `LEXARII_INDEX_ON_INGEST=true` queues it at the end of each successful extraction instead, for uploads and scanned files alike, so a new book is searchable as soon as it is on the shelf. The job is inserted only if the file is waiting for chunks under the current strategy version - the same rule the manual sweep uses, checked in the same statement as the insert - so a re-run of an interrupted extraction still queues it and a re-extract of a file already chunked does not throw its passages and embeddings away. The chunk job is keyed on the file, so the queue holds at most one per file however many paths ask.

Enrichment is the same, and off by default for the same reason: it is the model's time. An administrator queues it (`POST /api/v1/enrich`); `LEXARII_ENRICH_ON_INGEST=true` queues it at the end of each successful extraction instead, for the extracted file's work, if the work is waiting to be described under the current prompt version - the sweep's rule, `needsEnrichment`, checked in the statement that inserts the job. The job is keyed on the work and belongs to whoever the extraction was for: the uploader, or the administrator who scanned, who may then follow it. When two extractions of one work ask at once, the queue keeps the first job, so the enrichment is attributed to the first extraction's requester. Because it may be followed by somebody who does not hold every file of the work, its error says only what kind of thing went wrong - never the file sampled, what the parser made of it, or what the model replied; the log has those. A work counts as waiting only if it has an EPUB or PDF to sample. On ingest it also waits a week after the model was last asked about it under the same prompt with nothing to show for it, so re-extractions do not spend the model on the same nothing again; a job that never reached the model - skipped until it gave up, because the model was off - does not count. The administrator's sweep has no cooldown: somebody pressing the button wants it asked now.

**The queue's order is one table**, `domain.JobType.Priority`: scan, then extract, then chunk, then embed, and enrichment last. A worker takes the highest priority runnable job, the oldest of equals. Enrichment is last because it is the model's time per book; queued ahead of extraction, as it once was, enrichment on ingest would hold every worker at the model while a first scan's books waited to reach the shelf. With no chat model configured the setting is ignored rather than filling the queue with jobs that can only wait. Bytes removed for everyone never get this far, since their extraction records nothing.

---

## The retrieval pipeline

This is the part that has to be right, and the part most likely to be got wrong by guessing.

```
question
   │
   ├─▶ 1. ROUTE        classify intent + subject          (cheap, saves the most)
   │
   ├─▶ 2. SCOPE        book-level only (a conversation pinned to one book)
   │                   NOT by subject - measured harmful, see below
   │
   ├─▶ 3. RETRIEVE     dense (pgvector cosine, HNSW)  ∪  sparse (Postgres FTS)
   │                   fuse with Reciprocal Rank Fusion, k = 60  →  ~50 candidates
   │
   ├─▶ 4. RERANK       cross-encoder, or LLM-as-reranker over titles + snippets
   │                   ~50  →  ~8
   │
   ├─▶ 5. ASSEMBLE     explicit token budget, dedupe, neighbour expansion only
   │                   at broken boundaries; each passage tagged with
   │                   book · author · chapter · locator
   │
   └─▶ 6. GENERATE     answer only from passages · cite every claim ·
                       say "not in your library" when unsupported
```

**Why hybrid and not pure vector search:** a technical library is full of proper nouns, symbols, and rare terms — exactly where dense retrieval fails and BM25 shines. Dense-only retrieval looks fine in a demo and disappoints on the first real question.

**Why routing comes first:** `"how many unread books do I have"` needs SQL and zero retrieval. `"which book covers category theory"` needs Work-level metadata, not passages. Sending everything down the same path burns the local model's context on questions that never needed it.

**Why the router's subject guess does not narrow the search — measured, not assumed.** Scoping retrieval to the books whose subjects matched the route cost **28 points of work Recall@10** on the eval set (0.875 → 0.594) and 21 of MRR. Demoting it from a filter to a modest boost still lost 12 points (0.750), promoting five questions and demoting nine — five of those from rank 1 to outside the top ten entirely. "About the right subject" and "answers this question" are different properties, and in a library where 48 books are tagged `mathematics` the first is nearly free. The prefilter is a scale optimisation for a corpus far larger than this one; at 81,230 passages a library-wide search costs 11ms. Revisit it when searching everything is actually slow, and re-run `make eval` when you do.

**Why reranking is not optional:** it buys more answer quality per unit of effort than any other single stage, because it is the only stage that reads the candidate text in light of the actual question.

**The reader's own notes are searched beside the passages, and kept apart from them.** A margin note is what the reader thought; a passage is what the book said. They reach the answer prompt in separate blocks, numbered in one citation sequence so `[11]` is simply the eleventh thing the answer was shown. Only ever the asker's own — the filter is `user_id`, deliberately not the readable-books view that governs passages, because a shared book does not share the margins.

Notes are found by keyword, with a fuzzy fallback, and the costs were measured rather than assumed:

| | |
|---|---|
| keyword search, 20,000 notes | 2.9 ms |
| trigram index, 20,000 notes | 15.2 ms |
| `word_similarity` over one person's notes | ~6 µs per note |

**The trigram index is slower than no index**, which is the part worth remembering: it searches every reader's notes before the `user_id` filter reduces the work to a few hundred rows. So there is no index, and the fuzzy pass costs about 6 ms for somebody with a thousand notes and 30 ms for a heavy annotator with five thousand — against a question that takes roughly 2.7 seconds end to end. It runs only when the keyword search finds nothing, so the ordinary question pays none of it.

Both thresholds are measured. Keyword: the question's words are ORed rather than ANDed, because a note is a sentence and a question rarely uses all of its words — `"is fuzz testing worth doing?"` failed against a note about fuzz testing on the word *worth* alone. ORing needs a floor, and `ts_rank_cd ≥ 0.1` is below any real match (0.8–3.0 observed) and above the noise (0.0). Fuzzy: `word_similarity ≥ 0.4`, where the right note scores 0.489 against a question with a word misspelled and the worst unrelated pairing scores 0.231. That margin is thin, which is why the fuzzy pass runs last and why anything it finds is labelled a note rather than quoted as a book.

**A backlink between books is found, not declared.** Nobody types the links between the books on a shelf. What exists instead is one book naming another in its bibliography, its index, its further-reading list — and the full-text index built for retrieval already holds every one. `Mentions` searches it for a work's title as a phrase plus its author's surname in the same passage, scoped to what the viewer can open.

Both halves are load-bearing, measured on the 168-book library: the title alone finds "Linear Algebra" as a phrase in 23 other books and "Reinforcement Learning" in 11, none of them citing anything, because a title that is also a subject matches every book about the subject. With the surname, 31 of 168 books have a mention and the ones read by hand are real. A one-word title and a surname under four letters are both skipped for the same reason. The surname is matched against the text with word boundaries rather than through the tsvector, because stemming turns "Powers" into "power" and would match the word. 9ms for the slowest book, 351ms for all 168 — a live query, not a table.

**A PDF's own highlights are read in the browser, for the same reason covers are drawn there.** The marks are rectangles over the page; recovering the words under one needs to know where the words are, and pdf.js gives every run of text a position while the server's extractor reports every word at position zero on exactly the books that carry annotations. What it takes to do it accurately, learned by measurement on a book holding 1,051 underlines:

- `quadPoints` is a `Float32Array`, not an array. `Array.isArray` rejected all 1,051 while reporting there was nothing to import.
- An underline sits *below* the baseline, so comparing the area a mark shares with a text box finds nothing. The test is horizontal overlap plus nearness to one baseline.
- pdf.js returns a whole line as one run, so a mark over three words of a forty-word line has to be cut out of it by proportion — there are no per-character positions.
- Runs must be grouped into lines first and the mark matched to exactly one. Matching every run whose band overlapped turned a highlight over `(2.2.3)` into `integra ( 2.2.3) nts of`.

805 of the 1,051 recover their words; the rest mark figures and equations set as vector art, where there is no text to recover. Nothing is ever written back into the book file — the library directory is read-only, which is the first invariant in this project.

**Evaluation is part of the pipeline, not an afterthought.** `eval/questions.jsonl` holds question → expected book(s) → expected passage(s). Recall@10 and MRR are reported before and after every change to chunking, fusion, or reranking. Retrieval tuned by vibes regresses silently.

---

## Data model in Postgres

Extensions: `vector`, `pg_trgm`, `pgcrypto`.

```
works ──┬── work_authors ── authors
        ├── work_tags ── tags
        ├── collection_works ── collections (nestable)
        ├── enrichment_records
        └── editions ──┬── book_files ──┬── chunks ── embeddings (model_id!)
                       │                ├── annotations
                       └── covers       └── reading_progress

conversations ── messages ── citations ──▶ chunks
jobs   (FOR UPDATE SKIP LOCKED)
users, sessions
```

Index strategy:

- `embeddings.vector` — HNSW, `vector_cosine_ops`, `m=16`, `ef_construction=64`; `ef_search` tuned per query with `SET LOCAL`.
- `chunks.tsv` — generated `tsvector` column, GIN index. Computed in the database, never in Go.
- `works.title`, `authors.name` — `pg_trgm` GIN, for fuzzy library search. A different problem from chunk retrieval, deserving its own index.
- `book_files.content_hash` — `UNIQUE`. Re-ingest idempotence enforced by the database, not by application logic.

---

## Frontend

SvelteKit 2 + Svelte 5 runes + TypeScript strict. Plain scoped CSS driven by design tokens. Budget: initial route under ~120KB gzipped.

Three surfaces, deliberately separate:

- **Library** — virtualised cover grid, search, filters, collections, book detail. The front door.
- **Reader** — paginated CSS multi-column, no chrome, locator-based position, offline-capable. Its own module because it is a genuinely hard problem and the whole point of the app.
- **Chat** — SSE-streamed answers with citation chips that deep-link into the reader at the exact locator. That link is what makes the RAG trustworthy.

Data loading uses `+page.server.ts` load functions and form actions. Nothing the page needs to render is fetched in `onMount`.

The PWA shell and the offline book cache are a dry run for the Android client — patterns that port are preferred over patterns that do not.

---

## The catalogue every reader already speaks

`/opds` serves the library as OPDS 1.2, which is Atom XML. Version 1.2 rather than the newer JSON 2.0 because 1.2 is what the readers people actually own can open, and a catalogue nothing can read is not a catalogue.

It is the same store queries the web shelf uses — a catalogue is a shelf with a different skin — so search, subjects, collections and paging behave identically by construction rather than by agreement. One acquisition link per book, to the best file the viewer can read, which the listing query already chooses; a second query per book to enumerate every format would cost a query per book for a choice almost nobody makes. A book whose drive is unmounted gets no acquisition link at all, because a link that resolves to an error is worse than an absence.

**Authentication is the part that decides whether this works.** A reader app knows how to send a username and a password and nothing else, so Basic auth is accepted — and the password may be a device token from `/auth/token`, which is the way to use it: revocable on its own, and the account password never goes into a reader's settings screen where it sits in cleartext on a phone. The token is tried first and costs one indexed lookup; a real password falls through to the same throttle as every other sign-in.

Two things had to be right and were not, both found by pointing something at it:

- `/opds` was being **served to anybody**. `RequireAuth` treats everything outside `/api` as a static asset of the web client, which is correct for the sign-in page and catastrophic for a read of somebody's whole library.
- A refusal carried no `WWW-Authenticate` header, so a reader app got a 401 it could not act on instead of a password box. That single missing header is most of what "the catalogue does not work" means in this category of software.

---

## Running Lexarii inside something else

Some installations put Lexarii behind their own front door or mount it in a larger Go program. The seams for that are small and generic, so nobody has to fork the authentication to add a rule of their own.

**An access policy after sign-in.** `Router.Authorize` (an `AuthorizeFunc`) is asked once per protected request, after the session, device token, Basic credentials or trusted proxy has said who the user is, and never on an open route - a policy must not be able to close the sign-in page. It returns nil to allow, a `*PolicyError{Status, Code, Message}` to refuse in the standard error shape, or any other error, which refuses with a 500: a policy that could not be evaluated did not allow anything. A read-only maintenance window or an account suspended without being deleted are a few lines each. Unset, the router behaves exactly as it always has.

**The same protections on handlers mounted beside the router.** `Recover`, `LogRequests` and `SecureHeaders` are what the router wraps everything it serves in; they are exported so a handler served next to it gets the same panic recovery, request log and security headers. Apply them in the router's order, `Recover(LogRequests(SecureHeaders(h)))`.

**Background work that has to wait.** The worker pool runs whatever handlers it is given, so an installation can wrap them before passing them to `worker.New`. A wrapper that decides a job may not run yet - work queued for one person waiting until their share of the model's time comes round, say - returns `worker.Defer(until, reason)`. The job goes back to the queue, pending, runnable from `until`, with the attempt its claim took given back and no skip counted, and `reason` becomes its progress note. `until` is held between one second and 62 days from now, and a deferral the database cannot record at once is tried again rather than left for the reclaimer to count against the job - never recorded as a skip, which would count toward a limit meant for a model that is not there. Every change a worker makes to a job it claimed - progress, success, failure, a skip, a deferral - names that worker and does nothing if it no longer holds the job, so a worker that went quiet, was reclaimed from and comes back late cannot undo another's run. Worker ids carry the host, the process and a random part, so this holds across two processes sharing one database, a rolling restart included. Unlike a skip, which backs off on its own clock and gives up eventually, a deferral waits exactly as long as it is told. The reason is shown to whoever may follow the job, so it should be neutral. `Job.RequestedBy` says whose work a job is - the uploader for an upload's extraction and the enrichment it queues, the administrator for a scan, nobody for the installation's own sweeps.

**Pages a signed-out visitor may see.** The web client checks the session on every route and sends a signed-out visitor to `/signin`. A route that should be shown to anybody returns `PUBLIC_ROUTE` from its load function (`$lib/auth/public`) - in `+page.ts` for one page, in `+layout.ts` for a group of them - and `/signin` itself is declared that way. This only decides what the client shows: a public page can display nothing the server would not give an anonymous request.

**An installation's own look.** `LEXARII_BRANDING_DIR` serves a directory read-only at `/branding/` (`BrandingFiles`, mounted as `Router.Branding`: an allowlist of stylesheet, image and font types, no listings, no dotfiles, a size cap, opened with `os.OpenInRoot`), and `AuthHandler.Branding` tells the client, through the open providers response, to link `/branding/branding.css` after its own styles. A program that serves `/branding/` itself sets only the flag, or mounts its own handler as `Router.Branding`. What that stylesheet may rely on - `data-page` on `<body>` for every route, the sign-in page's `.auth-card`, `.auth-brand`, `.auth-links` and `.auth-decoration`, and the design tokens - is a supported contract, written down in [BRANDING.md](BRANDING.md). It is CSS only: nothing in the directory can add markup or run script.

**What the server lets through without a session** is decided by `RequireAuth` on the method and the escaped path, the way the router matches them: the open endpoints for their own method only, the federated sign-in routes as exactly `GET /api/v1/auth/{provider}/start` or `/callback`, and the web client. A path the router could read differently from that check never counts as open, so a signed-out request whose path contains `%`, a backslash, `//`, or a `.`/`..` segment gets a 401 rather than the app shell. No route of the web client looks like that.

---

## Syncing a client that was offline

A phone that has been out of signal for a week is the case this is shaped around. Two endpoints, `GET` and `POST /api/v1/sync`, and four rules that decide everything else.

**Identity comes from the client.** A mark made on a plane has to exist before any server has seen it, so the client generates the UUID. That id is also what makes the protocol safe to repeat: the connection that drops is the one carrying the response, so every push is re-sent, lands on the same rows, and changes nothing the second time.

**Time comes from the client, and the cursor comes from the server.** A pushed change carries the timestamp of when it was MADE, not when it was sent — a phone in a tunnel for a week did not edit anything at the moment it found signal. A pull carries the server's clock back as `until`, to be sent as the next `since`, because two clocks a minute apart silently lose or repeat a minute of somebody's reading. Reads start a couple of seconds before `since`: a row's `updated_at` is set when its statement runs and becomes visible when its transaction commits, which is later, so a narrow window would drop a change that committed just after the cursor was taken.

**A deletion is a fact that has to be stored.** `annotation_tombstones` holds the id, the book and the moment, and is written in the same transaction as the delete — by the ordinary web delete too, not only the sync path, since deleting a highlight in the browser is the direction that actually happens. Without it a deleted mark is indistinguishable from one the client has not been told about, and it comes back on the next sync. A tombstone also refuses a resurrection: an offline edit of a deleted mark does not re-create it. It is a separate table rather than a `deleted_at` column because the live table has a dozen queries against it, and the one that forgot the predicate would be the one that showed somebody a mark they had deleted.

**Marks are last-write-wins; positions are not.** A mark is small, edited by one person, rarely in two places at once, so the version edited most recently survives and the merge nobody could explain is not worth building. A position is two facts and gets two rules: where the reader IS takes the newest by the reader's own clock, backwards included, because turning back a chapter is reading; how far they GOT only ever advances, from any device, with no clock involved at all — which is what stops a laptop that opened chapter one from erasing an evening's reading on a phone. That second rule needs no conflict resolution because it cannot conflict.

Refusals are per item. A book that was unshared while the phone was away is not a reason to discard the other forty changes it carried back.

---

## Deployment

**Single binary.** The built client is embedded with `go:embed` and served by Go. `CGO_ENABLED=0`, static, cross-compiled for `linux/amd64` and `linux/arm64`. `./lexarii` plus a `DATABASE_URL` is the whole install.

**docker compose.** `lexarii` + `postgres:18` with pgvector. The library directory is mounted **read-only** in the compose file, deliberately and with a comment.

`/api/v1/health` is open and answers only `ok`/`degraded`/`down` (503 when the database is gone; model pings are cached for 30s and shared between requests). To an administrator it reports version, database reachability, and LLM reachability *separately*, so a self-hoster debugging "the AI does nothing" can see which leg is down.

### Hosted chat providers (OpenRouter and similar)

The chat model is any OpenAI-compatible endpoint, so a hosted one works by configuration alone. The embedding and rerank models are configured separately and are deliberately not switched along with it: changing the embedding model invalidates every stored vector and forces a full re-embed.

What a hosted provider needs that llama.cpp does not, checked against OpenRouter's documentation (<https://openrouter.ai/docs/api-reference/overview>, `/streaming`, `/errors-and-debugging`, `/limits`, <https://openrouter.ai/docs/guides/features/structured-outputs>, <https://openrouter.ai/docs/guides/routing/provider-selection>):

- **Provider-specific request fields and headers** go through `LEXARII_LLM_EXTRA_BODY` and `LEXARII_LLM_EXTRA_HEADERS` rather than code. Fields the client owns (model, messages, stream, response_format, token budget, temperature, stop, reasoning_effort, tools) and `Authorization` cannot be set that way; startup fails naming the variable.
- **Structured output is only guaranteed by providers that support it.** Route with `{"provider":{"require_parameters":true}}`. Replies are validated in Go regardless.
- **Failures arrive differently.** Errors after the stream starts are `data:` events on a 200, with the code sometimes a string; `: OPENROUTER PROCESSING` comment lines are keep-alives; reasoning text is `reasoning` rather than `reasoning_content`. A 402 means no credit and is not retried, unless it carries `Retry-After` (a transient in-flight spending cap). A `Retry-After` is honoured up to 30 seconds for background work and 2 seconds for a request a person is waiting on (routing then falls back at once); longer is handed back so the job is requeued instead of a worker sleeping on it. The worker treats a rejected key or an empty balance as a skip rather than a failure, because it is the same for every queued job and one fix revives them all (skips back off and are bounded by `maxSkips`, about nine hours, after which jobs do fail); a skipped job waits at least the provider's `Retry-After`.
- **Health must check the key.** `/models` is public on OpenRouter, so `Ping` also asks `GET /key` when an API key is configured; a 401 there, or on `/models`, is unhealthy. A server with no `/key` route (llama.cpp) is unaffected.

**Privacy.** With a hosted chat provider, text from your books leaves your machine: the sampled opening of each work during enrichment, and the retrieved passages and the question in every chat answer. The provider and whichever model host it routes to receive that. Lexarii cannot see or enforce what they do with it. OpenRouter documents two per-request controls: `provider.data_collection: "deny"` (route only to providers that, by OpenRouter's account, do not collect user data) and `provider.zdr: true` (only endpoints with a zero-data-retention policy), and says its own data-policy labels are its best knowledge and "not a definitive source of third party data policies". Account-level privacy settings also exist. Treat these as routing filters, not guarantees, read the provider's terms, and do not use a hosted provider for a library whose contents must not leave the machine. The default, a local llama.cpp, sends nothing anywhere.

**The build is reproducible, and `make reproducible` proves it** — building everything twice and comparing the bytes. F-Droid rebuilds a tagged commit and compares the result against what was published, so this has to be a property somebody can check rather than one the project asserts. Go was already there: `-trimpath`, `CGO_ENABLED=0` and pinned ldflags make identical source produce an identical binary. The client was not. SvelteKit names each build `Date.now()` by default and bakes the name into every generated module, so two builds a second apart shared **not one output file** — every chunk renamed, every hash different, and the embedded client different in turn. The name is the commit now, overridable with `LEXARII_BUILD_VERSION` for a pipeline that already knows the version, and passed into the Docker build where there is no git to ask.

---

## Decisions already made, with reasons

| Decision | Why | What it rules out |
|---|---|---|
| Go, stdlib `net/http` | Go 1.22 routing is enough; one static binary; trivial cross-compilation | Gin, Echo, Fiber, Chi |
| Postgres + pgvector | HNSW, real concurrency, FTS and vectors in one query planner, one datastore to back up | A separate vector database |
| SvelteKit + Svelte 5 | Smallest real-framework payload; fits the light-but-good brief | React/Next (bundle weight), server-rendered templates (too weak for the reader) |
| LLM over OpenAI-compatible HTTP | Works with llama.cpp, Ollama, vLLM, LM Studio, or hosted, unchanged; keeps `CGO_ENABLED=0` | CGo llama.cpp bindings |
| Locators, not percentages | Survive font changes, rotation, and re-ingest; the only way citations and highlights stay valid | Scroll-offset position sync |
| Work / Edition / BookFile split | Two files of one book are one shelf item; citations attach to works, positions to files | A single flat `books` table |
| Hybrid retrieval + rerank | Proper nouns and symbols break dense-only search; reranking is the best quality-per-effort stage | Pure vector similarity |
| SSE, not WebSockets | One-directional, proxy-friendly, trivially reconnectable | A bidirectional socket layer |
| Jobs in Postgres (`SKIP LOCKED`) | No extra broker to deploy; resumable across restarts | Redis, NATS, RabbitMQ |
