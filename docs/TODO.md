<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Todo

Work that is known, wanted, and **not** yet done. [ROADMAP.md](ROADMAP.md) says what
the milestones are and what has been learned; this file says what is currently
blocked and on whom, so nothing quietly falls off the end.

Anything here that turns out to be blocked on nobody should be done rather than
listed.

## Papercuts, known and left alone

- ~~**The container image has never been built end to end.**~~ **It has now:
  `make image`.** The blocker was one flag. This machine's Docker cannot set up
  the default bridge - "failed to add the host <=> sandbox pair interfaces:
  operation not supported" - and it fails at the first step needing the
  network, which is `npm ci`. With `--network=host` the whole thing builds.

  Verified from the image rather than from the Dockerfile: 29 steps, 30.8MB,
  `lexarii -version` prints the version passed in as a build argument, so the
  entrypoint and the ldflags injection both work, and it runs as uid 10001
  rather than root.

  The CI job that does this on every push had never run either, and the reason
  was a problem the workflow had made for itself: it ran inside a `container:`,
  a job in a container has no Docker of its own, so it needed the runner's
  socket mounted in - and act_runner drops that mount unless its config
  allowlists it. The answer was not a runner setting. It was to drop the
  container. The job runs on the host now, the way the n8n-builder workflow on
  this same server always has, and pushes the result to this server's own
  registry so the image CI verifies is the image somebody can pull.

- **The image is built for one architecture.** The Dockerfile deliberately does
  not pin its build stages to `$BUILDPLATFORM` - it is BuildKit-only, and the
  legacy builder fails outright on the empty string it expands to - so a second
  architecture in CI means qemu emulating `npm ci` and a Go build rather than
  cross-compiling them. Multi-arch belongs to whoever publishes a release,
  where tens of minutes is affordable.

- **MOBI files never get a cover, and it does not matter.** Measured: both
  MOBI files in this library sit inside works that also hold a readable EPUB
  and a PDF, each with a cover, so the shelf shows a cover for both books. The
  file has none; the book does. Left as it is, and recorded so nobody spends an
  afternoon on it. The original note follows.

- **MOBI files never get a cover.** Two in the real library, both duplicates of
  a book also held as EPUB or PDF, so the shelf shows the sibling's cover and
  nothing looks wrong. There is no MOBI reader either: the format is recorded
  and listed, not opened. Worth fixing together or not at all.
- ~~**Cover rendering only runs while the library page is open.**~~ **Done**,
  and this entry was wrong twice. It did not say how many were left: the count
  was computed, stored in `$state`, and never rendered anywhere. And the number
  it computed was the batch, which is 24, so it would have said "24 left" after
  every round of 24. The store counts the whole queue now, scoped to the
  viewer, and the shelf shows it with a **Draw them now** / **Pause** control -
  which is the "tell it to get on with it" this asked for. It still runs only
  in the browser, and that part is not a papercut: a PDF's first page needs a
  rasteriser `CGO_ENABLED=0` keeps out of the server.

## Blocked on Adam

These cannot be finished from inside the repository.

- **GitHub mirror credentials.** `scripts/mirror.sh` is a deliberate stub and the
  CI job that calls it is inert. Waiting on how the other projects do it: the
  token, its scope, and whether the push is a mirror push or a branch push.
  Canonical stays `scm.poniatowski.dev/adam/lexarii`.

  **This is also what blocks the F-Droid submission.** `android/fdroid/com.lexarii.app.yml`
  is a draft with a clearly marked placeholder `SourceCode`/`Repo` URL:
  fdroiddata's build servers need to clone the repository anonymously, which
  `scm.poniatowski.dev` does not currently allow and the GitHub mirror is
  exactly for. See [android-release.md](android-release.md) for the rest of
  the checklist - tagging the first `android-v` release and, separately and
  optionally, whether Adam wants to sign a release himself to verify it
  against what F-Droid's build reproduces.

## Product decisions, now made

Both of the questions that sat here are answered, and the answers are
implemented rather than merely recorded:

