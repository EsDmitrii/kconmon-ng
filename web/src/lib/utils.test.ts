import { describe, expect, it } from "vitest";
import {
  CHECKBOX_CLASS,
  endSentence,
  escapeLabelValue,
  fmtEventTime,
  isValidAdhocAddress,
  plural,
  runInstant,
  runsAtOrBefore,
} from "./utils";

describe("escapeLabelValue", () => {
  it("escapes the two characters PromQL string literals care about", () => {
    expect(escapeLabelValue('a"b\\c')).toBe('a\\"b\\\\c');
  });
});

describe("fmtEventTime", () => {
  it("hands back the wire's own bytes for an unparseable stamp", () => {
    expect(fmtEventTime("not a time")).toBe("not a time");
  });
});

/**
 * runsAtOrBefore — the Time Machine's cut across a run list (QA round 2,
 * finding #6). GET /api/v1/runs has no time filter, so a card rendering its
 * newest page under a banner reading "you are viewing 12:00" was listing runs
 * that had not happened yet.
 */
describe("runInstant", () => {
  it("prefers the moment the run STARTED", () => {
    expect(runInstant({ createdAt: "2026-08-01T10:00:00Z", startedAt: "2026-08-01T11:00:00Z" })).toBe(
      Date.parse("2026-08-01T11:00:00Z"),
    );
  });

  it("falls back to creation for a run that has not started yet", () => {
    expect(runInstant({ createdAt: "2026-08-01T10:00:00Z" })).toBe(Date.parse("2026-08-01T10:00:00Z"));
  });

  it("answers +Infinity for a stamp it cannot parse, so the cut EXCLUDES it", () => {
    expect(runInstant({ createdAt: "nonsense" })).toBe(Number.POSITIVE_INFINITY);
  });
});

describe("runsAtOrBefore", () => {
  const runs = [
    { id: "after", createdAt: "2026-08-01T13:00:00Z", startedAt: "2026-08-01T13:00:01Z" },
    { id: "at", createdAt: "2026-08-01T11:59:00Z", startedAt: "2026-08-01T12:00:00Z" },
    { id: "before", createdAt: "2026-08-01T09:00:00Z" },
  ];
  const at = new Date("2026-08-01T12:00:00Z");

  it("is the identity function while Live", () => {
    expect(runsAtOrBefore(runs, null)).toBe(runs);
  });

  it("drops a run that started after the viewed instant", () => {
    expect(runsAtOrBefore(runs, at).map((r) => r.id)).toEqual(["at", "before"]);
  });

  it("keeps a run that started exactly ON the instant — at-or-before, not before", () => {
    expect(runsAtOrBefore(runs, at).some((r) => r.id === "at")).toBe(true);
  });

  it("drops an unplaceable run rather than smuggling it into a view of the past", () => {
    expect(runsAtOrBefore([{ id: "x", createdAt: "" }], at)).toEqual([]);
  });
});

/** QA round 4, finding #12: "1 hops". */
describe("plural", () => {
  it("agrees the noun with the count", () => {
    expect(plural(1, "hop")).toBe("1 hop");
    expect(plural(2, "hop")).toBe("2 hops");
    expect(plural(0, "hop")).toBe("0 hops");
  });

  it("takes an explicit plural for an irregular noun", () => {
    expect(plural(1, "entry", "entries")).toBe("1 entry");
    expect(plural(3, "entry", "entries")).toBe("3 entries");
  });
});

/**
 * QA round 4, finding #13. The client half of store.validateAdhocAddress: the
 * four shapes the AGENT can actually dial, and nothing stricter — a policy
 * refusal from the allowlist belongs to the agent, not to this form.
 */
describe("isValidAdhocAddress", () => {
  it.each([
    ["a bare host", "example.test"],
    ["a fully-qualified host", "example.test."],
    ["a host with an underscore, which Go's resolver accepts", "my_svc.example.test"],
    ["an IPv4 literal", "10.0.0.1"],
    ["a bare IPv6 literal", "2001:db8::1"],
    ["a bracketed IPv6 literal", "[::1]"],
    ["host:port", "example.test:8443"],
    ["ip:port", "10.0.0.1:8080"],
    ["bracketed IPv6 with a port", "[::1]:443"],
    ["an http URL", "http://example.test"],
    ["an https URL with a path", "https://example.test/health"],
  ])("accepts %s", (_name, address) => {
    expect(isValidAdhocAddress(address)).toBe(true);
  });

  it.each([
    ["the finding's own garbage", "sdfsdfsdf sdf!!"],
    ["empty", ""],
    ["whitespace only", "   "],
    ["a doubled dot", "example..test"],
    ["a leading hyphen label", "-example.test"],
    ["a non-numeric port", "example.test:http"],
    ["port zero", "example.test:0"],
    ["a port out of range", "example.test:99999"],
    ["a trailing colon", "example.test:"],
    ["a scheme the checker never parses", "ftp://example.test"],
    ["an http URL with no host", "http://"],
  ])("refuses %s", (_name, address) => {
    expect(isValidAdhocAddress(address)).toBe(false);
  });

  it("trims, because every sender trims before resolving", () => {
    expect(isValidAdhocAddress("  10.0.0.1  ")).toBe(true);
  });
});

/**
 * endSentence — QA round 5, finding #10. RFC 7807 details are PHRASES, so a UI
 * that concatenates one with a sentence of its own produced a run-on: "no
 * incident with that id The page is showing the default investigation".
 */
describe("endSentence", () => {
  it("closes a phrase so the next sentence can start", () => {
    expect(endSentence("no incident with that id")).toBe("no incident with that id.");
  });

  it.each([
    ["a full stop", "The incident could not be read."],
    ["a question mark", "Who deleted it?"],
    ["an exclamation", "Gone!"],
    ["a colon, which introduces what follows", "Refused because:"],
    ["a semicolon", "Refused; see below;"],
  ])("leaves a phrase already ending in %s alone", (_name, text) => {
    expect(endSentence(text)).toBe(text);
  });

  it("trims, so a detail with trailing whitespace does not get a floating stop", () => {
    expect(endSentence("  no such rule  ")).toBe("no such rule.");
  });

  it("keeps an empty string empty — a lone full stop is not a sentence", () => {
    expect(endSentence("")).toBe("");
    expect(endSentence("   ")).toBe("");
  });
});

/* CHECKBOX_CLASS — QA round 5, finding #14. The native control keeps its
   behaviour and gains this console's theming; accent-color is the whole
   mechanism, so its absence is the regression worth pinning. */
describe("CHECKBOX_CLASS", () => {
  it("tints the native check with the theme token rather than the OS accent", () => {
    expect(CHECKBOX_CLASS).toContain("accent-primary");
  });

  it("carries the same size and focus ring as the pickers' controls", () => {
    expect(CHECKBOX_CLASS).toContain("size-4");
    expect(CHECKBOX_CLASS).toContain("focus-visible:ring-ring");
  });
});
