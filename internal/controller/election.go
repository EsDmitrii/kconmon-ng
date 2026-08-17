package controller

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Lease timings: a failover completes well inside one agent TTL, so agents relocate before the new
// leader would start evicting them.
const (
	defaultLeaseDuration = 15 * time.Second
	defaultRenewDeadline = 10 * time.Second
	defaultRetryPeriod   = 2 * time.Second

	defaultLeaseName = "kconmon-ng-controller"

	// serviceAccountNamespaceFile is where kubelet projects the pod's namespace.
	serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace" //nolint:gosec // G101: a path, not a credential
)

// electionOptions is everything the Lease-backed election needs; tests shorten the timings.
type electionOptions struct {
	client        kubernetes.Interface
	namespace     string
	leaseName     string
	identity      string
	leaseDuration time.Duration
	renewDeadline time.Duration
	retryPeriod   time.Duration
}

// electionOptionsFor builds in-cluster options from the pod's downward-API environment. The
// namespace is empty when it cannot be determined, which the caller treats as "no election".
func electionOptionsFor(client kubernetes.Interface) *electionOptions {
	identity := os.Getenv("KCONMON_NG_POD_NAME")
	if identity == "" {
		identity, _ = os.Hostname()
	}

	namespace := os.Getenv("KCONMON_NG_POD_NAMESPACE")
	if namespace == "" {
		if b, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
			namespace = strings.TrimSpace(string(b))
		}
	}

	leaseName := os.Getenv("KCONMON_NG_LEASE_NAME")
	if leaseName == "" {
		leaseName = defaultLeaseName
	}

	return &electionOptions{
		client:        client,
		namespace:     namespace,
		leaseName:     leaseName,
		identity:      identity,
		leaseDuration: defaultLeaseDuration,
		renewDeadline: defaultRenewDeadline,
		retryPeriod:   defaultRetryPeriod,
	}
}

// runLeaderElection contends for the Lease until ctx is done. Losing it is normal, so the replica
// keeps contending instead of exiting.
func (c *Controller) runLeaderElection(ctx context.Context, opts *electionOptions) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: opts.leaseName, Namespace: opts.namespace},
		Client:     opts.client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: opts.identity},
	}

	cfg := leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: opts.leaseDuration,
		RenewDeadline: opts.renewDeadline,
		RetryPeriod:   opts.retryPeriod,
		// Hand the lease back on SIGTERM so a rolling restart fails over in one retry period.
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(context.Context) {
				slog.Info("acquired controller lease",
					"lease", opts.leaseName, "namespace", opts.namespace, "identity", opts.identity)
				c.SetLeader(true)
			},
			OnStoppedLeading: func() {
				slog.Warn("lost controller lease",
					"lease", opts.leaseName, "namespace", opts.namespace, "identity", opts.identity)
				c.SetLeader(false)
			},
		},
	}

	for ctx.Err() == nil {
		le, err := leaderelection.NewLeaderElector(cfg)
		if err != nil {
			slog.Error("leader election unavailable, staying a standby", "error", err)
			return
		}

		le.Run(ctx)

		select {
		case <-ctx.Done():
		case <-time.After(opts.retryPeriod):
		}
	}
}
