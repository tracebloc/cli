// Package cluster handles everything the CLI needs to talk to a
// Kubernetes cluster running the tracebloc parent client chart:
//
//   - Locating the customer's kubeconfig + picking a context/namespace
//   - Discovering the parent release (jobs-manager Deployment + its
//     labels)
//   - Minting an ingestor ServiceAccount token via TokenRequest
//
// Everything in this package is exercise-able without a real cluster
// thanks to client-go's fake clientset; see *_test.go.
package cluster

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubeconfigOptions captures every knob the customer can turn when
// telling the CLI which cluster + namespace to talk to. Defaults
// mirror kubectl: $KUBECONFIG > ~/.kube/config; current-context if
// --context unset; the context's default namespace if --namespace
// unset; "default" if even the context doesn't pin one.
//
// All fields are zero-value-safe — an empty Options{} produces the
// same behaviour as kubectl run with no flags.
type KubeconfigOptions struct {
	// Path overrides $KUBECONFIG and ~/.kube/config when set.
	// Honors path expansion (e.g. "~/.kube/config" → absolute).
	Path string

	// Context picks a non-current-context from the loaded
	// kubeconfig. Empty = use whatever current-context points at.
	Context string

	// Namespace overrides the context's default. Empty = pull from
	// the context's namespace, falling back to "default".
	Namespace string
}

// ResolvedConfig is what Load() returns: a usable rest.Config plus
// the source-of-truth metadata that the rest of the CLI needs (the
// namespace it resolved to, the context name it used). The
// metadata is separate from rest.Config because rest.Config doesn't
// carry the context name out of the box — it's discarded after the
// merge — and we want to show the customer "you're talking to
// context X" in diagnostic output.
type ResolvedConfig struct {
	RestConfig *rest.Config
	Context    string
	Namespace  string

	// ServerURL is the cluster's API server URL (the value from
	// the context's cluster.server field). Exposed for `tracebloc
	// cluster info` to show "you're targeting <url>" so the
	// customer can sanity-check before doing anything destructive.
	ServerURL string
}

// Load resolves the kubeconfig + flags into a rest.Config ready for
// kubernetes.NewForConfig(). It does NOT make any network calls —
// pure parsing of local files + env. Network errors only happen
// when the caller subsequently uses the returned RestConfig.
func Load(opts KubeconfigOptions) (*ResolvedConfig, error) {
	// NewDefaultClientConfigLoadingRules() wires up the full
	// kubectl-style lookup chain: ExplicitPath > $KUBECONFIG
	// (multi-file merging) > ~/.kube/config. Constructing
	// ClientConfigLoadingRules{} directly with an empty
	// ExplicitPath does NOT fall back to those defaults — it
	// just refuses to load anything ("no configuration has been
	// provided" was the symptom). Surfaced during the first
	// real-cluster smoke test of `tracebloc cluster info`.
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if explicit := expandPath(opts.Path); explicit != "" {
		// Setting ExplicitPath bypasses the env-var merge
		// (intentional — when a customer says "use THIS file",
		// we don't mix it with their env-var-listed others).
		loadingRules.ExplicitPath = explicit
	}

	overrides := &clientcmd.ConfigOverrides{}
	if opts.Context != "" {
		overrides.CurrentContext = opts.Context
	}
	if opts.Namespace != "" {
		// Setting Context.Namespace overrides the kubeconfig's
		// per-context default. This matches `kubectl --namespace`
		// semantics.
		overrides.Context.Namespace = opts.Namespace
	}

	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

	restConfig, err := loader.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building rest config from kubeconfig: %w", err)
	}

	// Pull the namespace the way kubectl does: explicit override
	// wins; otherwise the context's namespace; otherwise "default".
	// loader.Namespace() handles that fallback chain.
	ns, _, err := loader.Namespace()
	if err != nil {
		return nil, fmt.Errorf("resolving namespace from kubeconfig: %w", err)
	}

	// rest.Config doesn't carry the context name out of the box —
	// fish it back out of the raw kubeconfig so diagnostics can
	// show it. RawConfig() does a fresh read each call; cache if
	// this ever becomes hot (it isn't).
	rawCfg, err := loader.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("reading raw kubeconfig: %w", err)
	}
	contextName := overrides.CurrentContext
	if contextName == "" {
		contextName = rawCfg.CurrentContext
	}

	return &ResolvedConfig{
		RestConfig: restConfig,
		Context:    contextName,
		Namespace:  ns,
		ServerURL:  restConfig.Host,
	}, nil
}

// NewClientset is a tiny wrapper that constructs a kubernetes
// clientset from a ResolvedConfig. Exists so callers don't have to
// import k8s.io/client-go/kubernetes themselves for the common case
// of "I have a ResolvedConfig, I want a clientset."
func NewClientset(rc *ResolvedConfig) (kubernetes.Interface, error) {
	cs, err := kubernetes.NewForConfig(rc.RestConfig)
	if err != nil {
		return nil, fmt.Errorf("constructing kubernetes clientset: %w", err)
	}
	return cs, nil
}

// expandPath handles `~/.kube/config` → `/home/user/.kube/config`
// since clientcmd's ExplicitPath wants an absolute path. Empty
// strings pass through unchanged (they signal "use defaults" to
// clientcmd).
func expandPath(p string) string {
	if p == "" {
		return ""
	}
	if p[0] != '~' {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Best-effort: return the unexpanded path and let
		// clientcmd's error surface mention it. Failing here would
		// hide the more useful "tried to read ~/.kube/config and
		// got X" error downstream.
		return p
	}
	return filepath.Join(home, p[1:])
}
