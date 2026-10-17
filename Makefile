.PHONY: help dev run build reproducible image push-image test lint migrate-up migrate-down migrate-reset eval listeval clean db-up db-down generate licenses authors titles tags classifyeval metadata bylines password android-reader-js android-reader-test

BINARY      := bin/lexarii
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
DATABASE_URL ?= postgres://lexarii@localhost:5432/lexarii?sslmode=disable

# Local settings come from .env when there is one.
#
# Sourced by the shell rather than parsed by Go: a dotenv library would be a
# dependency earning its place only in development, and `set -a` gives the same
# result in three lines. Values already in the environment win, so
# `LEXARII_LOG_LEVEL=debug make dev` still does what it looks like it does.
DOTENV := $(wildcard .env)
ifeq ($(DOTENV),.env)
ENVLOAD := set -a; . ./.env; set +a;
else
ENVLOAD :=
endif

# The password of the database docker-compose.yml creates, which lives in .env
# because compose requires it there. It reaches psql, goose and the Go tests as
# PGPASSWORD, which libpq and pgx both read, so none of the URLs in this file
# carries one and nothing puts it on a command line. Read by sourcing .env, as
# ENVLOAD does, so quoting means the same thing in both. A PGPASSWORD already in
# the environment wins; a Postgres that trusts local connections needs neither.
POSTGRES_PASSWORD ?= $(if $(DOTENV),$(shell . ./.env >/dev/null 2>&1; printf '%s' "$$POSTGRES_PASSWORD"))
ifneq ($(strip $(POSTGRES_PASSWORD)),)
PGPASSWORD ?= $(POSTGRES_PASSWORD)
export PGPASSWORD
endif

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

db-up: ## start postgres
	docker compose up -d postgres
	@until docker compose exec -T postgres pg_isready -U lexarii >/dev/null 2>&1; do sleep 0.5; done
	@echo "postgres ready"

db-down: ## stop postgres
	docker compose down

# No migrate-up here any more: the server applies the schema itself at
# startup, and this target was fetching goose from the network on every run to
# do work that was about to be done again a second later.
# The server is supervised by scripts/watch, which rebuilds it when Go source
# changes. It used to be a plain `go run`, which builds ONCE: the server then
# served the same binary for the rest of the session while the tree moved on
# underneath it. Nothing said so, and the failure is silent and convincing -
# the code is right, the test passes, the endpoint answers the old way, and
# the obvious conclusion is that the code is wrong. It cost three wrong
# diagnoses in one afternoon, one of them a path declared safe that was
# actually serving a whole library to anybody who asked.
dev: db-up ## run server + client, both reloading on change
	@echo "server :8080 · client :5173 · both reload on save"
	@(cd web && npm run dev) & $(ENVLOAD) go run ./scripts/watch ./cmd/lexarii

run: ## run the built binary with .env loaded
	@$(ENVLOAD) $(BINARY)

build: ## build client, embed it, produce static binary
	cd web && npm ci && npm run build
	rm -rf internal/http/static && cp -r web/build internal/http/static
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/lexarii
	@echo "built $(BINARY) $(VERSION)"
# The binary above already has the real client embedded in it. What is left on
# disk is build output sitting on top of a TRACKED file: go:embed refuses an
# empty directory, so internal/http/static/index.html is committed as a
# placeholder, and every build overwrote it and left the tree dirty. That is how
# a built index.html got committed once, which serves a broken page to anyone
# who clones and runs a plain `go build`.
#
# This was left alone once on the grounds that restoring would silently revert
# somebody who had edited the placeholder on purpose. It would not: the `rm -rf`
# two lines up has already deleted their edit by this point, so the choice here
# is between leaving build output in its place or putting the committed file
# back. Restoring is the one that loses nothing.
	@git checkout -- internal/http/static/index.html 2>/dev/null || true

