package cli

// Draining the INSTALLER's spool — backend#2217, option (b).
//
// THE PROBLEM THIS SOLVES. `scripts/lib/telemetry.sh` produces one contract event
// per install run and spools it, but it cannot deliver: the ingest endpoint needs
// a bearer credential and the installer holds only `TRACEBLOC_CLIENT_ID` /
// `TRACEBLOC_CLIENT_PASSWORD`, a provisioning pair with no exchange for a token.
// It also never reads the CLI's config. So its records had no route at all, and
// #1906 cannot help — that is a pod Collector reading container stdout, and these
// are files on the operator's own machine, often written when no cluster exists.
//
// WHY THE CLI AND NOT THE INSTALLER. The CLI already owns the token (device login
// writes it to `~/.tracebloc/config.json`), and since #542 it already owns a
// spool, a drain loop and the OTLP mapping. Teaching it one more file to read
// makes the CLI the single credential holder and leaves the installer with no
// credential handling at all. The alternative — the installer reading the CLI's
// config — delivers nothing for the failures that matter most: `validate_config`
// and `early_data_dir_guard` run BEFORE provisioning, so there is no token on
// disk yet at the moment those events are written. Those are precisely the
// failures the installer's `$TMPDIR` fallback exists to preserve.
//
// THE FALLBACK PATH IS A GLOB, NOT AN INDEX. `_telemetry_fallback_spool` uses
// `mktemp .../tracebloc-telemetry-XXXXXX`, so the exact name is unpredictable but
// the PATTERN is fixed. Globbing it needs no change to the installer and no new
// shared state — an index file would be a second thing to keep in step, and the
// thing it indexed would still have to exist.
//
// EVERY RECORD IS FILTERED BY ITS OWN ENVIRONMENT, and this is not optional. The
// CLI's own spool is partitioned by env in the FILENAME (see telemetrySpoolPath);
// the installer's is not, and its records carry whatever `CLIENT_ENV` that run
// used. Forwarding them blind would post a `prod`-labelled install failure to
// whichever backend this CLI invocation happens to point at — the exact
// label-versus-destination leak #542's second review finding was about. So a
// record is forwarded only when its `deployment.environment` matches this run's,
// and the rest are left where they are for a future invocation against that env.

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	// installerSpoolGlob matches `_telemetry_fallback_spool`'s mktemp template.
	installerSpoolGlob = "tracebloc-telemetry-*"

	// installerDrainMax bounds how many installer records one invocation carries,
	// on top of the CLI's own. Deliberately small: the installer's spool caps at
	// TB_TELEMETRY_SPOOL_MAX=50, and a `tracebloc login` should not turn into the
	// largest request this host has ever sent.
	installerDrainMax = 10
)

// resourceEnvironment is the attribute the filter reads. Resource scope, so it is
// on the resource map rather than the record's own attributes.
const resourceEnvironment = "deployment.environment"

// installerFallbackDirVars names every environment variable an installer twin may
// pick its pre-log fallback directory from, in the order the twins try them.
//
// TRANSCRIBED FROM THE PRODUCERS, and the residual is stated rather than hidden:
// they live in `tracebloc/client`, so nothing in this repo's CI can prove this
// list still agrees with them. Keeping them in step is a review rule until the
// two repos share a fixture.
//
//	scripts/lib/telemetry.sh  _telemetry_fallback_dir
//	    $TMPDIR -> $HOME -> /tmp
//	scripts/lib/telemetry.ps1 Get-TelemetryFallbackSpool
//	    $USERPROFILE -> $HOME -> [IO.Path]::GetTempPath(), i.e. $TMP / $TEMP
//
// THE WINDOWS HALF WAS MISSING, and that was the whole defect (backend#2377).
// Windows does not set `TMPDIR` and usually does not set `HOME` — `USERPROFILE`
// is its home variable — so the search reduced to `/tmp`, a path that does not
// exist there. Every Windows pre-log install failure was written and never
// collected, which is exactly the class the fallback exists for: `validate_config`
// and `early_data_dir_guard` run before there is a log or a data dir, so the
// fallback file is their only record.
//
// The glob is unaffected: `tracebloc-telemetry-*` already matches the twin's
// `tracebloc-telemetry-<id>.jsonl` as well as bash's suffix-less mktemp name.
var installerFallbackDirVars = []string{"TMPDIR", "HOME", "USERPROFILE", "TEMP", "TMP"}

