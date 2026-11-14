#!/usr/bin/env bash
# Publishes the branch Vercel deploys from: the current commit plus a bundle
# of the newest sessions (segments are not kept in the main history).
#
#   deploy/vercel/publish.sh            # bundle ./data, push branch "vercel-deploy"
#   MAX_MB=200 DATA=data BRANCH=vercel-deploy deploy/vercel/publish.sh
#
# The branch is a single commit on top of HEAD and is force-pushed each time,
# so old bundles do not accumulate. A Vercel Function may be at most 250 MB
# uncompressed, which is why the bundle is capped (default 200 MiB).
set -euo pipefail
cd "$(dirname "$0")/../.."
DATA=${DATA:-data}
BRANCH=${BRANCH:-vercel-deploy}
MAX_MB=${MAX_MB:-200}

if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "publish: commit or stash your changes first" >&2; exit 1
fi
TMP=$(mktemp -d)
trap 'git worktree remove --force "$TMP/tree" 2>/dev/null || true; rm -rf "$TMP"' EXIT
git worktree add --quiet --detach "$TMP/tree" HEAD
go run ./cmd/bundle -data "$DATA" -out "$TMP/tree/data" -max-mb "$MAX_MB"
git -C "$TMP/tree" add --force data
git -C "$TMP/tree" commit --quiet -m "Data bundle for Vercel ($(date -u +%Y-%m-%d), from $(git rev-parse --short HEAD))"
git push --quiet --force origin "$(git -C "$TMP/tree" rev-parse HEAD):refs/heads/$BRANCH"
echo "pushed $BRANCH at $(git -C "$TMP/tree" rev-parse --short HEAD)"
