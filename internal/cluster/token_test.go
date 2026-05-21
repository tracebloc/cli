package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestMintIngestorToken_TokenRequest_HappyPath(t *testing.T) {
	const ns = "tracebloc"
	cs := fake.NewClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "ingestor", Namespace: ns},
	})

	// The fake clientset doesn't implement the TokenRequest
	// subresource by default — we have to inject a reactor. This
	// mirrors what the real API server does: returns a stamped
	// Status.Token.
	cs.PrependReactor("create", "serviceaccounts",
		func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			ca, ok := action.(k8stesting.CreateAction)
			if !ok {
				return false, nil, nil
			}
			if ca.GetSubresource() != "token" {
				return false, nil, nil
			}
			tr := ca.GetObject().(*authenticationv1.TokenRequest)
			tr.Status.Token = "fake-token-deadbeef"
			return true, tr, nil
		})

	tok, err := MintIngestorToken(context.Background(), cs, ns, "ingestor", 600, nil)
	if err != nil {
		t.Fatalf("MintIngestorToken: %v", err)
	}
	if tok.Token != "fake-token-deadbeef" {
		t.Errorf("Token = %q, want %q", tok.Token, "fake-token-deadbeef")
	}
	if tok.Source != TokenSourceTokenRequest {
		t.Errorf("Source = %v, want TokenSourceTokenRequest", tok.Source)
	}
	if tok.ExpirationSeconds != 600 {
		t.Errorf("ExpirationSeconds = %d, want 600", tok.ExpirationSeconds)
	}
}

func TestMintIngestorToken_FallsBackToStaticSecret(t *testing.T) {
	const ns = "tracebloc"

	// Pre-seed a service-account-token Secret bound to the
	// ingestor SA. This is what older clusters auto-created on SA
	// creation; modern clusters require admins to create it by
	// hand.
	staticSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ingestor-token-abc123",
			Namespace: ns,
			Annotations: map[string]string{
				corev1.ServiceAccountNameKey: "ingestor",
			},
		},
		Type: corev1.SecretTypeServiceAccountToken,
		Data: map[string][]byte{
			"token": []byte("static-token-cafebabe"),
		},
	}
	cs := fake.NewClientset(
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "ingestor", Namespace: ns},
		},
		staticSecret,
	)

	// Make TokenRequest fail with a recoverable error
	// (Forbidden = the user lacks RBAC, which is the dominant
	// reason customers fall through to the static-secret path).
	cs.PrependReactor("create", "serviceaccounts",
		func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			if ca, ok := action.(k8stesting.CreateAction); ok && ca.GetSubresource() == "token" {
				return true, nil, apierrors.NewForbidden(
					corev1.Resource("serviceaccounts/token"),
					"ingestor",
					errors.New("user cannot create serviceaccounts/token"),
				)
			}
			return false, nil, nil
		})

	tok, err := MintIngestorToken(context.Background(), cs, ns, "ingestor", 600, nil)
	if err != nil {
		t.Fatalf("MintIngestorToken: %v", err)
	}
	if tok.Token != "static-token-cafebabe" {
		t.Errorf("Token = %q, want fallback static-token-cafebabe", tok.Token)
	}
	if tok.Source != TokenSourceStaticSecret {
		t.Errorf("Source = %v, want TokenSourceStaticSecret", tok.Source)
	}
	if tok.ExpirationSeconds != 0 {
		t.Errorf("ExpirationSeconds = %d, want 0 (static secrets don't expire client-side)", tok.ExpirationSeconds)
	}
}

func TestMintIngestorToken_NoFallbackOnUnrecoverableError(t *testing.T) {
	// Network errors (or any non-403/404/405 error) shouldn't
	// trigger the static-secret fallback — they'd mask the real
	// underlying problem. Pin that behavior.
	const ns = "tracebloc"
	cs := fake.NewClientset()

	cs.PrependReactor("create", "serviceaccounts",
		func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			if ca, ok := action.(k8stesting.CreateAction); ok && ca.GetSubresource() == "token" {
				return true, nil, errors.New("simulated network failure: i/o timeout")
			}
			return false, nil, nil
		})

	_, err := MintIngestorToken(context.Background(), cs, ns, "ingestor", 600, nil)
	if err == nil {
		t.Fatal("expected non-recoverable error to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("expected the network-error message to surface, got: %v", err)
	}
}

func TestMintIngestorToken_BothPathsFailWithRemediation(t *testing.T) {
	// Worst case: TokenRequest forbidden AND no static secret. The
	// error must surface both failures plus an actionable
	// remediation hint.
	const ns = "tracebloc"
	cs := fake.NewClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "ingestor", Namespace: ns},
	})

	cs.PrependReactor("create", "serviceaccounts",
		func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			if ca, ok := action.(k8stesting.CreateAction); ok && ca.GetSubresource() == "token" {
				return true, nil, apierrors.NewForbidden(
					corev1.Resource("serviceaccounts/token"),
					"ingestor",
					errors.New("forbidden"),
				)
			}
			return false, nil, nil
		})

	_, err := MintIngestorToken(context.Background(), cs, ns, "ingestor", 600, nil)
	if err == nil {
		t.Fatal("expected error when both TokenRequest + static fallback fail")
	}
	for _, want := range []string{
		"TokenRequest failed",
		"static secret also failed",
		"Remediation",
		"create", // remediation mentions granting the verb
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected combined error to mention %q, got: %s", want, err)
		}
	}
}

func TestTokenSource_String(t *testing.T) {
	cases := map[TokenSource]string{
		TokenSourceTokenRequest: "TokenRequest",
		TokenSourceStaticSecret: "static secret",
		TokenSource(99):         "unknown",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("TokenSource(%d).String() = %q, want %q", s, got, want)
		}
	}
}
