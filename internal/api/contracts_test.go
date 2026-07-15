package api

// Backend response-shape contract tests (backend#1106 WS-D.2, cli#291).
//
// internal/api/testdata/*.json are REAL backend responses — serialized by the
// backend's own view/serializer/middleware stack, shape-asserted in backend CI
// (metaApi/tests/test_cli_response_contracts.py), and vendored here at a
// pinned ref by scripts/sync-backend-fixtures.sh (scripts/.backend-ref).
//
// The unit tests in client_test.go feed the client hand-written JSON, which
// pins the CLI's *expectations* — but drifts silently when the backend renames
// a field: the CLI would decode the new body green and hand callers Go zero
// values (empty account, id 0, adopted never true). These tests close that
// class by replaying the vendored REAL bodies through the client's actual
// decode paths and asserting every load-bearing field comes out non-zero (or
// the right sentinel / typed error).
//
// Fixture envelope: {"status": <http status>, "body": <json body or null>}.
// Volatile values (pks, secrets, timestamps) are pinned upstream to non-zero
// placeholders; shapes, types, and enum values are real.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fixtureEnvelope mirrors the backend fixture wrapper: the HTTP status is part
// of the contract (200-adopt vs 201-mint, 204 no-content, 426/403/409).
type fixtureEnvelope struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func loadFixture(t *testing.T, name string) fixtureEnvelope {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s (run scripts/sync-backend-fixtures.sh to seed): %v", name, err)
	}
	var env fixtureEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("fixture %s is not a {status, body} envelope: %v", name, err)
	}
	if env.Status == 0 {
		t.Fatalf("fixture %s has no status", name)
	}
	return env
}

// writeEnvelope replays one fixture as an HTTP response. A null body (e.g. the
// 204 auth_revoke fixture) writes no payload, matching the real backend.
func writeEnvelope(w http.ResponseWriter, env fixtureEnvelope) {
	if len(env.Body) > 0 && string(env.Body) != "null" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(env.Status)
		_, _ = w.Write(env.Body)
		return
	}
	w.WriteHeader(env.Status)
}

// fixtureClient serves the named fixture for every request and returns a
// Client pointed at it.
func fixtureClient(t *testing.T, name string) *Client {
	t.Helper()
	env := loadFixture(t, name)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, env)
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: "test-token", HTTP: srv.Client()}
}

// coveredFixtures is the full manifest this file asserts on. The completeness
// test below fails when a synced fixture has no assertions (the "new upstream
// contract, invisible locally" hole) or a covered fixture stopped syncing.
// Keep in lock-step with scripts/sync-backend-fixtures.sh and the backend
// test module.
var coveredFixtures = []string{
	"auth_revoke.json",
	"device_code.json",
	"device_token_error_access_denied.json",
	"device_token_error_authorization_pending.json",
	"device_token_error_expired_token.json",
	"device_token_error_slow_down.json",
	"device_token_success.json",
	"edge_device_admins.json",
	"edge_device_adopt.json",
	"edge_device_create.json",
	"edge_device_list.json",
	"edge_device_patch_cluster_id.json",
	"edge_device_revoke.json",
	"error_403_client_write.json",
	"error_409_cluster_conflict.json",
	"error_409_cluster_in_use.json",
	"error_426_upgrade_required.json",
	"userinfo.json",
}

func TestContractFixtureManifestComplete(t *testing.T) {
	onDisk, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, p := range onDisk {
		found[filepath.Base(p)] = true
	}
	covered := map[string]bool{}
	for _, name := range coveredFixtures {
		covered[name] = true
		if !found[name] {
			t.Errorf("fixture %s is covered here but missing from testdata/ — run scripts/sync-backend-fixtures.sh", name)
		}
	}
	for name := range found {
		if !covered[name] {
			t.Errorf("testdata/%s is synced but has NO contract assertions — add it to coveredFixtures and a test", name)
		}
	}
}

// ── Device Authorization Grant ──────────────────────────────────────────────

