import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { Annotation, LiveEvent } from "@/lib/types";
import { LivePage, mergeFeedRows, LIVE_ANNOTATION_RANGE_SECONDS } from "./live";

/** Global annotations inline in the scrollback. */

const offsetHeightDescriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetHeight");
const offsetWidthDescriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetWidth");

beforeAll(() => {
  Object.defineProperty(HTMLElement.prototype, "offsetHeight", { configurable: true, get: () => 600 });
  Object.defineProperty(HTMLElement.prototype, "offsetWidth", { configurable: true, get: () => 900 });
});

afterAll(() => {
  if (offsetHeightDescriptor) Object.defineProperty(HTMLElement.prototype, "offsetHeight", offsetHeightDescriptor);
  if (offsetWidthDescriptor) Object.defineProperty(HTMLElement.prototype, "offsetWidth", offsetWidthDescriptor);
});

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function ev(seq: number, over: Partial<LiveEvent> = {}): LiveEvent {
  return {
    id: `${seq}-1785276000000000000`,
    seq,
    type: "check_observed",
    severity: "info",
    scope: "node-a→node-b",
    timestamp: "2026-08-01T11:59:00Z",
    summary: `event ${seq}`,
    details: {},
    ...over,
  };
}

function ann(over: Partial<Annotation> = {}): Annotation {
  return {
    id: "a-1",
    startAt: "2026-08-01T11:58:00Z",
    scope: "",
    text: "rolled the gateway",
    createdBy: "user:ada",
    createdAt: "2026-08-01T11:58:01Z",
    ...over,
  };
}

describe("mergeFeedRows", () => {
  it("files an annotation at its own timestamp among the events", () => {
    const rows = mergeFeedRows(
      [ev(3, { timestamp: "2026-08-01T12:00:00Z" }), ev(1, { timestamp: "2026-08-01T11:00:00Z" })],
      [ann({ startAt: "2026-08-01T11:30:00Z" })],
    );
    expect(rows.map((r) => r.kind)).toEqual(["event", "annotation", "event"]);
  });

  it("namespaces the annotation key so it can never collide with an event id", () => {
    const rows = mergeFeedRows([], [ann({ id: "abc" })]);
    expect(rows[0].key).toBe("annotation:abc");
  });

  it("keeps the events' own (timestamp, seq) order — the sort is stable", () => {
    const rows = mergeFeedRows([ev(9), ev(8), ev(7)], []);
    expect(rows.map((r) => r.key)).toEqual([ev(9).id, ev(8).id, ev(7).id]);
  });

  it("sorts an unparseable startAt to the bottom rather than poisoning the order", () => {
    const rows = mergeFeedRows([ev(1)], [ann({ id: "junk", startAt: "whenever" })]);
    expect(rows[rows.length - 1].key).toBe("annotation:junk");
  });

  it("is the identity on the event list when there are no annotations", () => {
    const events = [ev(2), ev(1)];
    expect(mergeFeedRows(events, []).map((r) => r.key)).toEqual(events.map((e) => e.id));
  });

  it("returns annotations alone when there are no events", () => {
    expect(mergeFeedRows([], [ann()]).map((r) => r.kind)).toEqual(["annotation"]);
  });
});

interface RenderOpts {
  events?: LiveEvent[];
  annotations?: Annotation[];
}

function renderPage(opts: RenderOpts = {}) {
  const { events = [], annotations = [] } = opts;
  const annotationQueries: URLSearchParams[] = [];
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    if (href.startsWith("/api/v1/annotations")) {
      annotationQueries.push(new URLSearchParams(href.split("?")[1] ?? ""));
      return Promise.resolve(json({ annotations, nextCursor: "" }));
    }
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events, nextCursor: "" }));
    if (href.includes("/api/v1/auth/me")) {
      return Promise.resolve(json({ subject: { kind: "user", id: "u1" }, permissions: ["annotations:read"] }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["version"], { version: "1.7.0", commit: "abc", capabilities: ["events"] });
  qc.setQueryData(["config"], {
    auth: { mode: "anonymous", role: "admin", loginPath: "" },
    anonymousBanner: true,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: true },
  });
  const utils = render(
    <QueryClientProvider client={qc}>
      <TimeMachineProvider>
        <LivePage />
      </TimeMachineProvider>
    </QueryClientProvider>,
  );
  return { ...utils, annotationQueries };
}

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
  window.history.pushState({}, "", "/live");
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

describe("LivePage annotations", () => {
  it("asks for the GLOBAL scope only — a node's private note is not fleet news", async () => {
    const { annotationQueries } = renderPage();
    await waitFor(() => expect(annotationQueries.length).toBeGreaterThan(0));
    expect(annotationQueries.every((qs) => qs.has("scope") && qs.get("scope") === "")).toBe(true);
  });

  it("bounds the fetch to a day", async () => {
    const { annotationQueries } = renderPage();
    await waitFor(() => expect(annotationQueries.length).toBeGreaterThan(0));
    const qs = annotationQueries[0];
    const span = (Date.parse(qs.get("to") ?? "") - Date.parse(qs.get("from") ?? "")) / 1000;
    expect(span).toBe(LIVE_ANNOTATION_RANGE_SECONDS);
  });

  it("renders the note inline, in its own row style", async () => {
    renderPage({ events: [ev(1)], annotations: [ann({ text: "rolled the gateway" })] });
    await screen.findByText("rolled the gateway");
    const row = (await screen.findAllByTestId("annotation-feed-row"))[0];
    expect(row.className).toContain("border-l-primary");
    expect(row.textContent).toContain("Note");
    expect(row.textContent).toContain("user:ada");
  });

  it("says span rather than moment for a ranged mark", async () => {
    renderPage({ annotations: [ann({ endAt: "2026-08-01T12:05:00Z" })] });
    const row = (await screen.findAllByTestId("annotation-feed-row"))[0];
    expect(row.textContent).toContain("Annotation (span)");
  });

  it("does not count as an event in the counts line", async () => {
    renderPage({ events: [ev(1)], annotations: [ann()] });
    await screen.findByText("rolled the gateway");
    expect(screen.getByText(/Showing 1 of 1 events/)).toBeTruthy();
  });

  it("keeps the feed out of its blank slate when only a note is present", async () => {
    renderPage({ events: [], annotations: [ann()] });
    await screen.findByText("rolled the gateway");
    expect(screen.queryByText("Waiting for events")).toBeNull();
  });

  it("anchors the annotation window at t while engaged", async () => {
    window.history.pushState({}, "", "/live?at=2026-08-01T09:00:00Z");
    const { annotationQueries } = renderPage();
    await waitFor(() => expect(annotationQueries.length).toBeGreaterThan(0));
    expect(annotationQueries[0].get("to")).toBe("2026-08-01T09:00:00.000Z");
  });

  it("offers no create affordance here — the cards and Explore own that", async () => {
    renderPage({ annotations: [ann()] });
    await screen.findByText("rolled the gateway");
    expect(screen.queryByRole("button", { name: /annotate/i })).toBeNull();
  });
});