# Prove the build is reproducible, rather than claiming it.
#
# F-Droid rebuilds a tagged commit and compares what comes out against what was
# published; a difference means the app is not accepted. That is a property
# somebody has to be able to CHECK, so this checks it: build everything twice
# and compare the bytes.
#
# It caught the one thing that was wrong. SvelteKit names each build after
# `Date.now()` by default and bakes the name into the generated modules, so two
# builds of identical source shared not one output file - every chunk renamed,
# every hash different. The name is the commit now (web/svelte.config.js).
reproducible: ## build twice and prove the two are identical
	@echo "building the client, twice"
	@cd web && npm run build >/dev/null 2>&1
	@cd web && find build -type f | sort | xargs sha256sum | sha256sum | cut -d' ' -f1 > /tmp/lexarii-client-1
	@cd web && npm run build >/dev/null 2>&1
	@cd web && find build -type f | sort | xargs sha256sum | sha256sum | cut -d' ' -f1 > /tmp/lexarii-client-2
	@cmp -s /tmp/lexarii-client-1 /tmp/lexarii-client-2 		|| (echo "the client is NOT reproducible:"; cat /tmp/lexarii-client-1 /tmp/lexarii-client-2; exit 1)
	@echo "client: $$(cat /tmp/lexarii-client-1)"
	@echo "building the binary, twice"
	@cp -r web/build/. internal/http/static/
	@CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o /tmp/lexarii-bin-1 ./cmd/lexarii
	@CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o /tmp/lexarii-bin-2 ./cmd/lexarii
	@git checkout -- internal/http/static/index.html 2>/dev/null || true
	@cmp -s /tmp/lexarii-bin-1 /tmp/lexarii-bin-2 \
		|| (echo "the binary is NOT reproducible"; sha256sum /tmp/lexarii-bin-1 /tmp/lexarii-bin-2; exit 1)
	@echo "binary: $$(sha256sum /tmp/lexarii-bin-1 | cut -d' ' -f1)"
	@echo "identical both times"

# Tests that touch the database get their OWN database.
#
# The store tests truncate shared tables in setup, so pointing them at the
# development database silently destroys whatever library is sitting in it -
# which is exactly what happened once. A separate database costs nothing and
# removes the whole class of accident.
TEST_DB ?= lexarii_test
TEST_DATABASE_URL ?= postgres://lexarii@localhost:5432/$(TEST_DB)?sslmode=disable

# ADMIN_DATABASE_URL points at the server, not a specific database, so this
# works whether Postgres runs under docker compose, as a plain container, or on
# the host. Assuming `docker compose exec` breaks the moment it does not.
ADMIN_DATABASE_URL ?= postgres://lexarii@localhost:5432/postgres?sslmode=disable

test-db: ## create the throwaway test database and migrate it
	@psql "$(ADMIN_DATABASE_URL)" -tAc "SELECT 1 FROM pg_database WHERE datname='$(TEST_DB)'" | grep -q 1 || \
		psql "$(ADMIN_DATABASE_URL)" -q -c 'CREATE DATABASE $(TEST_DB)'
	@go run github.com/pressly/goose/v3/cmd/goose@latest -dir migrations postgres "$(TEST_DATABASE_URL)" up >/dev/null
	@echo "test database $(TEST_DB) ready"

# The Android reader page runs here too: about seven seconds with a browser,
# and a one-line SKIP in well under one without - see android-reader-test.
test: test-db ## go tests with race detector + frontend typecheck and unit tests + the Android reader page
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race ./...
	cd web && npm run check && npm test
	@$(MAKE) --no-print-directory android-reader-test

lint: ## format check, vet, linters
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "gofmt failures above"; exit 1)
	go vet ./...
	@# The spec is generated from the Go types, so a stale spec means the
	@# committed TypeScript describes a contract the server does not have.
	@go run ./scripts/apispec -check
	@# And the Android client is generated from the same types, so the same
	@# applies to it - with the difference that nobody notices a stale Kotlin
	@# type until it is on somebody's phone.
	@go run ./scripts/apikotlin -check
	@# The Android reader runs the web reader's locator code, compiled. A stale
	@# copy computes element paths the web and the indexer no longer agree
	@# with, and every position it saves lands somewhere else on the other side.
	@tmp=$$(mktemp) && ($(ANDROID_READER_JS) --outfile=$$tmp --log-level=warning) && \
		cmp -s $$tmp android/app/src/main/assets/reader/locator.js; \
		status=$$?; rm -f $$tmp; \
		test $$status -eq 0 || (echo "android/.../assets/reader/locator.js is stale: make android-reader-js"; exit 1)
	@# `cmd && lint || echo` reads as "skip when it is missing" and does not mean
	@# that: the || also catches a lint run that FOUND something, so every real
	@# failure printed "not installed" and exited 0. `make lint` reported success
	@# while thirteen errcheck findings and a staticcheck one stood.
	@if command -v golangci-lint >/dev/null; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed, skipped"; \
	fi
	@# A var() naming a token nobody defines is silent in the browser: the
	@# declaration is dropped and the property keeps its initial value.
	cd web && npm run --silent check:tokens
	@# CI's frontend job runs Prettier; checking here means a formatting slip
	@# fails on the laptop instead of in CI.
	cd web && npx prettier --check . >/dev/null || (cd web && npx prettier --check . 2>&1 | grep warn; echo "prettier: run 'cd web && npx prettier --write .'"; exit 1)

