package submit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// ForwardedConnection is a live port-forward to an in-cluster
// Service. LocalPort is the random port the kernel picked on the
// CLI host; the Submitter POSTs to http://localhost:LocalPort.
//
// Close MUST be called when the caller is done — otherwise the
// goroutine running the SPDY tunnel leaks for the lifetime of the
// process.
type ForwardedConnection struct {
	LocalPort int

	stopCh chan struct{}
	done   chan struct{}
}

// Close tears down the port-forward. Safe to call multiple times.
func (f *ForwardedConnection) Close() {
	select {
	case <-f.stopCh:
		return // already closed
	default:
	}
	close(f.stopCh)
	// Wait briefly for the goroutine to drain — without this, a
	// fast Close-then-process-exit could race the SPDY teardown
	// and leave a half-open connection on the apiserver side.
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
	}
}

// PortForwardJobsManager opens a port-forward to a Pod backing the
// jobs-manager Service in `namespace`. The customer's CLI runs
// off-cluster (on a laptop, in a CI runner outside the cluster
// network); the discovered jobs-manager URL is a
// *.svc.cluster.local name that's NOT resolvable from there. The
// port-forward routes traffic through the kubeconfig-authenticated
// kube-apiserver connection — same machinery `kubectl port-forward`
// uses internally.
//
// Returns a ForwardedConnection whose LocalPort the caller targets
// for HTTP. Bugbot PR #10 r3 caught the broken-by-design assumption
// in the initial Phase 4 implementation.
//
// Lifecycle: caller MUST defer Close(). The port-forward stays open
// for the entire submit + watch sequence (submit needs a single
// POST, watch only uses the kubeconfig API server connection, so
// strictly speaking we could close right after the POST — but
// keeping it open is simpler and the resource cost is one idle
// goroutine).
func PortForwardJobsManager(
	ctx context.Context,
	cs kubernetes.Interface,
	restConfig *rest.Config,
	namespace, serviceName string,
	targetPort int,
) (*ForwardedConnection, error) {
	// 1. Find a Running Pod backing the Service. client-go's
	//    port-forward API speaks Pods, not Services — even though
	//    kubectl port-forward accepts both, it resolves the Service
	//    to a Pod internally.
	pod, err := pickServicePod(ctx, cs, namespace, serviceName)
	if err != nil {
		return nil, fmt.Errorf("resolving Service %s/%s to a Pod: %w",
			namespace, serviceName, err)
	}

	// 2. Build the SPDY transport. client-go bundles helpers in
	//    transport/spdy for exactly this; the round-tripper handles
	//    the SPDY upgrade negotiation the apiserver expects on the
	//    /portforward subresource.
	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building SPDY transport: %w", err)
	}

	// 3. Construct the portforward URL on the Pod. The path is
	//    /api/v1/namespaces/<ns>/pods/<pod>/portforward; we build
	//    it via the REST client so kubeconfig's authentication +
	//    TLS config flow through automatically.
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(pod.Name).
		SubResource("portforward")

	dialer := spdy.NewDialer(upgrader,
		&http.Client{Transport: transport},
		"POST", req.URL())

	// 4. Create the port-forwarder. "0:<targetPort>" means "pick
	//    any free local port; map it to <targetPort> in the Pod."
	//    The kernel allocates the local port at goroutine start;
	//    we read it back after readyCh fires.
	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	pf, err := portforward.New(dialer,
		[]string{fmt.Sprintf("0:%d", targetPort)},
		stopCh, readyCh,
		io.Discard, // forwarder's stdout — verbose listener logs we don't want
		io.Discard, // forwarder's stderr
	)
	if err != nil {
		return nil, fmt.Errorf("creating port-forwarder: %w", err)
	}

	// 5. Launch the forward goroutine. ForwardPorts blocks until
	//    stopCh closes (or it errors). The select below waits for
	//    EITHER ready (success) or done (early failure).
	done := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		defer close(done)
		// ForwardPorts is the long-running call. Its return value
		// is meaningful only on early failure — on normal close,
		// it returns nil after stopCh fires.
		if err := pf.ForwardPorts(); err != nil {
			errCh <- err
		}
	}()

	select {
	case <-readyCh:
		// happy path — port allocated, tunnel up
	case err := <-errCh:
		return nil, fmt.Errorf("port-forward to %s/%s failed during startup: %w",
			namespace, pod.Name, err)
	case <-ctx.Done():
		close(stopCh)
		return nil, ctx.Err()
	}

	ports, err := pf.GetPorts()
	if err != nil {
		close(stopCh)
		return nil, fmt.Errorf("reading allocated port: %w", err)
	}
	if len(ports) == 0 {
		close(stopCh)
		return nil, fmt.Errorf("port-forward allocated zero ports")
	}

	return &ForwardedConnection{
		LocalPort: int(ports[0].Local),
		stopCh:    stopCh,
		done:      done,
	}, nil
}

// pickServicePod resolves a Service to a Running Pod backing it.
// Uses the Service's own selector (read from the Service spec) to
// match Pods — same mechanism the cluster's own kube-proxy uses.
//
// Picks the first Running Pod found; in the common case there's
// only one (jobs-manager is single-replica by chart default). For
// multi-replica deployments, picking any Running Pod is fine —
// they're load-balanced equivalents.
func pickServicePod(ctx context.Context, cs kubernetes.Interface, namespace, serviceName string) (*corev1.Pod, error) {
	svc, err := cs.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading service %s/%s: %w", namespace, serviceName, err)
	}
	if len(svc.Spec.Selector) == 0 {
		// A Service with no selector is one whose Endpoints are
		// hand-managed (e.g. ExternalName). The chart's jobs-
		// manager is a normal selector-based Service so this
		// shouldn't happen in production; the error makes the
		// debugging path obvious if it ever does.
		return nil, fmt.Errorf(
			"service %s/%s has no selector — can't resolve to a Pod for port-forwarding",
			namespace, serviceName)
	}

	// Build a label selector from the Service's spec.selector map.
	// strings.Join keeps the order deterministic for readable
	// error output; the order doesn't affect the actual Pods
	// returned.
	parts := make([]string, 0, len(svc.Spec.Selector))
	for k, v := range svc.Spec.Selector {
		parts = append(parts, k+"="+v)
	}
	selector := strings.Join(parts, ",")

	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing Pods for service %s/%s: %w",
			namespace, serviceName, err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning {
			return p, nil
		}
	}
	return nil, fmt.Errorf(
		"no Running Pod backing service %s/%s (found %d Pod(s); "+
			"check `kubectl get pods -n %s -l %s`)",
		namespace, serviceName, len(pods.Items), namespace, selector)
}