- **One shelf or several: SEVERAL.** Books belong to whoever added them and are
  shared with named people. Migration 17 retired the shared library; see
  docs/DOMAIN.md for the access model.
- **What a second person may do: read, annotate, ask.** Changing the shared
  parts of the installation - scanning, enrichment, resolving suggestions,
  cancelling jobs - requires an administrator. Uploading does too by default;
  `LEXARII_UPLOAD_POLICY=users` lets everybody add books to their own shelf,
  which is safe because a file can have several holders (migration 33).
- **Letting go of a book: done.** `DELETE /books/{id}/holding` takes a book off
  the caller's own shelf, with the same rule as account deletion for that one
  file. Their notes and reading position are kept; see docs/DOMAIN.md.
- **Still open: a storage quota per person.** With `LEXARII_UPLOAD_POLICY=users`
  any account can fill the data directory, one capped file at a time. What a
  person holds is already a sum over `book_file_holders` (`Pool.HeldStorage`);
  nothing enforces a limit on it yet.

## M3, still to do

The provider client, prompt, sampler, job, and review screen are done. What the
milestone still asks for:

- ~~Author normalisation~~ and ~~tag normalisation~~ are done; see the roadmap
  for why neither uses similarity. The controlled vocabulary this entry asked
  for was not built, and the reason is worth keeping: the 136 single-book
  subjects are nearly all correct and specific - bayesian statistics, ansible,
  device drivers, econometrics - and there was no screen anywhere that listed
  subjects, so nothing was cluttered by them. What was missing was a way to see
  what the library is about at all. /subjects now shows subjects by book count
  with the single-book tail behind a disclosure. If that screen later shows
  real duplication, curate from what it shows rather than from a count.
- ~~Bogus author names from source files~~ - `PlausibleAuthor` now rejects a
  single all-lowercase token, which is what `radha` and `Sys` both are. The
  rule this entry proposed - a single token spread across UNRELATED subjects -
  was measured and abandoned: the subject overlap for `radha` is 0.40, the same
  as Arnold Robbins and better than Dawn Griffiths at 0.25, because authors of
  introductory books write across unrelated topics. Being one lowercase word is
  the signal; across 135 real author names it rejects exactly those two.
  Existing rows are not touched by a rule change, so `make authors` reports
  them and `ARGS=-prune` detaches them. **Run against the real library: there
  is nothing to prune.** Neither `radha` nor `Sys` is in the authors table any
  more - they did not survive a re-ingest - and none of the 133 remaining names
  fails `PlausibleAuthor`. The three pairs above the similarity threshold are
  all correctly kept apart, including David and Dawn Griffiths, who co-author
  three books and are two people.
- ~~**The confidence bar does not work with gemma4-12b.**~~ **Answered, by
  asking a different question.** Counted across the whole library: of 1,576
  claims the model has made, **1,560 came back at exactly 1.00** and the other
  16 at 0.90. The bar is 0.60, so it has never blocked a single value. A number
  that is always the same is worse than no number, because the code around it
  reads as though something is being checked.

  The bar stays - it is provenance, and other models do calibrate - but the
  thing that now actually decides is whether the value is IN THE BOOK. A
  title, a subtitle, a series and an edition are all printed; if the model's
  answer is nowhere in the pages it was shown, it came from the filename or
  from nothing.

  Measured on 25 books before it was built, because a signal that does not
  separate anything is what was being replaced: 21 titles appear in the text
  and 4 do not. Two of those four look correct, and PDF extraction is lossy
  enough that a real title can genuinely be missing from a sample - so an
  ungrounded value is **suggested rather than applied**, never discarded. The
  cost of being wrong is one row in the review screen.

## Found by using it

- ~~**A question got a shelf instead of an answer.**~~ Fixed in route/v3.
  "What is the best agile methodology?" routed to `broad-topic`, which by
  design retrieves nothing and synthesises nothing - so the reply was eight
  titles, from a library holding a book that compares Scrum, Kanban and XP and
  could simply have answered.

  Refusing to synthesise is right for "tell me about mathematics" and wrong
  for a question somebody could answer in a paragraph. `broad-topic` is now a
  TOPIC with no question in it; anything that actually asks something is a
  lookup, however wide its subject. Intent accuracy on the eval stayed at
  1.000 and work Recall@10 went 0.892 to 0.919.

