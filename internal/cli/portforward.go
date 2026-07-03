package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// controllerPort is the controller's HTTP API port inside the pod.
const controllerPort = 8080

// controllerLabelSelector matches controller pods managed by the chart.
const controllerLabelSelector = "app.kubernetes.io/name=kconmon-ng,app.kubernetes.io/component=controller"

// portForwardReadyTimeout bounds how long we wait for the tunnel to come up.
const portForwardReadyTimeout = 30 * time.Second

// Connection is an established route to a controller's HTTP API. Close must be
// called to tear down the underlying port-forward.
type Connection struct {
	BaseURL string
	Close   func()
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

// Connect finds a running controller pod (searching all namespaces when one was
// not given) and port-forwards a random local port to it.
func (k *kubeConnector) Connect(ctx context.Context) (*Connection, error) {
	cfg, err := k.restConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}

	pod, err := k.findControllerPod(ctx, clientset)
	if err != nil {
		return nil, err
	}

	return startPortForward(ctx, cfg, clientset, pod)
}

// findControllerPod locates a running controller pod. When a namespace is set
// it searches only there; otherwise it searches all namespaces.
func (k *kubeConnector) findControllerPod(ctx context.Context, clientset kubernetes.Interface) (*corev1.Pod, error) {
	ns := k.namespace // "" means all namespaces
	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: controllerLabelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing controller pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			return p, nil
		}
	}

	where := "any namespace"
	if ns != "" {
		where = "namespace " + ns
	}
	return nil, fmt.Errorf("no running kconmon-ng controller pod found in %s (selector %q)", where, controllerLabelSelector)
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

	local, err := freeLocalPort(ctx)
	if err != nil {
		return nil, err
	}

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	errCh := make(chan error, 1)

	ports := []string{fmt.Sprintf("%d:%d", local, controllerPort)}
	fw, err := portforward.New(dialer, ports, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		close(stopCh)
		return nil, fmt.Errorf("creating port-forward: %w", err)
	}

	go func() {
		if ferr := fw.ForwardPorts(); ferr != nil {
			errCh <- ferr
		}
	}()

	select {
	case <-readyCh:
	case ferr := <-errCh:
		close(stopCh)
		return nil, fmt.Errorf("establishing port-forward: %w", ferr)
	case <-time.After(portForwardReadyTimeout):
		close(stopCh)
		return nil, errors.New("timed out establishing port-forward to controller")
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", local)
	var closeOnce sync.Once
	return &Connection{BaseURL: baseURL, Close: func() { closeOnce.Do(func() { close(stopCh) }) }}, nil
}

// freeLocalPort asks the OS for a free TCP port on the loopback interface.
func freeLocalPort(ctx context.Context) (int, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("finding free local port: %w", err)
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("unexpected listener address type")
	}
	return addr.Port, nil
}
