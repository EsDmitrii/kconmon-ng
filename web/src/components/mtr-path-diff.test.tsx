import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { PathDiff, diffPaths, fmtRttDeltaNs, summarisePathChange } from "./mtr-path-diff";
import type { MTRHop, PathSnapshot } from "@/lib/types";

function hop(over: Partial<MTRHop> = {}): MTRHop {
  return { number: 1, ip: "10.0.0.1", rttNs: 1_000_000, lossRatio: 0, ...over };
}

/** A path written as IPs, numbered from 1, with an optional RTT per hop —
 *  the diff cares about the IP order and the RTTs, and nothing else. */
function path(ips: string[], rttsNs: number[] = []): MTRHop[] {
  return ips.map((ip, i) => hop({ number: i + 1, ip, rttNs: rttsNs[i] ?? 1_000_000 }));
}

function snapshot(over: Partial<PathSnapshot> = {}): PathSnapshot {
  return {
    id: "s1",
    sourceNode: "node-a",
    destination: "node-b",
    pathHash: "aaaaaaaaaaaa0000",
    hopCount: 1,
    hops: path(["10.0.0.1"]),
    firstSeen: "2026-08-01T10:00:00Z",
    lastSeen: "2026-08-02T10:00:00Z",
    traceCount: 3,
    ...over,
  };
}

afterEach(cleanup);

describe("diffPaths", () => {
  it("calls an identical path all-same and carries a signed RTT delta on every row", () => {
    const rows = diffPaths(path(["10.0.0.1", "10.0.0.2"], [1_000_000, 5_000_000]), path(["10.0.0.1", "10.0.0.2"], [2_000_000, 4_000_000]));

    expect(rows.map((r) => r.kind)).toEqual(["same", "same"]);
    // b - a: the newer path is the subject, the older one the baseline.
    expect(rows[0].rttDeltaNs).toBe(1_000_000);
    expect(rows[1].rttDeltaNs).toBe(-1_000_000);
    expect(rows[0].aHop?.ip).toBe("10.0.0.1");
    expect(rows[0].bHop?.ip).toBe("10.0.0.1");
  });

  it("reports a hop only the newer path has as added, with no a-side hop", () => {
    const rows = diffPaths(path(["10.0.0.1", "10.0.0.3"]), path(["10.0.0.1", "10.0.0.2", "10.0.0.3"]));

    expect(rows.map((r) => r.kind)).toEqual(["same", "added", "same"]);
    expect(rows[1].bHop?.ip).toBe("10.0.0.2");
    expect(rows[1].aHop).toBeUndefined();
    // Nothing to compare against, so no delta rather than a delta of zero.
    expect(rows[1].rttDeltaNs).toBeUndefined();
  });

  it("reports a hop only the older path had as removed, with no b-side hop", () => {
    const rows = diffPaths(path(["10.0.0.1", "10.0.0.2", "10.0.0.3"]), path(["10.0.0.1", "10.0.0.3"]));

    expect(rows.map((r) => r.kind)).toEqual(["same", "removed", "same"]);
    expect(rows[1].aHop?.ip).toBe("10.0.0.2");
    expect(rows[1].bHop).toBeUndefined();
  });

  it("pairs a one-for-one substitution into a single changed row carrying both hops", () => {
    const rows = diffPaths(path(["10.0.0.1", "10.0.0.2", "10.0.0.9"]), path(["10.0.0.1", "10.0.0.7", "10.0.0.9"]));

    expect(rows.map((r) => r.kind)).toEqual(["same", "changed", "same"]);
    expect(rows[1].aHop?.ip).toBe("10.0.0.2");
    expect(rows[1].bHop?.ip).toBe("10.0.0.7");
    // Two DIFFERENT machines: a millisecond difference between them is not a
    // latency change, so no delta is offered.
    expect(rows[1].rttDeltaNs).toBeUndefined();
  });

  it("zips a longer substitution run and leaves the surplus as its own row", () => {
    const rows = diffPaths(path(["10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.9"]), path(["10.0.0.1", "10.0.0.7", "10.0.0.9"]));

    expect(rows.map((r) => r.kind)).toEqual(["same", "changed", "removed", "same"]);
    expect(rows[1].bHop?.ip).toBe("10.0.0.7");
    expect(rows[2].aHop?.ip).toBe("10.0.0.3");
  });

  it("reports a reorder as one removal and one addition, which is what an IP-order alignment can honestly see", () => {
    const rows = diffPaths(path(["10.0.0.1", "10.0.0.2", "10.0.0.3"]), path(["10.0.0.1", "10.0.0.3", "10.0.0.2"]));

    expect(rows.map((r) => r.kind)).toEqual(["same", "removed", "same", "added"]);
    expect(rows[1].aHop?.ip).toBe("10.0.0.2");
    expect(rows[3].bHop?.ip).toBe("10.0.0.2");
  });

  it("never anchors two unanswered hops to each other — '*' is 'unknown', not 'the same machine'", () => {
    const rows = diffPaths(path(["10.0.0.1", "*", "10.0.0.9"]), path(["10.0.0.1", "*", "10.0.0.9"]));

    expect(rows.map((r) => r.kind)).toEqual(["same", "changed", "same"]);
    expect(rows[1].aHop?.ip).toBe("*");
    expect(rows[1].bHop?.ip).toBe("*");
  });

  it("degenerates honestly: an empty older path is all additions, an empty newer path all removals", () => {
    expect(diffPaths([], path(["10.0.0.1"])).map((r) => r.kind)).toEqual(["added"]);
    expect(diffPaths(path(["10.0.0.1"]), []).map((r) => r.kind)).toEqual(["removed"]);
    expect(diffPaths([], [])).toEqual([]);
  });

  it("shares nothing when the two paths have no hop in common", () => {
    const rows = diffPaths(path(["10.0.0.1"]), path(["10.9.9.9"]));

    expect(rows.map((r) => r.kind)).toEqual(["changed"]);
  });

  /* Hostile QA: a hops field that is not a list reached the alignment and threw
     out of the whole page. The server never sends one — a proxy or a replay
     might. */
  it("treats a hops field that is not a list as no hops", () => {
    expect(diffPaths(null as unknown as MTRHop[], path(["10.0.0.1"])).map((r) => r.kind)).toEqual(["added"]);
    expect(diffPaths(null as unknown as MTRHop[], undefined as unknown as MTRHop[])).toEqual([]);
  });

  /* A delta is only a delta between two REAL readings; subtracting an absent
     RTT produced NaN and the column had to print an em dash for it anyway. */
  it("carries no delta on a shared hop when one side has no reading", () => {
    const withRtt = path(["10.0.0.1"], [2_000_000]);
    const without = [{ number: 1, ip: "10.0.0.1", rttNs: null as unknown as number, lossRatio: 0 }];
    const [row] = diffPaths(without, withRtt);
    expect(row.kind).toBe("same");
    expect(row.rttDeltaNs).toBeUndefined();
  });
});