- ~~**A reading list answered the wrong subject.**~~ Fixed. "What is a good
  agile methodology?" routed perfectly - agile methodology, software
  development, project management - and came back with C#, Docker and Fluent
  Python, while Head First Agile sat in the library untouched. findBooks
  returned on the FIRST subject that matched anything: the second subject had
  twelve books, it returned eight of them alphabetically, and never reached
  project-management where the right book is.

  Every subject is weighed now, and a hit on a small subject counts for more
  than a hit on a large one - twelve books tagged software development barely
  narrows a shelf, three tagged project management says something. A book
  under several of the asked-for subjects adds them up.

  That was not enough on its own. Asked "what is the BEST agile methodology?"
  the router returned only two subjects, project management among them no
  longer, and the exact slug `agile-methodology` does not exist in this
  library - which files that book under `agile-software-development`. So a
  subject is now matched on any whole word it shares with a tag, weighted by
  how informative that word is: `agile` appears in one tag and says a great
  deal, `development` appears in a dozen and says almost nothing. Subjects
  scoring under half the best match are dropped rather than ranked last,
  because a list of eight where seven are padding reads as a broken answer.

  ~~**There is no eval set for reading lists.**~~ **There is now:**
  `eval/readinglists.jsonl`, 13 questions, `make listeval`, baseline in
  `eval/readinglists-baseline.json`. A reading list fails differently from an
  answer - not "the wrong passage" but "eight books, seven of them padding" -
  so it is scored on how much of the right shelf came back rather than on the
  rank of the first hit.

  First baseline, on the real library: **work Recall@8 1.000, multi-book
  coverage 0.880, intent accuracy 1.000, refused correctly 0.667.**

  Two things it found immediately. **"Medieval European history" comes back
  with a book** - one, not eight, but the library holds nothing medieval and
  the right answer is to say so. And the harness would have scored that as a
  PASS: a broad-topic control was being marked on having been routed away from
  passage retrieval, which for a reading list is the normal path rather than a
  refusal. It is now marked on the shelf it actually returned.

  The cruel control passes: "Cookery and recipes" returns nothing, from a
  library holding eleven books with "Cookbook" in the title and not one of them
  about food.

- ~~**One book is called "BirminghamMumbai".**~~ **Both halves fixed.** It was
  `azuredevopsexplained.pdf`, and that string is the publisher's two office
  cities run together off the copyright page. It turned out to be one of
  ELEVEN books filed under something that is not their name - six whole title
  pages, one 300 characters long, one a remark the model made *about* the
  title page - and every one came from the model rather than from a file.
  `PlausibleTitle` now refuses a title over 200 characters, a bare edition
  statement, and a handful of phrases that appear on front matter and never in
  the name of a book. The eleven were corrected by hand, which is possible now
  that a book can be edited at all.

## The catalogue lookup, and why most of it missed

`make metadata ARGS=-apply` was run against the real library and filled 88 of
164 books on its first pass. The other 76 were mostly not misses at all.

**A catalogue writes the subtitle into the title.** OpenLibrary holds "Head
First Agile: A Brain-Friendly Guide to Agile Principles, Ideas, and Real-World
Practices" where the book's own metadata says "Head First Agile", and
`sameTitle` demanded exact equality - 20 rejections in the first 40 lookups,
every one a book the catalogue plainly had. Titles now also match on the part
before the colon.

**Equality, not prefix, and that keeps some books unmatched on purpose.** A
prefix rule matches "Linear Algebra" against "Linear Algebra Done Right" and
would write a different book's year into this library. For the same reason
"Linux Pocket Guide, 3E" finds nothing: the record it would match is a
different edition, with a different year and a different ISBN, so no answer is
the right answer. **A book that stays empty here is not necessarily a bug.**

