# Lexarii — Roadmap

Ordered by dependency and by risk, not by excitement. The hard, load-bearing decisions come first, because they are the expensive ones to change later.

No dates. Milestones ship when their exit criteria are met.

---

## Guiding sequence

Two things must be right before anything is built on top of them:

1. **Locators.** Reading position, highlights, chunk positions, and chat citations all depend on them. Getting them wrong means re-migrating every annotation and every chunk later.
2. **The Work / Edition / BookFile split.** Retrofitting it means rewriting every foreign key in the system.

Everything else can be changed cheaply. So M0 and M1 exist to nail those two, and the fun parts wait.

---

## M0 — Foundations

Nothing user-visible. This is the milestone that makes the rest cheap.

- ~~Go module, directory skeleton, `cmd/lexarii` with config loading, `log/slog`, and graceful shutdown.~~ **Done.**
- ~~Postgres up via docker compose, extensions enabled, goose wired, first migration applying and rolling back cleanly.~~ **Done:** 7 migrations, 21 tables, 69 indexes, full `up`/`reset`/`up` verified against Postgres 18 + pgvector.
- ~~`internal/domain` with the entities from [DOMAIN.md](DOMAIN.md), and the **`Locator` type with its tests**.~~ **Done:** Work/Edition/BookFile/Author/Tag/Collection/Chunk/Embedding/Annotation/ReadingProgress, plus `Locator`, `Range`, RRF fusion, and intent routing, all tested.
- ~~SvelteKit app scaffolded, TypeScript strict, design tokens file, one page that hits `/api/v1/health`.~~ **Done** (pending `npm install` once Node 24 is in place).
- ~~Makefile, `.env.example`, CI running fmt / vet / build / test / check.~~ **Done.**
- ~~Licence chosen and applied.~~ **Done:** AGPL-3.0-or-later, `LICENSE` + `NOTICE`, SPDX headers on every source file.

**Exit:** `make dev` brings up the database, the server, and the client. Health check green. CI passes. Migrations round-trip.

**Status: met, with one caveat.** Verified locally: server connects to Postgres 18 + pgvector, `/api/v1/health` reports `ok` with the model absent and `degraded` (still HTTP 200) with it unreachable, migrations round-trip, and 12 Go tests pass including 7 store tests against a real database. The caveat is that Docker's bridge networking is unavailable in this environment, so verification used a host-networked container; `docker compose up` itself is unverified.

---

## M1 — Ingest and the library

The library becomes real. Still no LLM anywhere.

- ~~EPUB parsing: container → OPF → spine, NCX and nav TOC, `dc:` metadata, cover extraction.~~ **Done:** EPUB 2 + 3, refinements, calibre series convention, ISBN normalisation, playOrder-aware NCX, deterministic output.
- ~~PDF text extraction, with scanned-PDF detection marking `needs_ocr` rather than ingesting empty text.~~ **Done:** `github.com/ledongthuc/pdf` (BSD-3-Clause, pure Go, keeps `CGO_ENABLED=0` working). Per-page and per-document panic recovery; hostile input tested by bit-flipping and truncating a valid PDF at hundreds of offsets.
- ~~Library directory scan, SHA-256 hashing, move/rename detection, per-file error isolation.~~ **Done:** streaming hash, cancellable mid-file, symlinks skipped by default and never followed out of the library, read-only verified by snapshotting the tree before and after a scan.
- ~~Job queue on Postgres with `FOR UPDATE SKIP LOCKED`~~ **Done:** claim/progress/succeed/fail with exponential backoff, dedup by partial unique index, stale-lock reclaim for dead workers. Verified with 8 concurrent workers draining 60 jobs under `-race`, zero double-claims. ~~**Remaining:** wiring scan and extract as job handlers~~ **Done:** `internal/worker` pool with panic isolation, skip-vs-fail semantics, and stale-job reclaim; scan and extract handlers wired into `main`. ~~**Remaining:** SSE progress and the HTTP surface.~~ **Done:** `POST /api/v1/scan` returns `202 {jobId}`; `GET /api/v1/jobs/{id}/events` streams SSE with a guaranteed terminal event; library list, work detail, and stats endpoints with cursor pagination.

A bug worth recording: `withRequestLog` wrapped the `ResponseWriter` to capture status codes, which hid `Flush` from everything downstream. A `w.(http.Flusher)` assertion therefore failed in real serving and SSE would never have flushed - progress events would have reached the browser only when the stream closed. Fixed with an `Unwrap` method and `http.ResponseController`. It was caught because the SSE tests exercise the full router rather than calling the handler directly, which is now the rule for handler tests.

**Search reaches untitled books.** Measured against the real corpus, `search="linear algebra"` returned nothing despite the library containing `appliedlinearalgebraandoptimizationusingmatlab.pdf`: with no embedded title there was nothing to match. Migration 9 adds a generated `search_key` column - the filename reduced to bare lowercase alphanumerics, trigram-indexed - and the query normalises the search term the same way. `linear algebra` now finds 2 books, `set theory` 1, `numerical methods` 2. It is a fallback, not a substitute for real metadata: it finds the book without pretending to know its title.
- ~~Work / Edition / BookFile / Author persistence, with metadata provenance recorded.~~ **Done:** single-transaction idempotent upsert keyed on content hash; move/rename recognised; author and tag deduplication; provenance ledger in `enrichment_records`; missing files flagged rather than deleted.
- ~~Archive-safety hardening: zip-slip, decompression bombs, XXE. Fuzz the path sanitiser.~~ **Done:** `internal/ingest/safezip.go`. Fuzzer found a real idempotence bug (`./0:` → `0:`, rejected on re-sanitisation); the failing input is kept in `testdata/fuzz/` as a permanent regression seed.
- ~~Library grid UI: virtualised covers, search, sort, filter, book detail page.~~ **Done.** SvelteKit 2 + Svelte 5 runes, 43 KB of JS and CSS gzipped against a 120 KB budget, embedded into the Go binary with `go:embed`. Drag-and-drop upload, live scan progress over SSE, infinite scroll on the cursor API, and a book detail page that shows provenance and parser warnings rather than hiding them.

  **Coverless books are the design, not an edge case.** Every one of the 167 PDFs in the test corpus lacks a cover image, so the fallback carries the whole grid: each book gets a stable hue derived from its title, making the shelf navigable by memory the way a physical one is.

  **Browser-native virtualisation** via `content-visibility: auto` with `contain-intrinsic-size`, rather than hand-rolled windowing, because it survives a responsive grid whose column count changes with the viewport.

  Four defects found by actually looking at the running app against a real library:

  1. **The shelf read "empty" while the header reported books arriving.** The list loaded once at mount and never followed a running ingest - and a scan can be started from another tab, another device, or before the page was opened. The shelf now watches the queue rather than only reacting to a scan it started.
  2. **`sveltekit` was imported from the wrong package.** `vite.config.ts` pulled it from `@sveltejs/vite-plugin-svelte` instead of `@sveltejs/kit/vite`; the build failed outright.
  3. **A three-format book clipped its own format list.** `EPUB MOBI PDF · 104 sections` overflowed a narrow card, and the formats are exactly how a user tells a readable copy from an unreadable one.
  4. **`.gitignore` excluded the `go:embed` placeholder**, so a fresh clone would not compile - `go:embed` fails outright on an empty directory. The build output stays ignored; the placeholder is now tracked.

**Validated against a real library.** 167 technical PDFs, 3.3 GB, 72,006 pages: all 167 ingested with zero failures and zero warnings, peak 45 MB RSS, and a size+mtime manifest of the library directory was byte-identical after three full ingests - the read-only promise verified against real user data rather than fixtures.

Two defects the corpus found that fixtures had not:

1. **Producer artefacts were being enshrined as publisher facts.** Seven titles were source filenames (`Algebra_P1_Book.indb`, `MOCL003-FM.dvi`, `Morgan-Claypool.book`). Because embedded metadata outranks everything, no later correction could ever displace them. `ingest.PlausibleTitle` now rejects them, leaving the field empty with a warning rather than guessing.
2. **A path check rejected real authors.** Guarding against `DOMAIN\jsmith` also rejected `Abi Aghayere/ Jason Vigil`, losing the authors of three books. Narrowed to backslash, and `ingest.SplitContributors` now splits slash- and semicolon-separated lists into individual browsable people. Commas are deliberately not split: `Knuth, Donald` is one person.

