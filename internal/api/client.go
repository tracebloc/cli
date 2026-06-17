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
	if c.Token != "" {
		req.Header.Set("Authorization", "Token "+c.Token)
	}
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
