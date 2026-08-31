package main

import (
	"net"
	"testing"
)

// Property: every fleet and probe identity the rig can generate is unique and passes the
// controller's validateAgentMeta contract (non-empty id/node, parseable pod IP). A duplicate IP or
// ID would silently merge two agents in the registry and quietly shrink the measured fleet.
func TestRigIdentitiesUniqueAndValid(t *testing.T) {
	seenIP := make(map[string]int)
	seenID := make(map[string]int)

	check := func(kind string, idx int, id, node, ip string) {
		t.Helper()
		if id == "" || node == "" {
			t.Fatalf("%s %d: empty id or node", kind, idx)
		}
		if net.ParseIP(ip) == nil {
			t.Fatalf("%s %d: pod IP %q does not parse", kind, idx, ip)
		}
		if prev, dup := seenIP[ip]; dup {
			t.Fatalf("%s %d: IP %s already used by index %d", kind, idx, ip, prev)
		}
		if prev, dup := seenID[id]; dup {
			t.Fatalf("%s %d: ID %s already used by index %d", kind, idx, id, prev)
		}
		seenIP[ip] = idx
		seenID[id] = idx
	}

	for i := range 5000 {
		info := fleetAgentInfo(i)
		check("fleet", i, info.ID, info.NodeName, info.PodIP)
		// The identity contract mirrored from internal/agent resolveIdentity: ID = node-pod.
		if info.ID != info.NodeName+"-"+info.PodName {
			t.Fatalf("fleet %d: ID %q is not node-pod", i, info.ID)
		}
	}
	for p := 1; p <= 200; p++ {
		info := probeAgentInfo(p)
		check("probe", p, info.ID, info.NodeName, info.PodIP)
	}
}

// A restart must change the pod identity (new pod name => new agent ID, like a real pod
// replacement) while keeping the node identity stable.
func TestRestartedIdentityChangesPodKeepsNode(t *testing.T) {
	base := fleetAgentInfo(42)
	r1 := restartedAgentInfo(base, 42, 1)
	if r1.NodeName != base.NodeName {
		t.Fatalf("restart changed node name: %q -> %q", base.NodeName, r1.NodeName)
	}
	if r1.ID == base.ID || r1.PodName == base.PodName {
		t.Fatalf("restart kept pod identity: id=%q pod=%q", r1.ID, r1.PodName)
	}
	if r1.PodIP != base.PodIP {
		t.Fatalf("rig keeps the pod IP stable across restarts: %q -> %q", base.PodIP, r1.PodIP)
	}
	if r1.ID != r1.NodeName+"-"+r1.PodName {
		t.Fatalf("restarted ID %q is not node-pod", r1.ID)
	}
}