**Measured metadata coverage** (PDF-only corpus): 100/167 titled, 102/167 authored, **0 publishers, 0 ISBNs, 1 subject**. Two consequences worth carrying forward: ISBN-based Work merging will never fire for a PDF library, and roughly 40% of a real shelf has no usable title - which is the concrete case M3 enrichment exists to solve, with the body text already extracted and waiting.

**Validated against real EPUB and MOBI files.** Two EPUBs and two MOBIs of books already held as PDFs - 171 files in total. Both EPUBs parsed with zero warnings, carrying publisher, ISBN, language, and correctly split multi-author credits, none of which any of the 167 PDFs had. Four defects this exposed:

3. **A book owned in two formats appeared twice.** Merging required a matching ISBN-13 and PDFs essentially never carry one. Measured on the 171 files, filename stems gave a far safer signal than fuzzy title matching: 167 distinct stems, and every collision a genuine same-book set. Migration 10 adds `stem_key`, guarded by requiring different formats and a stem of at least 12 characters.
4. **The merged book's metadata depended on ingest order.** Whichever file a worker finished first created the Work, so a metadata-less PDF beating its EPUB twin left the book permanently titled "Untitled" with no ISBN or publisher. `backfillMetadata` now fills empty fields from later files, using the provenance ledger to decide what counts as already declared, so publisher facts and user edits still cannot be overwritten. Both orders now produce identical results.
5. **`pageCount` conflated pages with spine items.** An EPUB reporting 104 sections and a PDF reporting 268 pages were serialised under the same field, so the UI would have labelled a 300-page book "104 pages". Now `unitCount` plus `unitType`.
6. **Unreadable formats were silently invisible.** MOBI files were not listed, not errored, and gave the user no way to tell whether Lexarii had noticed them. `.mobi`, `.azw3`, `.cbz`, and `.cbr` are now scanned and recorded as `unsupported` - visible, honestly labelled, and attached to the right book by the stem rule.

Final state: **167 works from 171 files**, the two multi-format books each holding `epub:ready pdf:ready mobi:unsupported`, zero failures, and the library directory byte-identical after five full ingests.

**Exit:** point Lexarii at a real folder of a few hundred mixed books; every one appears with correct title, author, and cover, or with a visible, specific error. Re-running the scan changes nothing. Renaming a file does not duplicate it. No book file is ever written to.

---

## M1.5 — Bulk upload

Books reach Lexarii by being copied into the library directory. That is fine on a server you can reach with rsync or Syncthing and useless from a phone or a laptop across the house.

- ~~Streamed multipart upload endpoint.~~ **Done:** `POST /api/v1/uploads`. Each part is written straight to disk while being hashed; nothing calls `ParseMultipartForm`, which would buffer a whole bulk upload into memory.
- ~~Content-addressed managed storage.~~ **Done:** `LEXARII_UPLOAD_DIR`, a directory Lexarii owns, deliberately distinct from the read-only `LEXARII_LIBRARY_DIR`. Startup refuses a configuration that puts one inside the other. Files land at `books/ab/<hash>.<ext>`, which removes path traversal as a category rather than defending against it.
- ~~Deduplication as the recovery story.~~ **Done:** re-uploading a batch after a failed transfer is the supported recovery. Identical content is recognised, nothing is rewritten, and the response says `duplicate` per file. Verified on 163 MB of real books: re-uploading three of them added nothing to disk and queued no work.
- ~~Per-file outcomes.~~ **Done:** one bad file in a batch costs that file. A truncated transfer is reported as `interrupted` with the files that landed listed, because the recovery depends on knowing which ones need re-sending - and a transport failure is distinguished from a rejected file, so a dropped connection never tells the user their book was bad.

**Two defects real uploads exposed:**

1. **Content-addressed storage broke same-book merging.** `stem_key` was derived from the path, and an uploaded file's path is a hash. Migration 11 adds `original_filename` and regenerates `stem_key` from it, falling back to the path for scanned files. Verified: uploading an EPUB and a MOBI of one book produces one shelf item.
2. **The library card represented a book by its oldest file, not its best.** A book held as a readable EPUB and an unsupported MOBI showed as unreadable with no page count, purely because the MOBI was ingested first - contradicting the documented intent that "a work with one readable file and one broken one is readable". The representative file is now chosen by status.

**Still true, and worth telling users:** browser upload is right for "I bought three books". For a first import of thousands, rsync or a mounted share is still the better tool, and the read-only library directory path remains fully supported alongside uploads.

## M2 — The reader

The milestone the whole project is for.

- ~~CSS multi-column pagination, one-gesture page turns.~~ **Done.** Column width and advance distance are both computed in JavaScript from the real element geometry, because expressing one as a CSS `calc()` and the other as `clientWidth` looks equivalent and is not - `clientWidth` includes padding, and the columns drift further out of alignment with every page turn. The column gap is derived from the viewport's padding for the same reason: `overflow: hidden` clips at the border box, so a narrower gap leaves the next page visibly peeking through the margin.
- ~~Typography controls: family, size, line height, measure, justification — persisted per user.~~ **Done**, in `localStorage`, per user rather than per book: someone who needs larger text needs it in every book.
- ~~Themes: light, sepia, dark, true black. `prefers-reduced-motion` respected.~~ **Done.**
- ~~Chrome that fades away. Centre tap restores; edges turn.~~ **Done**, with a keyboard path for every action.
- ~~Locator-based reading position, optimistic local write, debounced sync, sensible conflict resolution.~~ **Done, and it is the payoff for the whole locator design.**

  A position is saved as the element path of the paragraph or heading being read - `epub:11/section[1]/div[1]/h1[1]:0` - never as a page number. Verified by reading to Chapter 7, changing the font from 20px to 26px and narrowing the measure, and reloading: the chapter re-laid-out from 4 pages to 10, and the reader still landed on exactly the right heading. A stored page number would have been meaningless.

  Conflicts resolve to the FURTHEST position, in SQL, not the most recent write. Two devices open on one book is the normal case, and silently rewinding someone who read ahead on their phone is the single outcome that must not happen. The response returns the STORED position so the client adopts what won rather than assuming its own write did.

  Saves are debounced, flushed on `pagehide` and on tab hide, and never block reading: a failed save costs nothing because the next page turn tries again.
- Highlights and notes anchored to locator ranges, with visible orphan state rather than silent loss.
- Keyboard navigation, focus management, honest reading-order DOM.
- ~~Book HTML sanitised before it reaches the DOM. Remote asset loads blocked.~~ **Done.** `bluemonday` allowlist (BSD-3-Clause), tested against inline scripts, event handlers, `javascript:` URLs, iframes, objects, forms, SVG-borne scripts, and meta refresh. Images are rewritten to an asset endpoint before sanitising, so a remote or traversing reference cannot survive; only image media types are served back, and never as a document.

  The book's own CSS is **discarded entirely, not sanitised**. That is what a reader does rather than what a browser does: publisher stylesheets set fixed widths and their own page geometry, which fight the column layout and which the user never asked for.

  One defect worth recording: `bluemonday` silently drops an allowed element that has no permitted attributes, so MathML collapsed to bare text - `H₂O` became `H2O`, markup gone and the equation quietly wrong. Every bare structural and MathML element now also needs `AllowNoAttrs`. For a technical library this was the difference between readable maths and nonsense.
- Windowed rendering — never the whole book in the DOM.

**Content is served from the file on disk, not copied into the database.** A book is already stored once; storing its text again would double the disk cost of every library. Opening an EPUB per request is cheap because only the zip directory is read, and re-opening rather than caching means a deleted or replaced file is never served from a stale handle.

**Verified against a real book:** O'Reilly's *97 Things Every Java Programmer Should Know*, 104 spine items, 100 TOC entries, cover image rendering through the asset endpoint, justified text with real hyphenation in all four themes.

**PDF reading**, taken out of listed order because 167 of the 171 books in the test library are PDFs and the reader refused every one of them. Annotations would have polished the two books that already opened.

A PDF is not reflowable, so it gets its own surface rather than a degraded version of the EPUB one: fixed pages rendered by `pdfjs-dist` (Apache-2.0, libre, F-Droid-safe), served over HTTP range requests so a 158 MB reference book does not download in full before page one appears. Pages render at device pixel ratio, because an upscaled page of body text is the difference between readable and tiring.

