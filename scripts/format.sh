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

# The formatter's stdout goes to a FILE and run_formatter RETURNS the status;
# it never `exit`s and is never called inside `$( )`. That shape is deliberate and
# it is load-bearing (Bugbot High + @LukasWodka on #550): a function that `exit`s
# from a command substitution ends only the SUBSHELL, so the caller reads an empty
# capture, finds no drift, and prints "clean" — exit 0 on a formatter that never
# ran. Which is precisely the inert-verification failure (backend#1729) this file
# was written to prevent, so it must not be reintroduced here.
#
# scripts/tests/format-verify.sh asserts the propagation with a formatter stubbed
# to fail; keep that harness green rather than trusting this comment.
out_file=""
cleanup() { [ -n "$out_file" ] && rm -f "$out_file"; }
trap cleanup EXIT

out_file="$(mktemp "${TMPDIR:-/tmp}/format-sh.XXXXXX")" || {
  echo "format.sh: mktemp failed — refusing to report clean" >&2
  exit 2
}

# xargs, not a bare expansion: the list grows with the repo and a single argv has
# a hard size limit. printf is a builtin, so building the NUL stream is not itself
# subject to that limit. The list is non-empty (guarded above), so the BSD-vs-GNU
# "run once with no arguments" difference cannot bite.
#
# stderr is deliberately NOT redirected. `go run` writes module-download progress
# there, and folding it into the captured stdout would turn a cold cache into
# phantom "drift" filenames. Diagnostics go straight to the terminal instead.
run_formatter() {  # run_formatter <label> <cmd...>  -> 0 ok, 2 the tool failed
  local label="$1"; shift
  local rc
  printf '%s\0' "${files[@]}" | xargs -0 "$@" > "$out_file"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "==> ${label}: FAILED (exit ${rc}) — see the output above" >&2
    return 2
  fi
  return 0
}

goimports_cmd=(
  "$GO" run "golang.org/x/tools/cmd/goimports@${GOIMPORTS_VERSION}"
  -local "$LOCAL_PREFIX"
)

if [ "$mode" = "write" ]; then
  run_formatter "gofmt -s -w" gofmt -s -w || exit 2
  run_formatter "goimports -w" "${goimports_cmd[@]}" -w || exit 2
  echo "==> fmt: ${#files[@]} tracked Go file(s) formatted"
  exit 0
fi

fail=0

report() {  # report <heading> — print the drift file, if any, and set fail
  if [ -s "$out_file" ]; then
    echo "==> $1"
    sed 's/^/    /' "$out_file"
    fail=1
  fi
}

run_formatter "gofmt -s" gofmt -s -l || exit 2
report "gofmt -s needed on:"

# goimports -local: the stdlib / third-party / our-own import grouping that
# .golangci.yml's local-prefixes already declares. gofmt does not check grouping.
run_formatter "goimports" "${goimports_cmd[@]}" -l || exit 2
report "goimports (import grouping) needed on:"

if [ "$fail" -ne 0 ]; then
  echo "==> run \`make fmt\` to fix"
  exit 1
fi

echo "==> fmt-check: ${#files[@]} tracked Go file(s) clean"