migrate-up: ## apply migrations
	go run github.com/pressly/goose/v3/cmd/goose@latest -dir migrations postgres "$(DATABASE_URL)" up

migrate-down: ## roll back one migration
	go run github.com/pressly/goose/v3/cmd/goose@latest -dir migrations postgres "$(DATABASE_URL)" down

migrate-reset: ## verify every migration round-trips: up, down, up
	$(MAKE) migrate-up && $(MAKE) migrate-down && $(MAKE) migrate-up

api-spec: ## regenerate api/openapi.yaml from api/paths.yaml + the Go wire types
	go run ./scripts/apispec

api-types: ## regenerate the TypeScript types from the OpenAPI spec
	cd web && npx openapi-typescript ../api/openapi.yaml -o src/lib/api/types.gen.ts

api-kotlin: ## regenerate the Android wire types from the Go wire types
	go run ./scripts/apikotlin

# One implementation of element paths, not two. The Android reader is a WebView
# running this exact module, so a position saved on the phone resolves to the
# same paragraph in the browser and in the indexer's chunks.
ANDROID_READER_JS = cd web && node_modules/.bin/esbuild src/lib/reader/locator.ts \
	--bundle --format=iife --global-name=Locator --target=chrome80 --charset=utf8 \
	--banner:js='/* SPDX-License-Identifier: AGPL-3.0-or-later - GENERATED from web/src/lib/reader/locator.ts by make android-reader-js. Do not edit. */'

android-reader-js: ## compile the web reader's locator code into the Android reader
	$(ANDROID_READER_JS) --outfile=../android/app/src/main/assets/reader/locator.js

# The reader page inside the Android app, driven headless in Chromium - the
# engine the WebView is. Needs no SDK and no device, only a browser. With no
# browser it prints SKIP and exits 0, unlike the SDK targets below: it runs
# inside `make test`, and a missing browser is a machine not set up for it,
# not a broken tree. `npx playwright-core install --only-shell chromium` in
# web/ fetches one; READER_TEST_CHROMIUM=/path/to/chrome uses any other.
# FIREFOX=1 adds Firefox; REQUIRE=1 makes a missing browser a failure (CI).
android-reader-test: ## drive the Android reader page in headless Chromium (skips without a browser)
	cd web && READER_TEST_FIREFOX="$(FIREFOX)" READER_TEST_REQUIRE="$(REQUIRE)" \
		npm run --silent test:android-reader

# The Android client needs an SDK and a JDK, and neither is a Go or npm
# dependency, so these targets say what is missing rather than failing with a
# stack trace from Gradle. ANDROID_HOME is where `sdkmanager` put it.
ANDROID_HOME ?= $(HOME)/Android/Sdk

android-build: ## build the Android client (needs ANDROID_HOME and a JDK 17+)
	@test -d "$(ANDROID_HOME)" || (echo "no Android SDK at $(ANDROID_HOME); set ANDROID_HOME"; exit 1)
	cd android && ANDROID_HOME="$(ANDROID_HOME)" ./gradlew assembleDebug

android-test: ## unit-test the Android client on the JVM, no device needed
	@test -d "$(ANDROID_HOME)" || (echo "no Android SDK at $(ANDROID_HOME); set ANDROID_HOME"; exit 1)
	cd android && ANDROID_HOME="$(ANDROID_HOME)" ./gradlew testDebugUnitTest

android-release: ## build the unsigned release APK (what F-Droid builds)
	@test -d "$(ANDROID_HOME)" || (echo "no Android SDK at $(ANDROID_HOME); set ANDROID_HOME"; exit 1)
	cd android && ANDROID_HOME="$(ANDROID_HOME)" ./gradlew :app:assembleRelease
	@echo "built android/app/build/outputs/apk/release/app-release-unsigned.apk"

