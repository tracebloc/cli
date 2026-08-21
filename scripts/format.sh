#!/usr/bin/env bash
# =============================================================================
#  format.sh — the gofmt -s + goimports gate, scoped to TRACKED files (cli#549)
#
#  Both formatters used to run over `.`, which is the whole working TREE, not the
#  repo. `.` includes untracked directories, so any scratch path holding Go files
#  — a nested git worktree, a vendored copy, a build sandbox — was reported as
#  drift while every tracked file was correctly formatted:
#
#      ==> goimports (import grouping) needed on:
#          <untracked-scratch-dir>/internal/cli/data.go
#          ==> run `make fmt` to fix
#
#  CI never saw it (a fresh checkout has no untracked Go files), so it was a
#  local-only FALSE failure — and the remedy it printed, `make fmt`, was the
#  same bug in write mode: it rewrote files the repo does not track.
#
#  The file set is now `git ls-files '*.go'`: exactly what a PR can contain, so
#  local and CI cannot disagree about scope. Both call this script.
#
#  Usage:
#    scripts/format.sh --check   report drift, exit 1 if any   (make fmt-check)
#    scripts/format.sh --write   rewrite in place              (make fmt)
#
#  GO and GOIMPORTS_VERSION come from the environment; the Makefile passes them,
#  keeping the version declared once there (backend#1972, check-tool-pins.sh).
#
#  FAILS CLOSED (exit 2), because each of these would otherwise be reported as a
#  clean pass — the inert-verification class of backend#1729:
#    * not inside a git work tree — no way to know what is tracked
#    * an EMPTY file list — this module has Go files by construction, so "none
#      found" means the query broke, not that the tree is clean. It also matters
#      mechanically: bare `gofmt -l` with no path arguments reads STDIN, so an
#      unguarded empty list checks nothing and exits 0.
#
#  Portable to bash 3.2 (macOS default): no mapfile, no associative arrays.
# =============================================================================
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

GO="${GO:-go}"
GOIMPORTS_VERSION="${GOIMPORTS_VERSION:-v0.48.0}"
LOCAL_PREFIX="github.com/tracebloc/cli"

mode=""
case "${1-}" in
  --check) mode="check" ;;
  --write) mode="write" ;;
  *)
    echo "usage: scripts/format.sh --check | --write" >&2
    exit 2
    ;;
esac

git rev-parse --is-inside-work-tree >/dev/null 2>&1 || {
  echo "format.sh: not inside a git work tree — cannot tell which files are tracked," >&2
  echo "  and formatting the whole directory instead is the bug this replaced." >&2
  exit 2
}

# Tracked .go files that EXIST. `git ls-files` reports index entries, so a
# tracked-then-deleted file is still listed; passing it to gofmt is a hard error
# ("no such file or directory") on a tree that is merely mid-edit.
files=()
while IFS= read -r -d '' f; do
  [ -f "$f" ] && files+=("$f")
done < <(git ls-files -z -- '*.go')

if [ ${#files[@]} -eq 0 ]; then
  echo "format.sh: git ls-files '*.go' matched no existing tracked file." >&2
  echo "  This module has Go files by construction, so that is a broken query," >&2
  echo "  not a clean tree. Refusing to report clean." >&2
  exit 2
fi

# xargs, not a bare expansion: the list grows with the repo and a single argv has
# a hard size limit. printf is a builtin, so building the NUL stream is not itself
# subject to that limit. The list is non-empty (guarded above), so the BSD-vs-GNU
# "run once with no arguments" difference cannot bite.
# stderr is deliberately NOT captured. `go run` writes module-download progress
# there, and folding that into the captured stdout would turn a cold cache into
# phantom "drift" filenames. Diagnostics go straight to the terminal instead.
run_formatter() {  # run_formatter <label> <cmd...>
  local label="$1"; shift
  local out rc
  out="$(printf '%s\0' "${files[@]}" | xargs -0 "$@")"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "==> ${label}: FAILED (exit ${rc}) — see the output above" >&2
    exit 2
  fi
  printf '%s' "$out"
}

if [ "$mode" = "write" ]; then
  run_formatter "gofmt -s -w" gofmt -s -w >/dev/null
  run_formatter "goimports -w" \
    "$GO" run "golang.org/x/tools/cmd/goimports@${GOIMPORTS_VERSION}" \
    -local "$LOCAL_PREFIX" -w >/dev/null
  echo "==> fmt: ${#files[@]} tracked Go file(s) formatted"
  exit 0
fi

fail=0

drift="$(run_formatter "gofmt -s" gofmt -s -l)"
if [ -n "$drift" ]; then
  echo "==> gofmt -s needed on:"
  printf '%s\n' "$drift" | sed 's/^/    /'
  fail=1
fi

# goimports -local: the stdlib / third-party / our-own import grouping that
# .golangci.yml's local-prefixes already declares. gofmt does not check grouping.
drift="$(run_formatter "goimports" \
  "$GO" run "golang.org/x/tools/cmd/goimports@${GOIMPORTS_VERSION}" \
  -local "$LOCAL_PREFIX" -l)"
if [ -n "$drift" ]; then
  echo "==> goimports (import grouping) needed on:"
  printf '%s\n' "$drift" | sed 's/^/    /'
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "==> run \`make fmt\` to fix"
  exit 1
fi

echo "==> fmt-check: ${#files[@]} tracked Go file(s) clean"
