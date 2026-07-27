// Package api is the tracebloc CLI's client for the central backend REST API
// (browser login + client provisioning). It is distinct from internal/submit,
// which talks to the in-cluster jobs-manager: this one reaches the public
// backend over real TLS. RFC-0001 (backend#830); the device-flow endpoints
// (/device/code, /device/token) land in backend#835, so RequestDeviceCode /
// PollToken are written against the RFC 8628 spec and go live when that ships.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

// Backend environments (mirror CLIENT_ENV).
const (
	EnvDev  = "dev"
	EnvStg  = "stg"
	EnvProd = "prod"
)

const defaultTimeout = 30 * time.Second

// ── User-Agent: minimum-CLI-version handshake (RFC-0001 §13 / §14 R11 / C.1) ──
//
// Every backend request announces the CLI build as
//
//	User-Agent: tracebloc-cli/<version> (<os>/<arch>)
//
// so the backend's MIN_SUPPORTED_CLI_VERSION gate (backend#888) can answer a
// too-old client with 426 Upgrade Required. The version is the ldflags-injected
// build version, recorded once at startup; until then (e.g. a bare `go run`) it
// reports "dev", which the backend can't parse and therefore lets through — the
// right fail-open for local development.

// userAgent is the formatted header value, set once via SetUserAgent at startup.
// A package var (not a constructor arg) so every Client built anywhere — login,
// client provisioning — carries it without threading the version through each
// command; it mirrors the build metadata that already lives as a global in main.
var userAgent string

// SetUserAgent records the CLI version used in the User-Agent on every backend
// request. Call once from cli.NewRootCmd with the ldflags-injected version.
func SetUserAgent(version string) {
	if version == "" {
		version = "dev"
	}
	userAgent = fmt.Sprintf("tracebloc-cli/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH)
}

// currentUserAgent is the header value to send, with a "dev" fallback for builds
// that never called SetUserAgent (tests, `go run`).
func currentUserAgent() string {
	if userAgent != "" {
		return userAgent
	}
	return fmt.Sprintf("tracebloc-cli/dev (%s/%s)", runtime.GOOS, runtime.GOARCH)
}

// userAgentTransport injects the CLI User-Agent on every request that doesn't
// already set one. It wraps the real transport (which keeps proxy + system-CA
// behavior). Per the http.RoundTripper contract it clones the request before
// mutating headers rather than modifying the caller's request in place.
type userAgentTransport struct{ base http.RoundTripper }

func (t userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", currentUserAgent())
	}
	return t.base.RoundTrip(req)
}

// BaseURL maps a CLIENT_ENV value to the backend base URL — kept in lock-step
// with the installer's `_backend_url` and client-runtime's CLIENT_ENV→backend
// mapping. Unknown / empty → prod.
func BaseURL(env string) string {
	switch strings.ToLower(env) {
	case EnvDev:
		return "https://dev-api.tracebloc.io"
	case EnvStg:
		return "https://stg-api.tracebloc.io"
	default:
		return "https://api.tracebloc.io"
	}
}

// ResolveEnv picks the backend env: an explicit value (a --env flag) wins,
// then $CLIENT_ENV, then prod.
func ResolveEnv(explicit string) string {
	if explicit != "" {
		return strings.ToLower(explicit)
	}
	if e := os.Getenv("CLIENT_ENV"); e != "" {
		return strings.ToLower(e)
	}
	return EnvProd
}

// IsKnownEnv reports whether env is one of the recognized backends (dev/stg/prod,
// case-insensitively). Callers that let a human PICK the env (e.g. `login`) use it
// to reject a typo up front — BaseURL deliberately falls unknown values back to
// prod (a lenient library default), so without this a `--env staging`/`prd` typo
// would silently target production. Empty is NOT known here: resolve first
// (ResolveEnv turns empty into the prod default), then validate the result.
func IsKnownEnv(env string) bool {
	switch strings.ToLower(env) {
	case EnvDev, EnvStg, EnvProd:
		return true
	default:
		return false
	}
}