`pdfjs-dist` is 132 KB gzipped and is imported dynamically, so it loads only when a PDF is opened: the library route stays at 52 KB against a 120 KB budget.

Two decisions the running app forced:

- **Whole-page fit is the default, not fit-to-width.** Fit-to-width makes a page taller than the screen, so reading means scrolling - and the edges of that same screen turn the page. Scroll down, tap to continue, and the next page arrives having silently skipped what was below the fold. One tap must mean exactly one page.
- **Dark mode for a PDF is an inversion filter, and is labelled as one.** It flips photographs and diagrams too. Presenting it as the equal of the EPUB reader's real themes would be a lie told by the UI.

**Annotations are done:** highlights and notes anchored to locators, painted over the text, surviving reload and inline-markup changes, with orphans flagged rather than dropped. Four findings, every one of them invisible to the type checker and to the API:

1. **Highlights stored perfectly and painted nothing.** A selection inside one paragraph has that paragraph's *text node* as its common ancestor, and a `TreeWalker` never yields its own root - so the walk found zero nodes and the ordinary case silently painted nothing while the API reported success. The API being correct is exactly what made it hard to see.
2. **`splitText` moves live ranges.** Wrapping the first text node changed `range.startContainer` and `range.endContainer` underneath the loop, so every later boundary comparison read a range that had already moved. The geometry is now frozen before a single split happens.
3. **Painting was not idempotent.** It runs on every page turn, but never cleared what a previous pass had wrapped: highlights nested inside each other, and a deleted one stayed on screen until the chapter was re-fetched. `unpaintHighlights` restores the text first, and the result is asserted byte-for-byte.
4. **The reader hung on a frame that never came.** Chapter loading waited on `requestAnimationFrame` before measuring. A page that is not painting - a background tab, an occluded window, a throttled phone - never fires one, so the chapter rendered and was then never measured, never painted, and never saved. It is now raced with a timer, because a frame is a nicety and the measurement is not.

**Orphaning had only one direction.** A mark could be flagged but never unflagged, so a single bad render orphaned a note for good - and finding (1) did exactly that to a real highlight. The reader now reports what resolved as well as what did not, and `ClearAnnotationOrphan` was already sitting in the store waiting to be called.

**The locator module now has tests**, and they were checked against the broken code first: with the defect reintroduced, three of them fail. Its absence is why a load-bearing module shipped a bug that only a browser could see.

**Reading position was one value doing two jobs.** Conflicts resolved to the furthest position, so a second device could never rewind someone who had read ahead - but it also meant every backward move was discarded. Open the contents, jump back to chapter 7 to check something, close the book: it reopened at chapter 69, because the save was thrown away. The two facts are now stored separately. The CURRENT position takes the most recent write, because turning back a chapter is reading; the FURTHEST only ever advances, and is what a percentage on a shelf means. When they differ, the reader offers a way back to the furthest point rather than stranding it somewhere unreachable.

**PDFs have a table of contents.** Every outline entry used to report "unresolved", because a destination names a page by object reference and the library resolves references before handing them over, leaving no object id to compare. Pages are now identified by their own dictionary - which carries each page's /Contents and /Resources - and every shape the spec allows is followed: an explicit page reference, a /GoTo action, a named destination in either the modern name tree or the old /Dests dictionary, and a bare page index. Two pages that are byte-identical resolve to nothing rather than to the first match, because an entry on the wrong page is worse than one that cannot be followed.

Across the 167 real PDFs: **49,318 of 49,360 entries resolve**, in 165 of the files. Two findings came only from running it over all of them. Searching the name tree per entry was quadratic - one book has 47,750 named destinations and its 1,049 outline entries each scanned the lot, so opening it took five and a half seconds; it now descends by /Limits, and falls back to reading the tree once when /Limits lies, as it does in real files. And calling `Page(i)` per page re-descends the page tree every time, which was eight seconds on a 1,156-page reference book against a fifth of one for a single walk. The contents request is also no longer awaited before the first page renders: resolving an outline is work the reader should never wait on.

**Still outstanding in M2:** windowed rendering for very large EPUB chapters.

**Exit:** a 900-page technical PDF and a 40-chapter EPUB with MathML both read smoothly on a phone. Position survives a font-size change, a rotation, and a re-ingest. Nothing janks.

At this point Lexarii is already a good, usable product with no AI in it at all. That is deliberate.

---

## M1.75 — Accounts and access control

**This milestone exists because the plan did not have one, and the gap only became visible when reading positions needed an owner.**

Lexarii currently runs with **no authentication at all**. A single default user is created at startup and every request acts as it; the server logs a warning saying so on each boot. That is a defensible posture for a personal instance on a home network and is **not** defensible on the open internet, which matters because bulk upload was built partly for remote use.

The schema has carried `users` and `sessions` since migration 2, and every owned row - reading positions, annotations, collections, conversations - already has a `user_id`, so this is additive rather than a rework.

- ~~Password login, argon2id hashing, session cookies that are HttpOnly, SameSite=Lax, and Secure behind TLS.~~ **Done.** Tokens carry 256 bits of entropy and are stored only as a SHA-256 hash, so a database backup cannot be replayed as a login. `LEXARII_SECURE_COOKIES` defaults to true and logs a warning when turned off.
- ~~A real user lookup replacing `ProgressHandler.UserID`.~~ **Done**, and keeping it as one field rather than threading a fixed id through every handler is exactly what made this a small change rather than an audit.
- ~~First-run setup.~~ **Done**, and closed permanently once a usable account exists.

**Built for federation from the start, because retrofitting it is the painful version.** [Migration 12](../migrations/00012_identities.sql) separates a *person* from the *ways they prove who they are*: `user_identities` keyed on `(provider, provider_user_id)`, with the password hash moved off `users` entirely. The users table now knows nothing about authentication, which is what lets a provider be added without touching it.

Deliberate choices that make OIDC, OAuth2, LDAP, and AD additive rather than invasive:

- **`provider` is TEXT, not an enum.** Adding a provider must be a configuration change, not a database migration.
- **Three provider interfaces, not one.** `PasswordProvider` (local, LDAP, AD - all of which verify a password directly), `RedirectProvider` (OIDC, OAuth2 - credentials arrive in a callback, not the request), `HeaderProvider` (an authenticating reverse proxy). A design assuming credentials arrive in the request cannot be extended to one where they do not, so all three exist now even though only the first is implemented.
- **The login page asks the server what exists** rather than assuming a password form, so adding a provider changes the screen without changing the client.
- **`Identity.Subject` must be the OIDC `sub`, never the email.** Emails get reassigned between employees, and keying on one eventually hands a new joiner the previous holder's library. Documented at the field.
- **`Groups` is captured now** though nothing reads it, because a directory integration that discards them forces a second round trip later.
- **Federated providers may create accounts on first sign-in; local never may.** Automatic creation against a public issuer would let anyone in.

Three defects found while building it:

1. **An upgrade would have locked the owner out permanently.** The pre-auth placeholder account migrated with no password, so nobody could sign in - while setup refused to open because a user row existed. `HasAnyUser` now asks whether a *usable credential* exists, and setup **adopts** the placeholder rather than creating a second account beside it, which would have orphaned every reading position and annotation while appearing to work.
2. **Every IPv6 session failed to be created.** `clientIP` split on the last colon, yielding `[::1]`, which Postgres rejects as an `inet`. Local connections over IPv6 are the common case in development and on a home network.
3. **Account enumeration by timing.** An unknown username returned before any hashing happened. A decoy hash, generated at startup from random bytes, now makes a failed lookup cost the same as a failed password check: measured at 11.0 ms versus 10.6 ms, which is noise.

**OIDC is implemented, and implementing it is what proved the abstraction holds.** A provider whose credentials arrive in a *callback* rather than a form is exactly the shape a password-only design cannot accommodate, so building one now - rather than after LDAP and AD were also assumed - was the point.

Verified end to end against a real issuer with a real RSA signing key: discovery, JWKS, authorize, callback, token exchange, ID token verification, account creation, session issued. The provider is configuration only (`LEXARII_OIDC_*`), and **local passwords always remain available** so an unreachable or misconfigured issuer cannot lock the owner out of their own library.

