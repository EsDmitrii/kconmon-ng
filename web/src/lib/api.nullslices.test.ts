import { afterEach, describe, expect, it, vi } from "vitest";
import { getMatrix, getTopology } from "@/lib/api";

/* QA wave finding #1: Go marshals nil slices as JSON null, and a controller
 * running outside a cluster answers {"nodes":null,"agents":[...]} — which
 * killed the landing page and /topology with a TypeError, because types.ts
 * promises non-nullable arrays and every caller rightly trusts that.
 *
 * The transport is the single place that promise is enforced. These pins hold
 * getTopology/getMatrix to it; any new list-shaped endpoint gets the same
 * treatment (normalize at the fetcher, never guard at call sites). */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("nil-slice normalization at the transport", () => {
  it("getTopology folds null nodes/agents to empty arrays", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(json({ nodes: null, agents: null, historical: false }))));
    const topo = await getTopology();
    expect(topo.nodes).toEqual([]);
    expect(topo.agents).toEqual([]);
  });

  it("getTopology leaves real arrays alone", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          json({ nodes: [{ name: "a", zone: "z", ready: true }], agents: [], historical: false }),
        ),
      ),
    );
    const topo = await getTopology();
    expect(topo.nodes).toHaveLength(1);
    expect(topo.nodes[0].name).toBe("a");
  });

  it("getMatrix folds null nodes/cells to empty arrays", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.resolve(json({ protocol: "tcp", plane: "pod", nodes: null, cells: null, timestamp: "t" }))),
    );
    const m = await getMatrix("tcp");
    expect(m.nodes).toEqual([]);
    expect(m.cells).toEqual([]);
  });
});

describe("importConfig nil-slice normalization + the retry predicate", () => {
  it("folds null errors/warnings on every collection to empty arrays", async () => {
    const result = {
      dryRun: true,
      targets: { created: 1, updated: 0, skipped: 0, errors: null, warnings: null },
      webhooks: { created: 0, updated: 0, skipped: 1, errors: null, warnings: [{ name: "w", reason: "r" }] },
    };
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(json(result))));
    const { importConfig } = await import("@/lib/api");
    const res = (await importConfig({ version: 1 } as never, true)) as never as Record<
      string,
      { errors: unknown[]; warnings: unknown[] }
    >;
    expect(res.targets.errors).toEqual([]);
    expect(res.targets.warnings).toEqual([]);
    expect(res.webhooks.errors).toEqual([]);
    expect(res.webhooks.warnings).toHaveLength(1);
  });

  it("retryUnlessClientError: 4xx problems never retry, 5xx/429/network retry once", async () => {
    const { ApiError, retryUnlessClientError } = await import("@/lib/api");
    const problem = (status: number) => new ApiError({ type: "about:blank", title: "t", status });
    expect(retryUnlessClientError(0, problem(409))).toBe(false);
    expect(retryUnlessClientError(0, problem(422))).toBe(false);
    expect(retryUnlessClientError(0, problem(404))).toBe(false);
    expect(retryUnlessClientError(0, problem(429))).toBe(true);
    expect(retryUnlessClientError(0, problem(503))).toBe(true);
    expect(retryUnlessClientError(0, new TypeError("network"))).toBe(true);
    expect(retryUnlessClientError(1, problem(503))).toBe(false);
  });
});
