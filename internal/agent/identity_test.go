package agent

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/controller"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
)

// clearPodEnv blanks the Downward API variables so a test describes a bare
// host regardless of what the developer's shell carries.
func clearPodEnv(t *testing.T) {
	t.Helper()
	t.Setenv("KCONMON_NG_POD_NAME", "")
	t.Setenv("KCONMON_NG_POD_IP", "")
}

// A bare host has no Downward API: nodeName and podName fall back to the
// hostname, and the absence of a pod name marks the agent external.
func TestResolveIdentityBareHostFallsBackToHostname(t *testing.T) {
	clearPodEnv(t)
	cfg := config.DefaultConfig()
	cfg.Agent.AdvertiseAddress = "192.0.2.10"
	cfg.Agent.Zone = "dc-east"

	info, err := resolveIdentity(cfg)
	if err != nil {
		t.Fatalf("resolveIdentity: %v", err)
	}

	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}
	if info.NodeName != host {
		t.Errorf("NodeName = %q, want hostname %q", info.NodeName, host)
	}
	if info.PodName != host {
		t.Errorf("PodName = %q, want hostname fallback %q", info.PodName, host)
	}
	if want := host + "-" + host; info.ID != want {
		t.Errorf("ID = %q, want %q", info.ID, want)
	}
	if info.PodIP != "192.0.2.10" {
		t.Errorf("PodIP = %q, want the configured advertise address", info.PodIP)
	}
	if info.Zone != "dc-east" {
		t.Errorf("Zone = %q, want dc-east", info.Zone)
	}
	if info.Labels[externalAgentLabel] != "true" {
		t.Errorf("labels = %v, want %s=true on an agent without a pod name", info.Labels, externalAgentLabel)
	}
}

// In-cluster nothing may change: the Downward API env keeps filling the pod
// identity and no external marker appears.
func TestResolveIdentityInClusterKeepsPodIdentity(t *testing.T) {
	t.Setenv("KCONMON_NG_POD_NAME", "kconmon-ng-agent-x7c9k")
	t.Setenv("KCONMON_NG_POD_IP", "10.42.0.17")
	cfg := config.DefaultConfig()
	cfg.Agent.NodeName = "worker-3" // the loader put KCONMON_NG_NODE_NAME here
	cfg.Agent.Zone = "zone-b"

	info, err := resolveIdentity(cfg)
	if err != nil {
		t.Fatalf("resolveIdentity: %v", err)
	}
	if info.NodeName != "worker-3" {
		t.Errorf("NodeName = %q, want worker-3", info.NodeName)
	}
	if info.PodName != "kconmon-ng-agent-x7c9k" {
		t.Errorf("PodName = %q, want the pod env value", info.PodName)
	}
	if info.ID != "worker-3-kconmon-ng-agent-x7c9k" {
		t.Errorf("ID = %q, want node-pod", info.ID)
	}
	if info.PodIP != "10.42.0.17" {
		t.Errorf("PodIP = %q, want the Downward API pod IP", info.PodIP)
	}
	if _, marked := info.Labels[externalAgentLabel]; marked {
		t.Errorf("an in-pod agent must not carry the external marker, got labels %v", info.Labels)
	}
}

// Precedence for the advertise address: explicit config, then the Downward API
// pod IP, then outbound-interface autodetect — and every failure names a fix.
func TestResolveAdvertiseAddressPrecedence(t *testing.T) {
	// A loopback listener gives autodetect a real, routable destination; UDP
	// dial sends nothing, so nothing needs to answer.
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	loopbackController := pc.LocalAddr().String()

	tests := []struct {
		name           string
		configAddress  string
		podIPEnv       string
		controllerAddr string
		want           string
		wantErr        string
	}{
		{
			name:          "explicit config wins over the pod IP env",
			configAddress: "192.0.2.10",
			podIPEnv:      "10.42.0.17",
			want:          "192.0.2.10",
		},
		{
			name:     "pod IP env fills an empty config",
			podIPEnv: "10.42.0.17",
			want:     "10.42.0.17",
		},
		{
			name:           "autodetect from the route to the controller",
			controllerAddr: loopbackController,
			want:           "127.0.0.1",
		},
		{
			name:    "nothing available is a clear error",
			wantErr: "advertise",
		},
		{
			name:     "malformed pod IP env is refused, not forwarded to the controller",
			podIPEnv: "not-an-ip",
			wantErr:  "KCONMON_NG_POD_IP",
		},
		{
			name:          "config address with a port is refused",
			configAddress: "192.0.2.10:8080",
			wantErr:       "advertiseAddress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KCONMON_NG_POD_IP", tt.podIPEnv)
			cfg := config.DefaultConfig()
			cfg.Agent.AdvertiseAddress = tt.configAddress
			cfg.ControllerAddress = tt.controllerAddr

			got, err := resolveAdvertiseAddress(cfg)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want an error containing %q, got address %q", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAdvertiseAddress: %v", err)
			}
			if got != tt.want {
				t.Errorf("address = %q, want %q", got, tt.want)
			}
			if net.ParseIP(got) == nil {
				t.Errorf("address %q is not an IP literal; the controller would reject it", got)
			}
		})
	}
}

/*
M6-1's contract: the server side needs NO changes for external agents. A registration built from
hostname identity, an autodetected address and an explicit zone must pass the UNMODIFIED controller
validate path (validateAgentMeta) — this test registers against the real controller gRPC server, so
any future server-side tightening that would break bare-host agents fails here first.
*/
func TestHostIdentityRegistersAgainstUnmodifiedController(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	reg := controller.NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test_host_identity", prometheus.NewRegistry())
	srv := controller.NewGRPCServer(reg, m, false, nil, false)
	gs := grpc.NewServer()
	srv.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	// A bare host: no Downward API env at all, only a controller address and a zone.
	clearPodEnv(t)
	cfg := config.DefaultConfig()
	cfg.ControllerAddress = lis.Addr().String()
	cfg.Agent.Zone = "dc-east"

	info, err := resolveIdentity(cfg)
	if err != nil {
		t.Fatalf("resolveIdentity on a bare host: %v", err)
	}

	client, err := NewGRPCClient(cfg.ControllerAddress, ClientSecurity{})
	if err != nil {
		t.Fatalf("NewGRPCClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	_, resolvedZone, err := client.Register(ctx, info, cfg.HTTPPort)
	if err != nil {
		t.Fatalf("the unmodified controller rejected a host-identity registration: %v", err)
	}
	if resolvedZone != "dc-east" {
		t.Errorf("controller resolved zone %q, want the asserted dc-east", resolvedZone)
	}

	all := reg.GetAll()
	if len(all) != 1 {
		t.Fatalf("registry holds %d agents, want 1", len(all))
	}
	got := all[0]
	if net.ParseIP(got.PodIP) == nil {
		t.Errorf("registered address %q is not an IP literal", got.PodIP)
	}
	if got.Labels[externalAgentLabel] != "true" {
		t.Errorf("the external marker did not survive the register path, labels: %v", got.Labels)
	}
	if got.Zone != "dc-east" {
		t.Errorf("registered zone = %q, want dc-east", got.Zone)
	}
}