Verification rejects every attack it should, each with a test: a token issued by someone else, for a different client, expired, signed with the wrong key, carrying `alg: none`, carrying `HS256` where RSA is expected, or carrying no expiry at all. A signature check alone is not verification, and a library that only checks the signature is the classic way this goes wrong.

The callback is CSRF-protected with state carried in a short-lived cookie and compared in constant time. Without it an attacker completes the flow with their own code and silently signs the victim into the *attacker's* account, where everything the victim then reads or annotates belongs to them.

**Sign-in is rate limited**, on the username and the source address together: username alone lets one host spray a password across many accounts, address alone lets a botnet grind one account from many hosts. In memory rather than in Postgres, which is stated plainly as a trade-off - a restart clears the counters, but an attacker who can restart the server has already won. A bug found by its own test: the limit check was skipped on the first failure of a window, so a limit of one never locked anything out.

**LDAP and Active Directory are implemented, and the claim that they needed no new plumbing held exactly.** `internal/auth/ldap.go` plus configuration is the entire change: no schema migration, no change to sessions, no change to the login screen, which already asks the server what exists. Three providers now coexist, and local passwords remain available beside them so an unreachable directory cannot lock the owner out.

Verified against a **real OpenLDAP server** with real users and groups, because the failure modes that matter here are behaviours of the LDAP protocol rather than of this code, and a fake would only reproduce whatever the code already assumes.

Decisions that matter:

- **Authentication is by binding AS the user.** Lexarii never reads, compares, or stores a directory password - the directory decides, which is the entire reason for using one.
- **An empty password is refused before any bind.** LDAP treats a bind with an empty password as an ANONYMOUS bind, which most servers accept, so without this check every account in the directory authenticates with a blank password. It is the single most common way an LDAP integration is catastrophically wrong, and it has its own test.
- **The username is escaped into the filter.** Without it `*` matches every user in the directory. Four injection attempts are tested, including `*)(uid=*`.
- **A filter matching more than one person is refused** rather than binding as an arbitrary one of them.
- **Active Directory gets its own attribute defaults** - `sAMAccountName`, `displayName`, and a filter matching either the login name or the full UPN, since people type both. AD differs from OpenLDAP in every attribute that matters, and getting them wrong authenticates successfully while returning no identity at all.
- **`RequiredGroup` is refused as invalid credentials**, not as a distinct error, so group membership is not enumerable through the sign-in endpoint.
- **An unencrypted directory connection is logged as a warning**, because passwords then cross the network in the clear.

**Reverse-proxy SSO is implemented**, completing the set: local, OIDC, LDAP, Active Directory, and a trusted proxy, all able to run at once. For people already running oauth2-proxy, Authelia, Authentik, or a corporate gateway, where a second login screen is friction with no benefit.

It is the most dangerous provider to configure, because the credential is a plain header - so the unsafe configuration is made **impossible rather than discouraged**: a trusted-address list is required, refused at startup when absent, with the reason spelled out. A header is only a credential because of where it came from.

Header authentication runs *before* the session check, since behind an SSO proxy there is no login page to reach. That ordering exposed a real bug: `HeaderAuth` issues a session by setting a cookie on the RESPONSE, while `RequireAuth` re-validated from the REQUEST, which does not carry one yet - so it rejected the very request that established the session, which is every first request behind a proxy. `RequireAuth` now honours a user an earlier middleware already authenticated.

**The sessions screen is done.** Every live sign-in, which device it is, where from, how it authenticated, and when it was last used - with the current one marked, because someone trying to end a suspicious session must not end their own by mistake. Revoking is scoped to the owner, so a guessed id signs nobody out. Revoking your own session clears the cookie too, since leaving one pointing at a dead session shows a signed-in shell that fails on every request.

Two things it uncovered. **Expired sessions were never deleted** - `PurgeExpiredSessions` and a sweep interval both existed and nothing joined them, so the table grew without bound. It now sweeps on startup and every six hours, keeping dead rows for a week because they are evidence for the person reviewing their own devices, and an expired row cannot authenticate anything anyway. And **`EnsureDefaultUser` still inserted `users.password_hash`**, a column migration 12 removed when identities moved to their own table: it would have failed for any caller, and had none left.

**Still open here:** per-user vs shared library - reading positions and annotations are clearly per-user, but whether two people on one instance see one shelf or two is a product question rather than a technical one.

## M3 — LLM enrichment

The first time the user's own model is involved.

- OpenAI-compatible provider client: chat and embeddings, separate config blocks, timeouts, bounded retries, circuit breaker, global concurrency cap.
- Schema-constrained structured output via GBNF / `json_schema`, **plus** Go-side validation, fence stripping, and one repair retry.
- Versioned prompt templates in `internal/llm/prompts/`, with golden tests.
- Enrichment job: subject classification, tags, summary, series and author normalisation — from a bounded sample, never the whole book.
- Tag normalisation against a controlled vocabulary with embedding-similarity matching, so 5000 books do not produce 5000 near-duplicate tags.
- `EnrichmentRecord` provenance throughout; a UI that shows what the AI guessed and lets the user accept or reject it.
- Health endpoint reports LLM reachability separately from database reachability.

**Exit:** with the model server off, everything except enrichment works exactly as before. With it on, a 500-book library enriches to completion, survives a mid-run restart, and never overwrites publisher metadata.

**Done so far:** the provider client (chat, schema-constrained JSON, bounded retries, a concurrency gate), the versioned prompt with golden tests, the bounded sampler, the enrichment job, and the review screen. Tested against a stub model that speaks the same protocol, because the failure modes that matter - a code fence, a preamble, half an object, a 503 - are things a real model does occasionally and cannot be made to do on demand.

**The exit criterion holds.** With the model off: health reports `degraded` and names `llm`, every other endpoint answers 200, and enrichment jobs go to **pending with zero attempts consumed** rather than failing. Switching the model back on drained all 45 of them without intervention. That is the whole point of `ErrSkipped`: a library of 5,000 books must not become 5,000 permanently failed jobs because a machine was off overnight.

Two findings, both from running it over the real 167 books rather than from fixtures.

**The invariant earns its keep immediately.** Every title the model proposed for a book that already had one was a downgrade - "bash Cookbook" to "Cookbook", "Algorithmic Thinking" to "Learn To Solve Even The Hardest", "The Art of R Programming" to "Praise For The Art Of R Programm". All ten were refused and kept as suggestions. Publisher metadata beating model output is not a theoretical safeguard; it is load-bearing on the first run.

**Three things only the real model showed**, once Adam's gemma4-12b was reachable. None of them are visible against a fake server, because all three are properties of *that* model.

1. **It is a reasoning model, and reasoning was eating the entire answer.** It returns `reasoning_content` separately and fills `content` only once it has finished thinking. Given a token budget it spends the budget on thought: with 4,000 tokens it used all 4,000 reasoning about one book and returned *no answer at all*, stopping for "length". Worse, an empty answer looked like a transient failure, so the client retried it three times - the same prompt at temperature 0 spending the same budget the same way, three times a book, across a library. Structured extraction now asks for `reasoning_effort: none`, and an empty truncated reply is reported as a budget problem rather than retried. **126 seconds to 7.8 seconds, and 4,000 completion tokens to 123.**
2. **It brings the punctuation with the words.** Asked for the title of *Algebra Essentials* it answered `"Algebra Essentials, "` - a trailing comma and space inside the JSON string, not a parse error. It is copying a line off a title page and taking what followed it. Stored unexamined, that is the book's name. Extracted values are now trimmed of separators and unbalanced quotes, while sentence punctuation is kept, because a book really can be called *Who Owns the Future?*
3. **It reports confidence 1.00 for everything.** Every field of every book. So the confidence bar, which is what protects empty fields from weak guesses, is inert against this model - it is not that the model is always right, it is that it does not distinguish. The bar stays, because other models do calibrate, but it cannot be relied on here: plausibility checks and human review are what actually hold. [TODO.md](TODO.md) carries this.

