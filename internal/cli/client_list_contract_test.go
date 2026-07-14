package cli

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// ── cli#141: guard the installer's #303 pre-flight contract ──────────────────
//
// `client list` is a Hidden cobra command (RFC-0001 §7.10): with `client use`
// withdrawn a human has nothing to select, so it's kept off the user-facing
// surface. BUT the tracebloc installer's one-client-per-machine pre-flight
// (#303) still shells out to it. In the `client` repo, scripts/lib/provision.sh
// `_account_owns_namespace` runs:
//
//	out="$(tracebloc client list --plain 2>/dev/null)" || return 2
//	grep -Eq "namespace=${ns}([[:space:]]|$)" <<<"$out"
//
// and REFUSES to provision when the signed-in account doesn't own the namespace
// already installed on this machine. Hidden ≠ disabled, so the command still
// runs — but nothing else pins that `--plain` stays a valid flag or that the
// output keeps emitting `namespace=<ns>` in a form that grep matches. If either
// drifts, the installer's grep silently fails, `_account_owns_namespace` can no
// longer see the namespace, and #303 stops firing in the field with no error.
//
// These tests are the single-source guard for that cross-repo contract: they
// drive the REAL command (NewRootCmd → Execute, not runClientList directly) the
// way the installer does, and assert the literal string the installer greps for.
// A format change that would break the installer breaks these tests instead.

// installerNamespaceGrep mirrors, as closely as Go's RE2 allows, the extended
// regexp the installer greps `client list --plain` output with:
//
//	grep -Eq "namespace=${ns}([[:space:]]|$)"
//
// (?m) makes `$` match end-of-line as grep's line-oriented match does (not just
// end-of-text); QuoteMeta mirrors that the installer interpolates a *literal*
// namespace, not a glob. If this pattern stops matching the live output, the
// installer's grep stops matching too.
func installerNamespaceGrep(ns string) *regexp.Regexp {
	return regexp.MustCompile("(?m)namespace=" + regexp.QuoteMeta(ns) + "([[:space:]]|$)")
}

// listCmdIsHidden reports whether `client list` is still a Hidden subcommand —
// the premise this contract guards ("Hidden, yet the installer still runs it").
func listCmdIsHidden() bool {
	for _, c := range newClientCmd().Commands() {
		if c.Name() == "list" {
			return c.Hidden
		}
	}
	return false
}

func TestClientList_Plain_InstallerPreflightContract(t *testing.T) {
	const ownedNS = "acme-prod-01"

	// Two clients: one the account owns, and a second whose namespace has the
	// first's as a strict prefix — so the boundary assertions below are real.
	withClientBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[` +
			`{"id":7,"first_name":"acme-box","namespace":"` + ownedNS + `","location":"DE","status":1},` +
			`{"id":8,"first_name":"other-box","namespace":"` + ownedNS + `-staging","location":"US","status":0}` +
			`]`))
	})

	// Premise: the command really is Hidden. If it ever becomes visible this
	// test is no longer guarding the "hidden but callable" property it claims to
	// (and TestClientSubcommandVisibility would also flag the intent change).
	if !listCmdIsHidden() {
		t.Fatal("precondition: `client list` must be Hidden — cli#141 guards the hidden-but-installer-callable contract")
	}

	// (a) Drive the REAL root exactly as the installer does — `tracebloc client
	// list --plain` — through NewRootCmd + Execute, NOT runClientList directly.
	// This is what proves the Hidden command still DISPATCHES and that the
	// persistent --plain flag is still accepted on it. (If --plain were dropped,
	// the installer's command would exit non-zero → _account_owns_namespace
	// returns 2 → the pre-flight silently skips the refusal.)
	root := NewRootCmd(BuildInfo{Version: "test"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"client", "list", "--plain"})
	if err := root.Execute(); err != nil {
		t.Fatalf("`client list --plain` must still run while Hidden (the installer shells out to it): %v\noutput:\n%s", err, out.String())
	}
	got := out.String()

	// (b) The exact field the installer greps for, verbatim. This literal IS the
	// cross-repo contract with client/scripts/lib/provision.sh.
	if !strings.Contains(got, "namespace="+ownedNS) {
		t.Errorf("`client list --plain` output is missing the `namespace=%s` field the installer greps for.\n"+
			"If the list format changed, update client/scripts/lib/provision.sh (_account_owns_namespace) IN LOCKSTEP.\noutput:\n%s", ownedNS, got)
	}

	// The fuller literal fragment pins field ORDER + the whitespace separator too,
	// so any reformat (renamed / reordered / re-spaced field) trips this canary
	// and forces a human to reconcile the installer's grep.
	const wantFragment = "namespace=" + ownedNS + "   location=DE"
	if !strings.Contains(got, wantFragment) {
		t.Errorf("`client list --plain` no longer emits the exact %q fragment.\n"+
			"The installer tolerates any trailing whitespace, but a format change is worth a human check — reconcile provision.sh.\noutput:\n%s", wantFragment, got)
	}

	// The single-source assertion: the installer's OWN regex must match the live
	// output. If this fails, the #303 pre-flight's grep fails in the field.
	if !installerNamespaceGrep(ownedNS).MatchString(got) {
		t.Errorf("the installer's exact grep /namespace=%s([[:space:]]|$)/ does NOT match `client list --plain` output — #303 would stop firing.\noutput:\n%s", ownedNS, got)
	}

	// Boundary 1: a namespace that isn't present must not match (no false accept).
	if installerNamespaceGrep(ownedNS + "-nope").MatchString(got) {
		t.Errorf("installer grep matched a namespace not in the output — false positive would mis-judge ownership.\noutput:\n%s", got)
	}

	// Boundary 2: the installer anchors with ([[:space:]]|$), so a STRICT PREFIX
	// of a real namespace must NOT match (the next char is '1', not whitespace/EOL).
	// This pins that ownership is exact, not prefix-based — an account doesn't
	// "own" `acme-prod-0` just because it owns `acme-prod-01`.
	if installerNamespaceGrep("acme-prod-0").MatchString(got) {
		t.Errorf("installer grep matched a strict prefix of a namespace — the whitespace/EOL anchor was lost, so #303 could mis-judge ownership.\noutput:\n%s", got)
	}
}