func TestContractRequestDeviceCode(t *testing.T) {
	c := fixtureClient(t, "device_code.json")
	out, err := c.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("RequestDeviceCode on the real backend body: %v", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		t.Errorf("device_code/user_code decoded empty: %+v", out)
	}
	if out.VerificationURI == "" || out.VerificationURIComplete == "" {
		t.Errorf("verification URIs decoded empty: %+v", out)
	}
	if out.ExpiresIn <= 0 || out.Interval <= 0 {
		t.Errorf("expires_in/interval decoded zero: %+v", out)
	}
}

func TestContractPollTokenSuccess(t *testing.T) {
	c := fixtureClient(t, "device_token_success.json")
	token, err := c.PollToken(context.Background(), "any-device-code")
	if err != nil {
		t.Fatalf("PollToken on the real success body: %v", err)
	}
	if token == "" {
		t.Error("token decoded empty from the real success body")
	}
}

func TestContractPollTokenErrorEnums(t *testing.T) {
	// The four RFC 8628 §3.5 enums the poll loop switches on. If the backend
	// renamed one, PollToken would fall through to a generic *APIError and the
	// login flow would abort instead of pacing/stopping correctly.
	cases := map[string]error{
		"device_token_error_authorization_pending.json": ErrAuthorizationPending,
		"device_token_error_slow_down.json":             ErrSlowDown,
		"device_token_error_expired_token.json":         ErrExpiredToken,
		"device_token_error_access_denied.json":         ErrAccessDenied,
	}
	for name, want := range cases {
		c := fixtureClient(t, name)
		_, err := c.PollToken(context.Background(), "any-device-code")
		if !errors.Is(err, want) {
			t.Errorf("%s: got %v, want sentinel %v", name, err, want)
		}
	}
}

// ── Authenticated identity + sign-out ───────────────────────────────────────

func TestContractWhoAmI(t *testing.T) {
	c := fixtureClient(t, "userinfo.json")
	id, err := c.WhoAmI(context.Background())
	if err != nil {
		t.Fatalf("WhoAmI on the real backend body: %v", err)
	}
	if id.Email == "" {
		t.Error("email decoded empty")
	}
	if id.Account == "" {
		t.Error("account decoded empty — login's credential verification would report a blank account")
	}
	if id.FirstName == "" || id.Type == "" {
		t.Errorf("first_name/type decoded empty: %+v", id)
	}
}

func TestContractRevokeToken(t *testing.T) {
	c := fixtureClient(t, "auth_revoke.json")
	if err := c.RevokeToken(context.Background()); err != nil {
		t.Fatalf("RevokeToken on the real 204 response: %v", err)
	}
}

// ── Client provisioning ─────────────────────────────────────────────────────

// assertProvisionedClient checks every ProvisionedClient field the CLI acts on
// decodes non-zero from a real backend body — the exact silent-drift class.
func assertProvisionedClient(t *testing.T, name string, pc *ProvisionedClient) {
	t.Helper()
	if pc.ID == 0 {
		t.Errorf("%s: id decoded 0 — `use <id>` / PATCH targeting would break", name)
	}
	if pc.Name == "" {
		t.Errorf("%s: first_name decoded empty", name)
	}
	if pc.Username == "" {
		t.Errorf("%s: username decoded empty — the machine credential's login name", name)
	}
	if pc.Namespace == "" {
		t.Errorf("%s: namespace decoded empty — Helm install would target the wrong namespace", name)
	}
	if pc.ClusterID == "" {
		t.Errorf("%s: cluster_id decoded empty — adopt idempotency would break", name)
	}
	// The fixtures pin a freshly-minted client (status PENDING, non-zero).
	// STATUS_OFFLINE=0 is legal in the wild, but here a 0 means the field
	// itself stopped decoding.
	if pc.Status == 0 {
		t.Errorf("%s: status decoded 0 from a body that carries a non-zero status", name)
	}
}