// Client talks to the backend REST API. Token (the user token from login) is
// optional: the device-flow endpoints are unauthenticated; provisioning calls
// set it.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a Client for the given env. The HTTP client is proxy- and
// CA-aware — it honors HTTP(S)_PROXY/NO_PROXY and the system cert pool —
// because RFC-0001 must work behind a corporate / TLS-inspecting proxy
// (backend#830 Q1). Unlike internal/submit (in-cluster, no real TLS) this
// verifies certificates: it's the public backend.
func New(env string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(BaseURL(env), "/"),
		HTTP: &http.Client{
			Timeout: defaultTimeout,
			// Wrap the proxy/CA-aware transport so every request carries the
			// tracebloc-cli User-Agent (RFC-0001 §14 R11 / backend#888).
			Transport: userAgentTransport{base: &http.Transport{Proxy: http.ProxyFromEnvironment}},
		},
	}
}

// APIError is a non-2xx response, with the remote body surfaced verbatim.
type APIError struct {
	StatusCode int
	Body       string
	URL        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d: %s", e.URL, e.StatusCode, strings.TrimSpace(e.Body))
}

// UpgradeRequiredError is returned for an HTTP 426 from any endpoint: the CLI is
// below the backend's MIN_SUPPORTED_CLI_VERSION floor (RFC-0001 §14 R11 /
// backend#888). It's detected centrally (in post/get) so every call degrades to
// the same actionable "upgrade your CLI" message instead of a raw HTTP error.
type UpgradeRequiredError struct {
	MinVersion string // the server's minimum supported version, when it tells us
}

func (e *UpgradeRequiredError) Error() string {
	floor := "a newer version"
	if e.MinVersion != "" {
		floor = ">= " + e.MinVersion
	}
	return fmt.Sprintf(
		"this tracebloc CLI is too old for the server (requires %s). Update it — run "+
			"`tracebloc upgrade` (or re-run the install script) — then retry.",
		floor,
	)
}

// parseUpgradeRequired builds an *UpgradeRequiredError from a 426 body
// ({"error":"upgrade_required","min_version":"X"}). min_version is best-effort:
// the 426 status is the contract, so a body we can't parse still upgrades.
func parseUpgradeRequired(raw []byte) *UpgradeRequiredError {
	var body struct {
		MinVersion string `json:"min_version"`
	}
	_ = json.Unmarshal(raw, &body)
	return &UpgradeRequiredError{MinVersion: body.MinVersion}
}

// post sends an optional JSON body and returns the status code + raw response.
func (c *Client) post(ctx context.Context, path string, body any) (int, []byte, error) {
	return c.bodyRequest(ctx, http.MethodPost, path, body)
}

// patch sends an authenticated PATCH (used for the adopt-backfill of cluster_id
// onto an existing client — RFC-0001 §7.2 / R7, backend#883).
func (c *Client) patch(ctx context.Context, path string, body any) (int, []byte, error) {
	return c.bodyRequest(ctx, http.MethodPatch, path, body)
}

// bodyRequest sends an authenticated JSON-body request (POST/PATCH) and returns
// the status code + raw response. Shared so POST and PATCH stay identical on
// auth, content-type, and the 426 upgrade-required handling.
func (c *Client) bodyRequest(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response from %s: %w", url, err)
	}
	if resp.StatusCode == http.StatusUpgradeRequired {
		return resp.StatusCode, raw, parseUpgradeRequired(raw)
	}
	return resp.StatusCode, raw, nil
}

// setAuth attaches the stored token as a Bearer credential. The login token is
// a ClientAccessToken, authenticated by the backend's
// ClientAccessTokenAuthentication (keyword "Bearer", backend#835) — NOT the
// legacy DRF "Token" scheme.
func (c *Client) setAuth(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// get sends an authenticated GET and returns the status code + raw response.
func (c *Client) get(ctx context.Context, path string) (int, []byte, error) {
	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("building request: %w", err)
	}
	c.setAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response from %s: %w", url, err)
	}
	if resp.StatusCode == http.StatusUpgradeRequired {
		return resp.StatusCode, raw, parseUpgradeRequired(raw)
	}
	return resp.StatusCode, raw, nil
}

// ── Device Authorization Grant (RFC 8628) — backend endpoints land in #835 ──

