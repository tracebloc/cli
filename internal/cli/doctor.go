package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/doctor"
	"github.com/tracebloc/cli/internal/ui"
)

// installCmd is the one-line installer we point people at when there's no
// secure environment on this machine, or a component needs reinstalling. Kept in
// one place so every remedy says the same thing.
const installCmd = "bash <(curl -fsSL https://tracebloc.io/i.sh)"

// doctorRunFn is a test seam over doctor.Run (the cluster-side probe). Tests
// inject a fixed []doctor.Result so the roll-up + render can be exercised with a
// controlled mix without standing up a fake cluster.
var doctorRunFn = doctor.Run

// newDoctorCmd builds `doctor`, registered as top-level `tracebloc doctor` and
// (hidden) `tracebloc cluster doctor`. It answers, in plain language: is my
// secure environment connected to tracebloc and ready to run training — and if
// not, exactly what do I do. The Kubernetes detail lives behind --verbose;
// --diagnose writes a redacted bundle for support.
func newDoctorCmd(hidden bool) *cobra.Command {
	var (
		kubeconfigPath  string
		contextOverride string
		nsOverride      string
		diagnose        bool
	)

	cmd := &cobra.Command{
		Use:    "doctor",
		Hidden: hidden,
		Short:  "Check your secure environment is connected and ready to run training",
		Long: `Checks, in plain terms, whether your secure environment is connected to
tracebloc and ready to run training — and if something's wrong, exactly what to
do about it.

  --verbose    the full technical breakdown (for support)
  --diagnose   write a redacted support bundle to email to tracebloc

Exit codes:
  0   healthy
  2   a problem was found
  3   couldn't read your local config`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterDoctor(
				cmd.Context(),
				printerFor(cmd),
				kubeconfigPath, contextOverride, nsOverride, diagnose,
			)
		},
	}

	addKubeconfigFlags(cmd, &kubeconfigPath, &contextOverride, kubeconfigFlagUsage, contextFlagUsage)
	addNamespaceFlag(cmd, &nsOverride,
		"namespace where your secure environment is installed (default: your active client's)")
	cmd.Flags().BoolVar(&diagnose, "diagnose", false,
		"write a redacted support bundle for tracebloc support and exit")

	return cmd
}

func runClusterDoctor(
	ctx context.Context,
	p *ui.Printer,
	kubeconfigPath, contextOverride, nsOverride string,
	diagnose bool,
) error {
	// 1. Who you are — a local config read (instant), so we can answer even
	//    before any cluster is reachable (RFC-0001 §8.5).
	cfg, err := config.Load()
	if err != nil {
		p.Errorf("Couldn't read your tracebloc config — run `%s login` to recreate it.", launcher())
		return &exitError{code: exitLocalEnv, err: nil}
	}
	if !cfg.SignedIn() {
		p.Errorf("Not signed in — run `%s login`.", launcher())
		return &exitError{code: exitChecksFailed, err: nil}
	}
	if email := cfg.Current().Email; email != "" {
		p.Para("Signed in as " + email)
	} else {
		p.Para("Signed in")
	}

	// 2. Is the session still good with tracebloc? A 401 (expired/revoked) or a
	//    426 (CLI too old) is a hard stop with a one-command fix. A network error
	//    is NOT fatal — it folds into the Connected line below.
	tokenReachable := true
	apiClient := newAPIClient(cfg.CurrentEnv)
	apiClient.Token = cfg.Current().Token
	if _, werr := apiClient.WhoAmI(ctx); werr != nil {
		var ae *api.APIError
		var ue *api.UpgradeRequiredError
		switch {
		case errors.As(werr, &ae) && ae.StatusCode == http.StatusUnauthorized:
			p.Newline()
			p.Errorf("Your session expired — run `%s login`.", launcher())
			return &exitError{code: exitChecksFailed, err: nil}
		case errors.As(werr, &ue):
			p.Newline()
			p.Errorf("This CLI is out of date — update it: %s", installCmd)
			return &exitError{code: exitChecksFailed, err: nil}
		default:
			tokenReachable = false // can't reach tracebloc from here — Connected will say so
		}
	}

	// 3. Find the secure environment (local kubeconfig read).
	opts := cluster.KubeconfigOptions{Path: kubeconfigPath, Context: contextOverride, Namespace: nsOverride}
	bindActiveClientNamespace(&opts)
	resolved, err := loadClusterFn(opts)
	if err != nil {
		p.Newline()
		p.Errorf("No secure environment on this machine yet.")
		p.Hintf("     Set one up: %s", installCmd)
		return &exitError{code: exitLocalEnv, err: nil}
	}
	cs, err := newClientsetFn(resolved)
	if err != nil {
		p.Newline()
		p.Errorf("Couldn't connect to your secure environment — check your kubeconfig/context.")
		return &exitError{code: exitLocalEnv, err: nil}
	}
	p.Para(fmt.Sprintf("Secure environment %q", envDisplayName(resolved)))

	// 4. Probe the cluster (the granular checks stay for --verbose + --diagnose).
	results := doctorRunFn(ctx, cs, doctor.Options{Namespace: resolved.Namespace, ServerURL: resolved.ServerURL})

	// --diagnose: write the full technical detail to a file for support, then stop.
	if diagnose {
		return writeDiagnoseBundle(p, resolved, results)
	}

	// 5. Roll the granular checks up into two plain lines the owner can act on.
	p.Newline()
	connected, ready := summarizeDoctor(results, tokenReachable)
	renderHealth(p, connected)
	renderHealth(p, ready)

	if p.Verbose() {
		renderDoctorDetails(p, resolved, results)
	}

	// 6. Verdict + exit code (unchanged: 0 healthy, 2 a problem).
	p.Newline()
	if worseStatus(connected.status, ready.status) == doctor.StatusFail {
		p.Hintf("Still stuck? Email support@tracebloc.io with the output of `%s doctor --diagnose`.", launcher())
		return &exitError{code: exitChecksFailed, err: nil}
	}
	p.Successf("Everything looks good — you're ready to run training.")
	return nil
}

