// Package installer holds the ONE bootstrap idiom for the official tracebloc
// installer: its URL, and the shell command that downloads and runs it.
//
// Every place in the CLI that runs the installer or prints a copy-paste hint for
// it derives from here — `tracebloc upgrade`, `tracebloc prepare-host`, the
// doctor remedies, and the cluster-discovery error. That's deliberate: the same
// string is both executed and printed, so the command we tell a user to run is
// byte-identical to the one we just tried on their behalf, and a URL or idiom
// change lands everywhere at once (Bugbot #394/#397, cli#396).
package installer

// URL is the single source of truth for the installer script's location.
const URL = "https://tracebloc.io/i.sh"

// Cmd is the bare bootstrap — download the installer and run it, no subcommand.
// This is the copy-paste one-liner every remedy prints, and what `tracebloc
// upgrade` executes.
var Cmd = Script("", "")

// Script builds the one-line bash program that downloads the cosign-verified
// installer to a private temp file and runs THAT file, optionally with a
// subcommand (e.g. "prepare-host") and an environment assignment applied to the
// installer itself (e.g. "TB_PREPARE_USER=alice").
//
// Every part of the shape below is load-bearing. It is both executed (via
// `bash -c`) and printed for a human to paste, so it has to hold up in both.
//
//   - A downloaded FILE, run as `bash "$tmp"` — never `curl … | bash` and never
//     `bash <(curl …)`. The two rejected forms fail for DIFFERENT reasons, worth
//     keeping apart. `curl … | bash` steals stdin: the inner bash reads its
//     *program* from the pipe, so the installer's stdin is no longer the terminal
//     and any interactive prompt (sign-in, or which non-admin user gets runtime
//     access) gets EOF. `bash <(curl …)` does NOT steal stdin — process
//     substitution hands bash a `/dev/fd/N` *filename* to read the program from,
//     so stdin stays the TTY; that's exactly why it was the original choice, and
//     its only fatal flaw is fail-open (next bullet). Running a downloaded file
//     gets both properties at once: stdin stays the TTY AND curl's exit is checked.
//
//   - `set -e` + `curl -o`, so a failed download (network/DNS/HTTP) aborts with
//     curl's real exit status. This is the load-bearing reason to reject BOTH
//     pipe/substitution forms — and the ONLY thing wrong with `bash <(curl …)`:
//     `curl | bash` and `bash <(curl …)` alike leave bash reading an empty script
//     and exiting 0, so we'd report success while nothing ran. Downloading first
//     also means a truncated mid-stream download is never partially executed.
//
//   - Wrapped in a subshell `( … )`. This is what makes the string safe to PASTE,
//     not just to exec: pasted into the user's interactive shell, a bare `set -e`
//     would arm errexit on that shell and the failed download would close their
//     terminal, and the bare `trap … EXIT` would outlive the command. Scoping
//     both to a subshell keeps the fail-closed status (the subshell's exit status
//     is the command's) without touching the shell the user is sitting in.
//
//   - Written on ONE line, `;`-joined. Copy-pasting a multi-line block is
//     unreliable — notably PowerShell, where a multi-line paste can execute
//     bottom-up, running the installer before the download. A single line pastes
//     the same way everywhere. This rules out the multi-line form for anything we
//     print, which is why the printed and executed strings are one and the same.
//
//   - `--tlsv1.2` pins the TLS floor scripts/install.sh enforces on every
//     security-sensitive fetch, so this privileged download can never negotiate a
//     weaker protocol (Bugbot #397).
//
// env is applied to the inner `bash "$tmp"` rather than to the whole command
// because a subshell cannot take a variable assignment — `VAR=x (…)` is a bash
// syntax error — and the installer is what needs to see it anyway.
func Script(subcommand, env string) string {
	run := `bash "$tmp"`
	if env != "" {
		run = env + " " + run
	}
	if subcommand != "" {
		run += " " + subcommand
	}
	return `(set -e; tmp="$(mktemp)"; trap 'rm -f "$tmp"' EXIT; ` +
		`curl -fsSL --tlsv1.2 ` + URL + ` -o "$tmp"; ` + run + `)`
}
