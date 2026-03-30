#!/usr/bin/env bash
# Cursor stop hook: commit and push when the agent finishes, if the repo has changes.
# Reads hook JSON from stdin; writes JSON to stdout (required by Cursor).
set -euo pipefail
cat >/dev/null

ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "${ROOT}" ]]; then
  printf '%s\n' '{}'
  exit 0
fi
cd "$ROOT"

if git diff --quiet && git diff --cached --quiet; then
  printf '%s\n' '{}'
  exit 0
fi

git add -A
if ! git commit -m "chore: checkpoint after agent task" 2>/dev/null; then
  printf '%s\n' '{}'
  exit 0
fi

git push 2>/dev/null || true

printf '%s\n' '{}'
exit 0