describe("fmtRttDeltaNs", () => {
  it("signs the number so the direction reads without a legend", () => {
    expect(fmtRttDeltaNs(1_500_000)).toBe("+1.5ms");
    expect(fmtRttDeltaNs(-800_000)).toBe("-0.8ms");
    expect(fmtRttDeltaNs(0)).toBe("0.0ms");
    expect(fmtRttDeltaNs(undefined)).toBe("—");
  });

  /* "+0.0ms" was already refused for claiming a direction the number does not
     have; "-0.0ms" was making the same claim from the other side, for a delta of
     a few microseconds that rounds to nothing. */
  it("keeps a delta that rounds to nothing unsigned, whichever way it leans", () => {
    expect(fmtRttDeltaNs(-4_000)).toBe("0.0ms");
    expect(fmtRttDeltaNs(4_000)).toBe("0.0ms");
  });

  it("says nothing for a reading that is not a finite number", () => {
    expect(fmtRttDeltaNs(Number.NaN)).toBe("—");
    expect(fmtRttDeltaNs(Number.POSITIVE_INFINITY)).toBe("—");
    expect(fmtRttDeltaNs(null as unknown as number)).toBe("—");
  });
});

describe("PathDiff", () => {
  const a = snapshot({
    id: "old",
    pathHash: "aaaaaaaaaaaa0000",
    firstSeen: "2026-08-01T10:00:00Z",
    hops: path(["10.0.0.1", "10.0.0.2", "10.0.0.5"], [1_000_000, 5_000_000, 9_000_000]),
  });
  const b = snapshot({
    id: "new",
    pathHash: "bbbbbbbbbbbb1111",
    firstSeen: "2026-08-06T10:00:00Z",
    hops: path(["10.0.0.1", "10.0.0.3", "10.0.0.4", "10.0.0.5"], [3_000_000, 6_000_000, 7_000_000, 9_000_000]),
  });

  it("names both snapshots in the header so the reader knows which column is which", () => {
    render(<PathDiff a={a} b={b} />);

    const table = screen.getByRole("table", { name: /path diff/i });
    expect(within(table).getByText(/aaaaaaaaaaaa/)).toBeInTheDocument();
    expect(within(table).getByText(/bbbbbbbbbbbb/)).toBeInTheDocument();
  });

  it("marks every row with its kind and shows both sides aligned", () => {
    render(<PathDiff a={a} b={b} />);

    expect(screen.getAllByLabelText("same")).toHaveLength(2);
    expect(screen.getAllByLabelText("changed")).toHaveLength(1);
    expect(screen.getAllByLabelText("added")).toHaveLength(1);
  });

  it("shows a signed RTT delta on the rows where both sides are the same machine", () => {
    render(<PathDiff a={a} b={b} />);

    // hop 1: 1.0ms -> 3.0ms
    expect(screen.getByText("+2.0ms")).toBeInTheDocument();
    // hop 5: 9.0ms -> 9.0ms
    expect(screen.getByText("0.0ms")).toBeInTheDocument();
  });

  it("says so plainly when the two paths are identical rather than showing a wall of '='", () => {
    render(<PathDiff a={a} b={snapshot({ id: "twin", pathHash: "cccccccccccc2222", hops: a.hops })} />);

    expect(screen.getByText(/same hops in the same order/i)).toBeInTheDocument();
  });
});