// DeviceCodeResponse is the reply from POST /device/code.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// RequestDeviceCode starts the device flow.
func (c *Client) RequestDeviceCode(ctx context.Context) (*DeviceCodeResponse, error) {
	url := c.BaseURL + "/device/code"
	status, raw, err := c.post(ctx, "/device/code", nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &APIError{StatusCode: status, Body: string(raw), URL: url}
	}
	var out DeviceCodeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decoding device-code response: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, fmt.Errorf("device-code response missing device_code/user_code (got %q)", string(raw))
	}
	return &out, nil
}

// Device-flow poll outcomes (RFC 8628 §3.5): pending / slow_down mean "keep
// polling"; expired_token / access_denied are terminal.
var (
	ErrAuthorizationPending = errors.New("authorization_pending")
	ErrSlowDown             = errors.New("slow_down")
	ErrExpiredToken         = errors.New("expired_token")
	ErrAccessDenied         = errors.New("access_denied")
)

// PollToken polls POST /device/token once. It returns the user token on
// approval, or one of the Err* sentinels (pending/slow_down → keep polling,
// expired/denied → stop), or an *APIError for anything else.
func (c *Client) PollToken(ctx context.Context, deviceCode string) (string, error) {
	url := c.BaseURL + "/device/token"
	status, raw, err := c.post(ctx, "/device/token", map[string]string{"device_code": deviceCode})
	if err != nil {
		return "", err
	}
	var body struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body) // best-effort; the status + error field drive the result
	if status >= 200 && status < 300 {
		if body.Token == "" {
			return "", fmt.Errorf("device-token success response missing token (got %q)", string(raw))
		}
		return body.Token, nil
	}
	switch body.Error {
	case "authorization_pending":
		return "", ErrAuthorizationPending
	case "slow_down":
		return "", ErrSlowDown
	case "expired_token":
		return "", ErrExpiredToken
	case "access_denied":
		return "", ErrAccessDenied
	}
	return "", &APIError{StatusCode: status, Body: string(raw), URL: url}
}

// ── Authenticated calls (Bearer ClientAccessToken) ──

// Identity is the signed-in user, from GET /userinfo/.
type Identity struct {
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	Type      string `json:"type"`
	Account   string `json:"account"`
}

// WhoAmI fetches the signed-in user from the backend, authenticating with the
// stored token (Bearer). It confirms the token is live and returns the account
// — `login` uses it to verify the credential it just obtained. Requires Token.
func (c *Client) WhoAmI(ctx context.Context) (*Identity, error) {
	url := c.BaseURL + "/userinfo/"
	status, raw, err := c.get(ctx, "/userinfo/")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &APIError{StatusCode: status, Body: string(raw), URL: url}
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("decoding userinfo response: %w", err)
	}
	return &id, nil
}

// RevokeToken revokes the presenting credential server-side via POST /auth/revoke
// (backend#887, shipped in backend#903): Bearer in, 204 out, idempotent. `logout`
// calls this so a copied/leaked token stops authenticating after sign-out — local
// clearing alone left it valid (RFC-0001 §7.5 / R2). Requires Token. A non-2xx is
// returned as an *APIError; callers treat the call as best-effort.
func (c *Client) RevokeToken(ctx context.Context) error {
	url := c.BaseURL + "/auth/revoke"
	status, raw, err := c.post(ctx, "/auth/revoke", nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &APIError{StatusCode: status, Body: string(raw), URL: url}
	}
	return nil
}

// ── Client provisioning (Bearer-authed) — backend#836, /edge-device/ ──

// ProvisionedClient is a tracebloc client (machine), as returned by the
// EdgeDevice endpoints.
type ProvisionedClient struct {
	ID        int    `json:"id"`
	Name      string `json:"first_name"`
	Username  string `json:"username"`
	Namespace string `json:"namespace"`
	Location  string `json:"location"`
	Status    int    `json:"status"`
	// ClusterID is the kube-system namespace UID this client is anchored to
	// (RFC-0001 §6.3 / backend#883). Empty on legacy / not-yet-backfilled clients.
	ClusterID string `json:"cluster_id"`
	// NumRunningExperiments is how many training runs are active on this client
	// right now. `tracebloc delete`'s work-guard blocks on this — not on Status
	// (== online, which a healthy client always is) — so a live-but-idle
	// environment can still be offboarded after the typed-name confirm.
	NumRunningExperiments int `json:"num_running_experiments"`
}

