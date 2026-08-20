package cli

// Tests for the host transport — backend#2217.
//
// Every assertion here is written to FAIL on a specific defect, not to observe
// that the code ran. The review record on this epic (~40 findings across 13 PRs)
// is dominated by one class: a check that cannot fail, wearing a comment saying
// it can. Where a test's whole point is that some value reaches the wire, it
// asserts the value — never merely the absence of an error.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// event builds a distinguishable event.
func event(instanceID string, exitCode int) spooledEvent {
	return spooledEvent{
		Resource: map[string]string{
			"service.name":        "cli",
			"service.instance.id": instanceID,
			"service.version":     "1.2.3",
		},
		Attributes: map[string]any{
			"event.name":                  "cli.command.failed",
			"error.type":                  "usage",
			"tracebloc.cli.exit_code":     exitCode,
			"tracebloc.cli.duration_ms":   int64(41230),
			"tracebloc.cli.interactive":   false,
			"tracebloc.cli.sampling_rate": 0.5,
		},
	}
}

// decode unmarshals a payload into the receiver's view of it.
func decode(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	return doc
}

// flatten reproduces what the receiver's `_flatten` does: resource attributes,
// then the record's own. Derived from common/telemetry/otlp.py so the assertions
// below are about what the BACKEND will see, not about our own nesting.
func flatten(t *testing.T, entry any) map[string]any {
	t.Helper()
	e, ok := entry.(map[string]any)
	if !ok {
		t.Fatalf("resourceLogs entry is not an object: %T", entry)
	}
	out := map[string]any{}
	readAttrs := func(list any) {
		items, ok := list.([]any)
		if !ok {
			return
		}
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			key, _ := m["key"].(string)
			val, ok := m["value"].(map[string]any)
			if !ok || key == "" {
				continue
			}
			for _, kind := range []string{"stringValue", "boolValue", "intValue", "doubleValue"} {
				if v, present := val[kind]; present {
					out[key] = v
					break
				}
			}
		}
	}
	if res, ok := e["resource"].(map[string]any); ok {
		readAttrs(res["attributes"])
	}
	scopes, _ := e["scopeLogs"].([]any)
	for _, s := range scopes {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		recs, _ := sm["logRecords"].([]any)
		for _, r := range recs {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			readAttrs(rm["attributes"])
		}
	}
	return out
}

func resourceLogs(t *testing.T, payload []byte) []any {
	t.Helper()
	rl, ok := decode(t, payload)["resourceLogs"].([]any)
	if !ok {
		t.Fatalf("payload has no resourceLogs list: %s", payload)
	}
	return rl
}

// ---------------------------------------------------------------- the mapping

// The rule the receiver's parser calls out by name, and the one a batching
// implementation gets wrong: each event keeps its OWN resource.
func TestEachEventGetsItsOwnResourceLogsEntry(t *testing.T) {
	payload, err := otlpPayload([]spooledEvent{event("run-a", 1), event("run-b", 2)})
	if err != nil {
		t.Fatalf("otlpPayload: %v", err)
	}
	entries := resourceLogs(t, payload)
	if len(entries) != 2 {
		t.Fatalf("want 2 resourceLogs entries (one per event), got %d", len(entries))
	}
	// Assert the PAIRING, not just the count: a mapping that emitted two entries
	// but copied the first event's resource into both would pass a count check.
	got := map[string]any{}
	for _, e := range entries {
		flat := flatten(t, e)
		got[fmt.Sprint(flat["service.instance.id"])] = flat["tracebloc.cli.exit_code"]
	}
	if got["run-a"] != "1" {
		t.Errorf("run-a should carry exit_code 1, got %v (all: %v)", got["run-a"], got)
	}
	if got["run-b"] != "2" {
		t.Errorf("run-b should carry exit_code 2, got %v (all: %v)", got["run-b"], got)
	}
}

