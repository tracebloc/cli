package cluster

import (
	"context"
	"fmt"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// IngestorToken is what the customer (well, the CLI) authenticates
// to jobs-manager with. Bearer-token over HTTPS to
// /internal/submit-ingestion-run. The token is short-lived (10
// minutes default) so it can be regenerated per `tracebloc dataset
// push` invocation without long-term-credential concerns.
type IngestorToken struct {
	// Token is the raw bearer token string. Treat as sensitive;
	// don't log it. Diagnostics print SHA256(Token)[:8] instead.
	Token string

	// ExpirationSeconds matches the request — actual server-side
	// expiration may be capped by cluster policy
	// (--service-account-max-token-expiration on kube-apiserver).
	// We don't try to parse the JWT to read its `exp` claim
	// because the customer's diagnostic ("token expires in ~N
	// min") is accurate enough using the requested value.
	ExpirationSeconds int64

	// Source records how the token was obtained — TokenRequest
	// (the modern path) or a static secret (the fallback for
	// clusters where the user can't call TokenRequest). Surfaced
	// in `tracebloc cluster info` so the customer can see which
	// path their RBAC allowed.
	Source TokenSource
}

// TokenSource enumerates the two ways the CLI can obtain an
// ingestor SA token. Adding a third (e.g. a customer-managed
// long-lived secret reference) is a v0.2 extension.
type TokenSource int

const (
	// TokenSourceTokenRequest means we called the
	// /serviceaccounts/{name}/token subresource. The modern,
	// rotation-friendly path. Requires `create` permission on
	// "serviceaccounts/token" for the user's kubeconfig.
	TokenSourceTokenRequest TokenSource = iota

	// TokenSourceStaticSecret means we fell back to reading a
	// pre-existing Secret of type
	// kubernetes.io/service-account-token that references the
	// ingestor SA. Older k8s (pre-1.24 default) auto-created
	// these; on 1.24+ admins must create them by hand. Tokens
	// from this source are long-lived (no expiration), which is
	// less ideal but unblocks customers whose RBAC denies
	// TokenRequest.
	TokenSourceStaticSecret
)

func (s TokenSource) String() string {
	switch s {
	case TokenSourceTokenRequest:
		return "TokenRequest"
	case TokenSourceStaticSecret:
		return "static secret"
	default:
		return "unknown"
	}
}

// MintIngestorToken obtains a bearer token for the ingestor
// ServiceAccount in the given namespace. Tries TokenRequest first;
// if that's denied (RBAC) or unavailable (older API server) falls
// back to looking for a pre-existing service-account-token Secret.
// Returns an error with a clear remediation message if neither
// works.
//
// audiences may be nil/empty for the default API-server audience.
// Today jobs-manager's TokenReview-based validation accepts the
// default audience, so callers usually pass nil; future audiences
// (e.g. a forthcoming jobs-manager-specific audience) plug in here
// without API breakage.
func MintIngestorToken(
	ctx context.Context,
	cs kubernetes.Interface,
	namespace, saName string,
	expirationSeconds int64,
	audiences []string,
) (*IngestorToken, error) {
	// Try TokenRequest first.
	req := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: &expirationSeconds,
			Audiences:         audiences,
		},
	}
	tr, err := cs.CoreV1().ServiceAccounts(namespace).
		CreateToken(ctx, saName, req, metav1.CreateOptions{})

	if err == nil {
		return &IngestorToken{
			Token:             tr.Status.Token,
			ExpirationSeconds: expirationSeconds,
			Source:            TokenSourceTokenRequest,
		}, nil
	}

	// TokenRequest didn't work. Distinguish the cases we can fall
	// back from (permission denied / not-found / unsupported)
	// from cases we can't (e.g. context cancelled, network down).
	if !isTokenRequestRecoverable(err) {
		return nil, fmt.Errorf(
			"minting token for ServiceAccount %s/%s via TokenRequest: %w",
			namespace, saName, err,
		)
	}

	// Fallback: look for a Secret of type
	// kubernetes.io/service-account-token in the same namespace
	// whose annotations point at our SA.
	tok, fallbackErr := findStaticTokenSecret(ctx, cs, namespace, saName)
	if fallbackErr != nil {
		// Surface BOTH the original TokenRequest failure and the
		// fallback failure — they give the customer different
		// remediation options.
		return nil, fmt.Errorf(
			"no usable ingestor token. TokenRequest failed: %v. "+
				"Fallback to static secret also failed: %w. "+
				"Remediation: either grant your user the `create` verb on "+
				"`serviceaccounts/token` (RBAC), or have an admin create a "+
				"long-lived Secret of type kubernetes.io/service-account-token "+
				"that references the %s ServiceAccount in namespace %s.",
			err, fallbackErr, saName, namespace,
		)
	}
	return tok, nil
}

// isTokenRequestRecoverable reports whether a failed TokenRequest
// call is one we can fall back from (permission denied, API not
// available, SA missing) vs one we can't (context cancelled,
// network errors, etc.).
//
// Falling back on a network error would mask the network problem;
// the customer would see a less useful "static secret not found"
// instead of "couldn't reach the API server."
//
// Defers to k8s.io/apimachinery/pkg/api/errors for the classification
// rather than re-implementing the HTTP-status checks. The stdlib
// helpers key off Status.Reason (the typed enum) — that's the
// canonical interpretation, and our test file already constructs its
// fake errors via apierrors.NewForbidden, which sets both Code and
// Reason. Earlier versions of this file rolled their own helpers
// that read Status.Code numerically; Bugbot correctly flagged the
// divergence as a future-bug-waiting-to-happen for non-standard
// status errors.
func isTokenRequestRecoverable(err error) bool {
	if err == nil {
		return false
	}
	// Permission denied (StatusForbidden), SA missing
	// (StatusNotFound), API not available on this cluster — typically
	// pre-1.22 (StatusMethodNotAllowed/MethodNotSupported).
	return apierrors.IsForbidden(err) ||
		apierrors.IsNotFound(err) ||
		apierrors.IsMethodNotSupported(err)
}

// findStaticTokenSecret looks for a long-lived
// service-account-token Secret bound to the given SA. The legacy
// pre-k8s-1.24 way of authenticating an SA: kubectl auto-created
// these on SA creation. On modern clusters admins create them by
// hand when needed.
func findStaticTokenSecret(ctx context.Context, cs kubernetes.Interface, namespace, saName string) (*IngestorToken, error) {
	secrets, err := cs.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "type=kubernetes.io/service-account-token",
	})
	if err != nil {
		// If the user can't even list Secrets, surface that
		// directly — they may be missing more RBAC than just
		// TokenRequest, and "static secret fallback failed" is
		// less informative than the underlying error.
		return nil, fmt.Errorf("listing service-account-token secrets in %s: %w", namespace, err)
	}

	for _, s := range secrets.Items {
		// The Secret is bound to an SA via the
		// `kubernetes.io/service-account.name` annotation. (The
		// `.uid` annotation pins it to that exact SA instance —
		// we don't check that because we want any historically
		// created token-secret for the named SA to work.)
		if s.Annotations[corev1.ServiceAccountNameKey] != saName {
			continue
		}
		token, ok := s.Data["token"]
		if !ok || len(token) == 0 {
			continue // empty token field — skip
		}
		return &IngestorToken{
			Token:             string(token),
			ExpirationSeconds: 0, // static secrets don't expire client-side
			Source:            TokenSourceStaticSecret,
		}, nil
	}

	return nil, fmt.Errorf(
		"no Secret of type kubernetes.io/service-account-token bound to "+
			"ServiceAccount %s found in namespace %s",
		saName, namespace,
	)
}
