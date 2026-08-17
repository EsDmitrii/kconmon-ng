package cli

import (
	"context"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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

// TestFindControllerPodPrefersLeaseHolder is what keeps `kubectl kconmon` working against an HA
// deployment: only the leader answers topology and diagnostics, and this CLI turns the standby's
// 503 into a hard error rather than retrying.
func TestFindControllerPodPrefersLeaseHolder(t *testing.T) {
	clientset := fake.NewClientset(
		controllerPod("controller-a"),
		controllerPod("controller-b"),
		controllerLease("controller-b"),
	)

	k := &kubeConnector{namespace: testNamespace}
	pod, err := k.findControllerPod(context.Background(), clientset)
	if err != nil {
		t.Fatalf("findControllerPod: %v", err)
	}
	if pod.Name != "controller-b" {
		t.Errorf("picked %s, want the lease holder controller-b", pod.Name)
	}
}

// TestFindControllerPodFallsBackWithoutLease covers single-replica installs, which run without
// leader election and therefore without a Lease.
func TestFindControllerPodFallsBackWithoutLease(t *testing.T) {
	clientset := fake.NewClientset(controllerPod("controller-a"))

	k := &kubeConnector{namespace: testNamespace}
	pod, err := k.findControllerPod(context.Background(), clientset)
	if err != nil {
		t.Fatalf("findControllerPod: %v", err)
	}
	if pod.Name != "controller-a" {
		t.Errorf("picked %s, want controller-a", pod.Name)
	}
}

// TestFindControllerPodIgnoresForeignLease guards against matching an unrelated Lease that happens
// to sit in the same namespace.
func TestFindControllerPodIgnoresForeignLease(t *testing.T) {
	clientset := fake.NewClientset(
		controllerPod("controller-a"),
		controllerLease("some-other-workload"),
	)

	k := &kubeConnector{namespace: testNamespace}
	pod, err := k.findControllerPod(context.Background(), clientset)
	if err != nil {
		t.Fatalf("findControllerPod: %v", err)
	}
	if pod.Name != "controller-a" {
		t.Errorf("picked %s, want the running pod controller-a", pod.Name)
	}
}

// TestFindControllerPodNoRunningPods keeps the original error when nothing is running.
func TestFindControllerPodNoRunningPods(t *testing.T) {
	k := &kubeConnector{namespace: testNamespace}
	if _, err := k.findControllerPod(context.Background(), fake.NewClientset()); err == nil {
		t.Fatal("expected an error when no controller pod is running")
	}
}