**Authors are filled only where there are none.** 70 books had no author at
all - unattributed, unfindable by author, exported as citations with nobody's
name on them. A catalogue naming a different translator is worse than what the
file said; against silence it is better. Checked inside the transaction, so two
lookups cannot both decide a book was empty.

This is written down here because the code for it landed in the commit titled
"Offer every format to download, and read the cheapest", which does not mention
it: a blanket `git add` swept two unrelated changes into one message.

## Open, as of the reading-list eval

- **There is no way to send email**, so there is no password-reset link, no
  invitation, and no notification when somebody shares a book with you.
  `make password` covers the forgotten password for whoever can reach a
  terminal, which on a self-hosted box is usually the same person - but not on
  one shared with a household. Needs a decision before it is built: what it is
  FOR. A reset link means a token table, an expiry, a single-use rule and a
  form; a share notification means an address people have not been asked for.
  net/smtp is in the standard library, so the cost is the feature rather than
  the dependency.

Written down because "everything is fixed" is never true and the useful
question is which things are known.

- **61 books have no publisher.** Down from 164. Most of the rest are correct
  refusals - a catalogue record for a different edition is worse than an empty
  field - but not all of them are. Three books still have no author, down from
  seventy.
- **CBZ and CBR cannot be read**, and no book in this library is one. Scanned
  PDFs are not OCRed, and no book in this library is one either. Both are real
  gaps for somebody else's shelf and unmeasurable on this one; OCR additionally
  needs an engine outside this process, which `CGO_ENABLED=0` makes a decision
  rather than a dependency.
- **CI runs again, and the image job checks nothing.** Two separate facts.

  It had stopped entirely - not failing, never starting - for 23 commits,
  because the workflows asked for `ubuntu-latest` and the one runner on this
  server advertises a single label, `docker`. Run #41 is the first full green
  pass since.

  That `docker` label is CONTAINER-mode, which the image job proved by failing
  on its own error message: a job inside a container has no Docker daemon. The
  repository has no secrets set either, so the job cannot ask the registry
  whether the image exists. It therefore verifies nothing at all today, says so
  at full volume, and exits 0 rather than staying permanently red for a
  configuration gap. It climbs a rung the moment REGISTRY_USER and
  REGISTRY_PASSWORD are set, and reaches the real check - that the image STARTS
  - only with a host-mode runner.

  The image itself is built, pushed and verified by hand: `make push-image`.
- ~~**The Android client reads EPUBs and nothing else yet.**~~ **All three
  halves done.** The whole library is listed: a first page of 50, the rest
  behind it in pages of 200, sooner if the list is scrolled to its end - and
  the saved shelf is replaced only by a COMPLETE listing, so an offline launch
  never shrinks to a page. Covers come through the app's own client (they need
  the token), are decoded by the platform, and kept on disk as 160px
  thumbnails under a 24 MB bound; no image library. PDFs are read with the
  platform's PdfRenderer, a page at a time from `/page/{n}` with the
  neighbours fetched as `?prefetch=1`, positions as `pdf:<page>:0` through the
  same outbox. A PDF is kept as its file from `/file`. Keeping runs in
  WorkManager: it survives leaving the app and the process being killed, and
  resumes - an EPUB skips the chapters it has, a PDF asks for the rest with
  `Range` and `If-Range`. Checked on the emulator by killing the process
  mid-download: the file came out byte-identical. **Left:** highlights in
  PDFs (a rendered page has no text to select); covers are never revalidated,
  so a cover changed on the server shows the old one until the cache lets it
  go. The worker's own code runs in the JVM tests (`KeepJob`); the thin
  WorkManager adapter around it does not, since `work-testing` needs a real
  Context - Robolectric or a device - and it was checked on the emulator
  instead.
