package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/nodeboot"
	"github.com/tracebloc/cli/internal/ui"
)

// nodeboot teardown hooks — package vars so tests fake the k3d/helm/docker
// shell-outs (a delete test never touches a real cluster or the docker daemon).
var (
	uninstallChart  = nodeboot.UninstallChart
	teardownCluster = nodeboot.TeardownCluster
	pruneImages     = nodeboot.PruneImages
)

// osExecutable + osRemoveAll are seams for the self-removal + data-wipe steps so
// tests exercise them against a temp path instead of the real binary / ~/.tracebloc.
var (
	osExecutable = os.Executable
	osRemoveAll  = os.RemoveAll
)

// deleteOpts bundles the `tracebloc delete` flags.
type deleteOpts struct {
	yes             bool
	keepData        bool
	force           bool
	kubeconfigPath  string
	contextOverride string
	namespace       string
}

// newDeleteCmd wires the TOP-LEVEL `tracebloc delete` — offboarding this machine
// (RFC-0001 §7.10). It is deliberately NOT under `client` and NOT
// `client delete --uninstall`: on the single-machine CLI the host owns exactly
// one client, so "delete the client" is "remove tracebloc," and the top-level
// verb avoids colliding with `data delete`.
//
// It is a SOFT offboard with three explicit scopes shown to the user before a
// typed-client-name confirm:
//
//   - Removed from this machine: revoke the machine credential, `helm uninstall`,
//     `k3d cluster delete`, reclaim tracebloc images, `rm -rf ~/.tracebloc`, then
//     the CLI binary + `tb` alias.
//   - Retained on tracebloc as a record: the client, its datasets' catalog
//     entries, use cases, and trained models / leaderboards (revoke preserves the
//     row — never a hard destroy).
//   - Left in place (system-wide): Docker, Homebrew, kubectl/k3d/helm, NVIDIA —
//     listed, never removed, never a reboot.
func newDeleteCmd() *cobra.Command {
	var o deleteOpts
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Offboard this machine from tracebloc (revoke, uninstall, reclaim disk)",
		Long: `Removes tracebloc from this machine: revokes the machine credential,
uninstalls the Helm release, deletes the local cluster, reclaims the tracebloc
container images, and clears local state — then removes the CLI itself.

Your use cases, datasets' catalog entries, and the models trained here are KEPT
on tracebloc as a record (a colleague's model must not vanish because you
reclaimed this box). System software the installer laid down — Docker, kubectl,
k3d, helm, NVIDIA drivers — is left in place; remove it yourself if unused.

Destructive: on a single-host install the on-prem datasets live on this machine
and are erased. Not undoable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var pr prompter
			if !o.yes && isInteractiveTTY() {
				pr = surveyPrompter{}
			}
			return runDelete(cmd.Context(), printerFor(cmd), pr, o)
		},
	}
	cmd.Flags().BoolVar(&o.yes, "yes", false,
		"skip the typed-name confirmation (for automation)")
	cmd.Flags().BoolVar(&o.keepData, "keep-data", false,
		"uninstall the software but keep ~/.tracebloc (local config + on-host datasets)")
	cmd.Flags().BoolVar(&o.force, "force", false,
		"offboard even if tracebloc still reports this client online")
	addKubeconfigFlags(cmd, &o.kubeconfigPath, &o.contextOverride,
		"path to the kubeconfig for the target cluster (default: $KUBECONFIG, then ~/.kube/config)",
		"kubeconfig context for the target cluster (default: current-context)")
	addNamespaceFlag(cmd, &o.namespace,
		"namespace of this machine's tracebloc release (default: the active client's namespace)")
	return cmd
}

func runDelete(ctx context.Context, p *ui.Printer, pr prompter, o deleteOpts) error {
	client, cfg, err := authedClient()
	if err != nil {
		return &exitError{code: exitFailure, err: err}
	}
	prof := cfg.Current()
	if prof.ActiveClientID == "" {
		return &exitError{code: exitFailure, err: errors.New(
			"no active client on this machine — nothing to offboard")}
	}
	id, cerr := strconv.Atoi(prof.ActiveClientID)
	if cerr != nil {
		return &exitError{code: exitFailure, err: fmt.Errorf(
			"stored active client id %q is not numeric: %w", prof.ActiveClientID, cerr)}
	}
	name := prof.ActiveClientName
	if name == "" {
		name = prof.ActiveClientID
	}
	// The release namespace: --namespace wins, else the active client's cached
	// namespace (set at create time).
	ns := o.namespace
	if ns == "" {
		ns = prof.ActiveClientNamespace
	}

	// Work-guard: refuse to offboard while training runs are ACTIVE (offboarding
	// would kill them), unless --force. Block on RUNNING experiments, NOT on
	// "online": a healthy environment is always online (it heartbeats), so
	// blocking on that would make offboard impossible without --force and mislead
	// ("stop your training jobs" when there are none). A lookup failure is
	// non-fatal (the environment may be unreachable precisely because it's being
	// retired); warn and continue — the typed-name confirm and the teardown are
	// the real gates. 426/401/403 won't recover by continuing, so fail fast.
	if !o.force {
		pc, lerr := client.GetClient(ctx, id)
		switch {
		case lerr != nil:
			var ue *api.UpgradeRequiredError
			if errors.As(lerr, &ue) {
				return &exitError{code: exitFailure, err: lerr}
			}
			var ae *api.APIError
			if errors.As(lerr, &ae) && (ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden) {
				return &exitError{code: exitFailure, err: errors.New(
					"tracebloc rejected your credentials — run `tracebloc login`, then retry `tracebloc delete`")}
			}
			p.Hintf("Couldn't check for active training runs (%v) — continuing; the confirmation below still guards you.", lerr)
		case pc == nil:
			// The stored id isn't among this account's clients — likely a stale
			// pointer or the wrong account. Warn, then continue (the revoke below
			// will 403/404 if it isn't really ours).
			p.Hintf("This secure environment isn't in the signed-in account — continuing; if that's unexpected, check you're logged into the right account.")
		case pc.NumRunningExperiments > 0:
			// Resolve the launcher the way resources/home do — advertise `tb` only
			// when the real alias exists (not every install has it), else the name
			// the user invoked — so the suggested command is always copy-pasteable.
			relaunch := invokedName()
			if tbAliasAvailable() {
				relaunch = binTB
			}
			return &exitError{code: exitFailure, err: fmt.Errorf(
				"your secure environment %q has %d training run(s) active — offboarding would stop them. "+
					"Wait for them to finish or cancel them, then run `%s delete` again — or `%s delete --force` to stop them and offboard now",
				name, pc.NumRunningExperiments, relaunch, relaunch)}
		}
	}

	// The three-way scope PREVIEW — shown after the work-guard and before the
	// typed-name confirm, so the user sees exactly what WILL be removed, kept, and
	// left alone before committing to anything (RFC-0001 §7.10).
	renderOffboardSummary(p, name, o.keepData)

	// Typed-client-name confirmation (not [y/N]): the local data wipe is
	// irreversible, so require the user to type the client's name. --yes skips it
	// for automation.
	if !o.yes {
		if pr == nil {
			return &exitError{code: exitFailure, err: errors.New(
				"refusing to offboard without confirmation: pass --yes, or run on a terminal to type the client name")}
		}
		p.Newline()
		p.PromptHint("This is irreversible. Type the client name to confirm, or leave blank to cancel.")
		typed, perr := pr.Input(fmt.Sprintf("Type %q to offboard this machine", name), "", "", nil)
		if perr != nil {
			return mapClientErr(perr)
		}
		if strings.TrimSpace(typed) != name {
			p.Infof("Cancelled — the name didn't match. Nothing was removed.")
			return nil
		}
	}

	p.Newline()

	// 1. Revoke the machine credential server-side (POST /edge-device/<id>/revoke,
	//    §7.10 / C.6). This kills the credential without deleting the row — the
	//    retained history in scope 2 stays intact.
	revoked := true
	if rerr := client.RevokeClient(ctx, id); rerr != nil {
		revoked = false
		// A 426 (CLI too old) won't recover by continuing — the whole offboard talks
		// to the same backend — so fail fast with the upgrade message rather than
		// tear the machine down against a backend that can't process the revoke.
		// Mirrors the pre-offboard guard above (which treats 426 as terminal).
		var ue *api.UpgradeRequiredError
		if errors.As(rerr, &ue) {
			return &exitError{code: exitFailure, err: rerr}
		}
		var ae *api.APIError
		if errors.As(rerr, &ae) {
			switch ae.StatusCode {
			case http.StatusForbidden:
				// A 403 is a genuine authorization decision — you can't revoke a
				// client you don't manage; route to ask-an-admin (unchanged).
				return askAnAdmin(ctx, p, client, "offboard this machine", "offboarding")
			case http.StatusUnauthorized:
				// A 401 means the signed-in session is expired/revoked. With --force
				// the online-guard above is skipped, so DON'T silently tear the machine
				// down while a live credential remains — fail fast and point at sign-in,
				// mirroring the pre-offboard guard's 401 handling.
				return &exitError{code: exitFailure, err: errors.New(
					"tracebloc rejected your credentials — run `tracebloc login`, then retry `tracebloc delete`")}
			}
		}
		// Any OTHER revoke failure must NOT block the local teardown. Removing
		// tracebloc from THIS machine (helm uninstall, cluster teardown, on-host
		// data + config wipe, self-remove) is offline-capable and is the command's
		// primary job — it can't be held hostage to a best-effort remote call. This
		// hits on a 404 (a stale/wrong-account active-client pointer, or a backend
		// predating the /revoke route), a transient network/5xx error, etc. Warn and
		// continue, mirroring the online-guard above. The credential may remain live
		// server-side (revoked stays false), so the closing summary says so: the user
		// can revoke it from the dashboard, and the orphan reaper (backend#970) sweeps
		// a never-torn-down record later.
		p.Hintf("Couldn't revoke the credential server-side (%v) — continuing with local teardown. "+
			"The credential may still be live on tracebloc; revoke it from the dashboard if needed.", rerr)
	} else {
		p.Successf("Revoked this machine's credential — your secure environment %q stays on tracebloc as a record.", name)
	}

	// The teardown steps below are best-effort. (The credential is revoked when the
	// server-side revoke above succeeded; on a best-effort revoke failure it may
	// still be live — the closing summary reports which.) A step that leaves real
	// state behind — a live release, the local cluster, or on-host data — must NOT be
	// papered over by the final success line. Track it so the closing message tells
	// the truth (image reclaim is pure disk cleanup, so it's intentionally excluded —
	// its own warning already surfaces it).
	degraded := false

	// Clear the local enrollment pointer and persist it IMMEDIATELY — before the
	// best-effort teardown below — so the host never looks enrolled under the
	// now-revoked credential, even if a later step fails or the process is
	// interrupted mid-teardown (§7.5). On the default (wipe) path ~/.tracebloc —
	// config.json included — is removed below anyway; persisting here first makes
	// the --keep-data and killed-mid-teardown cases safe. Re-running `client create`
	// re-adopts by cluster_id.
	prof.ActiveClientID, prof.ActiveClientName, prof.ActiveClientNamespace = "", "", ""
	if serr := cfg.Save(); serr != nil {
		degraded = true
		p.Warnf("Couldn't clear the stored active-client pointer (%v) — the on-disk config "+
			"still names the revoked client; run `tracebloc logout` or remove it by hand.", serr)
	}

	// 2. Uninstall the Helm release (best-effort — the credential is already
	//    revoked; a leftover release is harmless and re-runnable).
	if ns != "" {
		if uerr := uninstallChart(ctx, ns, o.kubeconfigPath, o.contextOverride); uerr != nil {
			p.Warnf("Chart uninstall reported: %v", uerr)
			degraded = true
		} else {
			p.Successf("Uninstalled tracebloc.")
		}
	} else {
		// No cached namespace (a pre-cache config, or none passed) — we can't name
		// the release to uninstall. Say so rather than skip silently: the summary
		// promised the release would go, so a leftover must be called out.
		p.Warnf("Couldn't determine this client's namespace — skipped the Helm uninstall. " +
			"If a release is still installed, re-run with --namespace <ns>.")
		degraded = true
	}

	// 3. Tear down the local cluster (also prunes its kubeconfig entry).
	if terr := teardownCluster(ctx, nodeboot.ClusterName); terr != nil {
		p.Warnf("Cluster teardown reported: %v", terr)
		degraded = true
	} else {
		p.Successf("Removed the local environment.")
	}

	// 4. Reclaim the tracebloc container images (SCOPED to ghcr.io/tracebloc/*,
	//    never a blanket prune — best-effort).
	if perr := pruneImages(ctx); perr != nil {
		// Pure disk cleanup — a failure here changes nothing about the offboard
		// (it's excluded from `degraded`). Keep it a quiet, plain note; don't leak
		// the raw docker/daemon error at the user.
		// `docker image prune` would NOT help here: it only removes dangling
		// (untagged) images, and these are tagged ghcr.io/tracebloc/* refs. Point
		// at the scoped removal that actually matches the failure mode — the same
		// reference PruneImages targets — never a blanket prune.
		p.Infof("Some tracebloc images couldn't be reclaimed (harmless) — remove them later with `docker rmi $(docker images --filter=reference='ghcr.io/tracebloc/*' --format '{{.Repository}}:{{.Tag}}')`.")
	} else {
		p.Successf("Reclaimed tracebloc's downloaded images.")
	}

	// 5. Spare or wipe ~/.tracebloc. The active-client pointer was already cleared
	//    and persisted above, so here we only decide whether the data directory
	//    stays. Under --keep-data the token and on-host datasets remain.
	if o.keepData {
		p.Infof("Kept local data and config (~/.tracebloc); cleared the active-client pointer — --keep-data.")
	} else {
		if derr := removeHostDataDir(); derr != nil {
			degraded = true
			p.Warnf("Couldn't remove local data (%v) — cleared the active-client pointer; "+
				"remove the data by hand: rm -rf %s", derr, hostDataDirDisplay())
		} else {
			p.Successf("Removed local tracebloc data and config.")
		}
	}

	// 6. Remove the running CLI binary + its `tb` sibling symlink LAST (best-effort
	//    — on failure print the exact command; a brew-managed binary gets the brew
	//    hint instead). Done last so the earlier steps still run even if we can't
	//    delete ourselves. A failed self-removal leaves the CLI on disk, which the
	//    pre-confirm summary promised would go — so mark the offboard degraded.
	if !removeSelf(p) {
		degraded = true
	}

	p.Newline()
	// Be honest on BOTH axes: whether the server-side revoke succeeded (revoked) and
	// whether the local teardown completed (!degraded). Neither is guaranteed — the
	// revoke is best-effort on a non-terminal failure, and the teardown steps are
	// best-effort — so only claim "revoked / no longer connected" when it's true.
	switch {
	case revoked && !degraded:
		p.Successf("Offboarded %q. This machine is no longer connected to tracebloc.", name)
	case revoked && degraded:
		p.Warnf("Offboarded %q: the machine credential is revoked, so it can no longer connect to tracebloc — "+
			"but some cleanup above didn't complete. Finish the flagged steps by hand.", name)
	case !revoked && !degraded:
		p.Warnf("Tore down %q on this machine. The server-side revoke didn't complete, so the credential may "+
			"still be live on tracebloc — revoke it from the dashboard if needed (the orphan reaper sweeps it otherwise).", name)
	default: // !revoked && degraded
		p.Warnf("Tore down %q on this machine, but some cleanup above didn't complete and the server-side revoke "+
			"didn't complete — the credential may still be live on tracebloc (revoke it from the dashboard). "+
			"Finish the flagged steps by hand.", name)
	}
	return nil
}

// renderOffboardSummary prints the three-way PREVIEW — what will be removed,
// kept, and left alone — shown after the work-guard and before the typed confirm
// (RFC-0001 §7.10 mock). Plain language only; the Kubernetes/Helm/registry
// specifics stay out of the user's way.
func renderOffboardSummary(p *ui.Printer, name string, keepData bool) {
	p.Section("This will remove")
	p.Infof("This machine's credential — so tracebloc can no longer reach it")
	p.Infof("Your secure environment %q and everything it runs on this machine", name)
	p.Infof("tracebloc's downloaded images")
	if keepData {
		p.Infof("The tracebloc CLI (your local data & config are kept — --keep-data)")
	} else {
		p.Infof("Your local data & config (~/.tracebloc) and the tracebloc CLI — can't be undone")
	}

	p.Section("Kept on tracebloc")
	p.Infof("Your use cases and the models trained here")
	p.Infof("Your dataset records (marked unavailable, not deleted)")

	p.Section("Left alone")
	p.Infof("Docker and related tools — remove them yourself if you no longer need them")
}

// removeHostDataDir deletes the tracebloc host data directory (~/.tracebloc, or
// $TRACEBLOC_CONFIG_DIR when set — the same resolution config.Dir uses). It goes
// through the config package so a test's temp override is honored and the real
// ~/.tracebloc is never touched in tests.
func removeHostDataDir() error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	return osRemoveAll(dir)
}

// hostDataDirDisplay is the data dir for a user-facing hint; falls back to the
// literal ~/.tracebloc if it can't be resolved.
func hostDataDirDisplay() string {
	if dir, err := config.Dir(); err == nil {
		return dir
	}
	return "~/.tracebloc"
}

// removeSelf deletes the running CLI binary and its sibling `tb` symlink — the
// last offboarding step. Best-effort: on failure it prints the exact command to
// finish by hand (or a `brew uninstall` hint when the binary looks brew-managed),
// rather than fail the whole offboard, which has already succeeded.
//
// Returns false when the binary (or tracebloc's own `tb` alias) could not be
// removed, so the caller can mark the offboard degraded rather than print a
// clean-success closing while the CLI is still on disk.
func removeSelf(p *ui.Printer) (ok bool) {
	ok = true
	exe, err := osExecutable()
	if err != nil {
		p.Hintf("Couldn't locate the CLI binary to remove it (%v) — delete it by hand.", err)
		return false
	}

	// The `tb` alias is a sibling of the binary (the installer symlinks it next to
	// `tracebloc`); remove it first so a leftover alias doesn't dangle. Remove it
	// ONLY when it is our own symlink — a symlink whose target is this binary. The
	// installer is careful never to clobber a pre-existing `tb` from another tool
	// (install.sh: `readlink tb == PREFIX/tracebloc`); delete must be just as
	// careful, or offboarding one machine could delete an unrelated `tb` (e.g.
	// another CLI on the same PATH dir).
	tb := filepath.Join(filepath.Dir(exe), "tb")
	if tb != exe {
		switch exists, ours := aliasStatus(tb, exe); {
		case ours:
			if rmErr := osRemoveAll(tb); rmErr != nil {
				p.Hintf("Couldn't remove the `tb` alias (%v) — remove it by hand: rm -f %s", rmErr, tb)
				ok = false
			}
		case exists:
			// A `tb` that isn't our symlink belongs to another tool — leave it, and
			// say so (mirrors the installer's "already exists and isn't ours" note).
			p.Hintf("Left %s in place — it isn't tracebloc's `tb` alias.", tb)
		}
	}

	if rmErr := osRemoveAll(exe); rmErr != nil {
		if looksBrewManaged(exe) {
			p.Hintf("Couldn't remove the CLI (%v). It looks Homebrew-managed — finish with: brew uninstall tracebloc", rmErr)
		} else {
			p.Hintf("Couldn't remove the CLI (%v) — remove it by hand: rm -f %s", rmErr, exe)
		}
		return false
	}
	p.Successf("Removed the tracebloc CLI from this machine.")
	return ok
}

// aliasStatus reports whether a `tb` path exists and whether it is tracebloc's
// OWN alias — a symlink resolving to this binary. It mirrors the installer's
// ownership test (install.sh: `readlink tb == PREFIX/tracebloc`) so delete only
// removes what install created. A regular file, or a symlink pointing elsewhere,
// is `exists=true, ours=false` — another tool's `tb`, never to be deleted.
func aliasStatus(tb, exe string) (exists, ours bool) {
	fi, err := os.Lstat(tb)
	if err != nil {
		return false, false // no `tb` sibling at all
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return true, false // a regular file/dir named `tb` — not ours
	}
	target, err := os.Readlink(tb)
	if err != nil {
		return true, false
	}
	// The installer writes an absolute target; resolve a relative one against the
	// link's own directory before comparing.
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(tb), target)
	}
	if filepath.Clean(target) == filepath.Clean(exe) {
		return true, true
	}
	// exe may itself be a symlink (e.g. Intel Homebrew: /usr/local/bin/tracebloc →
	// Cellar); compare fully-resolved paths as a fallback.
	if rt, e1 := filepath.EvalSymlinks(target); e1 == nil {
		if re, e2 := filepath.EvalSymlinks(exe); e2 == nil && rt == re {
			return true, true
		}
	}
	return true, false
}

// looksBrewManaged reports whether a binary path sits under a Homebrew prefix, so
// removeSelf can point the user at `brew uninstall` instead of a raw `rm` that
// leaves brew's metadata dangling. Covers the common Homebrew roots on Apple
// Silicon (/opt/homebrew), Intel macOS (/usr/local/Cellar), and Linuxbrew.
//
// It also checks the symlink-resolved path: on Intel macOS the binary on PATH is
// /usr/local/bin/tracebloc, a symlink into the Cellar, and os.Executable may hand
// back that unresolved link — the raw path matches no marker, but its target does.
func looksBrewManaged(path string) bool {
	candidates := []string{path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		candidates = append(candidates, resolved)
	}
	for _, cand := range candidates {
		for _, marker := range []string{
			"/opt/homebrew/",
			"/usr/local/Cellar/",
			"/home/linuxbrew/",
			"/Homebrew/",
		} {
			if strings.Contains(cand, marker) {
				return true
			}
		}
	}
	return false
}
