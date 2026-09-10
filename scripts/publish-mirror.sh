#!/usr/bin/env bash
# =============================================================================
#  publish-mirror.sh — the publish half of the mirror pipeline: push what
#  scripts/publish-guard.sh staged and cleared to the public mirror repository.
#
#  Three subcommands, each one step of the workflow, each refusing on its own:
#
#    target   --mirror NAME --source-repo OWNER/REPO [--owner OWNER]
#             [--output FILE]
#             Validate the mirror name and print OWNER/NAME. Refuses an empty
#             name (the mirror is unset until it exists — there is no default),
#             a name with characters GitHub does not allow, and a target equal
#             to the source repository: publishing onto the source would
#             replace the default branch of the repo you are standing in.
#             --output appends `repo=OWNER/NAME` and `name=NAME` to FILE (the
#             workflow passes $GITHUB_OUTPUT).
#
#    tree     --stage DIR --repo OWNER/NAME --branch NAME --message TEXT
#             [--remote URL] [--output FILE]
#             Clone the mirror branch (or start it when the mirror has none),
#             replace its content with DIR, commit, PLAIN push. A diverged
#             remote rejects the push; nothing here ever forces. Prints
#             `pushed <sha>` or `unchanged <sha>`; --output appends
#             `result=pushed|unchanged` and `sha=<sha>` to FILE.
#
#  Results go to --output, refusals go to stdout: a caller that captured stdout
#  with `$(...)` to read the result would swallow the `::error::` line of a
#  refusal, so the workflow runs these commands directly and reads the file.
#  Nothing is written to --output on a refusal.
#
#    release  --tag TAG --repo OWNER/NAME --target SHA --assets DIR
#             --notes FILE [--prerelease]
#             Create TAG on the mirror at SHA with every file in DIR attached.
#             Refuses when TAG already exists on the mirror: a published release
#             is never overwritten, and a re-run of a mirrored release is a
#             human decision.
#
#  Exit 0 done; 1 refused (the message says why); 2 could not tell (an input
#  missing or unreadable, a remote that did not answer). "Cannot tell" never
#  publishes.
#
#  Authentication is the caller's: git reads its credential helper, `gh` reads
#  GH_TOKEN. Nothing here takes a token argument, so no token can land on a
#  command line. Commits are authored as PUBLISH_MIRROR_GIT_NAME /
#  PUBLISH_MIRROR_GIT_EMAIL (default: github-actions[bot]).
# =============================================================================
set -uo pipefail

die1() { echo "::error::publish-mirror: REFUSED — $1"; exit 1; }
die2() { echo "::error::publish-mirror: COULD NOT TELL — $1 (never publishes)"; exit 2; }

REPO_RE='^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$'
NAME_RE='^[A-Za-z0-9_.-]+$'

# emit_output FILE KEY=VALUE... — append results for the caller (the workflow's
# $GITHUB_OUTPUT). An unwritable file is "could not tell": a result the caller
# never receives is a publish it cannot finish or account for.
emit_output() {
  local file="$1"; shift
  [ -n "$file" ] || return 0
  printf '%s\n' "$@" >>"$file" || die2 "could not write results to '$file'"
}

