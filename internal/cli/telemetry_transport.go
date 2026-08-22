package cli

// Host telemetry transport for the CLI — backend#2217, option (c).
//
// WHAT WAS HERE BEFORE: `pendingSink()` returned nil, so every event was
// validated and dropped. The endpoint now exists (backend#1905/#2213), so this
// file is the delivery half.
//
// THE SHAPE, decided on backend#2217 rather than invented here: one inline POST
// attempt with a ~1s budget, spool to disk on any failure, drain a bounded
// number of pending events on the next invocation. The deciding argument
// against plain fire-and-forget is that IN A SHORT-LIVED CLI, ASYNC DELIVERY IS
// A LIE — a goroutine that outlives main() is killed at exit, so the only honest
// forms of "don't block" are "always block" or "always drop on failure", and
// dropping on failure discards precisely the partition-time events that are the
// most valuable thing the CLI can report. Hence: block briefly, then persist.
//
// Explicit non-goals, so nobody adds them later believing they were forgotten:
// no background daemon, no goroutine outliving the process, no retry loop.
// ONE attempt. The next invocation is the retry.
//
// THE SPOOL KEEPS DROP-OLDEST, AND THAT IS NOT A CONTRADICTION OF D7. D7's
// amended overflow row says drop-NEWEST because `exporterhelper` sheds at the
// entrance and offers nothing else — a platform constraint on the edge
// Collector, not a preference. This spool is our own code and can do what D7
// originally WANTED, so it does: the newest records describe the incident in
// progress. Same reasoning as `scripts/lib/telemetry.sh`'s `tail -n` trim in the
// installer, which this mirrors deliberately (0600, capped, oldest dropped).
//
// EVERY FAILURE PATH HERE IS SILENT AND RETURNS. A CLI that printed a warning
// because telemetry could not reach the backend would be a worse CLI, and the
// emitter's own rule is that no telemetry path may fail or delay the product.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/telemetry"
)

const (
	// telemetryIngestPath is the ingest boundary's versioned path
	// (backend/metaApi/urls.py). Versioned because a spooled payload written by
	// an old CLI can arrive after the backend has moved on.
	telemetryIngestPath = "/telemetry/v1/records/"

	// telemetryBudget bounds the WHOLE telemetry step — drain included — not
	// each attempt. #2217 sets ~1s per command.
	telemetryBudget = time.Second

	// telemetrySpoolMax caps the spool, mirroring the installer's
	// TB_TELEMETRY_SPOOL_MAX. Oldest are dropped past it.
	telemetrySpoolMax = 50

	// telemetryDrainMax bounds how many pending events one invocation carries.
	// Smaller than the cap on purpose: a full spool must not turn the next
	// command into the biggest request the endpoint sees from this host.
	telemetryDrainMax = 20

	// telemetrySpoolReadCap bounds a pathological file. We write this file, so
	// it cannot legitimately exceed the cap — but a corrupted or hand-edited one
	// must not be able to stall a CLI command.
	telemetrySpoolReadCap = 1 << 20 // 1 MiB
)

// telemetrySpoolPath is where unsent events wait, under the CLI's own config
// directory so it honours $TRACEBLOC_CONFIG_DIR like everything else.
//
// ONE SPOOL PER ENVIRONMENT, and the partition is a correctness requirement
// rather than tidiness. A single host-wide `pending.jsonl` leaks across
// invocations: records queued while signed in to prod get drained by the next
// command — which may be `tracebloc login --env dev` — and POSTed to dev's
// endpoint with dev's token while still carrying `deployment.environment=prod`.
// That is the same label-versus-destination mismatch `deliver` takes a resolved
// URL to prevent, reopened one level up, across invocations instead of inside
// one. (Bugbot on #542; reproduced — a prod-labelled record arrived at a dev
// endpoint before this change.)
//
// The consequence, stated rather than hidden: records for an environment the
// operator never uses again are never delivered. That is the right trade — they
// are capped, and there is usually no token for that environment to deliver them
// with anyway. Delivering them to the WRONG backend is not a better outcome than
// not delivering them.
func telemetrySpoolPath(env string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "telemetry", "pending-"+spoolEnvSlug(env)+".jsonl"), nil
}