// installerSpoolFiles returns every file that may hold installer records.
//
// Ordered predictable-first so a run with both delivers the data-dir spool before
// the scratch files, which is the order they were written in the common case.
// Missing files and unreadable directories are simply absent from the result:
// this runs on the exit path of every command and may never report a problem.
func installerSpoolFiles(getenv func(string) string) []string {
	var out []string

	// 1. The data-dir spool, which the installer writes once HOST_DATA_DIR exists.
	//    Same default the installer uses: $HOST_DATA_DIR, else ~/.tracebloc.
	base := strings.TrimSpace(getenv("HOST_DATA_DIR"))
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".tracebloc")
		}
	}
	if base != "" {
		out = append(out, filepath.Join(base, "telemetry", "pending.jsonl"))
	}

	// 2. The pre-log fallback files. Each twin picks ONE directory out of its own
	//    chain (installerFallbackDirVars records both chains), and bash also
	//    disqualifies $TMPDIR when the installer is running from inside it. From
	//    here we cannot tell which it chose, so every candidate is searched; a
	//    glob that matches nothing costs one syscall.
	candidates := make([]string, 0, len(installerFallbackDirVars)+1)
	for _, name := range installerFallbackDirVars {
		candidates = append(candidates, strings.TrimSpace(getenv(name)))
	}
	// bash's last resort, which is a literal rather than a variable.
	candidates = append(candidates, "/tmp")

	seen := map[string]bool{}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		// Clean, not TrimRight("/"): it normalises a trailing separator on BOTH
		// platforms, so `C:\Users\me\` and `C:\Users\me` dedupe as one directory.
		// Two names for one directory would forward the same install outcome
		// twice in a single batch.
		dir = filepath.Clean(dir)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		matches, err := filepath.Glob(filepath.Join(dir, installerSpoolGlob))
		if err != nil {
			continue
		}
		out = append(out, matches...)
	}
	return out
}

// installerRecords reads events for `env` out of the installer's spools.
//
// Returns the matching events and, per file, the events that did NOT match so the
// caller can write them back. A file whose records are all foreign is left
// untouched rather than rewritten — rewriting another component's file to change
// nothing is a needless risk on a path that must never disturb an install.
type installerBatch struct {
	// Events for this environment, ready to forward.
	events []spooledEvent
	// Per source file, the events that stay behind. Only files that actually
	// contributed a forwarded event appear here.
	remainder map[string][]spooledEvent
}

func installerRecords(files []string, env string, max int) installerBatch {
	batch := installerBatch{remainder: map[string][]spooledEvent{}}
	for _, path := range files {
		if len(batch.events) >= max {
			return batch
		}
		records := readSpool(path)
		if len(records) == 0 {
			continue
		}
		var mine, theirs []spooledEvent
		for _, rec := range records {
			// A record with no environment is NOT forwarded. The contract omits an
			// attribute rather than sending it empty, so an absent environment
			// means the emitter could not resolve one — and a record no query can
			// filter on is the defect the contract exists to remove. Left in place
			// rather than dropped: it is still evidence, just not deliverable.
			if rec.Resource[resourceEnvironment] == env && len(batch.events)+len(mine) < max {
				mine = append(mine, rec)
				continue
			}
			theirs = append(theirs, rec)
		}
		if len(mine) == 0 {
			continue
		}
		batch.events = append(batch.events, mine...)
		batch.remainder[path] = theirs
	}
	return batch
}

// clearInstallerRecords rewrites each source file with only the records that were
// left behind, deleting it when nothing remains.
//
// CALLED ONLY AFTER A SUCCESSFUL DELIVERY. On any failure the files are untouched,
// so the next invocation retries them — the same posture as the CLI's own spool.
// Reuses `writeSpool`, so these files inherit 0600 and the atomic temp+rename.
func clearInstallerRecords(remainder map[string][]spooledEvent) {
	for path, keep := range remainder {
		// writeSpool removes the file when `keep` is empty, which is right for the
		// mktemp fallbacks: the installer writes exactly one record per file and
		// never returns to them, so an emptied one is litter.
		_ = writeSpool(path, keep)
	}
}
