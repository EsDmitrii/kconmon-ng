import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, getMatrix, getTopology, getVersion, promqlQuery } from "./api";

function mockFetch(status: number, body: unknown, contentType = "application/json") {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      new Response(JSON.stringify(body), { status, headers: { "Content-Type": contentType } }),
    ),
  );
}

afterEach(() => vi.unstubAllGlobals());

describe("api client", () => {
  it("parses topology", async () => {
    mockFetch(200, { nodes: [{ name: "n1", zone: "z", ready: true }], agents: [], timestamp: "t" });
    const topo = await getTopology();
    expect(topo.nodes[0].name).toBe("n1");
  });

  it("sends protocol and plane as query params", async () => {
    mockFetch(200, { protocol: "udp", plane: "pod", nodes: [], cells: [], timestamp: "t" });
    await getMatrix("udp");
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe("/api/v1/matrix?protocol=udp&plane=pod");
  });

  it("throws ApiError on problem+json", async () => {
    mockFetch(503, { type: "about:blank", title: "prometheus not configured", status: 503 }, "application/problem+json");
    await expect(getMatrix("tcp")).rejects.toBeInstanceOf(ApiError);
  });

  it("returns Prometheus error envelopes instead of throwing", async () => {
    mockFetch(400, { status: "error", errorType: "bad_data", error: "parse error" });
    const res = await promqlQuery("up{");
    expect(res.status).toBe("error");
  });

  it("parses the version payload and its capability list", async () => {
    mockFetch(200, { version: "1.6.0", commit: "abc123", capabilities: ["events"] });
    const version = await getVersion();
    expect(version.capabilities).toEqual(["events"]);
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe("/api/v1/version");
  });

  it("tolerates a version payload with no capabilities field", async () => {
    mockFetch(200, { version: "1.5.0", commit: "abc123" });
    const version = await getVersion();
    expect(version.capabilities).toBeUndefined();
  });
});
