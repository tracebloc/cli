package cli

import "github.com/spf13/cobra"

// Shared usage strings for the cluster-targeting flags. The --kubeconfig and
// --context phrasing is shared by the cluster-info / data* / doctor commands
// (the client-anchor and offboard-delete commands word theirs differently and
// pass their own literals); namespaceFlagUsage is used by the commands that
// take the separate --namespace/-n flag via addNamespaceFlag.
const (
	kubeconfigFlagUsage = "path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)"
	contextFlagUsage    = "name of the kubeconfig context to use (default: kubeconfig's current-context)"
	namespaceFlagUsage  = "namespace where your tracebloc client is installed"
)

// addKubeconfigFlags registers the shared --kubeconfig/--context pair on cmd,
// binding them to the caller's variables. The name, default ("") and (lack of)
// shorthand match kubectl conventions; only the usage text is parametrized so
// each command can keep its own phrasing while the flag mechanics live in one
// place.
func addKubeconfigFlags(cmd *cobra.Command, kubeconfig, context *string, kubeconfigUsage, contextUsage string) {
	cmd.Flags().StringVar(kubeconfig, "kubeconfig", "", kubeconfigUsage)
	cmd.Flags().StringVar(context, "context", "", contextUsage)
}

// addNamespaceFlag registers the shared -n/--namespace flag. It is intentionally
// separate from addKubeconfigFlags so commands that take a kubeconfig but no
// namespace (e.g. `client create`) don't grow a spurious --namespace flag.
func addNamespaceFlag(cmd *cobra.Command, namespace *string, usage string) {
	cmd.Flags().StringVarP(namespace, "namespace", "n", "", usage)
}
