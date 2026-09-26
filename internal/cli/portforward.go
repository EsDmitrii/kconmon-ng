package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream" //nolint:staticcheck // SA1019: spdy.NewDialer and portforward.NewOnAddresses still take this Dialer
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// defaultControllerPort is config.httpPort's default, used when the pod names no "http" port.
const defaultControllerPort = 8080

// controllerLabelSelector matches controller pods managed by the chart.
const controllerLabelSelector = "app.kubernetes.io/name=kconmon-ng,app.kubernetes.io/component=controller"

// portForwardReadyTimeout bounds how long we wait for the tunnel to come up.
const portForwardReadyTimeout = 30 * time.Second

// Connection is an established route to a controller's HTTP API. Close must be
// called to tear down the underlying port-forward.
type Connection struct {
	BaseURL string
	Close   func()
	// Next connects to the next running controller pod, for when this one answered as a standby; nil
	// when there is none.
	Next func(ctx context.Context) (*Connection, error)
}

// Connector opens a Connection to a controller. It is the narrow seam that
// keeps command logic testable: tests substitute a connector that points at an
// httptest.Server instead of a real port-forward.
type Connector interface {
	Connect(ctx context.Context) (*Connection, error)
}

// kubeConnector establishes a client-go port-forward to a controller pod.
type kubeConnector struct {
	kubeconfig string
	context    string
	namespace  string
}

// newKubeConnector builds a Connector from the standard client-go flag inputs.
// An empty kubeconfig falls back to the standard loading rules (KUBECONFIG env,
// ~/.kube/config); an empty namespace triggers all-namespace pod discovery.
func newKubeConnector(kubeconfig, kubeContext, namespace string) *kubeConnector {
	return &kubeConnector{kubeconfig: kubeconfig, context: kubeContext, namespace: namespace}
}

func (k *kubeConnector) restConfig() (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if k.kubeconfig != "" {
		loadingRules.ExplicitPath = k.kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if k.context != "" {
		overrides.CurrentContext = k.context
	}
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	return cfg.ClientConfig()
}

// Connect finds the running controller pods (searching all namespaces when one was not given) and
// port-forwards a random local port to the first; Connection.Next moves on to the others.
func (k *kubeConnector) Connect(ctx context.Context) (*Connection, error) {
	cfg, err := k.restConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}

	pods, err := k.controllerPods(ctx, clientset)
	if err != nil {
		return nil, err
	}

	return connectInOrder(ctx, pods, func(ctx context.Context, pod *corev1.Pod) (*Connection, error) {
		return startPortForward(ctx, cfg, clientset, pod)
	})
}

// connectInOrder connects to pods[0] and hands the rest to Connection.Next.
func connectInOrder(ctx context.Context, pods []*corev1.Pod, dial func(context.Context, *corev1.Pod) (*Connection, error)) (*Connection, error) {
	conn, err := dial(ctx, pods[0])
	if err != nil {
		return nil, err
	}
	if others := pods[1:]; len(others) > 0 {
		conn.Next = func(ctx context.Context) (*Connection, error) { return connectInOrder(ctx, others, dial) }
	}
	return conn, nil
}

// controllerPods lists the running controller pods, the Lease holder first when a Lease names one.
// When a namespace is set it searches only there; otherwise it searches all namespaces.
func (k *kubeConnector) controllerPods(ctx context.Context, clientset kubernetes.Interface) ([]*corev1.Pod, error) {
	ns := k.namespace // "" means all namespaces
	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: controllerLabelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing controller pods: %w", err)
	}
	running := make([]*corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			running = append(running, p)
		}
	}

	if len(running) == 0 {
		where := "any namespace"
		if ns != "" {
			where = "namespace " + ns
		}
		return nil, fmt.Errorf("no running kconmon-ng controller pod found in %s (selector %q)", where, controllerLabelSelector)
	}

	first := leaseHolder(ctx, clientset, running)
	if first == nil {
		first = running[0]
	}
	// Only the first pod's own replicas follow it: without -n the list spans every namespace.
	ordered := []*corev1.Pod{first}
	for _, p := range running {
		if p != first && p.Namespace == first.Namespace {
			ordered = append(ordered, p)
		}
	}
	return ordered, nil
}