// assertDecodesFrom verifies every ProvisionedClient field the CLI acts on
// decoded to exactly what the raw backend row carries, catching BOTH silent-
// drift directions on a *real synced list* without rejecting legitimate zero
// values:
//
//   - struct-tag drift (a Go json tag is renamed): the wire row still carries
//     the old key with a value while the struct decodes zero, so decoded !=
//     wire — caught by the value compare.
//   - backend rename/drop on re-sync (the wire field is renamed and
//     scripts/sync-backend-fixtures.sh re-seeds the fixture): the old key
//     vanishes from the row, so a pure decoded-vs-wire compare reads the zero
//     value on BOTH sides and would silently agree. The presence check on the
//     always-present keys catches this — the key is gone, so it fails.
//
// The two fields that can be legitimately zero on a real row are handled
// precisely: status may be STATUS_OFFLINE=0 (present with value 0 → tolerated)
// and cluster_id is "Empty on legacy / not-yet-backfilled clients" (client.go),
// so cluster_id is exempt from the presence check and a backend rename of it
// stays covered by the strict single-fixture contract tests. For every field
// except cluster_id this is strictly stronger drift detection than the old
// non-zero check, while never rejecting a legitimate zero.
func assertDecodesFrom(t *testing.T, name string, raw json.RawMessage, pc *ProvisionedClient) {
	t.Helper()
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("%s: list row is not a JSON object: %v", name, err)
	}
	// present reports whether the row still carries the key with a non-null
	// value — a renamed or dropped backend field fails this, which is how the
	// re-sync drift direction is caught for keys the struct always expects.
	present := func(field string) bool {
		v, ok := wire[field]
		return ok && len(v) > 0 && string(v) != "null"
	}
	wantStr := func(field string) string {
		var s string
		_ = json.Unmarshal(wire[field], &s) // absent/null → "", the field's zero
		return s
	}
	wantInt := func(field string) int {
		var n int
		_ = json.Unmarshal(wire[field], &n) // absent/null → 0, the field's zero
		return n
	}
	// Always-present keys: assert the key survives (backend rename/drop guard)
	// AND that the decoded value matches the wire (struct-tag-drift guard).
	for _, f := range []string{"id", "first_name", "username", "namespace", "status"} {
		if !present(f) {
			t.Errorf("%s: row is missing %q — a renamed/dropped backend field the struct still decodes by that name", name, f)
		}
	}
	if got, want := pc.ID, wantInt("id"); got != want {
		t.Errorf("%s: id decoded %d, body carries %d — `use <id>` / PATCH targeting would break", name, got, want)
	}
	if got, want := pc.Name, wantStr("first_name"); got != want {
		t.Errorf("%s: first_name decoded %q, body carries %q", name, got, want)
	}
	if got, want := pc.Username, wantStr("username"); got != want {
		t.Errorf("%s: username decoded %q, body carries %q", name, got, want)
	}
	if got, want := pc.Namespace, wantStr("namespace"); got != want {
		t.Errorf("%s: namespace decoded %q, body carries %q — Helm install would target the wrong namespace", name, got, want)
	}
	if got, want := pc.Status, wantInt("status"); got != want {
		t.Errorf("%s: status decoded %d, body carries %d", name, got, want)
	}
	// cluster_id is presence-optional (legitimately empty on legacy / unanchored
	// rows); the value compare still catches struct-tag drift, and a backend
	// rename of cluster_id stays covered by the strict single-fixture tests.
	if got, want := pc.ClusterID, wantStr("cluster_id"); got != want {
		t.Errorf("%s: cluster_id decoded %q, body carries %q — adopt idempotency would break", name, got, want)
	}
}

func TestContractCreateClientMint(t *testing.T) {
	c := fixtureClient(t, "edge_device_create.json")
	pc, adopted, err := c.CreateClient(context.Background(), CreateClientRequest{})
	if err != nil {
		t.Fatalf("CreateClient on the real 201 body: %v", err)
	}
	if adopted {
		t.Error("a 201 mint must report adopted=false")
	}
	assertProvisionedClient(t, "edge_device_create.json", pc)
}