# Prove the release APK is reproducible, the same way `make reproducible` does
# for the server: build it twice, from two SEPARATE directories rather than
# twice in place, and compare the bytes. Two directories is what actually
# tests for the failure mode that matters here - an absolute build path or a
# stray git-derived value leaking into the output - which building twice in
# the same directory would never catch, since both runs would share the one
# path anyway.
#
# rsync excludes .gradle/ and build/: copying build output would let a stale
# artifact satisfy an UP-TO-DATE check instead of the copy actually compiling,
# which would prove nothing.
ANDROID_REPRO_1 := /tmp/lexarii-android-repro-1
ANDROID_REPRO_2 := /tmp/lexarii-android-repro-2
android-reproducible: ## build the release APK twice, from separate directories, and compare
	@test -d "$(ANDROID_HOME)" || (echo "no Android SDK at $(ANDROID_HOME); set ANDROID_HOME"; exit 1)
	@rm -rf $(ANDROID_REPRO_1) $(ANDROID_REPRO_2)
	@mkdir -p $(ANDROID_REPRO_1) $(ANDROID_REPRO_2)
	@rsync -a --exclude='.gradle' --exclude='build' --exclude='app/build' android/ $(ANDROID_REPRO_1)/
	@rsync -a --exclude='.gradle' --exclude='build' --exclude='app/build' android/ $(ANDROID_REPRO_2)/
	@echo "building the release APK, copy 1"
	@cd $(ANDROID_REPRO_1) && ANDROID_HOME="$(ANDROID_HOME)" ./gradlew :app:assembleRelease
	@echo "building the release APK, copy 2"
	@cd $(ANDROID_REPRO_2) && ANDROID_HOME="$(ANDROID_HOME)" ./gradlew :app:assembleRelease
	@apk1=$(ANDROID_REPRO_1)/app/build/outputs/apk/release/app-release-unsigned.apk; \
	apk2=$(ANDROID_REPRO_2)/app/build/outputs/apk/release/app-release-unsigned.apk; \
	sum1=$$(sha256sum "$$apk1" | cut -d' ' -f1); \
	sum2=$$(sha256sum "$$apk2" | cut -d' ' -f1); \
	if [ "$$sum1" = "$$sum2" ]; then \
		echo "identical both times: $$sum1"; \
	else \
		echo "the release APK is NOT reproducible:"; \
		echo "  copy 1: $$sum1"; \
		echo "  copy 2: $$sum2"; \
		if command -v zipinfo >/dev/null; then \
			echo "--- entries only one copy has ---"; \
			diff <(zipinfo -1 "$$apk1" | sort) <(zipinfo -1 "$$apk2" | sort) || true; \
		else \
			echo "(zipinfo not installed - skipping entry-list diff)"; \
		fi; \
		if command -v diffoscope >/dev/null; then \
			diffoscope "$$apk1" "$$apk2" || true; \
		else \
			echo "(diffoscope not installed - skipping a full diff)"; \
		fi; \
		if command -v apksigcopier >/dev/null; then \
			echo "(apksigcopier available, but both copies are unsigned - nothing to compare-copy)"; \
		fi; \
		exit 1; \
	fi

# Regenerate the fastlane icon from the real launcher icon (adaptive-icon
# background colour + foreground vector), rather than hand-drawing a second
# one that can drift from what actually ships. rsvg-convert's SVG rasteriser
# writes no timestamp or generator metadata, and Pillow's re-save strips
# whatever ancillary PNG chunks it did write, so the same source produces the
# same bytes every time - checked, not assumed: see docs/android-release.md.
android-fastlane-icon: ## regenerate fastlane/metadata/android/en-US/images/icon.png
	@command -v rsvg-convert >/dev/null || (echo "rsvg-convert not installed"; exit 1)
	@python3 -c "import PIL" 2>/dev/null || (echo "python3 Pillow not installed"; exit 1)
	@bg=$$(grep -oE '#[0-9A-Fa-f]{6}' android/app/src/main/res/values/colors.xml | head -1); \
	fg=$$(grep -oE 'android:pathData="[^"]+"' android/app/src/main/res/drawable/ic_launcher_foreground.xml | sed -E 's/android:pathData="([^"]+)"/\1/'); \
	tmp=$$(mktemp --suffix=.svg); \
	printf '<svg xmlns="http://www.w3.org/2000/svg" width="108" height="108" viewBox="0 0 108 108"><rect width="108" height="108" fill="%s"/><path fill="#FFFFFF" d="%s"/></svg>' "$$bg" "$$fg" > "$$tmp"; \
	mkdir -p android/fastlane/metadata/android/en-US/images; \
	rsvg-convert -w 512 -h 512 "$$tmp" -o /tmp/lexarii-icon-raw.png; \
	python3 -c "from PIL import Image; Image.open('/tmp/lexarii-icon-raw.png').convert('RGBA').save('android/fastlane/metadata/android/en-US/images/icon.png')"; \
	rm -f "$$tmp" /tmp/lexarii-icon-raw.png; \
	echo "wrote android/fastlane/metadata/android/en-US/images/icon.png"

# One target, because running the first without the second leaves the client
# describing a contract the server no longer has.
generate: api-spec api-types api-kotlin ## regenerate the whole API contract

