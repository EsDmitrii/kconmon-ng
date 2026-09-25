#!/usr/bin/env bash
# Real-kernel check of the pmtu verdicts across three network namespaces: pa (probe) -> pr (router)
# -> pb (echo). Needs root, iproute2 and iptables. Run it in a privileged container or on a CI VM,
# never in a workstation's own namespace.
set -euo pipefail
cd "$(dirname "$0")/../.."

bin=$(mktemp -d)/pmtu-netns
go build -o "$bin" ./hack/pmtu-netns

cleanup() {
  if [[ -n "${echo_pid:-}" ]]; then kill "$echo_pid" 2>/dev/null || true; fi
  for ns in pa pr pb; do ip netns del "$ns" 2>/dev/null || true; done
}
trap cleanup EXIT
cleanup

ip netns add pa; ip netns add pr; ip netns add pb
ip link add va type veth peer name vra; ip link set va netns pa; ip link set vra netns pr
ip link add vb type veth peer name vrb; ip link set vb netns pb; ip link set vrb netns pr
ip -n pa addr add 10.10.1.2/24 dev va; ip -n pr addr add 10.10.1.1/24 dev vra
ip -n pb addr add 10.10.2.2/24 dev vb; ip -n pr addr add 10.10.2.1/24 dev vrb
for ns in pa pr pb; do ip -n "$ns" link set lo up; done
ip -n pa link set va up; ip -n pr link set vra up; ip -n pr link set vrb up; ip -n pb link set vb up
ip -n pa route add default via 10.10.1.1
ip -n pb route add default via 10.10.2.1
ip netns exec pr sysctl -qw net.ipv4.ip_forward=1

ip netns exec pb "$bin" -mode echo -port 19090 &
echo_pid=$!
sleep 0.5

expect() { # name verdict path_mtu
  ip -n pa route flush cache 2>/dev/null || true
  local out
  out=$(ip netns exec pa "$bin" -mode probe -addr 10.10.2.2 -port 19090)
  echo "$1: $out"
  grep -q "\"verdict\":\"$2\"" <<<"$out" || { echo "FAIL: $1 wanted verdict $2"; exit 1; }
  grep -q "\"pathMtu\":$3[,}]" <<<"$out" || { echo "FAIL: $1 wanted pathMtu $3"; exit 1; }
}

expect "clean path" ok 1500

ip -n pr link set vrb mtu 1400
expect "narrow link, ICMP flows" reduced 1400

ip netns exec pr iptables -A OUTPUT -p icmp --icmp-type fragmentation-needed -j DROP
expect "narrow link, ICMP dropped" blackhole 1400

ip netns exec pr iptables -F OUTPUT
ip -n pr link set vrb mtu 1500
expect "link restored" ok 1500

echo "pmtu-netns: all four verdicts match"
