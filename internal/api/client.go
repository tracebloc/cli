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
			Timeout:   defaultTimeout,
			Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
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

// post sends an optional JSON body and returns the status code + raw response.
func (c *Client) post(ctx context.Context, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response from %s: %w", url, err)
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
	Email   string `json:"email"`
	Type    string `json:"type"`
	Account string `json:"account"`
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
}

// CreateClientRequest is the POST /edge-device/ body. The account is stamped
// server-side from the token; password is the machine credential the caller
// generates (write-only on the backend).
type CreateClientRequest struct {
	Name      string `json:"first_name"`
	Namespace string `json:"namespace"`
	Location  string `json:"location"`
	Password  string `json:"password"`
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

// maxListPages bounds how many pages ListClients will follow — a backstop
// against a misbehaving `next` chain, set well above any real account.
const maxListPages = 100

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
		// Unpaginated deployment → a bare array; return it as-is.
		var bare []ProvisionedClient
		if err := json.Unmarshal(raw, &bare); err == nil {
			return append(all, bare...), nil
		}
		var body struct {
			Next    string              `json:"next"`
			Results []ProvisionedClient `json:"results"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, fmt.Errorf("decoding client list: %w", err)
		}
		all = append(all, body.Results...)
		path = nextPath(body.Next)
	}
	return all, nil
}

// nextPath reduces a DRF `next` link (an absolute URL) to the path+query this
// client appends to BaseURL. Returns "" for an empty/unparseable link, which
// ends the pagination loop.
func nextPath(next string) string {
	if next == "" {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil {
		return ""
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
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
