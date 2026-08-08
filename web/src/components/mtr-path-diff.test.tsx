import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { PathDiff, diffPaths, fmtRttDeltaNs } from "./mtr-path-diff";
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
});

describe("fmtRttDeltaNs", () => {
  it("signs the number so the direction reads without a legend", () => {
    expect(fmtRttDeltaNs(1_500_000)).toBe("+1.5ms");
    expect(fmtRttDeltaNs(-800_000)).toBe("-0.8ms");
    expect(fmtRttDeltaNs(0)).toBe("0.0ms");
    expect(fmtRttDeltaNs(undefined)).toBe("—");
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
