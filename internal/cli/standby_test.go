package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// chainConnector hands out one Connection per server in order, the way kubeConnector walks the
// running controller pods, and records which connections were closed.
type chainConnector struct {
	urls   []string
	mu     sync.Mutex
	closed []int
}

func (c *chainConnector) conn(i int) *Connection {
	conn := &Connection{BaseURL: c.urls[i], Close: func() {
		c.mu.Lock()
		c.closed = append(c.closed, i)
		c.mu.Unlock()
	}}
	if i+1 < len(c.urls) {
		conn.Next = func(context.Context) (*Connection, error) { return c.conn(i + 1), nil }
	}
	return conn
}

func (c *chainConnector) Connect(context.Context) (*Connection, error) { return c.conn(0), nil }

func runCLIChain(t *testing.T, handlers []http.Handler, args ...string) (out string, code int, cc *chainConnector) {
	t.Helper()
	cc = &chainConnector{}
	for _, h := range handlers {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		cc.urls = append(cc.urls, srv.URL)
	}
	stdout, stderr, err := runRoot(t, cc, args...)
	return stdout + stderr, exitCode(err), cc
}

func standbyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not the leader", http.StatusServiceUnavailable)
	})
}

// A standby's 503 is not final: with the Lease unreadable the CLI may have picked the standby, so it
// moves on to the next running controller pod, which is the leader.
func TestCLIMovesPastAStandbyToTheLeader(t *testing.T) {
	out, code, cc := runCLIChain(t, []http.Handler{standbyHandler(), topologyHandler()}, "topology")
	if code != exitOK {
		t.Fatalf("exit=%d, want 0 from the leader behind the standby\n%s", code, out)
	}
	if !strings.Contains(out, "node-1") {
		t.Errorf("expected the leader's topology, got:\n%s", out)
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if len(cc.closed) != 2 {
		t.Errorf("closed connections %v, want both the standby's and the leader's", cc.closed)
	}
}

// Every pod a standby (leadership is moving): the last 503 is the answer, exit 1.
func TestCLIReportsTheStandbyWhenNoPodLeads(t *testing.T) {
	out, code, _ := runCLIChain(t, []http.Handler{standbyHandler(), standbyHandler()}, "topology")
	if code != exitError {
		t.Fatalf("exit=%d, want 1\n%s", code, out)
	}
}

// unreachableNextConnector connects to a standby whose next replica cannot be port-forwarded to.
type unreachableNextConnector struct{ url string }

func (c unreachableNextConnector) Connect(context.Context) (*Connection, error) {
	return &Connection{BaseURL: c.url, Close: func() {}, Next: func(context.Context) (*Connection, error) {
		return nil, errors.New(`pods "controller-b" is forbidden: cannot create resource "pods/portforward"`)
	}}, nil
}

// A standby's 503 alone reads as "no leader"; when a leader may exist but could not be reached, the
// error has to say why.
func TestCLISaysWhyTheNextControllerPodWasNotReached(t *testing.T) {
	srv := httptest.NewServer(standbyHandler())
	t.Cleanup(srv.Close)

	_, _, err := runRoot(t, unreachableNextConnector{url: srv.URL}, "topology")
	if exitCode(err) != exitError {
		t.Fatalf("exit=%d, want 1 (err=%v)", exitCode(err), err)
	}
	if msg := err.Error(); !strings.Contains(msg, "not the leader") || !strings.Contains(msg, "pods/portforward") {
		t.Errorf("error = %q, want the standby's answer and why the next pod could not be reached", msg)
	}
}

// Only a standby's answer moves on: any other error is the controller's verdict, and the next pod is
// never asked.
func TestCLIDoesNotRetryOtherErrors(t *testing.T) {
	var asked bool
	second := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { asked = true })
	_, code, _ := runCLIChain(t, []http.Handler{
		diagnosticsHandler(http.StatusNotFound, "no agent registered on source node\n"), second,
	}, "check", "ghost", "node-2")
	if code != exitError {
		t.Fatalf("exit=%d, want 1", code)
	}
	if asked {
		t.Error("a 404 was retried against the next controller pod")
	}
}

// With leases unreadable (RBAC without coordination.k8s.io), every running pod is a candidate, so a
// standby listed first does not strand the CLI.
func TestControllerPodsListsEveryRunningPodWhenTheLeaseIsUnreadable(t *testing.T) {
	clientset := fake.NewClientset(
		controllerPod("controller-a"),
		controllerPod("controller-b"),
		controllerLease("controller-b"),
	)
	clientset.PrependReactor("list", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New(`leases.coordination.k8s.io is forbidden`)
	})

	k := &kubeConnector{namespace: testNamespace}
	pods, err := k.controllerPods(context.Background(), clientset)
	if err != nil {
		t.Fatalf("controllerPods: %v", err)
	}
	if got := podNames(pods); !slices.Equal(got, []string{"controller-a", "controller-b"}) {
		t.Errorf("candidates %v, want both running pods", got)
	}
}

// Without -n the Lease is read only where controller pods run: a cluster-wide list would also read
// one Lease per node from kube-node-lease on every invocation.
func TestControllerPodsReadsLeasesOnlyWhereControllersRun(t *testing.T) {
	clientset := fake.NewClientset(controllerPod("controller-a"), controllerPod("controller-b"), controllerLease("controller-b"))

	k := &kubeConnector{}
	pods, err := k.controllerPods(context.Background(), clientset)
	if err != nil {
		t.Fatalf("controllerPods: %v", err)
	}
	if got := podNames(pods); !slices.Equal(got, []string{"controller-b", "controller-a"}) {
		t.Errorf("candidates %v, want the lease holder controller-b first", got)
	}
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "list" && a.GetResource().Resource == "leases" && a.GetNamespace() != testNamespace {
			t.Errorf("listed leases in namespace %q, want only %q", a.GetNamespace(), testNamespace)
		}
	}
}

func podNames(pods []*corev1.Pod) []string {
	out := make([]string, len(pods))
	for i, p := range pods {
		out[i] = p.Name
	}
	return out
}

// Without -n the search spans every namespace, and the pods after the first are only the same
// release's replicas: a standby's 503 must not land the CLI on another install's controller. The fake
// lists by namespace, then name, so the namespace decides which install leads.
func TestControllerPodsStayInTheFirstPodsNamespace(t *testing.T) {
	for _, tc := range []struct {
		foreignNamespace string
		want             []string
	}{
		{"zz-other-install", []string{"controller-a", "controller-b"}},
		{"another-install", []string{"controller-z"}},
	} {
		t.Run(tc.foreignNamespace, func(t *testing.T) {
			other := controllerPod("controller-z")
			other.Namespace = tc.foreignNamespace
			clientset := fake.NewClientset(controllerPod("controller-a"), controllerPod("controller-b"), other)

			k := &kubeConnector{}
			pods, err := k.controllerPods(context.Background(), clientset)
			if err != nil {
				t.Fatalf("controllerPods: %v", err)
			}
			if got := podNames(pods); !slices.Equal(got, tc.want) {
				t.Errorf("candidates %v, want %v", got, tc.want)
			}
		})
	}
}
