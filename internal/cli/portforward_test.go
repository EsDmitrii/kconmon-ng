package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream" //nolint:staticcheck // SA1019: the fake stands in for spdy.NewDialer, which still returns this type
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/portforward"
)

// testNamespace is the release namespace every fixture in this file lives in.
const testNamespace = "kconmon"

// controllerPod builds a running controller pod carrying the chart's component labels.
func controllerPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "kconmon-ng",
				"app.kubernetes.io/component": "controller",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// controllerLease builds the controller's leader Lease held by holder.
func controllerLease(holder string) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "kconmon-ng-controller", Namespace: testNamespace},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
}

// TestControllerPodsPrefersLeaseHolder is what keeps `kubectl kconmon` on the leader of an HA
// deployment: only the leader answers topology and diagnostics, and a standby first costs a 503 and a
// second port-forward.
func TestControllerPodsPrefersLeaseHolder(t *testing.T) {
	clientset := fake.NewClientset(
		controllerPod("controller-a"),
		controllerPod("controller-b"),
		controllerLease("controller-b"),
	)

	k := &kubeConnector{namespace: testNamespace}
	pods, err := k.controllerPods(context.Background(), clientset)
	if err != nil {
		t.Fatalf("controllerPods: %v", err)
	}
	if got := podNames(pods); !slices.Equal(got, []string{"controller-b", "controller-a"}) {
		t.Errorf("candidates %v, want the lease holder controller-b first, then the rest", got)
	}
}

// TestControllerPodsFallsBackWithoutLease covers single-replica installs, which run without
// leader election and therefore without a Lease.
func TestControllerPodsFallsBackWithoutLease(t *testing.T) {
	clientset := fake.NewClientset(controllerPod("controller-a"))

	k := &kubeConnector{namespace: testNamespace}
	pods, err := k.controllerPods(context.Background(), clientset)
	if err != nil {
		t.Fatalf("controllerPods: %v", err)
	}
	if got := podNames(pods); !slices.Equal(got, []string{"controller-a"}) {
		t.Errorf("candidates %v, want controller-a", got)
	}
}

// TestControllerPodsIgnoresForeignLease guards against matching an unrelated Lease that happens
// to sit in the same namespace.
func TestControllerPodsIgnoresForeignLease(t *testing.T) {
	clientset := fake.NewClientset(
		controllerPod("controller-a"),
		controllerLease("some-other-workload"),
	)

	k := &kubeConnector{namespace: testNamespace}
	pods, err := k.controllerPods(context.Background(), clientset)
	if err != nil {
		t.Fatalf("controllerPods: %v", err)
	}
	if got := podNames(pods); !slices.Equal(got, []string{"controller-a"}) {
		t.Errorf("candidates %v, want the running pod controller-a", got)
	}
}

// TestControllerPodsNoRunningPods keeps the original error when nothing is running.
func TestControllerPodsNoRunningPods(t *testing.T) {
	k := &kubeConnector{namespace: testNamespace}
	if _, err := k.controllerPods(context.Background(), fake.NewClientset()); err == nil {
		t.Fatal("expected an error when no controller pod is running")
	}
}

// The controller listens on config.httpPort, which the chart publishes as the container port named
// "http"; forwarding a hard-coded 8080 breaks every call once an operator moves it.
func TestControllerHTTPPortFollowsThePodSpec(t *testing.T) {
	pod := controllerPod("controller-a")
	pod.Spec.Containers = []corev1.Container{
		{Name: "sidecar", Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 3000}}},
		{Name: "controller", Ports: []corev1.ContainerPort{
			{Name: "metrics", ContainerPort: 9091},
			{Name: "http", ContainerPort: 9090},
		}},
	}
	if got := controllerHTTPPort(pod); got != 9090 {
		t.Errorf("controllerHTTPPort = %d, want the controller container's http port 9090", got)
	}
	if got := controllerHTTPPort(controllerPod("bare")); got != 8080 {
		t.Errorf("controllerHTTPPort without a named port = %d, want the default 8080", got)
	}
}

// fakeStreamConn is an upgraded connection that stays open until closed and records which remote
// port each forwarded local connection asked for.
type fakeStreamConn struct {
	closeOnce sync.Once
	closed    chan bool
	ports     chan string
}

func newFakeStreamConn() *fakeStreamConn {
	return &fakeStreamConn{closed: make(chan bool), ports: make(chan string, 8)}
}

func (c *fakeStreamConn) CreateStream(h http.Header) (httpstream.Stream, error) {
	if h.Get(corev1.StreamType) == corev1.StreamTypeError {
		select {
		case c.ports <- h.Get(corev1.PortHeader):
		default:
		}
	}
	return nil, errors.New("fake connection carries no streams")
}
func (c *fakeStreamConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
func (c *fakeStreamConn) CloseChan() <-chan bool             { return c.closed }
func (c *fakeStreamConn) SetIdleTimeout(time.Duration)       {}
func (c *fakeStreamConn) RemoveStreams(...httpstream.Stream) {}

// fakeDialer hands out conn, or blocks until release is closed when conn is nil.
type fakeDialer struct {
	conn    *fakeStreamConn
	release chan struct{}
}

func (d *fakeDialer) Dial(_ ...string) (httpstream.Connection, string, error) {
	if d.conn == nil {
		<-d.release
		return nil, "", errors.New("released")
	}
	return d.conn, portforward.PortForwardProtocolV1Name, nil
}

// The URL handed to the client must name a port the forwarder itself bound, on the loopback address
// that URL uses, and connections to it must reach the requested remote port.
func TestForwardServesTheLocalPortItReports(t *testing.T) {
	conn := newFakeStreamConn()
	c, err := forward(context.Background(), &fakeDialer{conn: conn}, 9090)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	t.Cleanup(c.Close)

	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Port() == "0" {
		t.Fatalf("BaseURL = %q, want http://127.0.0.1:<bound port>", c.BaseURL)
	}
	var d net.Dialer
	local, err := d.DialContext(context.Background(), "tcp", u.Host)
	if err != nil {
		t.Fatalf("nothing listens on the reported %s: %v", u.Host, err)
	}
	_ = local.Close()
	select {
	case got := <-conn.ports:
		if got != "9090" {
			t.Errorf("forwarded to remote port %s, want 9090", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the local connection was never forwarded")
	}
}

// Ctrl-C cancels the command context; a tunnel that is still being set up must give up at once
// instead of waiting out the 30s ready timeout.
func TestForwardReturnsWhenTheContextIsCancelled(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := forward(ctx, &fakeDialer{release: release}, 9090)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("forward error = %v, want the context's error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("forward took %s after the context ended, want it to return promptly", elapsed)
	}
}
