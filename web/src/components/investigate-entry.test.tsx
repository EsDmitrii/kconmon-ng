import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { TimeMachineProvider } from "@/lib/timemachine";
import { RelatedIncidents } from "./investigate-entry";

function renderRail(incidents: "pending" | "500", me: "known" | "503" = "known") {
  const fetchMock = vi.fn((url: string) => {
    if (String(url).startsWith("/api/v1/auth/me")) {
      return Promise.resolve(
        new Response(JSON.stringify({ type: "about:blank", title: "unavailable", status: 503, detail: "session store unavailable" }), {
          status: 503,
          headers: { "Content-Type": "application/problem+json" },
        }),
      );
    }
    if (String(url).startsWith("/api/v1/incidents")) {
      if (incidents === "pending") return new Promise<Response>(() => {});
      return Promise.resolve(
        new Response(JSON.stringify({ type: "about:blank", title: "boom", status: 500, detail: "store down" }), {
          status: 500,
          headers: { "Content-Type": "application/problem+json" },
        }),
      );
    }
    return Promise.resolve(new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  if (me === "known") {
    qc.setQueryData(["me"], {
      subject: { kind: "user", id: "ada", displayName: "Ada", groups: [], roles: ["viewer"] },
      permissions: ["incidents:read"],
    });
  }
  qc.setQueryData(["config"], {
    auth: { mode: "local", role: "", loginPath: "" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: true },
  });
  render(
    <QueryClientProvider client={qc}>
      <RelatedIncidents scope={{ kind: "node", a: "node-a", b: "" }} />
    </QueryClientProvider>,
  );
  return { fetchMock, qc };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

/* "No open incident" is a claim about the store; it needs an answer from the store. */
describe("RelatedIncidents before and without an answer", () => {
  it("does not claim there is no open incident while the list is loading", async () => {
    renderRail("pending");
    expect(await screen.findByRole("complementary", { name: "Open incidents" })).toBeInTheDocument();
    expect(screen.queryByText("No open incident names this object.")).not.toBeInTheDocument();
  });

  it("announces the load to assistive tech, not only a shimmer it cannot see", async () => {
    renderRail("pending");
    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    expect(within(rail).getByRole("status")).toHaveTextContent("Loading…");
  });

  it("says the list failed instead of claiming there is none", async () => {
    renderRail("500");
    expect(await screen.findByText(/store down/)).toBeInTheDocument();
    expect(screen.queryByText("No open incident names this object.")).not.toBeInTheDocument();
  });
});

/* AuthGate lets the shell render when "who am I" failed; the rail must not wait for it forever. */
describe("RelatedIncidents when the subject check failed", () => {
  it("says the access check failed instead of a skeleton that never resolves", async () => {
    const { fetchMock, qc } = renderRail("pending", "503");
    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    await waitFor(() => expect(qc.getQueryState(["me"])?.status).toBe("error"));
    expect(await within(rail).findByRole("alert")).toHaveTextContent(/session store unavailable/);
    expect(rail.querySelector(".skeleton")).toBeNull();
    expect(fetchMock.mock.calls.some(([u]) => String(u).startsWith("/api/v1/incidents"))).toBe(false);
  });
});

/* Engaged, the rail answers for the instant on screen: incidents open THEN, not incidents open now. */
describe("RelatedIncidents under the Time Machine", () => {
  const AT = "2026-01-01T02:00:00Z";
  const row = (over: Record<string, unknown>) => ({
    id: "inc-1",
    title: "node-a keeps flapping",
    scope: "node-a",
    fromAt: "2026-01-01T01:00:00Z",
    status: "open",
    notes: "",
    pinned: [],
    createdBy: "user:ada",
    createdAt: "2026-01-01T01:00:00Z",
    ...over,
  });

  function renderAt(incidents: unknown[], config: "ok" | "502" = "ok") {
    window.history.pushState({}, "", `/nodes/node-a?at=${AT}`);
    const urls: string[] = [];
    const fetchMock = vi.fn((url: string) => {
      urls.push(String(url));
      if (String(url).startsWith("/api/v1/incidents")) {
        /* Filtered and paged like ListIncidents: the window test coalesce(to_at, inf) >= from AND
           from_at < to, `incidents` already in created_at DESC order, the cursor an offset. */
        const q = new URLSearchParams(String(url).slice(String(url).indexOf("?")));
        const from = q.get("from") ? Date.parse(q.get("from") as string) : -Infinity;
        const to = q.get("to") ? Date.parse(q.get("to") as string) : Infinity;
        const matched = (incidents as { fromAt: string; toAt?: string }[]).filter(
          (i) => (i.toAt ? Date.parse(i.toAt) : Infinity) >= from && Date.parse(i.fromAt) < to,
        );
        const start = Number(q.get("cursor") ?? "0");
        const end = Math.min(matched.length, start + Number(q.get("limit") ?? "100"));
        return Promise.resolve(
          new Response(
            JSON.stringify({ incidents: matched.slice(start, end), nextCursor: end < matched.length ? String(end) : "" }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          ),
        );
      }
      if (String(url).startsWith("/api/v1/config")) {
        return Promise.resolve(
          new Response(JSON.stringify({ type: "about:blank", title: "Bad Gateway", status: 502, detail: "ingress upstream gone" }), {
            status: 502,
            headers: { "Content-Type": "application/problem+json" },
          }),
        );
      }
      return Promise.resolve(new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }));
    });
    vi.stubGlobal("fetch", fetchMock);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    qc.setQueryData(["me"], {
      subject: { kind: "user", id: "ada", displayName: "Ada", groups: [], roles: ["viewer"] },
      permissions: ["incidents:read"],
    });
    if (config === "ok") {
      qc.setQueryData(["config"], {
        auth: { mode: "local", role: "", loginPath: "" },
        anonymousBanner: false,
        controller: { configured: true },
        prometheus: { configured: true },
        database: { configured: true },
      });
    }
    render(
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <RelatedIncidents scope={{ kind: "node", a: "node-a", b: "" }} />
        </TimeMachineProvider>
      </QueryClientProvider>,
    );
    return { urls };
  }

  afterEach(() => {
    window.history.pushState({}, "", "/");
  });

  it("asks for the incidents open at t, not the ones open now", async () => {
    const { urls } = renderAt([row({ status: "resolved", resolvedAt: "2026-01-01T03:00:00Z" })]);
    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    const rows = await within(rail).findAllByTestId("related-incident");
    expect(rows).toHaveLength(1);
    expect(within(rows[0]).getByRole("link", { name: "node-a keeps flapping" })).toBeInTheDocument();

    /* No from/to: they match the incident's saved window, and "open at t" is its lifecycle. */
    const call = urls.find((u) => u.startsWith("/api/v1/incidents")) ?? "";
    const q = new URLSearchParams(call.slice(call.indexOf("?")));
    expect(q.get("status")).toBeNull();
    expect(q.get("from")).toBeNull();
    expect(q.get("to")).toBeNull();
  });

  it("drops an incident resolved by t even when its window is still open-ended, and one declared after t", async () => {
    renderAt([
      row({ id: "inc-later", title: "declared after t", createdAt: "2026-01-01T02:00:00.500Z" }),
      row({ id: "inc-old", title: "resolved before t", status: "resolved", resolvedAt: "2026-01-01T01:30:00Z" }),
    ]);
    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    expect(await within(rail).findByText("No incident open at that instant names this object.")).toBeInTheDocument();
    expect(within(rail).queryByText("No open incident names this object.")).toBeNull();
    expect(within(rail).queryByTestId("related-incident")).toBeNull();
  });

  it("finds the incident open at t behind a full page of newer ones resolved before t", async () => {
    const resolvedBefore = Array.from({ length: 50 }, (_, i) =>
      row({ id: `inc-r${i}`, title: `resolved ${i}`, status: "resolved", resolvedAt: "2026-01-01T01:30:00Z" }),
    );
    renderAt([...resolvedBefore, row({ id: "inc-a", title: "open at t", status: "resolved", resolvedAt: "2026-01-01T03:00:00Z" })]);
    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    const rows = await within(rail).findAllByTestId("related-incident");
    expect(rows.map((r) => within(r).getByRole("link").textContent)).toEqual(["open at t"]);
  });

  /* Investigate saves the window it looked at, which ends at the save; the incident stays open after it. */
  it("lists an incident saved from Investigate whose window ended before t", async () => {
    renderAt([
      row({
        id: "inc-saved",
        title: "saved from Investigate",
        fromAt: "2026-01-01T00:30:00Z",
        toAt: "2026-01-01T01:00:00Z",
        status: "resolved",
        resolvedAt: "2026-01-01T03:00:00Z",
      }),
    ]);
    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    const rows = await within(rail).findAllByTestId("related-incident");
    expect(rows.map((r) => within(r).getByRole("link").textContent)).toEqual(["saved from Investigate"]);
  });

  it("says so when the scan stopped at its page cap before the list ended", async () => {
    const resolvedBefore = Array.from({ length: 2550 }, (_, i) =>
      row({ id: `inc-r${i}`, title: `resolved ${i}`, status: "resolved", resolvedAt: "2026-01-01T01:30:00Z" }),
    );
    renderAt(resolvedBefore);
    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    expect(await within(rail).findByText(/The scan stopped at its page limit/)).toBeInTheDocument();
  });

  it("says nothing of a cap when the scan reached the end of the list", async () => {
    renderAt([row({ id: "inc-old", title: "resolved before t", status: "resolved", resolvedAt: "2026-01-01T01:30:00Z" })]);
    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    await within(rail).findByText("No incident open at that instant names this object.");
    expect(rail.textContent).not.toMatch(/page limit/);
  });

  it("states the instant it answers for under the title", async () => {
    renderAt([]);
    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    expect(await within(rail).findByText(/^at /)).toBeInTheDocument();
  });
});

/* A failed /config is not "no database": the rail and RecentChanges must not send the operator to a config key. */
describe("RelatedIncidents when /config failed", () => {
  it("says the configuration could not be read instead of asking for database.dsnFile", async () => {
    const fetchMock = vi.fn((url: string) => {
      if (String(url).startsWith("/api/v1/config")) {
        return Promise.resolve(
          new Response(JSON.stringify({ type: "about:blank", title: "Bad Gateway", status: 502, detail: "ingress upstream gone" }), {
            status: 502,
            headers: { "Content-Type": "application/problem+json" },
          }),
        );
      }
      return Promise.resolve(new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }));
    });
    vi.stubGlobal("fetch", fetchMock);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    qc.setQueryData(["me"], {
      subject: { kind: "user", id: "ada", displayName: "Ada", groups: [], roles: ["viewer"] },
      permissions: ["incidents:read"],
    });
    render(
      <QueryClientProvider client={qc}>
        <RelatedIncidents scope={{ kind: "node", a: "node-a", b: "" }} />
      </QueryClientProvider>,
    );
    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    expect(await within(rail).findByRole("alert")).toHaveTextContent(/ingress upstream gone/);
    expect(rail.textContent).not.toMatch(/database\.dsnFile/);
    expect(fetchMock.mock.calls.some(([u]) => String(u).startsWith("/api/v1/incidents"))).toBe(false);
  });
});
