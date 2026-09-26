#!/usr/bin/env bash
# Real-kernel check of the pmtu verdicts across three network namespaces: pa (probe) -> pr (router)
# -> pb (echo), over IPv4 and IPv6. Needs root, iproute2, iptables and ip6tables. Run it in a
# privileged container or on a CI VM, never in a workstation's own namespace.
set -euo pipefail
cd "$(dirname "$0")/../.."

tmp=$(mktemp -d)
bin=$tmp/pmtu-netns

del_ns() {
  for ns in pa pr pb; do ip netns del "$ns" 2>/dev/null || true; done
}
cleanup() {
  if [[ -n "${echo_pid:-}" ]]; then kill "$echo_pid" 2>/dev/null || true; fi
  del_ns
  rm -rf "$tmp"
}
trap cleanup EXIT

go build -o "$bin" ./hack/pmtu-netns
del_ns

ip netns add pa; ip netns add pr; ip netns add pb
ip link add va type veth peer name vra; ip link set va netns pa; ip link set vra netns pr
ip link add vb type veth peer name vrb; ip link set vb netns pb; ip link set vrb netns pr
ip -n pa addr add 10.10.1.2/24 dev va; ip -n pr addr add 10.10.1.1/24 dev vra
ip -n pb addr add 10.10.2.2/24 dev vb; ip -n pr addr add 10.10.2.1/24 dev vrb
ip -n pa addr add fd00:1::2/64 dev va nodad; ip -n pr addr add fd00:1::1/64 dev vra nodad
ip -n pb addr add fd00:2::2/64 dev vb nodad; ip -n pr addr add fd00:2::1/64 dev vrb nodad
for ns in pa pr pb; do ip -n "$ns" link set lo up; done
ip -n pa link set va up; ip -n pr link set vra up; ip -n pr link set vrb up; ip -n pb link set vb up
ip -n pa route add default via 10.10.1.1
ip -n pb route add default via 10.10.2.1
ip -n pa -6 route add default via fd00:1::1
ip -n pb -6 route add default via fd00:2::1
ip netns exec pr sysctl -qw net.ipv4.ip_forward=1
ip netns exec pr sysctl -qw net.ipv6.conf.all.forwarding=1

ip netns exec pb "$bin" -mode echo -port 19090 &
echo_pid=$!
# A probe sent before the echo is bound is refused and would read as unreachable.
echo_bound() { [[ -n $(ip netns exec pb ss -Hlun 'sport = :19090') ]]; }
for _ in $(seq 100); do
  if echo_bound; then break; fi
  kill -0 "$echo_pid" 2>/dev/null || { echo "FAIL: the echo responder exited"; exit 1; }
  sleep 0.1
done
echo_bound || { echo "FAIL: the echo never bound :19090"; exit 1; }

peer=10.10.2.2
probe() { # name verdict path_mtu [error substring]
  local out
  out=$(ip netns exec pa "$bin" -mode probe -addr "$peer" -port 19090)
  echo "$1: $out"
  grep -q "\"verdict\":\"$2\"" <<<"$out" || { echo "FAIL: $1 wanted verdict $2"; exit 1; }
  grep -q "\"pathMtu\":$3[,}]" <<<"$out" || { echo "FAIL: $1 wanted pathMtu $3"; exit 1; }
  if [[ -n "${4:-}" ]]; then
    grep -qF "$4" <<<"$out" || { echo "FAIL: $1 wanted an error naming: $4"; exit 1; }
  fi
}
expect() { # same arguments; starts from an empty route cache
  ip -n pa -4 route flush cache 2>/dev/null || true
  ip -n pa -6 route flush cache 2>/dev/null || true
  probe "$@"
}

expect "clean path" ok 1500

ip -n pr link set vrb mtu 1400
expect "narrow link, ICMP flows" reduced 1400

# The frag-needed above left a learned MTU of 1400 in pa's route cache. PROBE mode must ignore it:
# the restored path reads ok at 1500 without a flush.
ip -n pr link set vrb mtu 1500
if [[ $(ip -n pa route get 10.10.2.2) != *"mtu 1400"* ]]; then
  echo "FAIL: no learned MTU in pa's route cache, the next step would prove nothing"; exit 1
