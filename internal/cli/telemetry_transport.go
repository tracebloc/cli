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
func telemetrySpoolPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "telemetry", "pending.jsonl"), nil
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

// writeSpool replaces the spool with events, keeping the NEWEST
// telemetrySpoolMax and dropping the oldest past it.
//
// Written to a temp file and renamed, so a crash mid-write cannot leave a
// truncated spool. Two concurrent CLI commands can still lose a record to a
// last-writer-wins rename; that is accepted rather than locked, because a lock
// file on the exit path of every command is a new way for telemetry to hang the
// product, which is the one thing it may not do.
func writeSpool(path string, events []spooledEvent) error {
	if len(events) > telemetrySpoolMax {
		events = events[len(events)-telemetrySpoolMax:]
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if len(events) == 0 {
		err := os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
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
// TAKES THE RESOLVED URL, not an env to resolve for itself. Two reasons, and the
// second is the one that bit: a single resolution point means the record's label
// and its destination cannot disagree, and a `deliver` that computed
// api.BaseURL() internally had no seam — its own test posted to PRODUCTION.
func deliver(url, token string, ev spooledEvent, now time.Time) {
	path, err := telemetrySpoolPath()
	if err != nil {
		return
	}
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

	// No token means not signed in. Spool rather than attempt: the events are
	// still worth sending after the next login, and a POST with no credential
	// would spend the budget earning a 401.
	if token == "" {
		_ = writeSpool(path, append(pending, ev))
		return
	}

	ctx, cancel := context.WithDeadline(context.Background(), now.Add(telemetryBudget))
	defer cancel()

	switch postBatch(ctx, url, token, batch) {
	case postDelivered, postDiscard:
		// Both consume the batch. The difference is only whether the server
		// stored it, and neither is a reason to carry it again.
		_ = writeSpool(path, pending[len(drained):])
	case postRetry:
		_ = writeSpool(path, append(pending, ev))
	}
}

// telemetryToken reads the bearer token for env, best-effort.
//
// Read at delivery time rather than at startup so a command that signs in can
// deliver its own outcome event.
func telemetryToken(env string) string {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return ""
	}
	if p := cfg.Profile(env); p != nil {
		return p.Token
	}
	return ""
}

// pendingSink is the transport for backend#2217.
//
// Returns nil — validate-and-drop, telemetry.SetSink's documented contract —
// only when there is nowhere to spool and nothing to post to. Everything else
// is handled inside deliver, silently.
func pendingSink(env string) telemetry.Sink {
	url := api.BaseURL(env) + telemetryIngestPath
	return func(resource map[string]string, record map[string]any) {
		deliver(url, telemetryToken(env), spooledEvent{
			Resource:   resource,
			Attributes: record,
		}, time.Now())
	}
}
