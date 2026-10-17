#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Mirror main and tags to the GitHub read-only mirror.
#
# scm.poniatowski.dev is canonical; GitHub is downstream.
#
# Runs only from CI. MIRROR_URL must carry its own credentials, e.g.
#   https://x-access-token:${TOKEN}@github.com/<user>/<repo>.git
#
# TODO(adam): reconcile with the setup used on your other projects before
# enabling. Decide: mirror tags too, or main only? Deploy key or PAT?
set -euo pipefail

if [[ -z "${MIRROR_URL:-}" ]]; then
	echo "MIRROR_URL not set - skipping mirror"
	exit 0
fi

# Refuse to run anywhere but CI: this rewrites a remote's main branch.
if [[ -z "${CI:-}" ]]; then
	echo "refusing to mirror outside CI" >&2
	exit 1
fi

echo "mirroring main and tags to the GitHub mirror"
git push --prune "$MIRROR_URL" "refs/heads/main:refs/heads/main" "refs/tags/*:refs/tags/*"