func TestContractCreateClientAdopt(t *testing.T) {
	c := fixtureClient(t, "edge_device_adopt.json")
	pc, adopted, err := c.CreateClient(context.Background(), CreateClientRequest{})
	if err != nil {
		t.Fatalf("CreateClient on the real 200 body: %v", err)
	}
	if !adopted {
		t.Error("a 200 re-run must report adopted=true — the idempotent-provisioning signal")
	}
	assertProvisionedClient(t, "edge_device_adopt.json", pc)
}

func TestContractPatchClientClusterID(t *testing.T) {
	c := fixtureClient(t, "edge_device_patch_cluster_id.json")
	pc, err := c.PatchClientClusterID(context.Background(), 1, "any-cluster-id")
	if err != nil {
		t.Fatalf("PatchClientClusterID on the real 200 body: %v", err)
	}
	assertProvisionedClient(t, "edge_device_patch_cluster_id.json", pc)
}

func TestContractRevokeClient(t *testing.T) {
	c := fixtureClient(t, "edge_device_revoke.json")
	if err := c.RevokeClient(context.Background(), 1); err != nil {
		t.Fatalf("RevokeClient on the real 200 body: %v", err)
	}
}

func TestContractListClientsPaginated(t *testing.T) {
	// The real list body is DRF-paginated with a non-null `next`. Serve the
	// fixture as page 1; for the followed `next` path, serve the same rows
	// with `next` nulled — asserting the client both DECODES the real page
	// shape and FOLLOWS the real pagination link format.
	env := loadFixture(t, "edge_device_list.json")
	var page1 struct {
		Next    string            `json:"next"`
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(env.Body, &page1); err != nil {
		t.Fatalf("decoding fixture page: %v", err)
	}
	if page1.Next == "" {
		t.Fatal("fixture's `next` is empty — it must exercise pagination")
	}
	var lastBody map[string]json.RawMessage
	if err := json.Unmarshal(env.Body, &lastBody); err != nil {
		t.Fatal(err)
	}
	lastBody["next"] = json.RawMessage("null")
	lastPage, err := json.Marshal(lastBody)
	if err != nil {
		t.Fatal(err)
	}

	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "" {
			_, _ = w.Write(env.Body)
			return
		}
		_, _ = w.Write(lastPage)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "test-token", HTTP: srv.Client()}
	all, err := c.ListClients(context.Background())
	if err != nil {
		t.Fatalf("ListClients on the real paginated body: %v", err)
	}
	if requests != 2 {
		t.Errorf("expected the real `next` link to be followed once (2 requests), got %d", requests)
	}
	if want := 2 * len(page1.Results); len(all) != want {
		t.Fatalf("decoded %d clients, want %d", len(all), want)
	}
	// Assert decode FIDELITY per row rather than non-zero: a real synced list
	// can legitimately carry offline (status 0) or unanchored (empty cluster_id)
	// rows, and asserting those non-zero would fail CI on the next fixture
	// re-sync even though decoding is correct. The two served pages repeat the
	// same rows in order, so row i maps to page1.Results[i % len].
	for i := range all {
		raw := page1.Results[i%len(page1.Results)]
		assertDecodesFrom(t, "edge_device_list.json", raw, &all[i])
	}
}