// spoolEnvSlug reduces env to something safe in a filename.
//
// The real values are a closed set (dev/stg/prod), so this never fires in
// practice — it exists because a path segment built from a string is a path
// traversal waiting for the day that string stops being closed, and "../.." is a
// worse bug than an ugly filename. Anything unexpected becomes one bucket rather
// than being silently dropped: an undeliverable record is still evidence.
func spoolEnvSlug(env string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(env)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// readSpool returns every parseable event, oldest first.
//
// A malformed line is SKIPPED, not fatal: a torn write costs one record, and
// refusing the file would strand every good record behind it forever.
func readSpool(path string) []spooledEvent {
	f, err := os.Open(path) // #nosec G304 -- the CLI's own spool under config.Dir()
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var out []spooledEvent
	scanner := bufio.NewScanner(io.LimitReader(f, telemetrySpoolReadCap))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev spooledEvent
		// UseNumber, NOT json.Unmarshal — and this is the difference between the
		// first delivery and every retry sending the same event two ways.
		//
		// A plain unmarshal decodes every JSON number into float64, so an
		// `exit_code` that went out as a canonical `intValue` string on the
		// in-memory attempt came back as a float and went out as `doubleValue` on
		// the drained one. Same event, two encodings, and the wrong one on the
		// path the spool exists for — the partition-time retry. `json.Number`
		// defers the choice to `anyValue`, which can then still tell an integer
		// from a real. (@saqlainsyed007 and Bugbot on #542; reproduced before
		// fixing, and `TestSpoolRoundTripPreservesIntegerEncoding` is the
		// regression — the pre-existing integer test worked on the in-memory
		// shape only and stayed green through this.)
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&ev); err != nil {
			continue
		}
		if len(ev.Resource) == 0 && len(ev.Attributes) == 0 {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// wipedHostDir records the directory THIS process deliberately removed — the
// `tracebloc delete` offboard on its default (no `--keep-data`) path. Spool
// writes under it are dropped for the rest of the process: see writeSpool.
//
// WHY A LATCH AND NOT A CHECK. main.go emits the command-outcome event AFTER the
// command tree returns, so the offboard has already deleted ~/.tracebloc by the
// time telemetry runs — and telemetry's spool lives INSIDE that tree
// (<config.Dir()>/telemetry/pending-<env>.jsonl). "Does the dir exist?" is the
// wrong question: it doesn't, and writeSpool's job is to create it. The question
// is whether its absence is a fresh install (spool away) or a wipe the user just
// asked for (don't), and only the offboard knows which. So the offboard says so.
//
// backend#2314: `tracebloc delete` printed "✔ Removed local tracebloc data and
// config." and then the exit-path telemetry write re-created the tree behind it —
// empty when delivery succeeded, and holding an undeliverable event record when
// it didn't (after the wipe there is no token left to deliver with, so deliver
// takes its no-token spool branch every time). An explicit wipe that the CLI
// silently undoes on the way out is a broken promise, and a dropped telemetry
// record is unambiguously the cheaper loss.
//
// IT HOLDS THE WIPED DIRECTORY, NOT JUST A BOOLEAN, and that is about blast
// radius rather than precision for its own sake. A bare "telemetry is off now"
// flag is unscoped: it silences every later writeSpool in the process, including
// one for a path the offboard never touched. It is also permanently sticky in a
// test binary — `delete`'s own unit tests drive the real offboard, so a boolean
// latched there stays latched for every test that runs after it, and three
// unrelated spool tests failed exactly that way while this was being written.
// Recording the path answers the narrower and more useful question: is THIS
// spool inside the tree we removed?
//
// atomic.Value, not a plain string: the sink runs on the exit path while nothing
// else should still be writing, but "should" is not a guarantee and the race
// detector runs in CI.
var wipedHostDir atomic.Value // string

// markHostStateWiped records the directory this process deliberately removed, so
// the exit-path telemetry write does not resurrect it. Called by the offboard.
func markHostStateWiped(dir string) { wipedHostDir.Store(dir) }

// insideWipedHostDir reports whether path lies within a directory this process
// deliberately removed.
func insideWipedHostDir(path string) bool {
	root, _ := wipedHostDir.Load().(string)
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		// Different volumes, so not inside it.
		return false
	}
	// filepath.Rel returns a ".."-prefixed path for anything outside root, and
	// no error — so the prefix test, not the error, is what decides this.
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// writeSpool replaces the spool with events, keeping the NEWEST
// telemetrySpoolMax and dropping the oldest past it.
//
// Written to a temp file and renamed, so a crash mid-write cannot leave a
// truncated spool. Two concurrent CLI commands can still lose a record to a
// last-writer-wins rename; that is accepted rather than locked, because a lock
// file on the exit path of every command is a new way for telemetry to hang the
// product, which is the one thing it may not do.
func writeSpool(path string, events []spooledEvent) error {
	// The offboard removed the tree this spool lives in. Writing here would
	// re-create it — see wipedHostDir (backend#2314). Nothing to remove either:
	// the file went with the directory.
	if insideWipedHostDir(path) {
		return nil
	}
	if len(events) > telemetrySpoolMax {
		events = events[len(events)-telemetrySpoolMax:]
	}
	dir := filepath.Dir(path)
	if len(events) == 0 {
		// Nothing to write, so do NOT create the directory on the way to
		// deleting a file inside it — the MkdirAll used to run before this
		// branch, which re-created a wiped ~/.tracebloc/telemetry/ even on the
		// delivered path, where the spool is being emptied rather than filled
		// (backend#2314).
		err := os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "pending-*.jsonl")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// postOutcome classifies one delivery attempt.
type postOutcome int

const (
	postDelivered postOutcome = iota // 2xx — the batch is gone
	postRetry                        // spool it; a later run may succeed
	postDiscard                      // permanently unacceptable; stop carrying it
)

// classifyStatus maps an HTTP status to what to do with the batch.
//
// THE DISCARD CASE IS THE ONE THAT MATTERS. The endpoint answers 400 when a
// whole batch is unparseable. Re-spooling that would wedge the spool: the same
// bad batch would be re-sent by every future command, forever, and would push
// out good records at the cap. A permanent refusal must consume the batch.
//
// 401/403 retry rather than discard because they are a CREDENTIAL state, not a
// payload verdict — an expired token is refreshed by the next `tracebloc login`,
// and the records are still worth sending after it. 408/429 are explicitly
// transient. Everything 5xx is transient by definition.
func classifyStatus(code int) postOutcome {
	switch {
	case code >= 200 && code < 300:
		return postDelivered
	case code == http.StatusUnauthorized, code == http.StatusForbidden,
		code == http.StatusRequestTimeout, code == http.StatusTooManyRequests:
		return postRetry
	case code >= 400 && code < 500:
		return postDiscard
	default:
		return postRetry
	}
}

// postBatch makes the single delivery attempt.
func postBatch(ctx context.Context, url, token string, batch []spooledEvent) postOutcome {
	payload, err := otlpPayload(batch)
	if err != nil {
		// Nothing a later run would render differently.
		return postDiscard
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return postDiscard
	}
	req.Header.Set("Content-Type", "application/json")
	// Bearer, matching what internal/api already sends and what the endpoint's
	// TelemetryIngestAuthentication expects (its ClientAccessTokenAuthentication
	// base declares keyword = "Bearer"). The legacy per-user DRF token uses
	// keyword "Token"; sending the wrong one 401s silently.
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Timeout, DNS, refused connection — exactly the partition case the
		// spool exists for.
		return postRetry
	}
	defer func() { _ = resp.Body.Close() }()
	// Drained so the connection can be reused, and bounded so a hostile body
	// cannot be read into memory on a path that must stay cheap.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return classifyStatus(resp.StatusCode)
}

// deliver is the sink body: drain what is pending, add this event, attempt once,
// and persist whatever did not land.
//
// TAKES THE RESOLVED SPOOL PATH AND URL, not an env to resolve for itself. Three
// reasons, and the last two each bit once: a single resolution point means the
// record's label, its spool and its destination cannot disagree; a `deliver` that
// computed api.BaseURL() internally had no seam — its own test posted to
// PRODUCTION; and one that computed the spool path internally drained another
// environment's queue into this one's endpoint (Bugbot on #542).
func deliver(spool, url, token, env string, ev spooledEvent, now time.Time) {
	path := spool
	pending := readSpool(path)

	// The batch is the OLDEST pending events plus this one. Oldest first so a
	// spool that is persistently over the drain limit still makes progress
	// through its backlog rather than re-sending its tail.
	drained := pending
	if len(drained) > telemetryDrainMax {
		drained = drained[:telemetryDrainMax]
	}
	batch := make([]spooledEvent, 0, len(drained)+1)
	batch = append(batch, drained...)
	batch = append(batch, ev)

	// THE INSTALLER'S RECORDS RIDE ALONG (backend#2217 option b). It produces
	// contract events and cannot deliver them — it holds a provisioning pair, not
	// a bearer token — so the CLI, which does hold one, carries them. Filtered to
	// THIS environment by each record's own `deployment.environment`, because the
	// installer's spool is not partitioned by env the way ours is.
	//
	// Appended AFTER our own, so a full installer spool can never crowd out the
	// event this invocation just produced.
	installer := installerRecords(installerSpoolFiles(os.Getenv), env, installerDrainMax)
	batch = append(batch, installer.events...)

	// No token means not signed in. Spool rather than attempt: the events are
	// still worth sending after the next login, and a POST with no credential
	// would spend the budget earning a 401.
	if token == "" {
		_ = writeSpool(path, append(pending, ev))
		// The installer's files are deliberately NOT touched here. Not signed in
		// means they were never sent, and they are not ours to discard.
		return
	}

	ctx, cancel := context.WithDeadline(context.Background(), now.Add(telemetryBudget))
	defer cancel()

	switch postBatch(ctx, url, token, batch) {
	case postDelivered, postDiscard:
		// Both consume the batch. The difference is only whether the server
		// stored it, and neither is a reason to carry it again.
		_ = writeSpool(path, pending[len(drained):])
		// The installer's files are only ever touched on a path that CONSUMED
		// them. A discard clears them too, for the same reason it clears ours: a
		// permanently unparseable batch carried forever wedges every later send,
		// and these records are as unparseable as the rest of it.
		clearInstallerRecords(installer.remainder)
	case postRetry:
		_ = writeSpool(path, append(pending, ev))
	}
}

// telemetryToken reads the bearer token for the CURRENT SESSION, best-effort.
//
// Read at delivery time rather than at startup so a command that signs in can
// deliver its own outcome event.
//
// TAKES NO env, AND THAT IS THE FIX (tracebloc/cli#552). It used to take the
// telemetry label and look the profile up by it — but profiles are keyed on the
// RAW cfg.CurrentEnv, while the label has been through telemetryEnv, which
// lower-cases, trims (via sessionEnv) and remaps anything unrecognised onto
// prod. Whenever those disagreed the lookup did not miss loudly: Profile()
// CREATED an empty profile and returned no token, so delivery took the
// no-token spool path forever while authedClient — reading cfg.Current(), the
// raw key — kept working. The CLI looked signed in and healthy, and outcomes
// simply never arrived.
//
// The parameter was the whole defect: two keys for one concept. Removing it
// makes them impossible to disagree, rather than making them agree today.
//
// The token still goes to the right host. api.BaseURL routes an unrecognised
// env to prod exactly as telemetryEnv does, so the destination the label picks
// is the destination this session's client already uses. (That BaseURL routes
// unknown envs to prod at all is a real defect, tracked across three components
// on backend#2171 — see telemetryEnv's note. It is not this ticket's to change,
// and this fix deliberately does not diverge from that behaviour.)
func telemetryToken() string {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.CurrentToken()
}

// pendingSink is the transport for backend#2217.
//
// Returns nil — validate-and-drop, telemetry.SetSink's documented contract —
// only when there is nowhere to spool and nothing to post to. Everything else
// is handled inside deliver, silently.
func pendingSink(env string) telemetry.Sink {
	// BOTH derived from the same `env`, once. The record's label comes from the
	// same value (see RecordCommandOutcome), so label, spool and destination are
	// three views of one resolution rather than three chances to disagree.
	url := api.BaseURL(env) + telemetryIngestPath
	spool, err := telemetrySpoolPath(env)
	if err != nil {
		// Nowhere to spool and therefore no safe way to retry. Validate-and-drop
		// is telemetry.SetSink's documented contract for exactly this.
		return nil
	}
	return func(resource map[string]string, record map[string]any) {
		deliver(spool, url, telemetryToken(), env, spooledEvent{
			Resource:   resource,
			Attributes: record,
		}, time.Now())
	}
}