cmd_target() {
  local mirror="" source_repo="" owner="" output=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --mirror)      mirror="${2:-}"; shift 2 ;;
      --source-repo) source_repo="${2:-}"; shift 2 ;;
      --owner)       owner="${2:-}"; shift 2 ;;
      --output)      output="${2:-}"; shift 2 ;;
      *) die2 "target: unknown argument '$1'" ;;
    esac
  done
  [ -n "$source_repo" ] || die2 "target: --source-repo is required"
  [[ "$source_repo" =~ $REPO_RE ]] || die2 "target: --source-repo '$source_repo' is not OWNER/REPO"
  [ -n "$owner" ] || owner="${source_repo%%/*}"
  [ -n "$mirror" ] || die1 "no mirror repository is configured (MIRROR_REPO is unset) — the mirror has no default, so nothing is published until one is named"
  case "$mirror" in */*) die1 "mirror name '$mirror' must be a bare repository name in the '$owner' organisation, not OWNER/NAME" ;; esac
  [[ "$mirror" =~ $NAME_RE ]] || die1 "mirror name '$mirror' contains characters a repository name cannot"
  local full="$owner/$mirror"
  if [ "$(printf '%s' "$full" | tr '[:upper:]' '[:lower:]')" = "$(printf '%s' "$source_repo" | tr '[:upper:]' '[:lower:]')" ]; then
    die1 "mirror '$full' is this repository — publishing onto the source would replace its default branch"
  fi
  emit_output "$output" "repo=$full" "name=$mirror"
  printf '%s\n' "$full"
}

cmd_tree() {
  local stage="" repo="" branch="" message="" remote="" output=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --stage)   stage="${2:-}"; shift 2 ;;
      --repo)    repo="${2:-}"; shift 2 ;;
      --branch)  branch="${2:-}"; shift 2 ;;
      --message) message="${2:-}"; shift 2 ;;
      --remote)  remote="${2:-}"; shift 2 ;;
      --output)  output="${2:-}"; shift 2 ;;
      *) die2 "tree: unknown argument '$1'" ;;
    esac
  done
  [ -n "$stage" ] && [ -n "$repo" ] && [ -n "$branch" ] && [ -n "$message" ] || die2 "tree: --stage, --repo, --branch and --message are all required"
  [[ "$repo" =~ $REPO_RE ]] || die2 "tree: --repo '$repo' is not OWNER/NAME"
  [[ "$branch" =~ ^[A-Za-z0-9_./-]+$ ]] || die2 "tree: --branch '$branch' is not a branch name"
  [ -d "$stage" ] || die2 "tree: stage '$stage' is not a directory"
  [ -n "$(find "$stage" -type f | head -1)" ] || die2 "tree: stage '$stage' holds no files — an empty deliverable is not published"
  [ ! -e "$stage/.git" ] || die2 "tree: stage '$stage' contains a .git entry — that is a checkout, not a staged deliverable"
  [ -n "$remote" ] || remote="https://github.com/$repo.git"

  local name="${PUBLISH_MIRROR_GIT_NAME:-github-actions[bot]}"
  local email="${PUBLISH_MIRROR_GIT_EMAIL:-github-actions[bot]@users.noreply.github.com}"
  # The checkout lives in its own subdirectory of the scratch dir; error
  # captures live BESIDE it, never inside it, or they would be committed.
  local scratch work
  scratch="$(mktemp -d "${TMPDIR:-/tmp}/publish-mirror.XXXXXX")" && [ -d "$scratch" ] || die2 "tree: could not create a scratch directory"
  trap 'rm -rf "$scratch"' EXIT
  work="$scratch/work"
  mkdir -p "$work" || die2 "tree: could not create the checkout directory"

  git -C "$work" init -q || die2 "tree: git init failed"
  git -C "$work" remote add origin "$remote" || die2 "tree: could not add remote"
  # Absent-vs-unreachable are different answers: ls-remote's own status says
  # whether the remote answered; an empty answer says the branch is not there.
  local heads rc existed=0
  heads="$(git -C "$work" ls-remote --heads origin "refs/heads/$branch" 2>"$scratch/lsr.err")"; rc=$?
  [ "$rc" -eq 0 ] || die2 "tree: the mirror remote did not answer (git ls-remote exited $rc: $(tr '\n' ' ' <"$scratch/lsr.err"))"
  if [ -n "$heads" ]; then
    existed=1
    git -C "$work" fetch -q --depth 1 origin "refs/heads/$branch" || die2 "tree: could not fetch '$branch' from the mirror"
    git -C "$work" checkout -q -B "$branch" FETCH_HEAD || die2 "tree: could not check out '$branch'"
    find "$work" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} + || die2 "tree: could not clear the checkout"
  else
    git -C "$work" checkout -q --orphan "$branch" || die2 "tree: could not start branch '$branch'"
  fi
  cp -Rp "$stage"/. "$work"/ || die2 "tree: could not copy the stage into the checkout"
  git -C "$work" add -A || die2 "tree: git add failed"
  local sha
  if [ "$existed" -eq 1 ] && git -C "$work" diff --cached --quiet; then
    sha="$(git -C "$work" rev-parse HEAD)"
    emit_output "$output" "result=unchanged" "sha=$sha"
    echo "unchanged $sha"
    return 0
  fi
  git -C "$work" -c user.name="$name" -c user.email="$email" commit -q -m "$message" || die2 "tree: git commit failed"
  # A PLAIN push. If the mirror moved underneath us the push is rejected and
  # this exits 2; the answer is to re-run, never to force.
  git -C "$work" push -q origin "HEAD:refs/heads/$branch" 2>"$scratch/push.err" || die2 "tree: push to '$repo' '$branch' was rejected: $(tr '\n' ' ' <"$scratch/push.err")"
  sha="$(git -C "$work" rev-parse HEAD)"
  emit_output "$output" "result=pushed" "sha=$sha"
  echo "pushed $sha"
}

cmd_release() {
  local tag="" repo="" target="" assets="" notes="" prerelease=0
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --tag)        tag="${2:-}"; shift 2 ;;
      --repo)       repo="${2:-}"; shift 2 ;;
      --target)     target="${2:-}"; shift 2 ;;
      --assets)     assets="${2:-}"; shift 2 ;;
      --notes)      notes="${2:-}"; shift 2 ;;
      --prerelease) prerelease=1; shift ;;
      *) die2 "release: unknown argument '$1'" ;;
    esac
  done
  [ -n "$tag" ] && [ -n "$repo" ] && [ -n "$target" ] && [ -n "$assets" ] && [ -n "$notes" ] || die2 "release: --tag, --repo, --target, --assets and --notes are all required"
  [[ "$repo" =~ $REPO_RE ]] || die2 "release: --repo '$repo' is not OWNER/NAME"
  [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || die1 "release: '$tag' is not a release tag (vX.Y.Z or vX.Y.Z-<pre>)"
  [[ "$target" =~ ^[0-9a-f]{40}$ ]] || die2 "release: --target '$target' is not a full commit sha"
  [ -d "$assets" ] || die2 "release: assets '$assets' is not a directory"
  [ -s "$notes" ] || die2 "release: notes file '$notes' is missing or empty"
  local -a files=()
  while IFS= read -r f; do files+=("$f"); done < <(find "$assets" -mindepth 1 -maxdepth 1 -type f | sort)
  [ "${#files[@]}" -gt 0 ] || die2 "release: '$assets' holds no files — a release with no assets is not what a customer downloads"
  command -v gh >/dev/null 2>&1 || die2 "release: gh is not on PATH"

  # Existing release → refuse. `gh release view` exits 1 for "not found" AND for
  # auth or network failure, so the text decides which it was; anything that is
  # not a clear "not found" is "cannot tell".
  local err rc
  err="$(gh release view "$tag" --repo "$repo" 2>&1 >/dev/null)"; rc=$?
  if [ "$rc" -eq 0 ]; then die1 "release: '$tag' already exists on '$repo' — a mirrored release is never overwritten"; fi
  printf '%s' "$err" | grep -qi 'release not found' || die2 "release: could not read releases of '$repo' (gh exited $rc: $(printf '%s' "$err" | tr '\n' ' '))"

  local -a args=(release create "$tag" --repo "$repo" --target "$target" --title "$tag" --notes-file "$notes")
  [ "$prerelease" -eq 1 ] && args+=(--prerelease)
  gh "${args[@]}" "${files[@]}" || die2 "release: gh release create exited $?"
  echo "released $tag on $repo at $target with ${#files[@]} asset(s)"
}

[ "$#" -ge 1 ] || die2 "a subcommand is required: target | tree | release"
sub="$1"; shift
case "$sub" in
  target)  cmd_target "$@" ;;
  tree)    cmd_tree "$@" ;;
  release) cmd_release "$@" ;;
  -h|--help) sed -n '2,/^# ====/p' "$0" | sed 's/^# \{0,2\}//' ;;
  *) die2 "unknown subcommand '$sub' (target | tree | release)" ;;
esac
