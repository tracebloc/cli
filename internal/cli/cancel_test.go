// The cancellation contract, in one table. Every interactive prompt in the CLI
// can be backed out of two ways — Ctrl-C, or an answer that means "no" — and both
// must produce the SAME thing: a visible "Cancelled — …" line, exit 0, and no
// side effect. This file is the drift guard for that (backend#1253, the test
// convention proposed for this finding class in backend#930).
//
// Why it exists: `client create` and `delete` used to map Ctrl-C straight to a
// nil error, so aborting at the prompt exited 0 having printed nothing about it —
// byte-for-byte indistinguishable from a completed run for anything reading the
// stream, and inconsistent with the declined-answer branch sitting right beside
// it. Asserting the exit code alone would not have caught that; every row here
// asserts the exit code AND the user-visible output.
//
// Adding a prompt? Add a row. Both prompt doubles are shared, and the "did it act
// anyway" probe keeps a row honest: a printed note over a completed side effect
// would be a worse lie than silence.

package cli

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/ui"
)

// runner drives one command to its prompt, using the injected prompt double.
type runner func(*ui.Printer, prompter) error

// sideEffectProbe reports whether the command took its irreversible action
// despite the cancellation (a provision POST, an offboard revoke/teardown).
type sideEffectProbe func() bool

// setUpClientCreate wires `tracebloc client create` against a fake backend that
// records whether a client was ever POSTed. No --yes and a prompter present, so
// the run reaches the "Provision this client?" confirm.
func setUpClientCreate(t *testing.T) (runner, sideEffectProbe) {
	t.Helper()
	posted := false
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
		}
		_, _ = w.Write([]byte(`[]`)) // no existing clients on the account
	})
	signInAs(t, "Lab", "lab@example.com")
	return func(p *ui.Printer, pr prompter) error {
		return runClientCreate(context.Background(), p, pr, clientCreateOpts{})
	}, func() bool { return posted }
}

// setUpDelete wires `tracebloc delete` (offboard this machine) with a live
// client to remove and every teardown seam faked, so the run reaches the
// typed-client-name confirmation and any teardown step is recorded, not real.
func setUpDelete(t *testing.T) (runner, sideEffectProbe) {
	t.Helper()
	revoked := false
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke") {
			revoked = true
		}
		_, _ = w.Write([]byte(`{"id":5,"first_name":"gpu-box-01","namespace":"gpu-box-01","status":0}`))
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
	fn.install(t)
	return func(p *ui.Printer, pr prompter) error {
		return runDelete(context.Background(), p, pr, deleteOpts{})
	}, func() bool { return revoked || len(fn.calls) > 0 }
}

// TestPromptCancellation_IsVisibleAndCleanExit: for every prompt a user can back
// out of, the CLI prints why it stopped and exits 0 — and does not act.
//
// Exit 0 (not 130) is the convention: nothing was started, so there is no
// interrupted operation to report. exitInterrupted is reserved for a Ctrl-C that
// cuts short work already in flight — see exitcodes.go and cleanCancel.
func TestPromptCancellation_IsVisibleAndCleanExit(t *testing.T) {
	// declineClientCreate answers "No" at the confirm (the Ctrl-C row's twin).
	no := false

	cases := []struct {
		name string
		// how the user backed out, for failure messages.
		how string
		// setUp wires the command's world; pr is what the user "did".
		setUp func(*testing.T) (runner, sideEffectProbe)
		pr    prompter
		// wantOut is the line the user must see. The Ctrl-C and declined rows of
		// one command share it wherever the reason is the same — `delete`'s
		// mismatch row names the reason, which is more, never less.
		wantOut string
	}{
		{
			name:    "client create/ctrl-c at the confirm",
			how:     "Ctrl-C at \"Provision this client?\"",
			setUp:   setUpClientCreate,
			pr:      cancellingPrompter{},
			wantOut: "Cancelled — nothing was provisioned.",
		},
		{
			name:    "client create/answered no at the confirm",
			how:     "answering \"No\" at \"Provision this client?\"",
			setUp:   setUpClientCreate,
			pr:      &fakePrompter{confirm: &no},
			wantOut: "Cancelled — nothing was provisioned.",
		},
		{
			name:    "delete/ctrl-c while typing the name",
			how:     "Ctrl-C at the typed-name confirmation",
			setUp:   setUpDelete,
			pr:      cancellingPrompter{},
			wantOut: "Cancelled — nothing was removed.",
		},
		{
			name:    "delete/typed a name that didn't match",
			how:     "typing the wrong client name",
			setUp:   setUpDelete,
			pr:      typedNamePrompter{reply: "wrong-name"},
			wantOut: "Cancelled — the name didn't match. Nothing was removed.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, sideEffected := tc.setUp(t)
			out := &strings.Builder{}
			err := run(ui.New(out, ui.WithColor(false)), tc.pr)

			if got := ExitCodeFromError(err); got != exitOK {
				t.Errorf("exit code after %s = %d, want %d (backing out is a choice, not a failure): %v",
					tc.how, got, exitOK, err)
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("%s printed no cancellation note — a silent exit 0 is indistinguishable from success.\nwant a line containing: %q\ngot:\n%s",
					tc.how, tc.wantOut, out.String())
			}
			if sideEffected() {
				t.Errorf("%s must not act: the command went ahead anyway", tc.how)
			}
		})
	}
}