- ~~**`anchorFromSelection` in locator.ts mis-reads a selection that ends on an
  element boundary.**~~ **Fixed in locator.ts, for both readers.**
  `normaliseRange` moves a start forward and an end back onto text inside a
  block, so (next `<p>`, 0) ends at the end of the paragraph before it and a
  boundary in the whitespace between blocks lands in the nearest block. The
  selection itself is never touched, and the saved text is computed from the
  range rather than taken from `Selection.toString()`, which differs between
  engines. reader.js lost its copy of the workaround and of BLOCK. The locator
  format is unchanged; only which DOM position a selection maps to moved.

  Checking it end to end in the web reader found a worse bug beside it: a
  **mouse** selection was cleared the instant it was made. Chrome fires a
  click after every drag and triple-click, and the tap handler read "a
  selection is live" as a tap to dismiss it, so the Highlight button never
  stayed up. Touch was unaffected, because a long-press makes no click. The
  page now decides at pointerdown whether a selection was already showing,
  as the Android page does.
- ~~**Marks the Android reader cannot place are only counted.**~~ **Reported
  now**, as the web does: own marks in the chapter that the page could not
  place are flagged, flagged ones it now could are cleared, best effort and
  online only, and nothing is deleted. Checked on the emulator against the
  database, including a flag cleared from a chapter where every mark was
  placed. The page reports once after every chapter it shows, an empty list
  included, and says which chapter the report is about; the app drops a
  report for a chapter no longer on screen, and judges the last report again
  when marks arrive from the server after the page has painted.
- ~~**The Android reader page has no automated tests.**~~ **It has 78:**
  `make android-reader-test`, web/tests/android-reader/, also run by `make
  test` and by its own CI job. The page is assembled exactly as the app does
  it and driven headless in Chromium, the WebView's engine; `FIREFOX=1` adds
  Firefox. With no browser it prints SKIP and exits 0.

  Writing them found two bugs in the page. **Pages drifted on a fractional
  screen width**: the page was measured with `clientWidth`, which is rounded,
  while the columns are laid out at the real width, so a phone 411.43px wide
  was off by a fraction of a pixel more on every page. By the tenth, no
  paragraph counted as on screen and the position stopped being reported. The
  web reader had the same arithmetic and gets the same fix. And a **sideways
  swipe could navigate back** in Chrome, which `touch-action: pan-y` stops.
  The WebView offers no such gesture, but the page no longer relies on that.
- ~~**Shared marks have not been seen on a device.**~~ **Seen, and nothing
  needed fixing.** A second account on the scratch server shared a book with
  its highlight and note into the first: on the emulator both were drawn
  dashed, opened read-only with the author's name and no field, Delete or
  Save, were held on the device as shared and never pushed (the server's rows
  unchanged), and disappeared when the notes were taken out of the share. There
  is no API to create a second account, so it was made through the trusted
  proxy header, limited to 127.0.0.1, on the scratch server only.
- ~~**The web reader loses the paragraph on a resize.**~~ **Fixed.** It keeps
  `here`, the paragraph the reader last arrived on, and a relayout - a
  resize, a rotation, any typography setting - puts that paragraph back on
  screen. It is deliberately not updated from where a relayout lands, or
  dragging the size slider would walk the reader backwards a paragraph at a
  time. Checked in a browser against the old code, opening at paragraph 20:
  size 28 used to land on paragraph 10 and save that as the position; now
  every resize, size, leading and measure change keeps paragraph 20 in view.

## Built and unreachable

- **The API spec generator ignored embedded structs.** A Go struct that embeds
  another marshals its fields flat; `readFields` skipped any field without a
  tag, which is exactly what an embedded one is. `CollectionDetail` described
  itself as holding nothing but a list of books while the server sent six more
  fields, and the generated TypeScript agreed with the spec. Embedded structs
  are spliced in where they stand now, and a struct embedding something that is
  not a wire type fails the generator rather than quietly losing it.

A pattern worth naming, because it has now happened four times in one session:
the code exists, it works, and nothing calls it.

- `Route.subjects`, `Route.book` and the query rewrite: three documented
  features that had never run once, because the JSON schema made their fields
  optional and the model answered with only what was required.
- `editions.publisher`, the ISBNs, the year and the page count: written at
  ingest since M1 and read back by no endpoint and no screen, so the first
  thing that asked for them - citations - got an empty answer and no error.
