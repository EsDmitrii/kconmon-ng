import { describe, expect, it } from "vitest";
import {
  CHECKBOX_CLASS,
  endSentence,
  escapeLabelValue,
  fmtEventStamp,
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

  it("is 24-hour whatever locale tag it is handed", () => {
    const noon = new Date();
    noon.setHours(15, 12, 0, 0);
    expect(fmtEventTime(noon.toISOString(), "en-US")).toBe("15:12:00");
    expect(fmtEventTime(noon.toISOString(), "ru-RU")).toBe("15:12:00");
  });
});

/* QA scope 2, finding #10 — a change feed whose rows are time-only reads
   yesterday's 15:12 as this afternoon's. */
describe("fmtEventStamp", () => {
  const NOW = new Date("2026-08-09T12:00:00");

  it("is time-only for a row from the same day", () => {
    expect(fmtEventStamp("2026-08-09T15:12:00", undefined, NOW)).toBe("15:12:00");
  });

  it("carries the day for a row from any other one", () => {
    const stamp = fmtEventStamp("2026-08-08T15:12:00", undefined, NOW);
    expect(stamp).toContain("15:12:00");
    expect(stamp).not.toBe("15:12:00");
    expect(stamp).toBe(`${new Date("2026-08-08T15:12:00").toLocaleDateString(undefined, { month: "short", day: "numeric" })} 15:12:00`);
  });

  it("compares the DAY, not a 24-hour distance", () => {
    // 23:59 the previous night is under an hour ago and still another day.
    expect(fmtEventStamp("2026-08-08T23:59:00", undefined, new Date("2026-08-09T00:30:00"))).not.toBe("23:59:00");
  });

  it("hands back the wire's own bytes for an unparseable stamp", () => {
    expect(fmtEventStamp("not a time", undefined, NOW)).toBe("not a time");
  });
});

/** runsAtOrBefore — the Time Machine's cut across a run list. */
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
 * The client half of store.validateAdhocAddress: the four shapes the AGENT can actually dial, and
 * nothing stricter.
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

describe("CHECKBOX_CLASS", () => {
  it("tints the native check with the theme token rather than the OS accent", () => {
    expect(CHECKBOX_CLASS).toContain("accent-primary");
  });

  it("carries the same size and focus ring as the pickers' controls", () => {
    expect(CHECKBOX_CLASS).toContain("size-4");
    expect(CHECKBOX_CLASS).toContain("focus-visible:ring-ring");
  });
});