/* ── "path changed" said in words, not in two hashes ─────────────────────── */

/**
 * The owner on the Explorer: «ничего не понятно». A row badged "path changed"
 * next to two twelve-character hashes tells a reader that SOMETHING moved and
 * nothing about what. The route is the whole point of an MTR, so the change is
 * described as a route change: which hop, and from what to what.
 */
describe("summarisePathChange", () => {
  const chain = (...ips: string[]) =>
    ips.map((ip, i) => ({ number: i + 1, ip, hostname: "", rttNs: 1_000_000, lossRatio: 0 }));

  it("names the ONE hop that moved, and both of its addresses", () => {
    const s = summarisePathChange(chain("10.0.0.1", "10.244.9.17", "10.0.0.9"), chain("10.0.0.1", "10.244.9.21", "10.0.0.9"));
    expect(s).toEqual({ kind: "changed", hop: 2, from: "10.244.9.17", to: "10.244.9.21", total: 1 });
  });

  it("names an inserted hop and where it appeared", () => {
    const s = summarisePathChange(chain("10.0.0.1", "10.0.0.9"), chain("10.0.0.1", "10.0.0.5", "10.0.0.9"));
    expect(s).toEqual({ kind: "added", hop: 2, to: "10.0.0.5", total: 1 });
  });

  it("names a hop that dropped out, and where it used to be", () => {
    const s = summarisePathChange(chain("10.0.0.1", "10.0.0.5", "10.0.0.9"), chain("10.0.0.1", "10.0.0.9"));
    expect(s).toEqual({ kind: "removed", hop: 2, from: "10.0.0.5", total: 1 });
  });

  it("counts rather than lists once several hops moved at once", () => {
    const s = summarisePathChange(chain("a", "b", "c", "d"), chain("a", "x", "y", "d"));
    expect(s.kind).toBe("several");
    expect(s.total).toBe(2);
    // No hop or address: naming one of several would misdescribe the change.
    expect(s.hop).toBeUndefined();
  });

  it("says nothing changed when nothing did — the RTTs are not the route", () => {
    const a = chain("10.0.0.1", "10.0.0.9");
    const b = chain("10.0.0.1", "10.0.0.9").map((h) => ({ ...h, rttNs: 9_000_000 }));
    expect(summarisePathChange(a, b)).toEqual({ kind: "same", total: 0 });
  });

  it("ignores a non-responding hop, which is an absence and not an address", () => {
    // A `*` hop that stays a `*` has not moved anywhere.
    const s = summarisePathChange(chain("10.0.0.1", "*", "10.0.0.9"), chain("10.0.0.1", "*", "10.0.0.9"));
    expect(s.kind).toBe("same");
  });

  it("survives an empty path on either side rather than throwing", () => {
    expect(summarisePathChange([], chain("a")).kind).toBe("added");
    expect(summarisePathChange(chain("a"), []).kind).toBe("removed");
    expect(summarisePathChange([], []).kind).toBe("same");
  });

  /* Hostile QA. A hops field that is not a list at all is the shape a proxy or
     a replay can produce, and this function is called from the history list's
     badge on every row — it must not be the thing that empties the pane. */
  it("survives a hops field that is not a list", () => {
    expect(summarisePathChange(null as unknown as MTRHop[], chain("a")).kind).toBe("added");
    expect(summarisePathChange(null as unknown as MTRHop[], null as unknown as MTRHop[]).kind).toBe("same");
  });

  it("says nothing moved when a whole path of silence stays silent", () => {
    const stars = chain("*", "*", "*");
    expect(summarisePathChange(stars, stars)).toEqual({ kind: "same", total: 0 });
  });

  it("carries a unicode address through the sentence untouched", () => {
    const s = summarisePathChange(chain("узел-一"), chain("маршрутизатор-二"));
    expect(s).toMatchObject({ kind: "changed", from: "узел-一", to: "маршрутизатор-二" });
  });

  it("reads a one-hop path that grew a second hop as an addition", () => {
    expect(summarisePathChange(chain("10.0.0.1"), chain("10.0.0.1", "10.0.0.2"))).toMatchObject({
      kind: "added",
      to: "10.0.0.2",
      total: 1,
    });
  });
});