- `GET /api/v1/annotations`, whose DTO comment calls it "the notes view across
  the whole library": no client function, no page. **Now reachable** at
  /notes, and the three fields it never populated - the annotation's own book,
  its updated time, the end of its range - are populated.

All four were found by trying to USE the thing, never by reading the code.

## Books the model could not describe

Twenty-three works of 168 failed enrichment with "the model ran out of tokens
before answering", each after three attempts and about two minutes of GPU per
attempt. It was recorded here as two books and a guess about tables of contents;
both were wrong.

What it actually was: gemma4-12b falls into a **repetition loop inside the title
field**, and the JSON schema it was given accepted any string, so nothing could
stop it. Reproduced with the real model on `c80inanutshell.pdf` - 2 minutes 5
seconds, cut off at the 4,000-token limit. The same sample with an unconstrained
prompt answered in 11 seconds using 251 tokens, which is what ruled the sample
out as the cause.

Fixed by giving the wire schema the same length bounds Go already enforced after
the fact, so the grammar itself ends the loop: the same book now answers in 4.6
seconds. A frequency penalty was tried first and did not help - it changed what
the model repeated, not that it repeated - so it was removed rather than kept as
a knob nobody could justify. Runaway titles are rejected by `PlausibleTitle`,
which leaves the summary and subjects - both good in every case checked - to be
applied as usual, and leaves the title to publisher metadata, which is where it
should come from anyway.

All 23 were re-run afterwards and all 23 succeeded, in 10-20 seconds each,
every one of them gaining a summary and between two and seven tags. Their titles
went to the review queue rather than onto the work, because these books already
had publisher titles - which is the rule working, not a shortfall.

The loop turned out to be wider than the failures: it also spoiled title
suggestions for books that did NOT fail, because a title that stops repeating
before the budget runs out still returns. `go run ./scripts/titles` reports
those and `-reject` clears them; eight of 113 waiting suggestions were
unusable and are now rejected rather than sitting in the queue. The other 105
are real suggestions somebody should look at.

Both of these are now fixed, and both entries were wrong about the problem:

- ~~**The model writes its deliberation into the title.**~~ The guess here was
  right - a separate field for the reasoning - but not for the reason given. A
  field is not enough on its own; it has to come FIRST. llama.cpp emits
  required properties in schema order and `encoding/json` sorts map keys, so
  making every property required puts `printedTitleLine` and `reasoning` ahead
  of `title` in what the model writes. While they were optional the model wrote
  the title before it had anywhere to think, and thought in the title.
- ~~**Most title suggestions are the whole title page.**~~ They were not.
  Counted rather than eyeballed: of the 105, **two** were a title page. 30%
  carried an edition statement and 21% a subtitle - forbidden by the prompt,
  included anyway, because the model had nowhere else to put them. classify/v3
  gives a subtitle and an edition fields of their own. Measured on the twelve
  books v2 handled worst, clean titles went from 3/12 to 12/12, and 11 of those
  12 came from the ordering fix above rather than from the new wording.

  **The 105 suggestions themselves are still waiting.** They were produced by
  v2 and nothing has re-run enrichment over those books, so the queue still
  holds the old answers. `go run ./scripts/classifyeval` shows what v3 says
  about any of them without writing anything.

## Per-book sharing

Books are per-person, shared with named account-holders, readable without being
downloadable. Built and working end to end: an owner opens a book, ticks a
person, and that person can read it and nothing else on the shelf.