// int64 as a JSON STRING is the canonical proto3 encoding. A number here is
// accepted by our receiver but not canonical, so it is asserted.
func TestIntegersAreEncodedAsStrings(t *testing.T) {
	payload, err := otlpPayload([]spooledEvent{event("run", 2)})
	if err != nil {
		t.Fatalf("otlpPayload: %v", err)
	}
	flat := flatten(t, resourceLogs(t, payload)[0])
	for _, key := range []string{"tracebloc.cli.exit_code", "tracebloc.cli.duration_ms"} {
		v, ok := flat[key].(string)
		if !ok {
			t.Errorf("%s should be a JSON string (proto3 int64), got %T (%v)", key, flat[key], flat[key])
			continue
		}
		if v == "" {
			t.Errorf("%s encoded as an empty string", key)
		}
	}
	if !strings.Contains(string(payload), `"intValue":"2"`) {
		t.Errorf(`payload should contain "intValue":"2"; got %s`, payload)
	}
}

// A bool must not become an int. The receiver reads boolValue BEFORE intValue
// for exactly this reason; the sender must not make it necessary.
func TestBoolsUseBoolValueNotIntValue(t *testing.T) {
	payload, err := otlpPayload([]spooledEvent{event("run", 0)})
	if err != nil {
		t.Fatalf("otlpPayload: %v", err)
	}
	flat := flatten(t, resourceLogs(t, payload)[0])
	if v, ok := flat["tracebloc.cli.interactive"].(bool); !ok || v {
		t.Errorf("interactive should be boolean false, got %T (%v)", flat["tracebloc.cli.interactive"], flat["tracebloc.cli.interactive"])
	}
}

// THE DEFECT THIS EXISTS FOR. The emitter admits named scalar types by
// reflect.Kind; a concrete type switch at the seam would drop them silently.
// This test fails if anyValue is ever rewritten as `switch v := value.(type)`.
func TestNamedScalarTypesSurviveTheSeam(t *testing.T) {
	type reason string
	type attempts int
	ev := spooledEvent{
		Resource:   map[string]string{"service.name": "cli"},
		Attributes: map[string]any{"tracebloc.cli.reason": reason("quota"), "tracebloc.cli.attempts": attempts(3)},
	}
	payload, err := otlpPayload([]spooledEvent{ev})
	if err != nil {
		t.Fatalf("otlpPayload: %v", err)
	}
	flat := flatten(t, resourceLogs(t, payload)[0])
	if flat["tracebloc.cli.reason"] != "quota" {
		t.Errorf("named string type dropped at the seam: got %v", flat["tracebloc.cli.reason"])
	}
	if flat["tracebloc.cli.attempts"] != "3" {
		t.Errorf("named int type dropped at the seam: got %v", flat["tracebloc.cli.attempts"])
	}
}

// ------------------------------------------------------------------ the spool

func TestWriteSpoolKeepsNewestAndDropsOldest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry", "pending.jsonl")
	events := make([]spooledEvent, 0, telemetrySpoolMax+5)
	for i := 0; i < telemetrySpoolMax+5; i++ {
		events = append(events, event(fmt.Sprintf("run-%02d", i), i))
	}
	if err := writeSpool(path, events); err != nil {
		t.Fatalf("writeSpool: %v", err)
	}
	got := readSpool(path)
	if len(got) != telemetrySpoolMax {
		t.Fatalf("spool should be capped at %d, got %d", telemetrySpoolMax, len(got))
	}
	// DROP-OLDEST: run-00..run-04 are gone, run-05 is now first, and the last
	// event survives. Asserting the identities, not the length — a trim that
	// kept the WRONG end would also produce 50 records.
	if id := got[0].Resource["service.instance.id"]; id != "run-05" {
		t.Errorf("oldest survivor should be run-05 (oldest 5 dropped), got %q", id)
	}
	if id := got[len(got)-1].Resource["service.instance.id"]; id != "run-54" {
		t.Errorf("newest event must survive the trim, got %q", id)
	}
}

func TestSpoolFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry", "pending.jsonl")
	if err := writeSpool(path, []spooledEvent{event("run", 0)}); err != nil {
		t.Fatalf("writeSpool: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("spool holds a bearer-token-adjacent payload; want 0600, got %04o", perm)
	}
}

func TestReadSpoolSkipsTornLinesAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.jsonl")
	good, err := json.Marshal(event("run-good", 7))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(good) + "\n{\"resource\":{\"service.name\":\"cl\n" + string(good) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readSpool(path)
	if len(got) != 2 {
		t.Fatalf("a torn line must cost one record, not the file; got %d records", len(got))
	}
	for _, ev := range got {
		if ev.Resource["service.instance.id"] != "run-good" {
			t.Errorf("unexpected survivor: %v", ev.Resource)
		}
	}
}

func TestWriteSpoolRemovesTheFileWhenEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry", "pending.jsonl")
	if err := writeSpool(path, []spooledEvent{event("run", 0)}); err != nil {
		t.Fatalf("writeSpool: %v", err)
	}
	if err := writeSpool(path, nil); err != nil {
		t.Fatalf("writeSpool(nil): %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("an empty spool should leave no file behind; stat err = %v", err)
	}
}

// --------------------------------------------------------------- delivery

func TestClassifyStatus(t *testing.T) {
	cases := map[int]postOutcome{
		200: postDelivered, 202: postDelivered,
		400: postDiscard, // a permanently unparseable batch must not wedge the spool
		404: postDiscard,
		401: postRetry, // credential state, not a payload verdict
		403: postRetry,
		408: postRetry,
		429: postRetry,
		500: postRetry, 502: postRetry, 503: postRetry,
	}
	for code, want := range cases {
		if got := classifyStatus(code); got != want {
			t.Errorf("classifyStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestPostBatchSendsBearerAndJSON(t *testing.T) {
	var gotAuth, gotType, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	out := postBatch(context.Background(), srv.URL+telemetryIngestPath, "tok123", []spooledEvent{event("run", 1)})
	if out != postDelivered {
		t.Fatalf("202 should be postDelivered, got %v", out)
	}
	// "Bearer", not "Token": the endpoint's ClientAccessTokenAuthentication
	// declares keyword = "Bearer", and the wrong one 401s silently.
	if gotAuth != "Bearer tok123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok123")
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}
	if gotPath != telemetryIngestPath {
		t.Errorf("path = %q, want %q", gotPath, telemetryIngestPath)
	}
	if !strings.Contains(string(gotBody), `"resourceLogs"`) {
		t.Errorf("body should be an OTLP ExportLogsServiceRequest, got %s", gotBody)
	}
}

// The partition case: the server is unreachable, so the event must be on disk
// afterwards. This is the behaviour option (c) was chosen for.
func TestDeliverSpoolsWhenTheServerIsUnreachable(t *testing.T) {
	path := withTempConfigDir(t)
	// A closed listener's address: connection refused, immediately, and no
	// request ever leaves the machine.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + telemetryIngestPath
	srv.Close()

	deliver(url, "tok", event("run-partition", 3), time.Now())

	got := readSpool(path)
	if len(got) != 1 {
		t.Fatalf("an unreachable backend must leave the event spooled; got %d", len(got))
	}
	if id := got[0].Resource["service.instance.id"]; id != "run-partition" {
		t.Errorf("spooled the wrong event: %q", id)
	}
}

func TestDeliverSpoolsWhenNotSignedIn(t *testing.T) {
	path := withTempConfigDir(t)
	deliver("http://127.0.0.1:1"+telemetryIngestPath, "", event("run-anon", 0), time.Now())
	got := readSpool(path)
	if len(got) != 1 {
		t.Fatalf("no token must spool rather than post; got %d records", len(got))
	}
	if id := got[0].Resource["service.instance.id"]; id != "run-anon" {
		t.Errorf("spooled the wrong event: %q", id)
	}
}

func TestDeliverKeepsTheSpoolBoundedAcrossManyFailures(t *testing.T) {
	path := withTempConfigDir(t)
	for i := 0; i < telemetrySpoolMax+10; i++ {
		deliver("http://127.0.0.1:1"+telemetryIngestPath, "", event(fmt.Sprintf("run-%02d", i), i), time.Now())
	}
	got := readSpool(path)
	if len(got) != telemetrySpoolMax {
		t.Fatalf("spool must stay capped at %d across repeated failures, got %d", telemetrySpoolMax, len(got))
	}
	if id := got[len(got)-1].Resource["service.instance.id"]; id != "run-59" {
		t.Errorf("the most recent failure must be retained, got %q", id)
	}
}

// withTempConfigDir points config.Dir() at a temp dir and returns the spool path.
func withTempConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
	path, err := telemetrySpoolPath()
	if err != nil {
		t.Fatalf("telemetrySpoolPath: %v", err)
	}
	if !strings.HasPrefix(path, dir) {
		t.Fatalf("spool path %q should be under the overridden config dir %q", path, dir)
	}
	return path
}

