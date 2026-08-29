package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// externalAgentLabel marks an agent running outside any Pod (no Downward API
// pod name), so consoles and dashboards can tell bare-host agents apart
// without guessing from the pod name's shape.
const externalAgentLabel = "kconmon-ng.io/external"

/*
resolveIdentity builds what this agent asserts about itself at registration.

The agent block (nodeName/advertiseAddress/zone) arrives already env-overridden by the config
loader; only the pure pod identity — KCONMON_NG_POD_NAME and KCONMON_NG_POD_IP — is read from the
environment here, because those describe a Pod and are never config keys. Every value has a
bare-host fallback: hostname for the names, the outbound interface for the address. The result must
pass the controller's validateAgentMeta unchanged — that is the whole design: external agents need
no server-side accommodation.
*/
func resolveIdentity(cfg *config.Config) (model.AgentInfo, error) {
	hostname, _ := os.Hostname()

	nodeName := cfg.Agent.NodeName
	if nodeName == "" {
		nodeName = hostname
	}
	if nodeName == "" {
		return model.AgentInfo{}, errors.New(
			"agent node name is empty and the hostname is unavailable; set agent.nodeName (or KCONMON_NG_NODE_NAME)")
	}

	podName := os.Getenv("KCONMON_NG_POD_NAME")
	var labels map[string]string
	if podName == "" {
		// No pod name means no Pod: fall back to the hostname (the pre-M6
		// behavior) and mark the agent external for everyone downstream.
		podName = hostname
		if podName == "" {
			podName = nodeName
		}
		labels = map[string]string{externalAgentLabel: "true"}
	}

	addr, err := resolveAdvertiseAddress(cfg)
	if err != nil {
		return model.AgentInfo{}, err
	}

	return model.AgentInfo{
		ID:       fmt.Sprintf("%s-%s", nodeName, podName),
		NodeName: nodeName,
		PodName:  podName,
		PodIP:    addr,
		Zone:     cfg.Agent.Zone,
		Labels:   labels,
	}, nil
}

/*
resolveAdvertiseAddress decides the address peers will probe: explicit config first, then the
Downward API pod IP, then outbound-interface autodetect. Config beats the pod IP env — an operator
who wrote the address down meant it — while KCONMON_NG_ADVERTISE_ADDRESS still beats a file value
because the loader folds that env override in before we get here. Whatever wins must be an IP
literal: the controller publishes it fleet-wide and rejects anything net.ParseIP refuses
(validateAgentMeta), so a bad value fails HERE with the fix in the message instead of surfacing as
a registration rejection.
*/
func resolveAdvertiseAddress(cfg *config.Config) (string, error) {
	if a := cfg.Agent.AdvertiseAddress; a != "" {
		// Re-checked even though the loader validates, because New is also
		// reachable with a hand-built config.
		if net.ParseIP(a) == nil {
			return "", fmt.Errorf("agent.advertiseAddress %q must be an IP literal (no hostname, no port)", a)
		}
		return a, nil
	}
	if v := os.Getenv("KCONMON_NG_POD_IP"); v != "" {
		if net.ParseIP(v) == nil {
			return "", fmt.Errorf("KCONMON_NG_POD_IP %q is not an IP address", v)
		}
		return v, nil
	}
	return detectOutboundAddress(cfg.ControllerAddress)
}

/*
detectOutboundAddress asks the kernel which source address a datagram to the controller would leave
from: a UDP "dial" transmits nothing and needs no listener, it only resolves the route, so this
works while the controller is down — though resolving a controller hostname still needs DNS. The
answer is exactly the address peers can reach when the host has one route to the fleet; multi-homed
hosts whose probe traffic should use another interface must set agent.advertiseAddress explicitly.
*/
func detectOutboundAddress(controllerAddress string) (string, error) {
	if controllerAddress == "" {
		return "", errors.New("cannot autodetect the advertise address: controllerAddress is empty; " +
			"set agent.advertiseAddress (or controllerAddress) in the config")
	}
	// The dial itself cannot block (UDP has no handshake); only resolving a
	// controller HOSTNAME can, so bound it rather than let a hung resolver
	// wedge startup with no error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", controllerAddress)
	if err != nil {
		return "", fmt.Errorf("autodetecting the advertise address from the route to %s: %w; "+
			"set agent.advertiseAddress explicitly", controllerAddress, err)
	}
	defer func() { _ = conn.Close() }()

	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || local.IP == nil || local.IP.IsUnspecified() {
		return "", fmt.Errorf("autodetect found no usable source IP on the route to %s (got %v); "+
			"set agent.advertiseAddress explicitly", controllerAddress, conn.LocalAddr())
	}
	return local.IP.String(), nil
}