What remains:

  * ~~Prefetch is one page either side~~ - measured, and left alone. Once the
    book is parsed a page takes 22ms at the median, 45 at the ninetieth
    percentile and 60 at worst, so outrunning the window costs tens of
    milliseconds and not a visible stall. Each page is 308 KB at the median and
    579 at worst, so the window as it stands holds about a megabyte; widening
    it to three either side would roughly double both that and the traffic per
    turn to buy back 60ms. It is also not an audit problem, because a
    prefetched page already counts as a view and not as a page taken. Worth
    revisiting only for somebody reading over a slow link, where the numbers
    are different and nobody here can measure them.
  * ~~**The first page of a large PDF is still slow.**~~ **Fixed, and it was
    not the reading.** 5.9 seconds for a 112MB book is now 320ms, because
    pdf.Open no longer asks pdfcpu to VALIDATE the document before serving a
    page from it. Over the twelve largest books: 22.2s of opening became 2.0s,
    eleven times faster, and the memory they held went from 3,985MB to
    1,445MB.

    Validation was doing both. Measured separately, `ReadAndValidate` held
    873MB of the 874MB that `ReadValidateAndOptimize` held - the optimisation
    pass was never the expensive half. And it bought nothing: its job is to
    report that a file breaks the specification, and this package's answer to
    that was already "relaxed, then read it anyway", because a book that opens
    in every other viewer has to open here.

    Checked against the 25 largest PDFs in the library: same page count, same
    first page, no regressions.
  * ~~**Four books stay parsed in memory.**~~ **Was 2.7GB, now 819MB.** The
    measurement came first: a server idling at 46MB went to 2.7GB after the
    four largest PDFs in the library, plateauing between 2.4 and 3.1GB. Five
    sixths of that was pdfcpu's validation pass, which is now not run - see the
    entry above - and the same four books cost 819MB, levelling near 1GB over
    the next ten.

    Then the cache itself was fixed, and the claim above that a byte budget
    would need to weigh each book as it parses was **wrong**. Measured over the
    fourteen largest books, a parse holds between 1.06 and 1.66 times the file
    size:

        file     56MB  61MB  63MB  79MB  98MB 112MB 124MB 158MB
        parsed   76MB 101MB  87MB 102MB 119MB 176MB 156MB 167MB

    A stat is enough. The cache budgets bytes now, estimating at 1.7x - the top
    of the range, because being wrong low defeats the point - and evicts
    least-recently-used until it fits. A book larger than the whole budget is
    still served, alone, because refusing to open somebody's book for being big
    is a worse answer than using the memory while they read it.

    Resident memory runs about 1.45x the budget, Go keeping its own headroom.
    Opening all fourteen in a row: a 768MB budget levels at 1,114MB resident, a
    256MB budget stays between 168 and 393MB. Default is 512MB.

    `LEXARII_PDF_CACHE_MB` replaces the book count, which could not say how
    much memory it allowed.
  * **The log has no retention policy.** It is not a size problem, which is
    what this entry used to claim: the table aggregates, so one person reading
    a 104-chapter book end to end is ONE row. Measured on the real database,
    107 views occupy two rows. A household of five reading three borrowed books
    a day would add about 5,500 rows a year, which is well under a megabyte.
    The open question is not disk, it is how long a record of who read what
    SHOULD be kept - a decision about the installation rather than a tidy-up,
    and one nobody has made. Pruning it automatically would destroy the record
    the log exists to hold.
  * **The warning is only in the shelf.** An owner who does not open Lexarii
    for a week is not told until they do. Email or a push would need somewhere
    to send it, which this installation does not have.
  * ~~**An archived book can only be claimed whole.**~~ **Done.** The endpoint
    always accepted a person and the screen never offered one, so tidying up
    after somebody meant taking their books yourself and sharing them back.
    There is a picker beside each one now; leaving it on "me" is still one
    click, which is what an administrator wants most of the time.

## M4, still to do

Routing, retrieval, citations, conversations, library questions, and the chat
screen work. What the milestone still asks for:

- ~~Streaming~~ and ~~reranking~~ are done; the eval exists. What is left of M4
  is below.
- ~~Reranking needs a cross-encoder~~ - one was served and measured, and it
  earns its place: work MRR 0.748 -> 0.806, passage Recall@10 0.625 -> 0.688,
  passage MRR 0.434 -> 0.514, median question 1.6s -> 2.1s, with work Recall
  and multi-book coverage unchanged. bge-reranker-v2-m3 at Q8_0, 636MB, sharing
  a 12GB RTX 3060 with the chat and embedding models - so the quality figures
  travel and the timings do not. `eval/baseline.json` now measures the pipeline
  with it; `eval/baseline-no-rerank.json` is the same questions without.
  The measurement also found the handicap: passages were clipped to 700
  characters, sized to fit twenty of them in a chat prompt back when the chat
  model did this job. At 700 the cross-encoder was WORSE than no reranking on
  both MRRs. At 2,048 - all of nearly every chunk - everything improves. How
  much of a passage the ranker sees mattered more than which model did the
  ranking.
