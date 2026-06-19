package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// newClientCmd wires the `tracebloc client` subtree — provisioning and
// selecting the client (machine) this host enrolls as. The verbs are stubbed
// here: the implementation is cli#84 and depends on the backend device-grant
// (backend#835, for the user token from `tracebloc login`) and provisioning
// (backend#836). The command shape is in place now so the tree + help are
// stable and `--name`/`--location` are pinned.
func newClientCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Provision and manage tracebloc clients (machines)",
		Long: `Provision a tracebloc client for this machine and list/select clients
in your account.

Requires sign-in first (` + "`tracebloc login`" + `). Implemented in cli#84;
the backend it calls lands in backend#835 / #836.`,
	}
	cmd.AddCommand(newClientCreateCmd(), newClientListCmd(), newClientUseCmd())
	return cmd
}

// errClientNotYet is the shared "this lands in cli#84" stub error.
func errClientNotYet() error {
	return &exitError{code: 1, err: errors.New(
		"`tracebloc client` is not implemented yet — it lands in cli#84 and needs the " +
			"backend device-grant (backend#835) + provisioning (backend#836). " +
			"`tracebloc login` is the first piece.")}
}

func newClientCreateCmd() *cobra.Command {
	var name, location string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Provision a new client for this machine (--name, --location)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return errClientNotYet()
		},
	}
	cmd.Flags().StringVar(&name, "name", "",
		"human-readable client name (shown on your dashboard + carbon reports)")
	cmd.Flags().StringVar(&location, "location", "",
		"physical location zone for carbon footprint (e.g. DE); auto-detected + confirmed if omitted")
	return cmd
}

func newClientListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the clients in your account",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return errClientNotYet()
		},
	}
}

func newClientUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <client-id>",
		Short: "Enroll this machine as an existing client",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, _ []string) error {
			return errClientNotYet()
		},
	}
}
