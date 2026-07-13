package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/ui"
)

// runInfo drives runClusterInfo against a fake clientset through the
// loadClusterFn/newClientsetFn seams (namespace "default", context "test-ctx"),
// with a hermetic empty config dir so no active-client binding leaks in from the
// developer's real ~/.tracebloc. Returns the rendered output + the error. Before
// this PR the whole post-discovery path (token minting, the expiry switch, the
// install-print block) was unreachable in a test — runClusterInfo called
// cluster.Load directly, so only the broken-kubeconfig exit-3 branch was covered.
func runInfo(t *testing.T, cs kubernetes.Interface, tokenExpiry int64) (string, error) {
	t.Helper()
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withClusterSeams(t, cs)
	var buf bytes.Buffer
	err := runClusterInfo(context.Background(), ui.New(&buf, ui.WithColor(false)), "", "", "", tokenExpiry)
	return buf.String(), err
}

// mintTokenReactor makes the fake clientset's TokenRequest subresource return a
// stamped token (the fake doesn't implement it by default — mirrors token_test.go).
// A non-zero expiresAt populates the server's authoritative ExpirationTimestamp
// (the arm that shows a real remaining lifetime); a zero value leaves it unset
// (the "requested; server may cap shorter" arm).
func mintTokenReactor(cs *fake.Clientset, token string, expiresAt time.Time) {
	cs.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(ktesting.CreateAction)
		if !ok || ca.GetSubresource() != "token" {
			return false, nil, nil
		}
		tr := ca.GetObject().(*authenticationv1.TokenRequest)
		tr.Status.Token = token
		if !expiresAt.IsZero() {
			tr.Status.ExpirationTimestamp = metav1.NewTime(expiresAt)
		}
		return true, tr, nil
	})
}

// A reached cluster hosting no tracebloc client exits 4 (distinct from the
// kubeconfig exit-3), still errors.Is-identifiable as ErrNoParentRelease. The
// banner + Kubeconfig section print before discovery fails.
func TestRunClusterInfo_NoClientExit4(t *testing.T) {
	out, err := runInfo(t, fake.NewSimpleClientset(), 600)
	if got := ExitCodeFromError(err); got != 4 {
		t.Fatalf("exit code = %d, want 4;\n%s", got, out)
	}
	if !errors.Is(err, cluster.ErrNoParentRelease) {
		t.Errorf("want errors.Is(ErrNoParentRelease), got %v", err)
	}
	for _, want := range []string{"cluster diagnostics", "test-ctx"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Release discovered but no usable token → exit 5 (distinct from exit-4 no-release,
// so customers RBAC-debug separately from install issues). The install-print block
// renders BEFORE token minting, so a token failure still shows the discovered
// release — pin that ordering.
func TestRunClusterInfo_NoTokenExit5(t *testing.T) {
	cs := fake.NewSimpleClientset(jmDep("default"))
	// TokenRequest fails with a NON-recoverable (network-class) error, so there's
	// no static-secret fallback and MintIngestorToken surfaces the failure.
	cs.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(ktesting.CreateAction); ok && ca.GetSubresource() == "token" {
			return true, nil, errors.New("simulated i/o timeout")
		}
		return false, nil, nil
	})
	out, err := runInfo(t, cs, 600)
	if got := ExitCodeFromError(err); got != 5 {
		t.Fatalf("exit code = %d, want 5;\n%s", got, out)
	}
	for _, want := range []string{"Client install", "tracebloc", "1.6.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("install block should render before the token failure — missing %q:\n%s", want, out)
		}
	}
}