// leaseHolder returns the running controller pod named by a controller Lease, reading Leases only in
// the namespaces those pods run in. Only the leader answers topology and diagnostics; a standby
// returns 503, and withClient then moves to the next pod. A missing or unreadable Lease is not an
// error: single-replica installs run without leader election, and a caller without leases RBAC
// still reaches the leader through that retry.
func leaseHolder(ctx context.Context, clientset kubernetes.Interface, running []*corev1.Pod) *corev1.Pod {
	var namespaces []string
	for _, p := range running {
		if !slices.Contains(namespaces, p.Namespace) {
			namespaces = append(namespaces, p.Namespace)
		}
	}
	for _, ns := range namespaces {
		leases, err := clientset.CoordinationV1().Leases(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range leases.Items {
			holder := leases.Items[i].Spec.HolderIdentity
			if holder == nil {
				continue
			}
			for _, p := range running {
				if p.Name == *holder && p.Namespace == ns {
					return p
				}
			}
		}
	}
	return nil
}

func startPortForward(ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface, pod *corev1.Pod) (*Connection, error) {
	roundTripper, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("building spdy transport: %w", err)
	}

	reqURL := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("portforward").
		URL()

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, reqURL)
	return forward(ctx, dialer, controllerHTTPPort(pod))
}

// controllerHTTPPort is the port the controller's HTTP API listens on inside pod: the chart publishes
// config.httpPort as the controller container's port named "http".
func controllerHTTPPort(pod *corev1.Pod) int {
	fallback := 0
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		for _, p := range c.Ports {
			if p.Name != "http" || p.ContainerPort <= 0 {
				continue
			}
			if c.Name == "controller" {
				return int(p.ContainerPort)
			}
			if fallback == 0 {
				fallback = int(p.ContainerPort)
			}
		}
	}
	if fallback != 0 {
		return fallback
	}
	return defaultControllerPort
}

// forward opens a port-forward through dialer to remotePort and returns once the local end listens.
// The forwarder binds local port 0 itself and reports what it got, so no other process can take the
// port between choosing and binding it. Only 127.0.0.1 is bound because that is what BaseURL names:
// "localhost" would also bind ::1 and succeed when only one of the two was free.
func forward(ctx context.Context, dialer httpstream.Dialer, remotePort int) (*Connection, error) {
	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	errCh := make(chan error, 1)

	ports := []string{fmt.Sprintf("0:%d", remotePort)}
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, ports, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		close(stopCh)
		return nil, fmt.Errorf("creating port-forward: %w", err)
	}

	go func() {
		if ferr := fw.ForwardPorts(); ferr != nil {
			errCh <- ferr
		}
	}()

	timer := time.NewTimer(portForwardReadyTimeout)
	defer timer.Stop()
	select {
	case <-readyCh:
	case ferr := <-errCh:
		close(stopCh)
		return nil, fmt.Errorf("establishing port-forward: %w", ferr)
	case <-ctx.Done():
		close(stopCh)
		return nil, ctx.Err()
	case <-timer.C:
		close(stopCh)
		return nil, errors.New("timed out establishing port-forward to controller")
	}

	bound, err := fw.GetPorts()
	if err != nil {
		close(stopCh)
		return nil, fmt.Errorf("reading the port-forward's local port: %w", err)
	}
	if len(bound) == 0 || bound[0].Local == 0 {
		close(stopCh)
		return nil, errors.New("port-forward is ready but reports no local port")
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", bound[0].Local)
	var closeOnce sync.Once
	return &Connection{BaseURL: baseURL, Close: func() { closeOnce.Do(func() { close(stopCh) }) }}, nil
}