// healthLine is one rolled-up, user-facing health signal: a status, the plain
// line to show, and (on failure) one concrete action.
type healthLine struct {
	status doctor.Status
	text   string
	remedy string
}

// summarizeDoctor collapses the granular checks into the two lines the owner
// reads: "Connected to tracebloc" and "Ready to run training". Each expands to
// the specific plain-language problem + fix on failure; the Kubernetes detail
// stays in --verbose. When the environment is unreachable, readiness can't be
// assessed, so it degrades honestly to "can't check" (never a false ✔).
func summarizeDoctor(results []doctor.Result, tokenReachable bool) (connected, ready healthLine) {
	by := make(map[string]doctor.Result, len(results))
	for _, r := range results {
		by[r.Name] = r
	}
	reach := by["Cluster reachable"]

	switch {
	case reach.Status == doctor.StatusFail:
		// A failed reachability check has three very different fixes; word each
		// from the classification checkReachable attached, never a kubectl. An
		// unclassified fail (e.g. a hand-built result) defaults to "isn't
		// answering" — the safe, pre-existing interpretation.
		switch reach.Reach {
		case doctor.ReachNoEnv:
			connected = healthLine{doctor.StatusFail,
				"No secure environment installed here.",
				"Set one up: " + installCmd}
		case doctor.ReachError:
			connected = healthLine{doctor.StatusFail,
				"Not connected — couldn't read your secure environment.",
				fmt.Sprintf("Email support@tracebloc.io with the output of `%s doctor --diagnose`.", launcher())}
		default: // ReachUnreachable
			connected = healthLine{doctor.StatusFail,
				"Not connected — your secure environment isn't answering.",
				reach.Remedy}
		}
	case !tokenReachable || by["Backend egress (from this machine)"].Status == doctor.StatusFail:
		connected = healthLine{doctor.StatusFail,
			"Not connected — can't reach tracebloc from here.",
			fmt.Sprintf("Check your network / HTTP(S)_PROXY, then run `%s doctor` again.", launcher())}
	case by["Service Bus egress (requests-proxy)"].Status == doctor.StatusFail:
		connected = healthLine{doctor.StatusFail,
			"Training results can't reach tracebloc — experiments will stall.",
			fmt.Sprintf("Email support@tracebloc.io with the output of `%s doctor --diagnose`.", launcher())}
	default:
		connected = healthLine{doctor.StatusOK, "Connected to tracebloc", ""}
	}

	// Readiness is meaningless if we can't even reach the environment.
	if reach.Status == doctor.StatusFail {
		ready = healthLine{doctor.StatusUnknown, "Ready to run training — can't check yet", ""}
		return connected, ready
	}
	switch {
	case by["Pod health"].Status == doctor.StatusFail:
		ready = healthLine{doctor.StatusFail,
			"Not ready — part of your secure environment isn't running.",
			fmt.Sprintf("Reinstall with `%s`, or email support@tracebloc.io with `%s doctor --diagnose`.", installCmd, launcher())}
	case by["Image pull secret"].Status == doctor.StatusFail:
		ready = healthLine{doctor.StatusFail,
			"Not ready — the training images can't be pulled.",
			fmt.Sprintf("Email support@tracebloc.io with the output of `%s doctor --diagnose`.", launcher())}
	case by["Dataset volume (PVC)"].Status == doctor.StatusFail:
		ready = healthLine{doctor.StatusFail,
			"Not ready — dataset storage isn't available.",
			fmt.Sprintf("Email support@tracebloc.io with the output of `%s doctor --diagnose`.", launcher())}
	case by["Node capacity"].Status == doctor.StatusFail:
		ready = healthLine{doctor.StatusFail,
			"Not ready — not enough free compute to start a training.",
			"Free some up, or raise the machine's allocation in Docker Desktop → Resources."}
	default:
		ready = healthLine{doctor.StatusOK, "Ready to run training", ""}
	}
	return connected, ready
}