licenses: ## audit dependency licences for AGPL compatibility
	@command -v go-licenses >/dev/null || go install github.com/google/go-licenses@latest
	@# Not silenced and not forced to succeed. This target exists to protect an
	@# invariant - every dependency has to be AGPL-compatible - and a check that
	@# cannot fail protects nothing: with stderr discarded and `|| true`, a run
	@# that resolved no packages at all printed nothing and looked like a pass.
	@# go-licenses warns on stderr about assembly files it cannot follow, which
	@# is noise worth reading past rather than hiding.
	go-licenses report ./...
	@# --excludePrivatePackages: license-checker reports any package marked
	@# private as UNLICENSED, and lexarii-web is one. Left in, the summary
	@# always ends with an UNLICENSED line that is this project itself, which is
	@# precisely the kind of permanent false positive that teaches people to
	@# ignore the output.
	cd web && npx --yes license-checker --summary --excludePrivatePackages
	@echo "review output, then update NOTICE"

eval: ## retrieval quality: Recall@10 and MRR
	@$(ENVLOAD) go run ./scripts/eval -questions eval/questions.jsonl

# A reading list is a different product from an answer and fails differently:
# not "the wrong passage" but "eight books, seven of them padding". Kept in its
# own file so the retrieval baseline stays comparable across changes that touch
# only one of them.
# --network=host, which is not a preference.
#
# The default bridge fails on this machine - "failed to add the host <=>
# sandbox pair interfaces: operation not supported" - at the first step that
# needs the network, which is `npm ci`. That is why the image had never been
# built end to end here and the first CI push was the only test. With host
# networking it builds, so the Dockerfile can be checked before pushing rather
# than after.
image: ## build the container image and run the binary out of it
	docker build --network=host --build-arg VERSION=$(VERSION) -t lexarii:local .
	@echo
	@docker run --rm --network=host lexarii:local -version
	@docker run --rm --network=host --entrypoint id lexarii:local
	@docker images lexarii:local --format 'image size {{.Size}}'

# Publish to this server's own registry, which is the same host the repository
# lives on. Tagged with the commit as well as latest, so a machine can be told
# to run one particular build rather than whatever is newest.
REGISTRY_IMAGE ?= scm.poniatowski.dev/adam/lexarii
push-image: ## build and push the image to the Forgejo registry
	docker build --network=host --build-arg VERSION=$(VERSION) \
		-t $(REGISTRY_IMAGE):latest -t $(REGISTRY_IMAGE):$(VERSION) .
	@docker run --rm --network=host $(REGISTRY_IMAGE):$(VERSION) -version
	docker push $(REGISTRY_IMAGE):$(VERSION)
	docker push $(REGISTRY_IMAGE):latest
	@echo "pushed $(REGISTRY_IMAGE):$(VERSION)"

listeval: ## reading-list quality: how much of the right shelf comes back
	@$(ENVLOAD) go run ./scripts/eval -questions eval/readinglists.jsonl -k 8

# The maintenance commands, which are Go packages rather than shell scripts:
# `scripts/authors -prune` is a directory and does not execute. They also need
# DATABASE_URL, which .env has and a bare `go run` does not read - both of
# which are worth one line of Makefile rather than a paragraph of README.
#
# Every one of these reports by default and changes nothing. Pass the flag to
# act: `make authors ARGS=-prune`.
authors: ## report author rows that are one person twice, or not a person (ARGS=-merge|-prune)
	@$(ENVLOAD) go run ./scripts/authors $(ARGS)

titles: ## report title suggestions nobody could accept (ARGS=-reject)
	@$(ENVLOAD) go run ./scripts/titles $(ARGS)

tags: ## report tag rows that are one subject spelled several ways (ARGS=-merge)
	@$(ENVLOAD) go run ./scripts/tags $(ARGS)

password: ## set a local account's password (ARGS='-user adam')
	@$(ENVLOAD) go run ./scripts/password $(ARGS)

bylines: ## report books with no author; ARGS=-apply reads the byline off the title page
	@$(ENVLOAD) go run ./scripts/bylines $(ARGS)

metadata: ## report books with no publisher/year/ISBN; ARGS=-apply asks a catalogue
	@$(ENVLOAD) go run ./scripts/metadata $(ARGS)

classifyeval: ## run two classifier prompts over the same books (ARGS='-a v2 -b v3')
	@$(ENVLOAD) go run ./scripts/classifyeval $(ARGS)

clean:
	rm -rf bin dist internal/http/static web/build web/.svelte-kit
