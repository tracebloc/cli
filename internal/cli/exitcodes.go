// Named constants for every exit code the CLI produces. exit.go owns the
// extraction (ExitCodeFromError); this file owns the naming, so a reviewer
// reading `&exitError{code: exitTableExists, …}` never has to hold the
// number table in their head.

package cli

// Exit codes are the CLI's scripting contract: customers branch on them.
// docs/troubleshooting.md carries the cross-command table; `tracebloc data
// ingest --help` documents the fullest per-command list. Every non-test
// &exitError construction site names its code with one of these constants.
// The numeric values are FROZEN — changing one breaks customer scripts; add
// a new code instead of repurposing an old one.
//
// A few numbers carry more than one per-command meaning (they grew
// per-command before this file centralized the names). Those get one
// constant per MEANING sharing the value, so each construction site stays
// honest and the docs table maps number → per-command meaning.
const (
	// exitOK: success. Includes --dry-run completing, a guided run the
	// user cancelled cleanly, and doctor passing with warnings only.
	exitOK = 0

	// exitFailure: generic failure with no more specific bucket (cobra
	// usage errors; auth / client / delete command failures). Also what
	// ExitCodeFromError maps any non-exitError error to.
	exitFailure = 1

	// exitBadInput: the input didn't validate — a schema violation in a
	// spec (synthesized from flags or authored as YAML), an unsupported or
	// unknown --task, a misapplied task-scoped flag, an invalid table
	// name, or a resource size that doesn't fit this machine.
	exitBadInput = 2

	// exitChecksFailed: doctor only — one or more checks failed. Shares 2
	// with exitBadInput (both are "the CLI examined it; it isn't right").
	exitChecksFailed = 2

	// exitLocalEnv: the local environment refused — kubeconfig couldn't be
	// loaded, the dataset path is missing/unreadable, the local layout is
	// wrong, a YAML file didn't parse, or a prompt was needed off a
	// terminal (--no-input / --output-json).
	exitLocalEnv = 3

	// exitNoWorkspace: the cluster is reachable but no tracebloc client
	// (parent release) was found in the namespace — or its shared storage
	// or dataset list is missing, so the target can't be confirmed.
	exitNoWorkspace = 4

	// exitAuth: an ingestor SA token couldn't be minted, or jobs-manager
	// rejected it (401/403).
	exitAuth = 5

	// exitNoSuchDataset: data delete only — no dataset by that name on
	// this client (nothing to delete). Shares 5 with exitAuth; the
	// per-command meanings predate this file and are frozen with it.
	exitNoSuchDataset = 5

	// exitTableExists: the destination table already exists — re-run with
	// --overwrite to replace it, or pick a different --name.
	exitTableExists = 6

	// The exit-7 trio: an in-cluster operation failed midway. One value,
	// three meanings, named per site:
	//
	// exitStagingFailed: pre-flight succeeded but staging the files into
	// the workspace failed (Pod creation, image pull, exec stream, or
	// remote tar error).
	exitStagingFailed = 7
	// exitTeardownFailed: removing an existing table + its files failed
	// partway (data delete, or the teardown data ingest --overwrite runs).
	exitTeardownFailed = 7
	// exitQueryFailed: data list only — the cluster couldn't be queried
	// for its datasets.
	exitQueryFailed = 7

	// exitSubmitFailed: jobs-manager rejected the submitted run (a
	// non-auth 4xx/5xx), or the port-forward to it couldn't be set up.
	exitSubmitFailed = 8

	// exitIngestFailed: the ingestion Job exited non-zero, completed with
	// row-level failures the summary panel reports, or its outcome
	// couldn't be determined / followed within the watch window.
	exitIngestFailed = 9

	// exitInterrupted: the user hit Ctrl-C at an interactive prompt
	// (128+SIGINT, the shell convention). Emitted silent (err == nil) so
	// main() prints no "Error:" line on the way out.
	exitInterrupted = 130
)