- ~~**Multi-book coverage is unmoved at 0.500.**~~ **0.833.** The idea was
  right: route/v2 asks a comparison to name its sides, each side retrieves and
  ranks with its own words, and the results interleave round-robin. Fusing them
  into one pool and ranking that does not work - whichever side matches better
  takes the pool, which is the failure being fixed.

  The first attempt moved nothing, and why is the more useful finding. Every
  field on `Route` except `intent` was optional in the JSON schema, and
  gemma4-12b answered with exactly what was required. `subjects`, `book` and
  the `search` rewrite had therefore never once run, in any question, since
  they were written. **An optional property in a schema is a feature that does
  not exist.**

  Switching the rewrite on is how it was found to be a regression - passage
  Recall@10 0.719 to 0.656 - so it is gone rather than kept as an option.
  Retrieval costs 663ms more per question, which is the model writing fields it
  used to skip.
- ~~**PDF annotation extraction.**~~ **Done.** Highlights made in another
  reader and saved into the PDF itself: 805 recovered from one real book. Read
  in the browser, because a mark is a rectangle and turning one back into words
  needs the geometry of the rendered text. Never written back - the library
  folder is read-only.
- ~~**Annotations in the PDF reader.**~~ **Done.** pdf.js's text layer goes over
  the canvas - a canvas holds pixels, so until this there was literally nothing
  on the page to select - and marks are drawn as boxes in a layer of their own.

  Anchored by the WORDS, not by coordinates or offsets: a rectangle saved at
  one zoom means nothing at another, and the server extracts a PDF's text with
  a different library than the browser does, so their character offsets do not
  agree. The locator's offset field carries which OCCURRENCE of the phrase was
  marked, which is the only part of it the PDF reader has ever used.

  Four things went wrong and all four were worth finding:

  - pdf.js writes only `--font-height` and `--scale-x` on each span and expects
    its own stylesheet to turn those into a font size. Without those two rules
    the invisible text sits nowhere near the visible text, and a mark lands a
    line high and a third of the width of its own words.
  - A superseded render cleared the text layer a newer one had just filled.
  - Pressing the Note button collapsed the selection before the handler read
    it, so what was selected is captured when it is selected now.
  - **Both readers asked for something the schema forbids.** They created an
    annotation of type `note` with no body, and `annotations_note_has_body`
    refuses that - so the Note button has been answering 500 in the EPUB
    reader too, for as long as it has existed. Both create a highlight now,
    and writing a note promotes it; clearing one demotes it back.
- **Windowed rendering for very large chapters.** Chapters are rendered whole,
  and now there is a number for when that stops being acceptable. Laying one
  spine item into the reader's columns, measured on a desktop:

  | HTML | words | layout |
  |---|---|---|
  | 123 KB | 8k | 37 ms |
  | 491 KB | 31k | 146 ms |
  | 982 KB | 62k | 345 ms |
  | 1.9 MB | 124k | 1.0 s |
  | 3.9 MB | 249k | 4.9 s |

  Linear to about 1 MB, then worse than linear. The largest chapter in the real
  library is **123 KB and 37 ms**, so nothing here stalls; a phone is several
  times slower, which puts the first felt pause somewhere near 500 KB in a
  single spine item. The case that needs this is the single-file EPUB - a whole
  novel as one HTML document - which the library does not yet contain. Build it
  when one arrives, and size the window from these numbers rather than a guess.
- ~~**Offline sync for the Android client.**~~ **Done**, as `GET` and `POST
  /api/v1/sync`. Client-generated ids, the client's own timestamps, tombstones
  so a deleted mark stays deleted, last-write-wins for marks and a two-rule
  merge for positions. The client that will use it does not exist yet.