**A slow model found a poison pill in the job queue** - a bug that had nothing to do with the model, and that nothing slower than a real one would ever have hit. `ReclaimStale` returned an abandoned job to `pending` without checking whether it had an attempt left. Claiming increments attempts, so a job reclaimed on its last attempt violated `jobs_attempts_within_max`, the claim failed, and the worker tried again immediately. Four workers produced **518 failed claims in a few minutes**, and no other job in the queue could make progress. A job with no attempts left is now failed rather than requeued, and the claim query will not select one, so a stray row cannot wedge the pool. Reintroducing either half fails three tests.

**And a second queue bug behind the first.** A skipped job is rescheduled
without consuming an attempt - deliberately, so a model server off overnight
does not fail a whole library. It was also unbounded, and the classifier
deciding what counted as "temporary" was written as a block-list: everything was
transient unless it was one of two known model failures. A reply that was
deterministically too long fell through that gap and was retried every thirty
seconds forever, silently, holding a worker each time. Two books held half the
pool, and nothing in the queue said so, because a skip is not a failure. The
test is now an allow-list - only a condition a different moment could fix waits
- and skips are counted, backed off exponentially, and given up on. Those same
two books now fail after three attempts with a message naming the cause.

It also showed why the job went stale at all: a 12B model reading several thousand words takes minutes, and a second job waits behind it on the client's concurrency gate, so the handler could go far longer than the stale threshold without reporting. Enrichment now heartbeats while the model is thinking - reporting that it is alive without inventing a progress figure it does not have.

**Empty fields had no such protection**, and that is where the damage was. Nothing is being overwritten, so nothing stopped a weak guess becoming what the library says a book is: titles like "Copyrightc" and "Chattamvelli Et Al", read off a copyright page, were applied unreviewed. A claim must now clear a confidence bar to be applied without review, and a title must additionally pass the same plausibility check that stops a filename being recorded as a title at ingest. Below the bar it is still recorded, attributed, and shown for review - nothing is thrown away, it just does not become a fact on its own. Verified both ways on the real library: at 0.62 the fields applied, at 0.30 all 57 records waited.

---

## M4 — RAG and chat

**The library answers questions, in a conversation, with citations you can
open.** Ask "what is an eigenvalue?" and fourteen seconds later you get a
definition with `[3]` and `[6]` beside it, each a link into the book at the
passage it came from. Ask "and how is it used in practice?" - meaningless on its
own - and it knows what "it" is. **81,230 passages from 169 books, all
embedded.**

Routing, hybrid retrieval with RRF, citations, conversations, reading lists,
library questions answered from SQL, and the chat screen are done. What remains
is streaming, reranking, and an eval set.

**Conversations are stored, not just held.** The schema had been waiting since
migration 7 and it anticipated everything: intent, model, retrieval and
generation timings on every assistant turn, and citations linked to chunks -
which is what makes "open this passage" work from a thread reopened days later.
A question is written to the thread BEFORE the model is asked, so a question
that takes thirty seconds and then fails is still there. Threads are named after
the question that started them, because "Conversation 7" is not recognisable
months later.

**A follow-up is routed with its thread.** Routing "and how is it used in
practice?" alone produces a search for the words "in practice". The history is
carried into routing and into the answer, trimmed from the oldest end and
bounded twice - six turns and 3,000 characters - because the passages are what
the answer must come from and history must not push them out.

**Library questions never touch retrieval.** "How many books do I have?" is a
fact the database holds exactly; searching passages for it would return prose
about counting. It is answered from SQL under the same rule as everywhere else -
from the facts given, or not at all - and answers "You have 168 books" in 16ms
of SQL plus a second of phrasing.

The rule that stops it inventing nearly went too far: asked "what is most of my
library about?" it first said *"I cannot answer what the library is about"* and
then gave the correct subject counts. A subject breakdown IS a fact about a
collection; what it must not do is describe what a book argues. With that
distinction made explicit it answers "mostly mathematics (48), algebra (47),
programming (22)…" and stops apologising.

**Thirty-nine authors who did not exist.** Asked "who is my most collected
author?", the library answered "radha, with 5 books" - faithful to the data and
wrong about the world. Publisher metadata routinely credits a whole writing
team in one `<dc:creator>` element, and ingest was recording that verbatim:
"Kevlin Henney and Trisha Gee" was one author with two books, and neither
person existed on their own. 39 of 103 author rows were composites.

The separators the splitter already handled - "/" and ";" - appeared exactly
zero times in a real 169-book library. What actually appeared was " and " (27),
" & " (8), and the comma (17). The comma is the hard one, because it means two
opposite things: "Costa, Peter" is one person and "Joseph Albahari, Eric
Johannsen" is two. They are told apart by what precedes it - a sort name begins
with a surname, one token behind an optional run of particles - and a
conjunction anywhere in the string settles the question, because "A, B, and C"
is a list by construction. Degrees are dropped: "Phil Winder, Ph.D." is one
person, not a person and a doctorate.

Migration 16 applied the same rule to what was already stored: 103 authors
became 137, 111 links became 167, no orphans, and Arnold Robbins - who had been
scattered across four composite rows - reappeared with the 5 books he actually
wrote. It is irreversible and says so; `Down` is a no-op rather than a lie.

**Retrieval has a number now.** `make eval` was a Makefile target pointing at a
script that did not exist, and `eval/questions.jsonl` held four placeholders
saying REPLACE-WITH-WORK-ID. Retrieval quality was an opinion.

The set is 32 questions generated from real passages - one per book, sampled
past the front matter - where the correct book is known by construction, plus
five hand-written controls. Questions that could only be answered with the
passage already in hand ("what does this book cover?", table-of-contents
questions) were thrown out; they test nothing.

    32 questions scored, 5 abstentions, k=10
      work    Recall@10 0.875   MRR 0.724
      passage Recall@10 0.625   MRR 0.434
      refused correctly   1.000
      intent accuracy     1.000
      median retrieval    11 ms

**Those figures are lower bounds, and the caveat is the most important thing
about them.** Each question names the one book its passage came from, and a
personal library covers the same ground many times over. Asked how to match
Greek letters with a regular expression, retrieval returned the Regular
Expressions Cookbook and Mastering Regular Expressions ahead of the labelled
grep Pocket Reference - better answers, marked wrong. Three of the four work
misses are this. The absolute number does not describe the system; the
difference between two runs does, and both runs are wrong in the same way.
`-out` records a run, `-compare` measures the next against it.

The instrument was itself tested, by regressing it deliberately: narrowing the
cut-off from 10 to 3 moved work Recall from 0.875 to 0.750 and passage Recall
from 0.625 to 0.500. A measurement that cannot detect a known regression is not
a measurement.

Two of my own metrics were wrong first. Out-of-library questions were scored on
retrieving nothing, which dense search can never do - there is no empty vector
result, only ten nearest neighbours. What is actually being tested is whether
the library then refuses, so those questions are answered in full and the answer
is marked. Both now refuse, including "the recommended dosage of amoxicillin for
a child under five", where a confident answer would have been the worst failure
in the system: *"The passages I found do not answer this. They cover topics such
as algebra problems, project management, programming in C# and Rust…"* And the
metadata controls were counted as failures for being routed away from retrieval,
which is exactly what they are supposed to do.

**The eval was measuring its own labels.** Each question named one correct
book - the one its passage came from - and a personal library covers the same
ground many times over, so retrieval that returned a different and equally good
book was marked wrong. A judging pass now retrieves for every question, asks the
model whether each book's passage actually answers it, and records the ones that
do as `alsoAcceptable`, kept separate from the labels that are correct by
construction because they are weaker evidence.

It added eight books across thirty-seven questions - conservative, which is the
right direction for a judge - and moved the numbers without touching retrieval:

    work Recall@10  0.875 -> 0.906
    work MRR        0.724 -> 0.755

Nothing about the system changed. The labels had been wrong. The baseline was
re-recorded, because comparing two runs scored against different ground truth
means nothing.

**Two author rows were one person; three pairs that looked identical were
not.** Splitting composite credits created real people but left the other half
of the problem: "M. Tabatabaian" and "Mehrzad Tabatabaian" are one person, and
so are "Jon Manning" and "Jonathon Manning".

The obvious tool is trigram similarity, and on the real library it is actively
dangerous. The five closest pairs:

    Jonathon Manning ~ Jon Manning          0.71   same person
    R. Bronson       ~ G. Bronson           0.67   DIFFERENT people
    Dawn Griffiths   ~ David Griffiths      0.63   DIFFERENT people
    M. Tabatabaian   ~ Mehrzad Tabatabaian  0.58   same person
    R. R. Gupta      ~ Anuj Gupta           0.46   DIFFERENT people