// ─────────────────────────────────────────── the spool must not recode numbers

// THE DEFECT @saqlainsyed007 AND BUGBOT FOUND, as a regression test.
//
// `TestIntegersAreEncodedAsStrings` above operates on the in-memory shape and
// never touches writeSpool/readSpool, so it stayed green while every DRAINED
// record — the partition-time events the spool exists to preserve — encoded
// integers as `doubleValue`. The same event went out two ways, and the wrong way
// on the path the whole design is for.
//
// This asserts the two encodings AGREE, rather than asserting the round-trip
// looks right on its own: a test that only checked the drained payload could be
// satisfied by both paths being wrong together.
func TestSpoolRoundTripPreservesIntegerEncoding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry", "pending.jsonl")
	ev := event("run-roundtrip", 2)

	direct, err := otlpPayload([]spooledEvent{ev})
	if err != nil {
		t.Fatalf("otlpPayload(in-memory): %v", err)
	}
	if err := writeSpool(path, []spooledEvent{ev}); err != nil {
		t.Fatalf("writeSpool: %v", err)
	}
	reloaded := readSpool(path)
	if len(reloaded) != 1 {
		t.Fatalf("expected 1 spooled event back, got %d", len(reloaded))
	}
	drained, err := otlpPayload(reloaded)
	if err != nil {
		t.Fatalf("otlpPayload(drained): %v", err)
	}

	// The in-memory path is the reference. Asserted explicitly so a regression
	// there cannot make the comparison below pass by both sides breaking.
	if !strings.Contains(string(direct), `"intValue":"2"`) {
		t.Fatalf("the in-memory reference no longer encodes ints canonically: %s", direct)
	}
	if !strings.Contains(string(drained), `"intValue":"2"`) {
		t.Errorf("a drained record must encode exit_code as intValue, not doubleValue.\n"+
			" in-memory: %s\n drained  : %s", direct, drained)
	}
	// NOT "no doubleValue anywhere" — the fixture legitimately carries
	// `sampling_rate: 0.5`, which MUST stay a double. The first draft of this
	// test asserted the broad thing and failed on correct output, which would
	// have been a test bug reported as a code bug. Assert per-attribute instead.
	for _, intAttr := range []string{"tracebloc.cli.exit_code", "tracebloc.cli.duration_ms"} {
		if !strings.Contains(string(drained), `"key":"`+intAttr+`","value":{"intValue":"`) {
			t.Errorf("%s is not encoded as intValue on the drained path: %s", intAttr, drained)
		}
	}

	// Field by field, so the failure names WHICH attribute drifted rather than
	// leaving a reader to diff two payloads by eye.
	directFlat := flatten(t, resourceLogs(t, direct)[0])
	drainedFlat := flatten(t, resourceLogs(t, drained)[0])
	for _, key := range []string{
		"tracebloc.cli.exit_code",
		"tracebloc.cli.duration_ms",
		"tracebloc.cli.interactive",
		"tracebloc.cli.sampling_rate",
		"event.name",
		"service.instance.id",
	} {
		if directFlat[key] != drainedFlat[key] {
			t.Errorf("%s changed across the spool: in-memory %#v, drained %#v",
				key, directFlat[key], drainedFlat[key])
		}
	}
}

// A real (non-integral) number must still come back as a double, or the fix for
// integers would silently truncate every float the contract allows.
func TestSpoolRoundTripKeepsRealsAsDoubles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry", "pending.jsonl")
	ev := spooledEvent{
		Resource:   map[string]string{"service.name": "cli"},
		Attributes: map[string]any{"tracebloc.cli.sampling_rate": 0.25},
	}
	if err := writeSpool(path, []spooledEvent{ev}); err != nil {
		t.Fatalf("writeSpool: %v", err)
	}
	drained, err := otlpPayload(readSpool(path))
	if err != nil {
		t.Fatalf("otlpPayload: %v", err)
	}
	flat := flatten(t, resourceLogs(t, drained)[0])
	if got := flat["tracebloc.cli.sampling_rate"]; got != 0.25 {
		t.Errorf("a real must survive the spool as a double; got %#v from %s", got, drained)
	}
	if strings.Contains(string(drained), `"intValue":"0"`) {
		t.Errorf("0.25 was truncated to an integer across the spool: %s", drained)
	}
}
