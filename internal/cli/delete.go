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
	cmd.Flags().StringVar(&o.kubeconfigPath, "kubeconfig", "",
		"path to the kubeconfig for the target cluster (default: $KUBECONFIG, then ~/.kube/config)")
	cmd.Flags().StringVar(&o.contextOverride, "context", "",
		"kubeconfig context for the target cluster (default: current-context)")
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "",
		"namespace of this machine's tracebloc release (default: the active client's namespace)")
	return cmd
}

func runDelete(ctx context.Context, p *ui.Printer, pr prompter, o deleteOpts) error {
	client, cfg, err := authedClient()
	if err != nil {
		return &exitError{code: 1, err: err}
	}
	prof := cfg.Current()
	if prof.ActiveClientID == "" {
		return &exitError{code: 1, err: errors.New(
			"no active client on this machine — nothing to offboard")}
	}
	id, cerr := strconv.Atoi(prof.ActiveClientID)
	if cerr != nil {
		return &exitError{code: 1, err: fmt.Errorf(
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

	// The three-way scope summary — shown BEFORE the confirm so the user sees
	// exactly what is removed, kept, and left alone (RFC-0001 §7.10).
	renderOffboardSummary(p, name, ns, o.keepData)

	// Live-work guard (inherits §7.4): refuse if tracebloc still reports this
	// client online / with a running job, unless --force. The heartbeat is
	// advisory, so this is a courtesy stop, not the safety — the teardown itself
	// is the real gate. A lookup failure is non-fatal (the client may be
	// unreachable precisely because it's being retired); warn and continue.
	if !o.force {
		if st, found, lerr := lookupClientStatus(ctx, client, prof.ActiveClientID); lerr != nil {
			// A 426 (CLI too old) won't recover by continuing — the whole offboard
			// talks to the same backend, so fail fast with the upgrade message rather
			// than imply the guard was merely skipped. Other errors (5xx/429/network)
			// are transient: warn and continue, since the teardown is the real gate.
			var ue *api.UpgradeRequiredError
			if errors.As(lerr, &ue) {
				return &exitError{code: 1, err: lerr}
			}
			p.Hintf("Couldn't check whether this client is still online (%v) — continuing; pass --force to skip this check.", lerr)
		} else if !found {
			// The stored id isn't among this account's clients — likely a stale
			// pointer or the wrong account/env. Don't silently pass the guard: warn,
			// then continue (the revoke below will 403/404 if it isn't really ours).
			p.Hintf("This client isn't in the signed-in account's client list — continuing; if that's unexpected, check you're logged into the right account/env.")
		} else if st == clientStatusOnline {
			return &exitError{code: 1, err: fmt.Errorf(
				"client %q is still online (tracebloc reports it running) — stop its training jobs first, "+
					"or pass --force to offboard anyway", name)}
		}
	}

	// Typed-client-name confirmation (not [y/N]): the local data wipe is
	// irreversible, so require the user to type the client's name. --yes skips it
	// for automation.
	if !o.yes {
		if pr == nil {
			return &exitError{code: 1, err: errors.New(
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
	//    retained history in scope 2 stays intact. A 403 → ask-an-admin.
	if rerr := client.RevokeClient(ctx, id); rerr != nil {
		var ae *api.APIError
		if errors.As(rerr, &ae) && ae.StatusCode == http.StatusForbidden {
			return askAnAdmin(ctx, p, client, "offboard this machine", "offboarding")
		}
		return &exitError{code: 1, err: fmt.Errorf("revoking the machine credential: %w", rerr)}
	}
	p.Successf("Revoked this machine's credential (client %q kept on tracebloc as a record).", name)

	// The teardown steps below are best-effort (the credential is already revoked),
	// but a step that leaves real state behind — a live release, the local cluster,
	// or on-host data — must NOT be papered over by the final success line. Track it
	// so the closing message tells the truth (image reclaim is pure disk cleanup, so
	// it's intentionally excluded — its own warning already surfaces it).
	degraded := false

	// 2. Uninstall the Helm release (best-effort — the credential is already
	//    revoked; a leftover release is harmless and re-runnable).
	if ns != "" {
		if uerr := uninstallChart(ctx, ns, o.kubeconfigPath, o.contextOverride); uerr != nil {
			p.Warnf("Chart uninstall reported: %v", uerr)
			degraded = true
		} else {
			p.Successf("Uninstalled the Helm release %s.", ns)
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
		p.Successf("Deleted the local cluster %q.", nodeboot.ClusterName)
	}

	// 4. Reclaim the tracebloc container images (SCOPED to ghcr.io/tracebloc/*,
	//    never a blanket prune — best-effort).
	if perr := pruneImages(ctx); perr != nil {
		p.Warnf("Image reclaim reported: %v", perr)
	} else {
		p.Successf("Reclaimed the tracebloc container images.")
	}

	// 5. The credential is revoked, so the enrollment is dead — clear the local
	//    active-client pointer FIRST, regardless of --keep-data, so the host never
	//    looks enrolled as a revoked client (§7.5). Then either spare or wipe
	//    ~/.tracebloc. The token and (under --keep-data) on-host data stay;
	//    re-running `client create` re-adopts by cluster_id.
	prof.ActiveClientID, prof.ActiveClientName, prof.ActiveClientNamespace = "", "", ""
	if o.keepData {
		if serr := cfg.Save(); serr != nil {
			// The in-memory pointer was cleared but not persisted — the on-disk config
			// still names the revoked client. Mark degraded so the closing doesn't read
			// as a clean offboard, and tell the user it needs a hand.
			degraded = true
			p.Warnf("Kept local data, but couldn't clear the stored active-client pointer (%v) — "+
				"the on-disk config still names the revoked client; run `tracebloc logout` or remove it by hand.", serr)
		} else {
			p.Infof("Kept local data and config (~/.tracebloc); cleared the active-client pointer — --keep-data.")
		}
	} else {
		if derr := removeHostDataDir(); derr != nil {
			// The wipe failed, so the pointer isn't gone with the directory —
			// persist the cleared pointer so the host doesn't still look enrolled
			// under a dead credential.
			degraded = true
			if serr := cfg.Save(); serr != nil {
				p.Warnf("Couldn't remove local data (%v) or clear the active-client pointer (%v) — remove it by hand: rm -rf %s", derr, serr, hostDataDirDisplay())
			} else {
				p.Warnf("Couldn't remove local data (%v) — cleared the active-client pointer; remove the data by hand: rm -rf %s", derr, hostDataDirDisplay())
			}
		} else {
			p.Successf("Removed local tracebloc data and config.")
		}
	}

	// 6. Remove the running CLI binary + its `tb` sibling symlink LAST (best-effort
	//    — on failure print the exact command; a brew-managed binary gets the brew
	//    hint instead). Done last so the earlier steps still run even if we can't
	//    delete ourselves.
	removeSelf(p)

	p.Newline()
	// The credential is revoked either way, so the machine can no longer connect —
	// but only claim a clean offboard when the teardown actually completed. If a
	// step left real state behind, say so instead of printing an unqualified success.
	if degraded {
		p.Warnf("Offboarded %q: the machine credential is revoked, so it can no longer connect to tracebloc — "+
			"but some cleanup above didn't complete. Finish the flagged steps by hand.", name)
	} else {
		p.Successf("Offboarded %q. This machine is no longer connected to tracebloc.", name)
	}
	return nil
}

// renderOffboardSummary prints the removed / retained / left three-way summary
// (RFC-0001 §7.10 mock) shown before the typed confirm.
func renderOffboardSummary(p *ui.Printer, name, ns string, keepData bool) {
	p.Banner("tracebloc", "offboard this machine")

	p.Section("Removed from this machine")
	p.Infof("This machine's credential (revoked — tracebloc can no longer see it)")
	if ns != "" {
		p.Infof("The Helm release %q and the local cluster %q", ns, nodeboot.ClusterName)
	} else {
		p.Infof("The Helm release and the local cluster %q", nodeboot.ClusterName)
	}
	p.Infof("The tracebloc container images (ghcr.io/tracebloc/*)")
	if keepData {
		p.Infof("The CLI itself (local data + config kept: --keep-data)")
	} else {
		p.Infof("Local data + config (~/.tracebloc) and the CLI itself — irreversible")
	}

	p.Section("Kept on tracebloc, as a record")
	p.Infof("Your use cases and the models trained here")
	p.Infof("The datasets' catalog entries (marked unavailable, not deleted)")

	p.Section("Left in place (system-wide)")
	p.Infof("Docker, kubectl, k3d, helm — remove yourself if unused")
	p.Infof("(On a GPU box: NVIDIA drivers + container toolkit)")
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
func removeSelf(p *ui.Printer) {
	exe, err := osExecutable()
	if err != nil {
		p.Hintf("Couldn't locate the CLI binary to remove it (%v) — delete it by hand.", err)
		return
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
		return
	}
	p.Successf("Removed the tracebloc CLI from this machine.")
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