func TestContractListClientAdmins(t *testing.T) {
	c := fixtureClient(t, "edge_device_admins.json")
	admins, err := c.ListClientAdmins(context.Background())
	if err != nil {
		t.Fatalf("ListClientAdmins on the real body: %v", err)
	}
	if len(admins) == 0 {
		t.Fatal("no admins decoded from a body that carries them")
	}
	// Decode-fidelity per row (same rationale as assertDecodesFrom). email is a
	// domain-guaranteed identifier — the endpoint exists so the operator can
	// email the admin — so assert it non-empty AND matching the wire (a renamed
	// `email` tag decodes "" here → caught). name is a human display name that
	// can be legitimately blank for an admin who never set one, so only assert
	// it decodes to exactly what the row carries rather than requiring non-empty.
	var rows []map[string]json.RawMessage
	env := loadFixture(t, "edge_device_admins.json")
	if err := json.Unmarshal(env.Body, &rows); err != nil {
		t.Fatalf("decoding admins fixture rows: %v", err)
	}
	if len(rows) != len(admins) {
		t.Fatalf("decoded %d admins, fixture carries %d rows", len(admins), len(rows))
	}
	for i, a := range admins {
		var wantName, wantEmail string
		_ = json.Unmarshal(rows[i]["name"], &wantName)
		_ = json.Unmarshal(rows[i]["email"], &wantEmail)
		if a.Email == "" {
			t.Errorf("admin %d: email decoded empty — the ask-an-admin message would have no recipient", i)
		}
		if a.Email != wantEmail {
			t.Errorf("admin %d: email decoded %q, body carries %q", i, a.Email, wantEmail)
		}
		if a.Name != wantName {
			t.Errorf("admin %d: name decoded %q, body carries %q", i, a.Name, wantName)
		}
	}
}

// ── Load-bearing error bodies ───────────────────────────────────────────────

func TestContractUpgradeRequired426(t *testing.T) {
	c := fixtureClient(t, "error_426_upgrade_required.json")
	_, err := c.WhoAmI(context.Background())
	var upgrade *UpgradeRequiredError
	if !errors.As(err, &upgrade) {
		t.Fatalf("a real 426 body must surface as *UpgradeRequiredError, got %v", err)
	}
	if upgrade.MinVersion == "" {
		t.Error("min_version decoded empty — the upgrade message would lose the floor")
	}
}

func TestContract403ClientWrite(t *testing.T) {
	c := fixtureClient(t, "error_403_client_write.json")
	_, _, err := c.CreateClient(context.Background(), CreateClientRequest{})
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("a real 403 must surface as *APIError, got %v", err)
	}
	if ae.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", ae.StatusCode)
	}
	if ae.Body == "" {
		t.Error("403 body surfaced empty")
	}
}

// conflictBody mirrors exactly the fields internal/cli's conflictMessage
// parses out of a provisioning 409 — the user-guidance contract.
type conflictBody struct {
	Error          string `json:"error"`
	ClusterID      string `json:"cluster_id"`
	OwnerEmail     string `json:"owner_email"`
	HolderName     string `json:"holder_name"`
	HolderClientID int    `json:"holder_client_id"`
}

func decode409(t *testing.T, err error) conflictBody {
	t.Helper()
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("a real 409 must surface as *APIError, got %v", err)
	}
	if ae.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", ae.StatusCode)
	}
	var body conflictBody
	if err := json.Unmarshal([]byte(ae.Body), &body); err != nil {
		t.Fatalf("409 body is not the JSON the CLI parses: %v", err)
	}
	return body
}

func TestContract409ClusterConflict(t *testing.T) {
	c := fixtureClient(t, "error_409_cluster_conflict.json")
	_, _, err := c.CreateClient(context.Background(), CreateClientRequest{})
	body := decode409(t, err)
	if body.Error != "cluster_conflict" {
		t.Errorf("error enum = %q, want cluster_conflict — conflictMessage keys on it", body.Error)
	}
	if body.ClusterID == "" {
		t.Error("cluster_id decoded empty")
	}
	if body.OwnerEmail == "" {
		t.Error("owner_email decoded empty — the who-to-ask contact would vanish from the message")
	}
}

func TestContract409ClusterInUse(t *testing.T) {
	c := fixtureClient(t, "error_409_cluster_in_use.json")
	_, err := c.PatchClientClusterID(context.Background(), 1, "any-cluster-id")
	body := decode409(t, err)
	if body.Error != "cluster_in_use" {
		t.Errorf("error enum = %q, want cluster_in_use — conflictMessage keys on it", body.Error)
	}
	if body.HolderName == "" {
		t.Error("holder_name decoded empty — the message would name a blank sibling")
	}
	if body.HolderClientID == 0 {
		t.Error("holder_client_id decoded 0")
	}
}
