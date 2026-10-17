<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Cutting an Android release

This is the checklist for shipping a build of `android/`, and what F-Droid
specifically needs before it can carry it. See also `android/LICENSES-deps.md`
(dependency licence audit) and `android/fdroid/com.lexarii.app.yml`
(the draft fdroiddata metadata).

## The versioning scheme

`android/app/build.gradle.kts` derives `versionName` from a git tag rather
than from a number somebody typed or a build-time counter, because F-Droid's
auto-update has to recompute both `versionName` and `versionCode` from the
tag alone, without running this build first.

- **Tag shape:** `android-vMAJOR.MINOR.PATCH`, e.g. `android-v0.1.0`. Deliberately
  distinct from the server's own `vMAJOR.MINOR.PATCH` tags (`.forgejo/workflows/release.yml`)
  since the two ship on different schedules - the app does not need to move
  every time the server does, and vice versa.
- **`versionName`** is read from `git describe --tags --exact-match --match 'android-v*'`
  at configure time. Only an *exact* match counts: if HEAD is not precisely on
  an `android-v` tag (a normal commit, a dev build), `versionName` falls back
  to `0.0.1-dev` rather than guessing a real-looking version - so a dev build
  can never be mistaken for a release build in a bug report.
- **`versionCode`** is a pure function of `versionName`:
  `MAJOR * 1_000_000 + MINOR * 1_000 + PATCH`. `0.1.0` → `1000`. Anyone -
  including F-Droid's own tooling, without running Gradle - can recompute it
  from the tag name alone. This is why the scheme uses no commit count (which
  depends on clone depth: a shallow clone and a full clone disagree) and no
  timestamp (which is the one thing a reproducible build can never contain).

To cut a release:

1. Decide the version, e.g. `0.2.0`.
2. Update the changelog: add `android/fastlane/metadata/android/en-US/changelogs/<versionCode>.txt`
   (plain text, what changed, written for someone installing the update - see
   the existing `1000.txt` for the shape). F-Droid displays this on the update
   notification.
3. Commit that changelog (nothing else needs to change - `versionCode` and
   `versionName` are derived, not written by hand).
4. Tag: `git tag android-v0.2.0 && git push origin android-v0.2.0`.
5. `make android-release` builds the unsigned release APK from that tag and
   reports where it landed.
6. `make android-reproducible` builds it twice, from two separate directories,
   and confirms the two are byte-identical. Run this before trusting a tag is
   ready - it is what F-Droid's own rebuild-and-compare will do.

## What F-Droid needs from Adam, beyond what is in this repository

1. **The GitHub mirror needs to be live.** `scripts/mirror.sh` and the
   `mirror` job in `.forgejo/workflows/ci.yml` are written and stubbed out -
   see `docs/TODO.md`, "Blocked on Adam". fdroiddata's build servers clone the
   source anonymously, which needs a public URL; `scm.poniatowski.dev` is
   canonical but is not that URL today.
2. **The fdroiddata merge request itself.** `android/fdroid/com.lexarii.app.yml`
   is a draft, not submitted. Once the mirror is live and an `android-v` tag
   exists: fill in the real `SourceCode`/`IssueTracker`/`Repo` URLs (replacing
   the `PLACEHOLDER-org` marker), set `commit:` to the real tag, run
   `fdroid lint` and `fdroid readmeta` against a real fdroiddata checkout (see
   "Validating the draft" below), and open the merge request at
   <https://gitlab.com/fdroid/fdroiddata>.
3. **Optionally, a developer signing key**, so Adam can verify a release
   himself against what F-Droid publishes - see "Reproducible verification"
   below. Never required: F-Droid signs what it builds regardless.
4. **Real screenshots**, or a decision to ship without them initially -
   `android/fastlane/metadata/android/en-US/images/phoneScreenshots/README.md`
   has the capture procedure and says why none are committed yet (the shared
   emulator was in potential use by other in-flight work when this was
   written).

## Optional developer signing, for reproducible-build verification

F-Droid signs the APK it builds with its own key; this repository's release
build type is unsigned by default and that never changes. What a developer
keystore buys is the ability to sign a release **yourself** and compare the
result against what F-Droid publishes for the same tag - the entire point of
a reproducible build being one you can check rather than one you have to
trust.

To use it: create `android/keystore.properties` (gitignored, never committed)
with:

```properties
storeFile=/absolute/path/to/your.jks
storePassword=...
keyAlias=...
keyPassword=...
```

`android/app/build.gradle.kts` picks it up automatically when present and
signs the `release` build type; absent, `release` stays unsigned, which is
also what F-Droid's own build produces. The two signed outputs will not be
byte-identical to each other (different signatures over the same bytes), but
the *unsigned* content - what `make android-reproducible` compares - will be.

## Reproducible builds: what was checked, and how

`make android-reproducible` builds the release APK twice, from two separate
directory copies (not twice in the same directory - that would never catch an
absolute path or a per-invocation value leaking into the output), and compares
the SHA-256 of the resulting `app-release-unsigned.apk` byte-for-byte. It is
what F-Droid's own rebuild-and-compare does to every release.

Run as part of this work, with real output:

- **The Gradle wrapper is exactly what the official distribution produces.**
  `android/gradle/wrapper/gradle-wrapper.jar` was compared against a jar
  freshly generated by a `gradle wrapper --gradle-version 8.9` run against the
  Gradle 8.9 distribution fetched and checksum-verified against the sha256 already
  pinned in `gradle-wrapper.properties`. Both are
  `498495120a03b9a6ab5d155f5de3c8f0d986a449153702fb80fc80e134484f17` - byte-identical.
  This is a real trust chain a reviewer can re-run, not an assertion.
