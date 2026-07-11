package submit

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SubmitTimeout caps how long we wait for jobs-manager to respond to
// the POST. jobs-manager validates synchronously (schema re-check,
// idempotency lookup, Job creation) — the chart's hook bounds this
// at 30s and we mirror that. Beyond 30s, something genuinely wrong
// is happening server-side, and the customer wants the diagnostic
// rather than a longer wait.
const SubmitTimeout = 30 * time.Second

// Submitter is the narrow surface the orchestrator (submit.go) uses
// for the POST. Real impl is *HTTPSubmitter; tests use a fake that
// records the request and returns a synthetic response.
type Submitter interface {
	Submit(ctx context.Context, req *SubmitRequest) (*SubmitResponse, error)
}

// HTTPSubmitter is the production implementation. Wraps net/http
// with a fixed jobs-manager URL + a SA token from Phase 2's mint.
//
// Endpoint comes from Phase 2's cluster.DiscoverParentRelease
// (`http://<release>-jobs-manager.<ns>.svc.cluster.local:8080`),
// SubmitPath is hardcoded per the chart's published contract.
type HTTPSubmitter struct {
	// Endpoint is the full jobs-manager URL, e.g.
	// "http://release-jobs-manager.tracebloc.svc.cluster.local:8080".
	// No trailing slash — the submitter appends SubmitPath.
	Endpoint string

	// Token is the bearer token to send in the Authorization
	// header. Comes from Phase 2's cluster.MintIngestorToken.
	Token string

	// Client is the underlying *http.Client. Set in NewHTTPSubmitter
	// with a sensible timeout. Exposed for tests that need to point
	// at an httptest.Server with a custom RoundTripper.
	Client *http.Client
}

// SubmitPath is the well-known URL path on jobs-manager. Pinned
// here as a constant rather than a knob because the chart's
// post-install hook also pins it; if jobs-manager ever moves the
// endpoint, both have to bump together (which is a coordinated
// release across tracebloc/client + tracebloc/cli, exactly what
// you want for a protocol-level change).
const SubmitPath = "/internal/submit-ingestion-run"

// NewHTTPSubmitter returns a Submitter wired with a sensible
// timeout + a transport that DOESN'T do TLS verification against
// the in-cluster CA. The jobs-manager endpoint is HTTP-only inside
// the cluster (kube-proxy handles all the rest), so TLS isn't in
// the picture at all today. If a future jobs-manager exposes
// HTTPS, the InsecureSkipVerify path is the right v0.1 default
// because the customer's kubeconfig has already authenticated them
// to the cluster — the cluster-internal jobs-manager doesn't have
// a CA the laptop would recognize anyway.
func NewHTTPSubmitter(endpoint, token string) *HTTPSubmitter {
	return &HTTPSubmitter{
		Endpoint: strings.TrimRight(endpoint, "/"),
		Token:    token,
		Client: &http.Client{
			Timeout: SubmitTimeout,
			Transport: &http.Transport{
				// See doc on NewHTTPSubmitter for the
				// InsecureSkipVerify rationale.
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
		},
	}
}

// Submit POSTs the request body to jobs-manager and decodes the
// 201 response into a SubmitResponse. On non-201 status codes, the
// remote body is surfaced verbatim so the customer sees whatever
// jobs-manager said (typically a JSON {error, detail} from the
// fastapi handler) rather than just "HTTP 422".
//
// Replays (idempotency-key already seen → 200 with replay=true)
// are reported in the response struct; this method treats them as
// success because the upstream behavior IS "your run is already
// in progress / already done." The orchestrator handles the
// replay branch in its own diagnostic output.
func (s *HTTPSubmitter) Submit(ctx context.Context, req *SubmitRequest) (*SubmitResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		// Marshaling a struct of strings shouldn't fail at all;
		// surfacing the error means a future struct change broke
		// the wire format.
		return nil, fmt.Errorf("marshaling submit request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.Endpoint+SubmitPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building submit request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.Token)

	httpResp, err := s.Client.Do(httpReq)
	if err != nil {
		// Network errors (DNS, connection refused, TLS handshake
		// failure, ctx cancellation). The error wraps net.OpError
		// already; just frame it with our endpoint so the
		// customer knows what was being attempted.
		return nil, fmt.Errorf("POST %s%s: %w", s.Endpoint, SubmitPath, err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		// Short read on a 201 body — extremely rare; the
		// connection dropped between header and body.
		return nil, fmt.Errorf("reading submit response body: %w", err)
	}

	// 2xx (typically 201 Created; also 200 for replays) is success.
	// Everything else surfaces the remote body verbatim so the
	// customer sees jobs-manager's actual diagnostic (HTTP 4xx
	// schema-rejection, HTTP 5xx kube-apiserver-failure, etc.)
	// rather than just a status code.
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, &SubmitError{
			StatusCode: httpResp.StatusCode,
			Body:       string(respBody),
			Endpoint:   s.Endpoint + SubmitPath,
		}
	}

	var parsed SubmitResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		// 2xx but the body isn't the expected shape. Almost
		// certainly a protocol mismatch (jobs-manager version
		// drift). Include the raw body so the customer can pin
		// the version they're on.
		return nil, fmt.Errorf("decoding submit response (got body %q): %w", string(respBody), err)
	}
	if parsed.JobName == "" {
		// Defensive: a malformed-but-parsing response with no
		// job_name would silently break Phase 4's watch step.
		return nil, fmt.Errorf("submit response missing job_name (got body %q)", string(respBody))
	}
	if parsed.Namespace == "" {
		// Same shape as the job_name check: a missing namespace
		// would route subsequent k8s API calls (watch the Pod,
		// stream logs) at the empty string — kubelet returns
		// confusing errors, and the kubectl-logs reconnect hint
		// printed in --detach output would be malformed. Bugbot
		// PR #10 r2 flagged the gap.
		return nil, fmt.Errorf("submit response missing namespace (got body %q)", string(respBody))
	}
	return &parsed, nil
}

// SubmitError is the typed non-2xx response. Pulled into a struct
// (rather than an opaque string) so the orchestrator can branch on
// StatusCode for the exit-code mapping: 401/403 → auth exit code,
// 4xx other → submit-validation exit code, 5xx → submit-server.
//
// Implements `error` + an Is for errors.Is detection in tests.
type SubmitError struct {
	StatusCode int
	Body       string
	Endpoint   string
}

func (e *SubmitError) Error() string {
	// Compact framing: the customer sees status + body. The body
	// is jobs-manager's actual diagnostic (e.g. fastapi's
	// {"detail": "..."}) which is the actionable part.
	return fmt.Sprintf("jobs-manager %s returned HTTP %d: %s",
		e.Endpoint, e.StatusCode, strings.TrimSpace(e.Body))
}

// isSubmitError reports whether err is a *SubmitError. Convenience
// for the orchestrator's exit-code mapping; errors.As would also
// work but this reads cleaner at the branch site. Unexported: only
// same-package tests reference it today.
func isSubmitError(err error) bool {
	var se *SubmitError
	return errors.As(err, &se)
}
