package cli

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// newVersionCmd implements `tracebloc version`. Two output modes:
//
//   - default: a single human-readable line, easy to eyeball
//   - --output json: structured, easy to consume from CI and from the
//     forthcoming `tracebloc upgrade` command which compares the local
//     version against the latest GitHub release
//
// The version + gitSHA + buildDate fields come from -ldflags injection
// at build time (see cmd/tracebloc/main.go). For a `go run` or
// `go build` without flags they're "dev" / "unknown" / "unknown" —
// that's the right signal to support that they're not on a release
// build.
func newVersionCmd(info BuildInfo) *cobra.Command {
	var outputJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the tracebloc CLI version, git SHA, and build date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			payload := versionPayload{
				Version:   info.Version,
				GitSHA:    info.GitSHA,
				BuildDate: info.BuildDate,
				GoVersion: runtime.Version(),
				Platform:  fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
			}

			if outputJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(payload)
			}

			// Default human-readable line. Order matches the JSON
			// output for grep-ability across modes. Propagate the
			// Fprintf error (it returns one when the underlying
			// writer fails — e.g. a closed pipe) so cobra surfaces
			// it via the standard exit path; silently discarding
			// would mask the rare-but-real "wrote to a dead pipe"
			// case.
			_, err := fmt.Fprintf(
				cmd.OutOrStdout(),
				"tracebloc %s (%s, built %s, %s on %s)\n",
				payload.Version, payload.GitSHA, payload.BuildDate,
				payload.GoVersion, payload.Platform,
			)
			return err
		},
	}

	cmd.Flags().BoolVar(
		&outputJSON, "output-json", false,
		"emit the version payload as indented JSON instead of a single human-readable line",
	)

	return cmd
}

// versionPayload is the shape returned by `--output-json`. Public-ish
// field names (PascalCase + json tags) so external consumers can
// depend on the schema across versions.
type versionPayload struct {
	Version   string `json:"version"`
	GitSHA    string `json:"git_sha"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}