fi
probe "link restored, learned MTU still cached" ok 1500

ip -n pr link set vrb mtu 1400
ip netns exec pr iptables -A OUTPUT -p icmp --icmp-type fragmentation-needed -j DROP
expect "narrow link, ICMP dropped" blackhole 1400

ip netns exec pr iptables -F OUTPUT
ip -n pr link set vrb mtu 1500
expect "link restored" ok 1500

# Cilium keeps the pod's link at 1500 and puts the tunnel MTU on its routes. The probe must ask
# about the route's 1400, the size the pod sends, even where the path drops anything larger.
ip -n pa route replace default via 10.10.1.1 mtu 1400
ip netns exec pr iptables -A FORWARD -m length --length 1401:65535 -j DROP
expect "route MTU below the link, larger datagrams dropped" ok 1400
ip netns exec pr iptables -F FORWARD
ip -n pa route replace default via 10.10.1.1

# A hostNetwork agent whose node IP sits on lo: the egress link, not lo's 65536, sizes the probe.
ip -n pa addr add 10.10.9.9/32 dev lo
ip -n pa route replace default via 10.10.1.1 src 10.10.9.9
ip -n pr route add 10.10.9.9/32 via 10.10.1.2
expect "source address on lo" ok 1500
ip -n pr route del 10.10.9.9/32
ip -n pa route replace default via 10.10.1.1
ip -n pa addr del 10.10.9.9/32 dev lo

ip netns exec pr iptables -A FORWARD -p udp -j DROP
expect "every datagram dropped" unreachable 0 "did not answer a 64-byte datagram"
ip netns exec pr iptables -F FORWARD

# IPv6: a 40-byte header, IPV6_MTU read back, and ICMPv6 packet-too-big in place of frag-needed.
peer=fd00:2::2
expect "IPv6 clean path" ok 1500

ip -n pr link set vrb mtu 1400
expect "IPv6 narrow link, ICMPv6 flows" reduced 1400

ip -n pr link set vrb mtu 1500
if [[ $(ip -n pa -6 route get fd00:2::2) != *"mtu 1400"* ]]; then
  echo "FAIL: no learned MTU in pa's IPv6 route cache, the next step would prove nothing"; exit 1
fi
probe "IPv6 link restored, learned MTU still cached" ok 1500
ip -n pr link set vrb mtu 1400

ip netns exec pr ip6tables -A OUTPUT -p icmpv6 --icmpv6-type packet-too-big -j DROP
expect "IPv6 narrow link, ICMPv6 dropped" blackhole 1400

ip netns exec pr ip6tables -F OUTPUT
ip -n pr link set vrb mtu 1500
expect "IPv6 link restored" ok 1500

ip -n pa -6 route replace default via fd00:1::1 mtu 1400
ip netns exec pr ip6tables -A FORWARD -m length --length 1401:65535 -j DROP
expect "IPv6 route MTU below the link, larger datagrams dropped" ok 1400
ip netns exec pr ip6tables -F FORWARD
ip -n pa -6 route replace default via fd00:1::1

# An agent probed at a secondary address (advertiseAddress on a multi-homed host): the probes use
# connected sockets, so the echo must answer from the address it was probed at, whichever of the two
# the kernel would pick as the source of its reply.
udp_probe() { # name
  local out
  out=$(ip netns exec pa "$bin" -mode udp -addr "$peer" -port 19090)
  echo "$1: $out"
  grep -q '"success":true' <<<"$out" || { echo "FAIL: $1 wanted udp success"; exit 1; }
}
ip -n pb addr add 10.10.2.50/24 dev vb
ip -n pb addr add fd00:2::50/64 dev vb nodad
for peer in 10.10.2.2 10.10.2.50 fd00:2::2 fd00:2::50; do
  expect "echo at $peer, one of two addresses" ok 1500
  udp_probe "udp to $peer, one of two addresses"
done

echo "pmtu-netns: ok, reduced, blackhole and unreachable match on a real kernel over IPv4, and ok, reduced," \
  "blackhole over IPv6; PROBE mode and the route lookup ignore a cached MTU; a route MTU sizes the probe;" \
  "the echo answers from the address it was probed at"
