import { describe, expect, it } from "vitest";
import { isValidAdhocAddress } from "./utils";

/**
 * Contract-drift pins for isValidAdhocAddress. Its docstring claims it mirrors
 * Go's store.validateAdhocAddress (internal/console/store/targets.go), but the
 * pre-fix version accepted a set of shapes Go refuses, because JS Number()
 * speaks hex/exponent/sign/whitespace and Go's strconv.ParseUint(base 10) does
 * not -- and because net.SplitHostPort rejects colon shapes the old structural
 * IPv6 guess let through. Every case below asserts TS === Go's documented
 * behavior. Go's reasoning is recorded next to each row.
 */
describe("isValidAdhocAddress mirrors Go's port + colon grammar", () => {
  it.each([
    // net.SplitHostPort(":") -> host="", port="" -> ParseUint("") fails.
    ["a bare colon", ":"],
    // SplitHostPort("::::") -> host part ":::" has a colon -> "too many colons";
    // the whole string then fails netip.ParseAddr as an IPv6 literal.
    ["four colons", "::::"],
    // SplitHostPort(":80") -> host="" ; the empty host passes neither isIPLiteral
    // nor isHostname. The old code read ":80" as an IPv6 literal and accepted it.
    ["leading colon with a port", ":80"],
    // ParseUint("0x50", 10, 16) fails: hex is not a base-10 port.
    ["a hex port on an IP", "10.0.0.1:0x50"],
    ["a hex port on a host", "example.test:0x50"],
    // ParseUint("1e3", 10, 16) fails: an exponent is not a base-10 port.
    ["an exponent port after a lone colon", ":1e3"],
    ["an exponent port on a host", "example.test:1e3"],
    // ParseUint("+80", 10, 16) fails: ParseUint rejects a sign.
    ["a signed port after a lone colon", ":+80"],
    ["a signed port on a host", "example.test:+80"],
    ["a signed port on an IP", "10.0.0.1:+80"],
    // ParseUint(" 80", 10, 16) fails: ParseUint rejects surrounding whitespace.
    ["a space before the port after a lone colon", ": 80"],
    ["a space before the port on a host", "example.test: 80"],
  ])("refuses %s, matching Go", (_name, address) => {
    expect(isValidAdhocAddress(address)).toBe(false);
  });

  // The other direction: the tightened grammar must NOT start refusing shapes Go
  // still accepts, or it would block legitimate diagnostics runs.
  it.each([
    ["host:port with a decimal port", "example.test:8443"],
    ["ip:port with a decimal port", "10.0.0.1:8080"],
    ["a port with a leading zero, which ParseUint accepts", "example.test:080"],
    ["a bracketed IPv6 with a port", "[fe80::1]:443"],
    ["a bracketed IPv6 loopback with a port", "[::1]:443"],
    ["a bare IPv6 literal", "2001:db8::1"],
    ["a bare compressed IPv6 literal", "::1"],
    ["a bare host", "example.test"],
  ])("still accepts %s, matching Go", (_name, address) => {
    expect(isValidAdhocAddress(address)).toBe(true);
  });
});