- **`vcsInfo` is off.** AGP 8.3+ embeds `META-INF/version-control-info.textproto`
  in the APK by default - the git remote, branch, and commit it was built
  from. Confirmed present in a build with the setting untouched, and gone
  after setting `vcsInfo { include = false }` in the release build type
  (`android/app/build.gradle.kts`). It is disabled rather than merely
  documented because two people building the identical tagged commit can
  still disagree here: a shallow clone, a detached HEAD, or a git *worktree*
  instead of a plain checkout (which is how this very change was built and
  tested) all change or blank out what gets written. It is not a function of
  the source tree alone, which is the property that matters.
- **AGP's own dependency-metadata blob is off.** `dependenciesInfo { includeInApk = false; includeInBundle = false }`.
  This is the Google-encrypted blob identifying build provenance for Play's
  Integrity API - proprietary by construction, and an outright F-Droid
  rejection if present regardless of what the app depends on.
- **PNG crunching:** not applicable. Every icon resource in this app
  (`android/app/src/main/res/drawable/ic_launcher_foreground.xml`,
  `mipmap-anydpi-v26/*.xml`) is a vector drawable, not a raster PNG, so
  `isCrunchPngs` (the AGP setting that controls this) has nothing to crunch.
  Confirmed by checking `res/` for `.png` files: there are none.
- **Baseline profiles:** present, and not a reproducibility risk *for the
  APK*. `androidx.profileinstaller` and `androidx.startup` arrive transitively
  through Compose (see `android/LICENSES-deps.md`), and AGP automatically
  merges each dependency's own bundled `baseline-prof.txt` into
  `assets/dexopt/baseline.prof`/`.profm` **inside** the APK. That merge is a
  pure function of the locked dependency versions in
  `android/gradle/libs.versions.toml` (no ranges, ever - see the comment at
  the top of that file), so it is exactly as reproducible as the rest of the
  build, and `make android-reproducible` covers it along with everything
  else. Separately, AGP also writes `.dm` "cloud profile" files
  (`baselineProfiles/*/app-release-unsigned.dm`) **beside** the APK for Play's
  own distribution pipeline; those are not part of the APK and do not affect
  its hash.
- **Zip ordering / entry timestamps:** covered by the byte-for-byte compare
  itself rather than inspected separately - AGP's packaging tools already
  normalise both, and two builds from two different absolute directories
  producing an identical SHA-256 is what proves it held.
- **The fastlane icon is reproducible too.** `make android-fastlane-icon`
  rasterises the same background colour + foreground path the launcher icon
  uses (via `rsvg-convert`, then re-saved through Pillow to strip any PNG
  ancillary chunks a rasteriser might add) and produces the identical SHA-256
  on repeated runs - checked, not assumed.

**Result, on this machine:** `make android-reproducible` reports
`identical both times`, SHA-256
`02908f34c59cd658cccfb52d15aca89a82448f142410a47ad88e094f7d4d917c`, for a
build with no `android-v` tag reachable (so `versionName` fell back to
`0.0.1-dev` identically in both copies). Re-run it after tagging a real
release; the hash will differ (a real version number), but "identical both
times" is the property that matters and does not depend on the tag.

## Validating the draft fdroiddata metadata

`fdroidserver` installs cleanly with `pipx install fdroidserver` (no root
needed) and was used to check the draft:

```
pipx install fdroidserver
mkdir -p fdroiddata-check/metadata
cp android/fdroid/com.lexarii.app.yml fdroiddata-check/metadata/
# fdroid lint wants the real config/ from fdroiddata (categories, antifeatures,
# and their icons) to validate Categories/AntiFeatures against - see
# https://gitlab.com/fdroid/fdroiddata/-/tree/master/config
fdroid readmeta   # parses cleanly
fdroid lint com.lexarii.app   # clean, once config/ is populated
```

Both passed clean (the only output was an unrelated `apksigner not found`
warning - this machine has no Android build-tools `apksigner` installed,
which has nothing to do with the metadata file). Two real mistakes were
caught and fixed by actually running the linter rather than guessing the
schema:

1. `AntiFeatures` is a mapping in this fdroidserver version, not a list -
   `{}` for "none", not `[]`.
2. `Reading` is not a valid F-Droid category for this app; `Ebook Reader`
   ("Read ebooks, EPUB, PDF, and digital books and publications") is, and
   fits Lexarii exactly.

## Remaining blockers, as a checklist

- [ ] GitHub mirror live (`scripts/mirror.sh` filled in, CI `mirror` job
      passing) - blocks the `SourceCode`/`Repo` URL in the fdroiddata draft.
- [ ] First `android-v` tag cut (`android-v0.1.0` or later).
- [ ] `android/fdroid/com.lexarii.app.yml` updated with the real
      repository URL and commit, re-linted against a real fdroiddata
      checkout, and opened as a merge request there.
- [ ] Real screenshots captured and committed to
      `android/fastlane/metadata/android/en-US/images/phoneScreenshots/`
      (currently just a README explaining how, and why not yet).
- [ ] Decide whether Adam wants a developer signing key for reproducible-build
      verification (optional - see above).
- [ ] PDFs and a typography settings screen in the Android client - not an
      F-Droid blocker, but worth having before the first submission, per
      `docs/ROADMAP.md` M7.