// renderHealth prints one rolled-up line: ✔ for OK, ✖ + remedy for a problem,
// and a neutral · "can't check" for StatusUnknown (no false green, no alarm).
func renderHealth(p *ui.Printer, h healthLine) {
	switch h.status {
	case doctor.StatusOK:
		p.Successf("%s", h.text)
	case doctor.StatusUnknown:
		p.Infof("%s", h.text)
	default:
		p.Errorf("%s", h.text)
		if h.remedy != "" {
			p.Hintf("     %s", h.remedy)
		}
	}
}

// renderDoctorDetails is the --verbose (and --diagnose) technical breakdown:
// kubeconfig + every granular check. This is the only place Kubernetes
// vocabulary appears.
func renderDoctorDetails(p *ui.Printer, resolved *cluster.ResolvedConfig, results []doctor.Result) {
	p.Newline()
	p.Section("Details (for support)")
	p.Field("context", resolved.Context)
	p.Field("server", resolved.ServerURL)
	p.Field("namespace", resolved.Namespace)
	for _, r := range results {
		mark := "·"
		switch r.Status {
		case doctor.StatusOK:
			mark = "OK  "
		case doctor.StatusWarn:
			mark = "WARN"
		case doctor.StatusFail:
			mark = "FAIL"
		}
		p.Detailf("%s %s — %s", mark, r.Name, r.Detail)
		if r.Remedy != "" {
			p.Detailf("       %s", r.Remedy)
		}
	}
}

// writeDiagnoseBundle owns the support bundle (moved out of install-k8s.sh, which
// the user may not have on disk). It writes the redacted technical breakdown to a
// file the owner emails to support.
func writeDiagnoseBundle(p *ui.Printer, resolved *cluster.ResolvedConfig, results []doctor.Result) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tracebloc doctor — support bundle (%s)\n", time.Now().Format(time.RFC3339))
	bp := ui.New(&buf, ui.WithColor(false), ui.WithVerbose(true))
	renderDoctorDetails(bp, resolved, results)

	name := fmt.Sprintf("tracebloc-doctor-%s.txt", time.Now().Format("20060102-150405"))
	if werr := os.WriteFile(name, buf.Bytes(), 0o600); werr != nil {
		p.Errorf("Couldn't write the support bundle: %v", werr)
		return &exitError{code: exitLocalEnv, err: nil}
	}
	p.Successf("Wrote a support bundle to ./%s", name)
	p.Hintf("     Email it to support@tracebloc.io.")
	return nil
}

// launcher resolves the command name to print in remedies: "tb" on a real
// install (the alias is beside the CLI), else the invoked name — same rule the
// home screen uses, so copy-paste always works.
func launcher() string {
	if tbAliasAvailable() {
		return binTB
	}
	return invokedName()
}

// envDisplayName is the secure environment's user-facing handle — the namespace
// slug (RFC-0001 §7: the slug IS the handle).
func envDisplayName(r *cluster.ResolvedConfig) string {
	if r != nil && r.Namespace != "" {
		return r.Namespace
	}
	return "your secure environment"
}

// worseStatus returns the more severe of two doctor statuses (Fail > Warn > OK).
// StatusUnknown carries no signal, so it never worsens the verdict.
func worseStatus(a, b doctor.Status) doctor.Status {
	if a == doctor.StatusFail || b == doctor.StatusFail {
		return doctor.StatusFail
	}
	if a == doctor.StatusWarn || b == doctor.StatusWarn {
		return doctor.StatusWarn
	}
	return doctor.StatusOK
}