// CreateClientRequest is the POST /edge-device/ body. The account is stamped
// server-side from the token; password is the machine credential the caller
// generates (write-only on the backend).
type CreateClientRequest struct {
	Name      string `json:"first_name"`
	Namespace string `json:"namespace"`
	// Location is optional (cli#137): omitted when the operator gives no --location,
	// so the backend records the client with no location rather than a silent
	// default (backend#993). EdgeDevice.location is blank=True server-side.
	Location string `json:"location,omitempty"`
	Password string `json:"password"`
	// ClusterID anchors the client to this cluster (the kube-system namespace UID)
	// so create is get-or-create keyed on it (RFC-0001 §7.2 / backend#883). Omitted
	// when the cluster identity can't be read (dual-mode / legacy → plain mint).
	ClusterID string `json:"cluster_id,omitempty"`
}

// AdminContact is one "ask an admin" entry from GET /edge-device/admins/.
type AdminContact struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// CreateClient provisions a client — get-or-create keyed on cluster_id when one
// is supplied (RFC-0001 §7.2 / backend#883). The returned `adopted` is true when
// the backend matched an existing client for this cluster (HTTP 200, an idempotent
// re-run) and false when it minted a new one (HTTP 201). A 403 *APIError means the
// caller lacks CLIENT_WRITE (→ ask-an-admin, backend#836 Q4); a 409 *APIError means
// the cluster is bound to another account (cluster_conflict, R6).
func (c *Client) CreateClient(ctx context.Context, req CreateClientRequest) (pc *ProvisionedClient, adopted bool, err error) {
	url := c.BaseURL + "/edge-device/"
	status, raw, err := c.post(ctx, "/edge-device/", req)
	if err != nil {
		return nil, false, err
	}
	if status < 200 || status >= 300 {
		return nil, false, &APIError{StatusCode: status, Body: string(raw), URL: url}
	}
	var out ProvisionedClient
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("decoding create-client response: %w", err)
	}
	// 200 = adopted an existing client for this cluster_id; 201 = freshly minted.
	return &out, status == http.StatusOK, nil
}

// PatchClientClusterID backfills the cluster anchor onto an existing client
// (RFC-0001 §7.2 / R7, backend#883). The existing fleet predates cluster_id, so
// a client that's already live in-cluster has a null anchor — and a plain create
// keyed on the freshly-read kube-system UID would match nothing and mint a
// duplicate. PATCH /edge-device/{id}/ stamps the UID onto the live client so it
// (not a new mint) owns this cluster. The backend enforces write-once: a 409
// *APIError means the anchor is already set to a different value or is bound to
// another client (R6); a 403 *APIError means the caller lacks CLIENT_WRITE.
func (c *Client) PatchClientClusterID(ctx context.Context, id int, clusterID string) (*ProvisionedClient, error) {
	path := fmt.Sprintf("/edge-device/%d/", id)
	url := c.BaseURL + path
	status, raw, err := c.patch(ctx, path, map[string]string{"cluster_id": clusterID})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &APIError{StatusCode: status, Body: string(raw), URL: url}
	}
	var out ProvisionedClient
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decoding patch-client response: %w", err)
	}
	return &out, nil
}

// RevokeClient offboards a client's machine credential server-side via
// POST /edge-device/<id>/revoke/ (RFC-0001 §7.10 / C.6): it unsets the password,
// deletes the DRF token, and revokes the client's ClientAccessTokens in one
// transaction while PRESERVING the client row (and thus the shared training
// history / datasets / use cases retained on offboard). This is deliberately
// NOT DELETE /edge-device/<id>/ — a row delete would cascade the per-client
// training telemetry (§7.10 "hard destroy is rejected"), and DELETE isn't
// routed. A 200 (or any 2xx) means revoked; a 403 *APIError means the caller
// lacks CLIENT_WRITE (→ ask-an-admin). Requires Token (Bearer).
//
// The trailing slash matches the DRF DefaultRouter route (the `revoke` @action
// registers as /edge-device/<pk>/revoke/) — a slashless POST hits Django's
// APPEND_SLASH, which cannot redirect a body-bearing POST and errors instead.
func (c *Client) RevokeClient(ctx context.Context, id int) error {
	path := fmt.Sprintf("/edge-device/%d/revoke/", id)
	url := c.BaseURL + path
	status, raw, err := c.post(ctx, path, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &APIError{StatusCode: status, Body: string(raw), URL: url}
	}
	return nil
}