Similarity gets three of five wrong, and wrong in the expensive direction:
Dawn and David Griffiths are a married pair who co-author the same three Head
First books, and merging them destroys two bibliographies to tidy a list. So
the rule is structural - surnames must match exactly and first forenames must
be compatible, one an initial or a shortening of the other - with a second,
independent guard: **nobody is credited twice on their own book**, so two names
that co-author the same work are two people. That guard catches both dangerous
pairs on its own.

137 authors became 135, all 167 links preserved, and the losing spelling is kept
as an alias so a search for it still finds the person. Merging is destructive
and unrecoverable, so `scripts/authors` reports by default and only merges when
asked.

**Tag normalisation, and the same trap a second time.** 215 tags, 133 of them
naming a single book. Embedding similarity is exactly the wrong instrument:
"machine learning" and "deep learning" are as close as two phrases get, and
merging them destroys a distinction the reader is using. The rule is containment
plus a short list of words that carry no topical content - "computer
programming" is programming, while "systems programming" and "abstract algebra"
are their own subjects. Five merges: programming 24 to 30 books, command line 5
to 7, algorithms 1 to 3.

The long tail is not a normalisation problem and was left alone. A library
really does have exactly one book on cheminformatics; collapsing those needs a
controlled vocabulary, which is a decision about what the library is for.

**Books belong to people now.** The shared library is gone: every file has an
owner, and is visible to them and to the people they name. Two views are the
only definition of access - `readable_book_files` and `readable_works` - so no
query can quietly disagree with another about who may read what.

The leak that mattered was not the reader. It was retrieval: a search with no
viewer would have returned passages from books the asker cannot open, quoted
inside an answer, with a citation, looking authoritative. `Search` now refuses a
query with no viewer rather than running it unfiltered, and the same fail-closed
rule applies to the library listing. Stats are scoped too - "168 books" on an
account granted two says plainly that there are 166 more.

Sharing is per FILE rather than per work, because someone can reasonably be
given the EPUB and not the 51 MB PDF. Reading and downloading are separate
permissions, and only the owner may grant either: ownership is read from the row
inside the statement that writes, so a request cannot claim to be granting on
someone else's behalf, and a grantee cannot pass a book along. A book that is
not yours reports not-found rather than forbidden - someone who may not see a
book should not learn that it exists.

Annotations follow the same shape. A share hands over the owner's highlights
readable and never editable, requires read access by construction - marginalia
without the text it marks leaks that text one highlight at a time - and is
withdrawn the moment the book is, so nothing outlives the decision to stop
sharing. Shared marks carry the name of whoever made them, because an
unattributed highlight read months later becomes a thing you think you wrote.

**A PDF shared to be read is served a page at a time.** Until now withholding
the download permission made a PDF unreadable rather than read-only: the reader
pointed pdf.js at the whole file, so a grantee got a 403 and an empty screen.
Half a real library is PDFs, so the feature did not work for the case it
existed for.

Each page is now lifted out as a standalone one-page PDF, which pdf.js opens
exactly as it would a book. The measurement that decides whether this is usable
at all:

    parse the book      3.6s   (54 MB, 558 pages)
    write out one page  2ms

Doing the parse per page turn would have made it unreadable, so books are
parsed once and four are held open, evicted least-recently-used. Two people
opening the same title at once wait on one parse rather than starting two. On
the running server, as a real grantee: page 42 in 3.44s, page 43 in 6.5ms.

Verified end to end rather than assumed. As a grantee with read-only access:
`/file` answers 403, `/page/42` answers 200 with 224 KB out of a 54 MB book,
the library shows exactly the one book that was shared, and pdf.js in the
browser parses the result to a one-page document reading "Chapter 1 calling
functions". This needed pdfcpu - Apache-2.0, pure Go, so `CGO_ENABLED=0`
survives - and costs 3 MB of binary.

**The audit records what was read, not just what was permitted.** A grant says
someone MAY read a book; it does not say whether they read three pages or
pulled all 558 in four minutes, and for a feature justified by legal caution
that second thing is the useful one.

Two decisions shape it. It records only OTHER PEOPLE'S books - your own reading
is never logged, and the check is in the statement rather than a handler, so it
cannot be forgotten. And it aggregates: one row per person, book, day and kind,
carrying how many requests were made and which distinct units were seen. A row
per page view would have been 558 rows for one reading.

That distinction is not pedantry. Verified on the running server: a grantee read
pages 10, 11, 12 and then 11 again, and the log says **4 views, 3 distinct
pages** - re-reading is traffic, not more of the book taken. The owner then read
two pages of their own copy and the table stayed at one row. The work page shows
it as "A Reader — 3 of 558 pages".

**Flipping a shared PDF is smooth now.** The reader keeps a window of three
pages - the one being read and one either side - and warms the neighbours in
the background, so turning forward usually paints from memory rather than
waiting on a request. Entries are promises rather than documents, so turning
onto a page whose prefetch is still in flight waits for that request instead of
starting a second one, which is exactly what happens when somebody flips fast.

**Prefetching would have made the audit lie, so it does not.** A page fetched
on somebody's behalf is not a page they read: without a distinction, looking at
page 10 would be recorded as having seen 9, 10 and 11. The reader marks those
requests and the server counts them as traffic but not as pages taken. Measured
by flipping through the real book: **14 views, 7 pages read** - the seven
prefetched pages are visible as traffic and absent from what was taken. A script
pulling the whole book is if anything MORE obvious, since hundreds of views
against almost nothing read is a shape no reader produces.

**It also exposed a race that had been there all along.** `renderPage` cancelled
the previous render and then awaited the page, so two renders could both get
past the cancel and reach pdf.js on the same canvas - "cannot use the same
canvas during multiple render() operations". A network round trip per page turn
had been hiding it; making turns instant made it reliable. Renders now carry a
generation and wait for the previous one to let go of the canvas before
starting.

**What withholding a download actually buys, stated plainly.** It stops the
file being sent: an EPUB reader receives sanitised per-chapter HTML, which it
already did, so reconstructing the book means scraping a hundred requests rather
than clicking save. It does not stop a determined person keeping what they can
read. Screenshots and OCR exist and no arrangement of this code changes that.
The gain is effort and a record of who was given what, which is worth having and
is not the same as prevention. PDF does not have even that yet - the reader
streams the whole file, so a read-only grantee cannot open one at all.

**The API contract is generated now, and it is generated from the Go.** The
project had said for its whole life that `api/openapi.yaml` was the source of
truth and that `types.gen.ts` was generated from it. Neither was true: the spec
described one endpoint out of forty-six, `types.gen.ts` had never been created,
and `client.ts` carried thirty-two hand-written interfaces restating Go structs
from memory. Nothing failed when they drifted, which is why they had.

Hand-copying fifty structs into YAML would have recreated the same problem a
year later, so the schemas are not written at all. `scripts/apispec` reads the
Go source - field names from json tags, optionality from omitempty and pointers,
descriptions from the doc comments, which is why it parses the AST rather than
using reflection - and writes the components block. The paths stay hand-written
in `api/paths.yaml`, because what an endpoint MEANS is not in a struct.
`client.ts` now aliases the generated types instead of restating them, and
`make lint` fails if the spec is stale.

Two shapes had to move into `dto` first: the error body and the health response
were unexported types in `internal/http`, so the two things every endpoint can
return were the two things the contract could not describe.

The chain was verified by breaking it on purpose. Renaming one json tag in Go -
`title` to `titleRenamed` - produced five TypeScript errors naming the exact
components that read it. A safety net nobody has fallen into is not known to
work.

**The worst thing in the retrieval pipeline was the router.** The eval was
measuring a bare index lookup rather than the path an answer actually takes.
Pointing it at the real pipeline dropped work Recall@10 from 0.875 to **0.594** -
a 28-point hole that had been there the whole time, invisible because the
narrowing only gave up when it returned NOTHING, and a search narrowed to the
wrong books returns ten confident passages and looks exactly like success.

