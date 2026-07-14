package helm

import (
	"context"
	"testing"
)

func TestSupportsResetThenReuse_Probe(t *testing.T) {
	install(t, &fakeRunner{help: "Flags:\n      " + resetThenReuse + " strings\n"})
	if !supportsResetThenReuse(context.Background()) {
		t.Error("help advertising the reuse flag → true")
	}
	install(t, &fakeRunner{failOn: "upgrade --help"})
	if supportsResetThenReuse(context.Background()) {
		t.Error("a probe error → conservative false")
	}
}

func TestRepoPresent_Match(t *testing.T) {
	if !repoPresent(repoName + "\thttps://charts.example\n") {
		t.Error("a list naming the repo in column 1 → present")
	}
	if repoPresent("NAME\tURL\nother\thttps://x\n") {
		t.Error("a list without the repo → absent")
	}
	if repoPresent("") {
		t.Error("empty list → absent")
	}
}

func TestEnsureRepo_FatalErrors(t *testing.T) {
	// repo-add fails (the empty list means the repo isn't present → add is tried).
	install(t, &fakeRunner{failOn: "repo add"})
	if err := ensureRepo(context.Background()); err == nil {
		t.Error("a repo-add failure must be fatal")
	}
	// repo-update fails (add succeeds, update is failOn'd).
	install(t, &fakeRunner{failOn: "repo update"})
	if err := ensureRepo(context.Background()); err == nil {
		t.Error("a repo-update failure must be fatal")
	}
}

// TestUpgrade_EnsureRepoFailureIsFatal drives the full non-dry-run Upgrade path
// (reuse-flag probe → ensureRepo) and asserts an unresolvable repo aborts it.
func TestUpgrade_EnsureRepoFailureIsFatal(t *testing.T) {
	install(t, &fakeRunner{failOn: "repo add"}) // remote chart → ensureRepo runs → add fails
	if _, err := Upgrade(context.Background(), baseParams()); err == nil {
		t.Error("Upgrade must fail when the helm repo can't be ensured")
	}
}