// maxListPages bounds how many pages ListClients will follow — a backstop
// against a misbehaving `next` chain, set well above any real account.
const maxListPages = 100

// GetClient fetches a single client by its dashboard id (GET /edge-device/{id}/).
// The detail route is the same one PatchClientClusterID/RevokeClient address, and
// returns one ProvisionedClient. This is the O(1) way to check ONE client's
// status — unlike ListClients, which pages through the whole account (the
// home-screen heartbeat must not do that under its ~1.2s budget, cli#338).
// A 404 returns (nil, nil) so the caller can distinguish "no such client" from
// a transport/backend error.
func (c *Client) GetClient(ctx context.Context, id int) (*ProvisionedClient, error) {
	path := fmt.Sprintf("/edge-device/%d/", id)
	url := c.BaseURL + path
	status, raw, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status < 200 || status >= 300 {
		return nil, &APIError{StatusCode: status, Body: string(raw), URL: url}
	}
	var out ProvisionedClient
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decoding get-client response: %w", err)
	}
	return &out, nil
}

// ListClients returns ALL clients in the caller's account (GET /edge-device/).
// The endpoint is DRF-paginated, so this follows `next` to the end — list,
// `use <id>`, and create-time collision detection must see every client, not
// just the first page. Also tolerates a bare (unpaginated) list body.
func (c *Client) ListClients(ctx context.Context) ([]ProvisionedClient, error) {
	var all []ProvisionedClient
	path := "/edge-device/"
	for pageNum := 0; path != ""; pageNum++ {
		if pageNum >= maxListPages {
			return nil, fmt.Errorf("client list exceeded %d pages — aborting", maxListPages)
		}
		reqURL := c.BaseURL + path
		status, raw, err := c.get(ctx, path)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			return nil, &APIError{StatusCode: status, Body: string(raw), URL: reqURL}
		}
		// Unpaginated deployment → a bare array. Only valid as the sole response
		// (a paginated chain is a `{next,results}` object on every page), so guard
		// to page 0 — a stray bare body mid-chain must not silently end the loop.
		if pageNum == 0 {
			var bare []ProvisionedClient
			if err := json.Unmarshal(raw, &bare); err == nil {
				return bare, nil
			}
		}
		var body struct {
			Next    string              `json:"next"`
			Results []ProvisionedClient `json:"results"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, fmt.Errorf("decoding client list: %w", err)
		}
		all = append(all, body.Results...)
		next, perr := nextPath(body.Next)
		if perr != nil {
			return nil, perr
		}
		path = next
	}
	return all, nil
}

// nextPath reduces a DRF `next` link (an absolute URL) to the path+query this
// client appends to BaseURL. An empty link returns ("", nil) — the normal
// end of pages. A non-empty link that won't parse is an error, NOT a silent
// "", so the loop never quietly stops mid-list and returns only the pages
// seen so far (list / `use` / collision checks must see every client).
func nextPath(next string) (string, error) {
	if next == "" {
		return "", nil
	}
	u, err := url.Parse(next)
	if err != nil {
		return "", fmt.Errorf("client list: unparseable pagination link %q: %w", next, err)
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery, nil
	}
	return u.Path, nil
}

// ListClientAdmins returns who in the account can provision (the ask-an-admin
// path), from GET /edge-device/admins/ (backend#836 Q4).
func (c *Client) ListClientAdmins(ctx context.Context) ([]AdminContact, error) {
	url := c.BaseURL + "/edge-device/admins/"
	status, raw, err := c.get(ctx, "/edge-device/admins/")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &APIError{StatusCode: status, Body: string(raw), URL: url}
	}
	var out []AdminContact
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decoding admins response: %w", err)
	}
	return out, nil
}
