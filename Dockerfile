# SPDX-License-Identifier: AGPL-3.0-or-later

# Lexarii in a container.
#
# The whole install is one static binary with the web client embedded in it, so
# the interesting part of this file is the two build stages that produce that
# binary; the runtime stage is deliberately almost empty.
#
# Multi-arch, and buildable with either builder. `docker buildx build
# --platform linux/amd64,linux/arm64` produces both; a plain `docker build`
# produces one for the host. That works because CGO_ENABLED=0 is a project
# invariant - nothing here links against C, which is also why the model
# connection is HTTP rather than bindings.
#
# The builder stages deliberately do NOT pin themselves to $BUILDPLATFORM.
# Doing so lets BuildKit cross-compile rather than emulate arm64 under qemu,
# which is minutes against tens of minutes - but it is a BuildKit-only
# variable, and the legacy builder fails outright on the empty string it
# expands to there. Correct everywhere beats fast on one builder. If every
# machine that builds this has buildx, adding `--platform=$BUILDPLATFORM` to
# the two stages below is worth it.

# --- The web client -----------------------------------------------------------
#
# The output is JavaScript, which is the same bytes whatever it was built on.
FROM node:24-alpine AS client

WORKDIR /src/web

# Dependencies first, so a change to the app does not re-download the world.
# `npm ci` and not `npm install`: the lockfile is the input, and a build that
# silently resolves a different tree is not reproducible.
COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./

# The name this build gives itself, which SvelteKit bakes into every generated
# module. Left alone it is Date.now(), and two builds of identical source then
# share not one output file - which is the difference between a reproducible
# image and a claim about one. There is no git in this stage, so it is passed
# in; without it the fallback is a constant, which is still deterministic.
ARG VERSION=docker
ENV LEXARII_BUILD_VERSION=${VERSION}
RUN npm run build

# --- The binary ---------------------------------------------------------------
FROM golang:1.27-alpine AS build

WORKDIR /src

# Modules first, for the same reason as above.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The client is embedded with go:embed, so it has to be in place before the Go
# build runs. The repository ships a placeholder index.html here - go:embed
# fails outright on an empty directory - which this replaces.
COPY --from=client /src/web/build/ ./internal/http/static/

# BuildKit sets these per requested platform; the legacy builder leaves them
# empty, which `env` then simply omits, and Go builds for the host. Passed
# through `env` rather than as a command prefix because the shell decides what
# is an assignment before it expands anything, so `${X:+GOOS=$X} go build`
# would look for a command named GOOS=linux.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=docker

RUN CGO_ENABLED=0 env ${TARGETOS:+GOOS=$TARGETOS} ${TARGETARCH:+GOARCH=$TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/lexarii ./cmd/lexarii

# --- Runtime ------------------------------------------------------------------
#
# Alpine rather than scratch. Scratch would be smaller and this binary needs
# nothing from a distribution, but Lexarii runs on somebody's home server and
# the first thing that happens when a container misbehaves there is that its
# owner wants a shell inside it. Eight megabytes is a fair price for being able
# to look.
FROM alpine:3.21

# Certificates for talking to anything over HTTPS - a model behind a reverse
# proxy, and the external metadata lookups that come later. Timezone data so
# timestamps render in the operator's zone rather than UTC.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 -h /data lexarii

COPY --from=build /out/lexarii /usr/local/bin/lexarii

# Derived artifacts: covers, extracted text, uploaded books. Owned by the
# unprivileged user because this is the only path the process writes to.
#
# The LIBRARY directory is deliberately absent. It is mounted read-only by
# compose, and Lexarii never writes to somebody's book files.
RUN mkdir -p /data && chown lexarii:lexarii /data
VOLUME ["/data"]

USER lexarii
WORKDIR /data

ENV LEXARII_DATA_DIR=/data \
    LEXARII_LIBRARY_DIR=/library \
    LEXARII_ADDR=:8080

EXPOSE 8080

# The health endpoint reports each dependency separately and answers 200 while
# degraded, because a library with no model configured is working exactly as
# intended. A 503 here means Postgres is gone.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/api/v1/health >/dev/null || exit 1

ENTRYPOINT ["lexarii"]