// Happy path via TokenRequest WITH a server ExpirationTimestamp: the first arm
// of the expiry switch ("expires in ~<remaining>"). Also pins the security
// contract — the raw token is never printed, only sha256(token)[:8].
func TestRunClusterInfo_TokenRequestWithExpiry(t *testing.T) {
	cs := fake.NewSimpleClientset(jmDep("default"))
	mintTokenReactor(cs, "tok-abcdef", time.Now().Add(9*time.Minute))
	out, err := runInfo(t, cs, 600)
	if err != nil {
		t.Fatalf("want success, got %v;\n%s", err, out)
	}
	for _, want := range []string{"TokenRequest", "expires in", "Ready for", "not configured"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Mutation guard: with a server ExpiresAt set we take the FIRST switch arm
	// (show the authoritative remaining lifetime), NOT the requested-lifetime
	// fallback — both arms print the "expires in" label, so this negative
	// assertion is what actually distinguishes them.
	if strings.Contains(out, "requested; server may cap shorter") {
		t.Errorf("with a server ExpiresAt, must not fall back to the requested-lifetime arm:\n%s", out)
	}
	if strings.Contains(out, "tok-abcdef") {
		t.Errorf("the raw token must never be printed:\n%s", out)
	}
	hash := sha256.Sum256([]byte("tok-abcdef"))
	if want := hex.EncodeToString(hash[:8]); !strings.Contains(out, want) {
		t.Errorf("expected sha256[:8]=%s in output:\n%s", want, out)
	}
}

// Happy path via TokenRequest with NO server timestamp: the second arm — falls
// back to the requested lifetime, hedged with "server may cap shorter".
func TestRunClusterInfo_TokenRequestNoServerExpiry(t *testing.T) {
	cs := fake.NewSimpleClientset(jmDep("default"))
	mintTokenReactor(cs, "tok", time.Time{}) // no ExpirationTimestamp
	out, err := runInfo(t, cs, 600)
	if err != nil {
		t.Fatalf("want success, got %v;\n%s", err, out)
	}
	if !strings.Contains(out, "requested; server may cap shorter") {
		t.Errorf("no-server-timestamp arm should show the requested-lifetime note:\n%s", out)
	}
}

// Static-secret fallback (TokenRequest forbidden → recoverable): the third/default
// arm — no expiry, so the display reads "never (static-secret fallback)" and the
// source is reported as the static secret.
func TestRunClusterInfo_StaticSecretNeverExpires(t *testing.T) {
	cs := fake.NewSimpleClientset(
		jmDep("default"),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "ingestor-token-x",
				Namespace:   "default",
				Annotations: map[string]string{corev1.ServiceAccountNameKey: "ingestor"},
			},
			Type: corev1.SecretTypeServiceAccountToken,
			Data: map[string][]byte{"token": []byte("static-tok")},
		},
	)
	cs.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(ktesting.CreateAction); ok && ca.GetSubresource() == "token" {
			return true, nil, apierrors.NewForbidden(
				corev1.Resource("serviceaccounts/token"), "ingestor", errors.New("denied"))
		}
		return false, nil, nil
	})
	out, err := runInfo(t, cs, 600)
	if err != nil {
		t.Fatalf("want success via static-secret fallback, got %v;\n%s", err, out)
	}
	for _, want := range []string{"static secret", "never (static-secret fallback)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// The digest if/else: when the chart's jobs-manager carries INGESTOR_IMAGE_DIGEST,
// info prints the digest itself — never the "<not configured>" placeholder (which
// the other happy-path tests already exercise for the empty case).
func TestRunClusterInfo_ImageDigestConfigured(t *testing.T) {
	d := jmDep("default")
	d.Spec.Template.Spec.Containers = []corev1.Container{{
		Name: "jobs-manager",
		Env:  []corev1.EnvVar{{Name: "INGESTOR_IMAGE_DIGEST", Value: "sha256:cafef00d"}},
	}}
	cs := fake.NewSimpleClientset(d)
	mintTokenReactor(cs, "tok", time.Now().Add(time.Minute))
	out, err := runInfo(t, cs, 600)
	if err != nil {
		t.Fatalf("want success, got %v;\n%s", err, out)
	}
	if !strings.Contains(out, "sha256:cafef00d") {
		t.Errorf("configured digest should be printed:\n%s", out)
	}
	if strings.Contains(out, "not configured") {
		t.Errorf("digest is set — must not show the not-configured placeholder:\n%s", out)
	}
}