Demoting the subject guess from a filter to a boost recovered part of it, to
0.750, and was still worse than ignoring subjects entirely. Per question the
boost promoted five and demoted nine, and five of those nine fell from rank 1 to
outside the top ten - a boost cannot lift the right passage, so what happened is
that ten wrong-but-on-topic passages climbed over it. "About the same subject"
and "answers this question" are different properties, and in a library where 48
books are tagged mathematics the first is nearly free. Routing now decides only
whether to retrieve at all, which it does with perfect accuracy on the eval set.

**Reranking was built, measured, and left switched off.** It is the stage the
architecture calls not optional, and it is the only one that reads a candidate
in light of the question. Against gemma4-12b, letting it drop passages cost 9
points of Recall@10; restricting it to reordering fixed that. What remained:

    work MRR     0.724 -> 0.748   (+0.024)
    passage MRR  0.434 -> 0.444   (+0.010)
    work Recall  0.875 -> 0.812   (-0.062)
    median       1.6s  -> 6.8s

A gain of 0.024 on 32 questions is less than one question's worth of movement,
paid for with five seconds of someone's time. The code is kept, tested, and off:
the verdict is about this model on this library, and a cross-encoder or a larger
corpus could reverse it. `make eval -rerank` re-runs the comparison.

**Answers are written, not delivered.** The first token now arrives in about
250ms instead of at the end of a twenty-second wait; measured against the real
server, 19 deltas between 153ms and 1.4s. The passages go out earlier still, as
soon as retrieval finishes and seconds before the model writes a word - they are
the evidence, they are useful alone, and they are ready. `EventSource` cannot
POST, so the client reads the response body and parses the SSE framing itself
rather than inventing a GET endpoint that would put every question in an access
log.

The question is written to the thread before the stream opens, and the answer is
stored with `context.WithoutCancel` - someone who closes the tab after twenty
seconds asked a question and got an answer, and both should be there when they
come back. Streaming also revealed that llama.cpp omits usage totals from a
stream unless asked: without `stream_options.include_usage` every streamed
answer recorded zero tokens. It now records 4,011 prompt and 486 completion on a
real question.

**The timings shown were understating the wait.** Routing is a model call
costing two to three seconds and it was counted in neither retrieval nor
generation, so a question that took eight seconds reported one. Routing is now
part of finding, and the two halves add up to the wall clock: 2,196ms + 6,361ms
against 8.56s measured.

**A race in the priority gate, found by running its own test five times.**
Reserving a slot for interactive work checked the count and then incremented,
which is not atomic: under load several background requests passed the check
together and took every slot. The reservation is now a compare-and-swap taken
before the gate slot. The test was right the first time; the implementation was
not.

The embedding model was checked before anything was built on it, and it holds
up: 768 dimensions, and two sentences about linear algebra score 0.576 against
0.187 for one about sourdough. Without that property none of the rest would
work, and it is cheaper to find out first.

**Chunks anchor to blocks, not to character offsets in a chapter.** That is the
same thing an annotation anchors to, so a citation resolves through exactly the
code path a highlight does and opens the reader at the passage rather than near
it. It also meant the Go extractor had to produce element paths identical to the
reader's TypeScript - which was verified by running both over the same markup
rather than by reading the two functions and believing they agreed.

Four bugs, all found by running it over 169 real books rather than fixtures.

1. **A latent bug in annotations, not in retrieval.** Both `chunks` and
   `annotations` had a CHECK comparing `(major, path, offset)` as a row, which
   orders the element path as TEXT. Text order is not document order: a passage
   running from `p[74]` into `a[29]/p[4]` - later on the page, earlier in the
   alphabet - was rejected as backwards. A highlight spanning those two blocks
   would have been refused, and the annotation feature had shipped with that in
   it. The constraint now asserts only what SQL can know, and the `chunks` and
   `annotations` tables were fixed together.
2. **NUL bytes.** Extracted book text contains them and Postgres text cannot
   hold one at all, so a single NUL failed a whole 900-page book. Six real books
   did exactly that.
3. **`CopyFrom` cannot write a pgvector column.** It sends binary, pgx has no
   codec for the type, and pgvector read the text literal's first bytes as a
   length - reporting "vector cannot have more than 16000 dimensions", which is
   what `[0` looks like when read as a dimension count. Vectors go through a
   parameterised insert with an explicit cast, which needs no new dependency.
4. **A job cannot enqueue its own successor.** Embedding deduplicates on a fixed
   target so that only one pass is ever queued; the running job holds that
   target, so the successor it enqueued collided with itself and vanished.
   Embedding drains the backlog in batches instead, which is also the more
   honest shape: the backlog is a query, so an interrupted run resumes where it
   stopped.

**Priority turned out to be a statement about what the library owes first.**
Chunking sat behind 150 enrichment jobs at a minute each, so search would not
have worked until the next day for no reason. Discovery first, then being
searchable, then being described.

### Retrieval and answering

**A token estimate cannot bound this corpus, so it stopped being the guard.**
The embedding server refused a passage as "2,941 tokens" that the estimate had
put at 900. Measured against the real model over real chunks: chars/4 was 1.82x
under across a random sample and 2x under on one book; counting punctuation
separately was 4.6x *over* on a page of dot leaders while still 1.7x under
elsewhere. How text tokenises depends on a vocabulary this code does not have.
The estimate now only sizes passages; a character ceiling is the bound, and it
holds exactly - including the overlap carried between chunks, which was
overshooting it by 12% until the ceiling was made to count that too.

**The model is the only authority on what fits**, so one passage it refuses now
costs one passage rather than the whole library: a refused batch is retried one
input at a time, and what is still refused is named in the log and skipped.

**A person must never queue behind background work.** A question took over three
minutes - it was waiting for an enrichment slot on the shared model. Yielding
was not enough, because a request already in flight cannot be interrupted and
enrichment takes up to a minute a book, so one slot is now reserved: background
work may hold at most all-but-one.

**Reasoning had to go here too, and this one was measured rather than assumed.**
Answering looked like the one stage where deliberation might earn its cost. With
ten passages, gemma4-12b spent **four minutes and its entire 4,000-token budget
thinking, and returned nothing**. With reasoning off the same question took 36
seconds and produced a correct, cited answer that correctly refused to invent a
definition the passages did not contain. The passages are already in front of
it; there is nothing left to deliberate.

**Chunks anchor to blocks so a citation lands where a highlight would**, and the
Go extractor's element paths were checked against the reader's TypeScript by
running both over the same markup - byte-identical, including the case where
empty paragraphs still count in the sibling index.


- Structure-aware chunking with locators and `chunk_strategy_version`.
- Embedding job with `model_id` per row; resumable; HNSW built after bulk load.
- Query routing: `factual-lookup`, `find-book`, `summarise-book`, `broad-topic`, `metadata`.
- SQL prefilter scoping by subject, collection, tag, author.
- Hybrid retrieval — pgvector cosine ∪ Postgres FTS — fused with RRF.
- Reranking down to ~8 passages under an explicit token budget.
- Grounded generation: citations mandatory, "not in your library" when unsupported.
- Chat UI with SSE streaming and citation chips that deep-link into the reader at the exact locator.
- **`eval/questions.jsonl` and a Recall@10 / MRR harness, built with the pipeline, not after it.**

**Exit:** ask twenty real questions across the library. Answers cite passages that genuinely support them, every citation link lands in the right place in the reader, and an out-of-library question gets an honest refusal rather than a confabulation. Retrieval metrics are recorded as the baseline for all future tuning.

---

## M5 — Release v0.1

- ~~Multi-arch static binaries, `linux/amd64` and `linux/arm64`, `CGO_ENABLED=0`.~~ **Done:** built on a tag by `.forgejo/workflows/release.yml`, cross-compiled rather than emulated, each one asked its own `-version` before anything publishes.
- ~~Client embedded via `go:embed`. `./lexarii` + `DATABASE_URL` is the whole install.~~ **Done**, and it is only true since the schema started travelling with it: the binary applies its own migrations at startup, so there is no second command to remember and no `migrations/` directory to ship. `LEXARII_AUTO_MIGRATE=false` for an operator who would rather not.
- ~~`docker compose up` path, library directory mounted read-only.~~ **Done and documented**, but see the status below: the image build itself is unverified here.
- ~~Multi-arch container images published on tag.~~ **Done:** `linux/amd64` + `linux/arm64`, `latest` moving only for a real version tag. Registry credentials are optional, so the workflow builds both architectures whether or not anything is configured to receive them.
- ~~Real README: screenshots, a five-minute quickstart, hardware guidance for the local model, and honest limitations.~~ **Done:** four screenshots from a real 168-book library, Docker quickstart first, measured reranking guidance naming the card it was measured on, and a "what does not work yet" section covering unreadable formats, scanned PDFs, DRM, and untitled books.
- ~~Dependency licence audit and NOTICE file.~~ **Done**, after `make licenses` was found to be swallowing its own failures - it was missing three transitive dependencies of bluemonday.
- Push to the GitHub mirror. **Blocked** on the repository URL and credentials; `scripts/mirror.sh` and the CI job are written and stubbed out.

**Exit:** a stranger with Docker and an ebook folder is reading a book within five minutes, from the README alone.

**Status: one job away.** CI has now run - it never had before, because nothing
had ever been pushed - and the backend and frontend jobs pass: gofmt, vet,
golangci-lint, build, API-spec freshness, migrations up/down/up, and
`go test -race ./...` against a real Postgres. Three things had to be fixed to
get there, none of them in the code: the runner's label pointed at a container
image that does not exist, `golangci-lint-action@v6` refuses a golangci-lint
2.x version string, and a job running in a container does not reach a service
container by its name on this runner - so the workflow now tries each place a
Postgres could be and uses whichever answers.

The image job remains, and it needs one line in the runner's own config -
`valid_volumes: ['/var/run/docker.sock']` - because a job in a container has no
Docker daemon of its own. The job now says so itself rather than failing with
"cannot connect to the Docker daemon".

**Previously: not yet met, and the gap is one thing.** Walking the quickstart as a stranger found that install fundamentally broken - the image carried no migrations and the binary never ran any, so `docker compose --profile full up` served against an empty schema and failed every query. That is fixed and verified end to end against a throwaway database: 22 migrations applied 0 to 22 in under a second, first-account setup returned 201, a scan ingested 165 works, and chapter text served. What remains unverified is the container build itself, for the same reason recorded against M0 - this machine's Docker cannot create container networking, so `npm ci` in the client stage never runs. CI builds the image on every push, which is where that will be caught.

---

## M6 — Zotero-style research features

Deferred deliberately. The domain model already supports it; building it before M4 would have been premature.

- ~~Citation export: BibTeX, CSL-JSON, RIS.~~ **Done**, per book and per
  collection, rendered from what the library knows and never from a guess.
- ~~External metadata lookup: OpenLibrary, ISBN resolution.~~ **Done**, as a
  command somebody runs rather than a worker that runs itself. This is the one
  thing here that talks to a stranger, and a list of the books on a shelf is a
  personal thing: it does nothing unless `LEXARII_METADATA_BASE_URL` is set,
  reports without sending anything by default, and fills only EMPTY fields.
  Crossref is not done and is not planned: it indexes journal articles by DOI,
  and this is a library of books.
- ~~PDF annotation extraction~~ **done**; round-tripping deliberately not. A
  PDF's own highlights are imported from the book file - 805 of them out of one
  real book - and never written back, because the library directory is
  read-only and that is the first invariant here. It happens in the browser:
  the marks are rectangles, turning one back into words needs the geometry of
  the rendered text, and the server's extractor reports every word at position
  zero on the very books that carry annotations.
- ~~Reference collections, per-collection bibliographies.~~ **Done:** the
  schema has held collections since M0 and nothing above it existed. Create,
  rename, delete, file a book from its own page, and export the whole thing as
  BibTeX, RIS or CSL-JSON - which the citation endpoint gave almost free, since
  it already took the library's filters. Deleting a collection keeps the books:
  it is an arrangement of a shelf, not a box that owns what is in it.
- ~~Note search folded into retrieval.~~ **Done:** a reader's own notes are
  searched alongside the passages and given to the answer as what they are -
  the reader's view, not the book's. Cited in the same numbering, so [11] is
  the eleventh thing the answer was shown and the reader does not have to know
  which sort of thing it turned out to be. Strictly the asker's own: a shared
  book does not share the margins.

- ~~Backlinks between books.~~ **Done, and found rather than declared.** A
  backlink in a library of books is not a link somebody typed: it is a
  bibliography entry, an index line, a "further reading" list - one book naming
  another in its own text. The full-text index built for retrieval already held
  every one of them and nothing had ever asked.

  The test is the title as a phrase plus the author's surname in the same
  passage, and the surname is not optional. Measured on this library of 168
  books, the title alone put "Linear Algebra" in 23 other books and
  "Reinforcement Learning" in 11, none of them citing anything - a title that is
  also a subject matches every book about the subject. With the surname, 31 of
  168 books have at least one mention and the ones read by hand are real:
  `Sierra, Kathy (Head First Java), 4` is an index line in another book, and
  `bash Cookbook` names `Learning the bash Shell` in fifteen places.

  9ms for the slowest single book, 351ms for all 168, so it is a live query and
  not a table. A one-word title or a surname under four letters returns nothing,
  because both match far too much to show.

---

## M7 — Android on F-Droid

- ~~API hardening for a third-party client.~~ **Done.** Authentication was a
  cookie and nothing else, which is a browser assumption: a client now signs in
  at `/api/v1/auth/token` and carries a bearer token, listed and revoked beside
  every browser session under the name its owner gave the device. Versioning
  needs nothing built - `/api/v1` is the contract, the spec is generated from
  the Go, and a second version would be a second prefix.
- ~~Offline sync protocol: reconcile positions and annotations on reconnect.~~
  **Done**, as `GET` and `POST /api/v1/sync`. Client-generated ids so a mark
  made on a plane exists before a server sees it and a repeated push is
  harmless; the client's own timestamps so a week-old edit does not land as
  today's news; tombstones so a deleted mark stays deleted; last-write-wins for
  marks, and for positions the two-rule merge the schema already had - where
  you are takes the newest, how far you got only advances. Downloading a book
  to read offline was already there: `/api/v1/books/{id}/file`.
- ~~Reproducible builds.~~ **Done and checkable: `make reproducible` builds
  everything twice and compares the bytes.** Go was already reproducible;
  the client was not, and nothing had ever looked. SvelteKit names each build
  after `Date.now()` and bakes the name into every generated module, so two
  builds of identical source shared not one output file. The name is the commit
  now.
- Native client. **No Google Play Services, no Firebase, no proprietary SDK, no
  closed-source dependency at any depth** — these are F-Droid inclusion
  requirements, and every one of them is cheap now and expensive to unpick
  later. **Started**, at `android/`: Kotlin and Compose, wire types generated
  from the same Go structs as the web's. It signs in with a bearer token, keeps
  it across restarts, lists the library, and reads EPUBs. The reader is a
  WebView running the web reader's own locator code, compiled
  (`make android-reader-js`, checked by `make lint`), so a position saved on
  the phone names the same paragraph in the browser and in the indexer's
  chunks. Positions go back through the sync push with the device's clock.
  An EPUB can be kept on the device and read with no network, positions are
  on disk before they are sent, and an offline launch shows the last shelf.
  Highlights and notes are made from the text-selection menu, kept on the
  device before they are sent, and synced both ways, shared marks included.
  It lists the whole library, with covers, and shows all of it offline. PDFs
  are read with the platform's own renderer, a page at a time or from a kept
  file. A typography sheet - theme, font, size, line spacing, margins - is
  kept per device and handed to the page. Keeping a book runs in the
  background and resumes after the process is killed.
- F-Droid metadata and submission. Follows the client; there is nothing to
  submit without one.

---

## Known risks

| Risk | Mitigation |
|---|---|
| Locator instability breaks every annotation and citation | Locked down and tested in M0/M1, before anything depends on it |
| Local model too slow for a large library | Enrichment is a resumable background job; the library and reader never block on it |
| Retrieval quality disappoints on real questions | Hybrid + rerank from the start; eval harness built alongside the pipeline, not bolted on |
| Embedding model swap invalidates all vectors | `model_id` stored per row; re-embedding is a resumable job |
| Scanned PDFs are a large fraction of real libraries | Detected and flagged in M1; OCR scoped as its own later decision, not smuggled in |
| Scope creep from the Zotero feature set | Explicitly deferred to M6, with the domain model already shaped to absorb it |
