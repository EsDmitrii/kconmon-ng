import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { MaintenanceBar, useMaintenance } from "@/components/maintenance";
import { stampShort } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { MaintenanceWindow } from "@/lib/types";

/** The shared maintenance hook + bar — the annotations twin, and tested the same way for the same reasons. */

const NOW = "2026-08-01T12:00:00Z";

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const problem = (status: number, title: string, detail?: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

function win(over: Partial<MaintenanceWindow> = {}): MaintenanceWindow {
  return {
    id: "m-1",
    scope: "",
    startAt: "2026-08-01T11:30:00Z",
    endAt: "2026-08-01T11:45:00Z",
    reason: "switch upgrade",
    createdBy: "user:ada",
    createdAt: "2026-08-01T10:00:00Z",
    ...over,
  };
}

interface StubOpts {
  permissions?: string[];
  byScope?: Record<string, MaintenanceWindow[]>;
  onCreate?: (body: unknown) => Response;
  onDelete?: (id: string) => Response;
}

function stubFetch(opts: StubOpts = {}) {
  const { permissions = ["maintenance:read", "maintenance:write"], byScope = {}, onCreate, onDelete } = opts;
  const listCalls: URLSearchParams[] = [];
  const createBodies: unknown[] = [];
  const deleteIds: string[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (href.includes("/api/v1/auth/me")) {
      return Promise.resolve(
        json({ subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: [] }, permissions }),
      );
    }
    if (href.startsWith("/api/v1/maintenance/") && method === "DELETE") {
      const id = decodeURIComponent(href.slice("/api/v1/maintenance/".length));
      deleteIds.push(id);
      return Promise.resolve(onDelete ? onDelete(id) : new Response(null, { status: 204 }));
    }
    if (href.startsWith("/api/v1/maintenance") && method === "POST") {
      const body: unknown = JSON.parse(String(init?.body ?? "{}"));
      createBodies.push(body);
      return Promise.resolve(onCreate ? onCreate(body) : json(win({ id: "new" }), { status: 201 }));
    }
    if (href.startsWith("/api/v1/maintenance")) {
      const qs = new URLSearchParams(href.split("?")[1] ?? "");
      listCalls.push(qs);
      // " " stands for "the parameter was absent"; "" is the present-but-empty
      // (global-only) listing, which is a different REQUEST.
      const key = qs.has("scope") ? (qs.get("scope") as string) : " ";
      return Promise.resolve(json({ windows: byScope[key] ?? [], nextCursor: "" }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { fetchMock, listCalls, createBodies, deleteIds };
}

/** The hook rendered through the bar — the pairing every real surface uses. */
function Harness({ scope, rangeSeconds = 3600 }: { scope: string; rangeSeconds?: number }) {
  const { windows, error, refresh } = useMaintenance(scope, rangeSeconds);
  return <MaintenanceBar scope={scope} windows={windows} error={error} onChanged={() => void refresh()} />;
}

function renderHarness(scope: string, rangeSeconds = 3600) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return {
    qc,
    ...render(
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <Harness scope={scope} rangeSeconds={rangeSeconds} />
        </TimeMachineProvider>
      </QueryClientProvider>,
    ),
  };
}

function scopesAsked(listCalls: URLSearchParams[]): string[] {
  return [...new Set(listCalls.map((qs) => (qs.has("scope") ? `=${qs.get("scope")}` : "absent")))].sort();
}

/** Opens the create popover and returns nothing — the fields are queried by
 *  label from the document, exactly as an operator finds them. */
async function openForm(scope = "") {
  const view = renderHarness(scope);
  fireEvent.click(await screen.findByRole("button", { name: /maintenance/i }));
  /* role="form", not role="dialog" (QA round 3, finding #15) — the twin of the
     annotation form's own change, and for the same reason: this is a
     disclosure, not a modal. */
  await screen.findByRole("form", { name: "New maintenance window" });
  return view;
}

/** Drives one of the two DateTimePickers: open it, type the local day and wall-clock into its manual fields. */
function pickInstant(triggerName: "Start" | "End", date: string, time: string) {
  fireEvent.click(screen.getByRole("button", { name: triggerName }));
  fireEvent.change(screen.getByLabelText("Date"), { target: { value: date } });
  fireEvent.change(screen.getByLabelText("Time"), { target: { value: time } });
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(new Date(NOW));
  window.history.pushState({}, "", "/explore");
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

describe("useMaintenance scope semantics", () => {
  it("asks ONLY for the global listing on a global surface, with scope present-but-empty", async () => {
    const { listCalls } = stubFetch({ byScope: { "": [win()] } });
    renderHarness("");
    await screen.findByText(/switch upgrade/);
    expect(scopesAsked(listCalls)).toEqual(["="]);
  });

  it("asks for BOTH the surface scope and the global one on a scoped surface", async () => {
    const { listCalls } = stubFetch({
      byScope: {
        "": [win({ id: "g", reason: "fleet-wide upgrade" })],
        "node-a": [win({ id: "n", scope: "node-a", reason: "kernel patch" })],
      },
    });
    renderHarness("node-a");
    await screen.findByText(/kernel patch/);
    await screen.findByText(/fleet-wide upgrade/);
    expect(scopesAsked(listCalls)).toEqual(["=", "=node-a"]);
  });

  it("never asks with the scope parameter ABSENT — that would be every window in the fleet", async () => {
    const { listCalls } = stubFetch({ byScope: { " ": [win({ id: "other", scope: "node-z", reason: "somebody else" })] } });
    renderHarness("node-a");
    await waitFor(() => expect(listCalls.length).toBeGreaterThanOrEqual(2));
    expect(listCalls.every((qs) => qs.has("scope"))).toBe(true);
    expect(screen.queryByText(/somebody else/)).toBeNull();
  });

  it("bounds the fetch to the visible window: from = to - rangeSeconds", async () => {
    const { listCalls } = stubFetch();
    renderHarness("", 900);
    await waitFor(() => expect(listCalls.length).toBeGreaterThan(0));
    const to = Date.parse(listCalls[0].get("to") ?? "");
    const from = Date.parse(listCalls[0].get("from") ?? "");
    // The LENGTH is what rangeSeconds controls; the end is "now", which the
    // fake clock is deliberately allowed to advance past the set instant.
    expect(to - from).toBe(900_000);
    expect(to).toBeGreaterThanOrEqual(Date.parse(NOW));
    expect(to).toBeLessThan(Date.parse(NOW) + 5_000);
  });

  it("anchors the window at t while the Time Machine is engaged", async () => {
    window.history.pushState({}, "", "/explore?at=2026-08-01T09:00:00Z");
    const { listCalls } = stubFetch();
    renderHarness("", 3600);
    await waitFor(() => expect(listCalls.length).toBeGreaterThan(0));
    expect(listCalls[0].get("to")).toBe("2026-08-01T09:00:00.000Z");
    expect(listCalls[0].get("from")).toBe("2026-08-01T08:00:00.000Z");
  });

  it("de-duplicates a window that came back from both legs", async () => {
    const shared = win({ id: "same", reason: "one window" });
    stubFetch({ byScope: { "": [shared], "node-a": [shared] } });
    renderHarness("node-a");
    await waitFor(() => expect(screen.getAllByText("one window")).toHaveLength(1));
  });

  it("makes ZERO requests without maintenance:read — the M6 per-source gate", async () => {
    const { fetchMock, listCalls } = stubFetch({ permissions: ["annotations:read"] });
    renderHarness("node-a");
    // "Zero" has to mean zero rather than "not yet", so wait until the answer
    // that DECIDES the gate has landed and then let the queue drain twice.
    await waitFor(() => expect(fetchMock.mock.calls.some((c) => String(c[0]).includes("/auth/me"))).toBe(true));
    await new Promise((r) => setTimeout(r, 0));
    await new Promise((r) => setTimeout(r, 0));
    expect(listCalls).toEqual([]);
    expect(screen.queryByTestId("maintenance-bar")).toBeNull();
  });

  it("says so, once, when the listing fails", async () => {
    const fetchMock = vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/auth/me")) {
        return Promise.resolve(json({ subject: {}, permissions: ["maintenance:read"] }));
      }
      return Promise.resolve(problem(503, "database unavailable"));
    });
    vi.stubGlobal("fetch", fetchMock);
    renderHarness("");
    await screen.findByText("Maintenance windows are unavailable.");
  });
});

describe("MaintenanceBar affordances", () => {
  it("HIDES the create button without maintenance:write", async () => {
    stubFetch({ permissions: ["maintenance:read"] });
    renderHarness("");
    await screen.findByText(/0 maintenance windows in this window/);
    expect(screen.queryByRole("button", { name: /maintenance/i })).toBeNull();
  });

  it("HIDES the delete button without maintenance:write", async () => {
    stubFetch({ permissions: ["maintenance:read"], byScope: { "": [win()] } });
    renderHarness("");
    await screen.findByText(/switch upgrade/);
    expect(screen.queryByRole("button", { name: /delete maintenance window/i })).toBeNull();
  });

  it("shows the create button, enabled, with maintenance:write while Live", async () => {
    stubFetch();
    renderHarness("");
    expect(await screen.findByRole("button", { name: /maintenance/i })).not.toBeDisabled();
  });

  it("keeps create VISIBLE but DISABLED while the Time Machine is engaged", async () => {
    window.history.pushState({}, "", "/explore?at=2026-08-01T09:00:00Z");
    stubFetch();
    renderHarness("");
    expect(await screen.findByRole("button", { name: /maintenance/i })).toBeDisabled();
  });

  it("keeps delete VISIBLE but DISABLED while engaged", async () => {
    window.history.pushState({}, "", "/explore?at=2026-08-01T09:00:00Z");
    stubFetch({ byScope: { "": [win()] } });
    renderHarness("");
    expect(await screen.findByRole("button", { name: /delete maintenance window/i })).toBeDisabled();
  });

  it("names the surface's scope so an operator knows where a window will land", async () => {
    stubFetch();
    renderHarness("node-a→node-b");
    await screen.findByText(/scope node-a→node-b/);
  });

  /* stampShort is the shared helper every compact column draws through now, and
     it takes the INTERFACE locale rather than a bare undefined (QA scope 3,
     findings #7 and #18). "en" here because these cases render without a
     provider, which lib/i18n defines as English. */
  const compact = (iso: string) => stampShort(new Date(iso), "en");

  it("shows a window as a SPAN — both edges, not just its start", async () => {
    stubFetch({ byScope: { "": [win()] } });
    renderHarness("");
    const row = await screen.findByTestId("maintenance-item");
    expect(row.textContent).toContain(compact("2026-08-01T11:30:00Z"));
    expect(row.textContent).toContain(compact("2026-08-01T11:45:00Z"));
  });

  /* It is now wide enough to hold the compact pair and never truncates; the REASON is what gives way. */
  it("gives the range column room and never truncates it, keeping the FULL pair on title", async () => {
    stubFetch({ byScope: { "": [win()] } });
    renderHarness("");
    const stamp = await screen.findByTestId("maintenance-stamp");
    expect(stamp.className).toContain("lg:w-52");
    expect(stamp.className).toContain("shrink-0");
    expect(stamp.className).not.toContain("truncate");
    expect(stamp.getAttribute("title")).toBe(
      `${new Date("2026-08-01T11:30:00Z").toLocaleString(undefined, { hour12: false })} → ${new Date("2026-08-01T11:45:00Z").toLocaleString(undefined, { hour12: false })}`,
    );
  });

  /* The measurement this pins: on /settings the range "Aug 9, 11:31 AM → Aug 9, 01:31 PM" is 198px wide. */
  it("wraps at the arrow instead of overflowing — nowrap is per-stamp, not on the box", async () => {
    stubFetch({ byScope: { "": [win()] } });
    renderHarness("");
    const stamp = await screen.findByTestId("maintenance-stamp");
    // The container must NOT forbid the wrap...
    expect(stamp.className).not.toContain("whitespace-nowrap");
    // ...and each edge must stay in one piece.
    const stamps = stamp.querySelectorAll("span.whitespace-nowrap");
    expect(stamps.length).toBe(2);
    expect(stamps[0].textContent).toBe(compact("2026-08-01T11:30:00Z"));
    expect(stamps[1].textContent).toBe(compact("2026-08-01T11:45:00Z"));
    // The arrow is the break opportunity, and the visible text is unchanged.
    expect(stamp.textContent).toBe(
      `${compact("2026-08-01T11:30:00Z")} → ${compact("2026-08-01T11:45:00Z")}`,
    );
  });

  // Narrow (the Investigate rail, a phone), the range takes a row of its own
  // above the reason rather than fighting it for one line.
  it("stacks the range above the reason at narrow widths", async () => {
    stubFetch({ byScope: { "": [win()] } });
    renderHarness("");
    const stamp = await screen.findByTestId("maintenance-stamp");
    expect(stamp.className).toContain("basis-full");
    expect(stamp.className).toContain("lg:basis-auto");
  });

  it("gives the reason the row's remaining width", async () => {
    stubFetch({ byScope: { "": [win()] } });
    renderHarness("");
    const reason = await screen.findByTestId("maintenance-reason");
    expect(reason.className).toContain("flex-1");
    expect(reason.className).toContain("min-w-0");
  });

  /* QA scope 2, finding #19 — arming the delete adds a SECOND button to the
     row, and flex took every pixel of it out of the one column that says which
     window is about to go. The reason collapsed to a single character under
     the click asking you to confirm it. */
  it("keeps the reason readable once the confirm pair is in the row", async () => {
    stubFetch({ byScope: { "": [win({ reason: "core switch firmware upgrade" })] } });
    renderHarness("");
    fireEvent.click(await screen.findByRole("button", { name: /delete maintenance window/i }));
    await screen.findByRole("button", { name: /confirm delete maintenance window/i });
    const reason = screen.getByTestId("maintenance-reason");
    // A width FLOOR plus its own basis, so the row wraps rather than crushing
    // the identity out of it.
    expect(reason.className).toMatch(/basis-\d+/);
    expect(reason.className).toMatch(/min-w-\[\d+rem\]/);
    expect(reason).toHaveTextContent("core switch firmware upgrade");
  });
});

describe("create flow", () => {
  it("POSTs startAt + endAt + the surface's fixed scope + reason, in RFC3339", async () => {
    const { createBodies } = stubFetch();
    await openForm("node-a");
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "kernel patch" } });
    fireEvent.click(screen.getByRole("button", { name: "Create maintenance window" }));
    await waitFor(() => expect(createBodies).toHaveLength(1));
    // The defaults: now, and an hour of it. Both are whole minutes, which is
    // what the picker can express and therefore what the form must send.
    expect(createBodies[0]).toEqual({
      scope: "node-a",
      startAt: "2026-08-01T12:00:00.000Z",
      endAt: "2026-08-01T13:00:00.000Z",
      reason: "kernel patch",
    });
  });

  it("sends the instants the two pickers composed, including a FUTURE window", async () => {
    const { createBodies } = stubFetch();
    await openForm("");
    pickInstant("Start", "2026-08-05", "02:00");
    pickInstant("End", "2026-08-05", "04:30");
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "switch upgrade" } });
    fireEvent.click(screen.getByRole("button", { name: "Create maintenance window" }));
    await waitFor(() => expect(createBodies).toHaveLength(1));
    expect(createBodies[0]).toEqual({
      scope: "",
      startAt: new Date(2026, 7, 5, 2, 0, 0, 0).toISOString(),
      endAt: new Date(2026, 7, 5, 4, 30, 0, 0).toISOString(),
      reason: "switch upgrade",
    });
  });

  it("REFUSES an end at or before the start client-side — the store's CHECK, mirrored", async () => {
    const { createBodies } = stubFetch();
    await openForm("");
    pickInstant("End", "2026-07-31", "23:00");
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "backwards" } });
    fireEvent.click(screen.getByRole("button", { name: "Create maintenance window" }));
    await screen.findByText("The end must be after the start.");
    expect(createBodies).toHaveLength(0);
  });

  it("refuses an empty reason without going near the network", async () => {
    const { createBodies } = stubFetch();
    await openForm("");
    fireEvent.click(screen.getByRole("button", { name: "Create maintenance window" }));
    await screen.findByText("A reason is required.");
    expect(createBodies).toHaveLength(0);
  });

  it("caps the reason at the length the server enforces", async () => {
    stubFetch();
    await openForm("");
    expect(screen.getByLabelText("Reason")).toHaveAttribute("maxlength", "512");
  });

  it("surfaces a rejected create inline and keeps the form open", async () => {
    stubFetch({ onCreate: () => problem(422, "unprocessable", "endAt must be after startAt") });
    await openForm("");
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Create maintenance window" }));
    await screen.findByText("endAt must be after startAt");
    expect(screen.getByRole("form", { name: "New maintenance window" })).toBeTruthy();
  });

  it("refetches the window and closes the form after a successful create", async () => {
    const state = { rows: [] as MaintenanceWindow[] };
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      const href = String(url);
      const method = (init?.method ?? "GET").toUpperCase();
      if (href.includes("/api/v1/auth/me")) {
        return Promise.resolve(
          json({ subject: { kind: "user", id: "u1" }, permissions: ["maintenance:read", "maintenance:write"] }),
        );
      }
      if (href.startsWith("/api/v1/maintenance") && method === "POST") {
        state.rows = [win({ id: "fresh", reason: "just declared" })];
        return Promise.resolve(json(state.rows[0], { status: 201 }));
      }
      if (href.startsWith("/api/v1/maintenance")) {
        return Promise.resolve(json({ windows: state.rows, nextCursor: "" }));
      }
      return Promise.resolve(json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    renderHarness("");
    fireEvent.click(await screen.findByRole("button", { name: /maintenance/i }));
    fireEvent.change(await screen.findByLabelText("Reason"), { target: { value: "just declared" } });
    fireEvent.click(screen.getByRole("button", { name: "Create maintenance window" }));
    await screen.findByText("just declared");
    expect(screen.queryByRole("form", { name: "New maintenance window" })).toBeNull();
  });

  it("cancel closes the form and posts nothing", async () => {
    const { createBodies } = stubFetch();
    await openForm("");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("form", { name: "New maintenance window" })).toBeNull());
    expect(createBodies).toHaveLength(0);
  });
});

describe("delete flow", () => {
  it("DELETEs the row's id and refetches the window", async () => {
    const state = { rows: [win({ id: "doomed", reason: "wrong day" })] };
    const deleted: string[] = [];
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      const href = String(url);
      const method = (init?.method ?? "GET").toUpperCase();
      if (href.includes("/api/v1/auth/me")) {
        return Promise.resolve(
          json({ subject: { kind: "user", id: "u1" }, permissions: ["maintenance:read", "maintenance:write"] }),
        );
      }
      if (href.startsWith("/api/v1/maintenance/") && method === "DELETE") {
        deleted.push(decodeURIComponent(href.slice("/api/v1/maintenance/".length)));
        state.rows = [];
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (href.startsWith("/api/v1/maintenance")) {
        return Promise.resolve(json({ windows: state.rows, nextCursor: "" }));
      }
      return Promise.resolve(json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    renderHarness("");
    fireEvent.click(await screen.findByRole("button", { name: /^delete maintenance window/i }));
    fireEvent.click(await screen.findByRole("button", { name: /^confirm delete maintenance window/i }));
    await waitFor(() => expect(deleted).toEqual(["doomed"]));
    await waitFor(() => expect(screen.queryByText("wrong day")).toBeNull());
  });

  it("surfaces a failed delete on the row and keeps it", async () => {
    stubFetch({ byScope: { "": [win({ id: "gone", reason: "already deleted" })] }, onDelete: () => problem(404, "not found") });
    renderHarness("");
    fireEvent.click(await screen.findByRole("button", { name: /^delete maintenance window/i }));
    fireEvent.click(await screen.findByRole("button", { name: /^confirm delete maintenance window/i }));
    await screen.findByText("not found");
    expect(screen.getByText("already deleted")).toBeTruthy();
  });

  it("asks for a second click before deleting anything", async () => {
    const { deleteIds } = stubFetch({ byScope: { "": [win({ id: "doomed", reason: "wrong day" })] } });
    renderHarness("");
    fireEvent.click(await screen.findByRole("button", { name: /^delete maintenance window/i }));
    expect(deleteIds).toHaveLength(0);
    expect(screen.getByRole("button", { name: /^confirm delete maintenance window/i })).toBeInTheDocument();
  });

  it("backs out cleanly, leaving the row and its normal Delete", async () => {
    const { deleteIds } = stubFetch({ byScope: { "": [win({ id: "doomed", reason: "wrong day" })] } });
    renderHarness("");
    fireEvent.click(await screen.findByRole("button", { name: /^delete maintenance window/i }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(deleteIds).toHaveLength(0);
    expect(screen.getByText("wrong day")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^delete maintenance window/i })).toBeInTheDocument();
  });
});

/* ── QA round 2, finding #18: a disabled control says why ────────────────── */

describe("a time-disabled maintenance control carries its reason", () => {
  it("titles and describes the create button while engaged", async () => {
    window.history.pushState({}, "", "/explore?at=2026-08-01T09:00:00Z");
    stubFetch();
    renderHarness("");
    const button = await screen.findByRole("button", { name: /maintenance/i });
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute("title", "Time Machine is engaged — return to Live to act.");
    expect(document.getElementById(button.getAttribute("aria-describedby") as string)).toHaveTextContent(
      "Time Machine is engaged — return to Live to act.",
    );
  });
});

/* The button LOOKED guarded — Button disables itself while `loading` — but the flag it read was useState. */
describe("one window per click storm (#17)", () => {
  it("POSTs once for three rapid clicks", async () => {
    const { createBodies } = stubFetch();
    await openForm("node-a");
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "kernel patch" } });

    const submit = screen.getByRole("button", { name: "Create maintenance window" });
    /* One task, three clicks, no render between them — fireEvent.click flushes
       React between calls, so a suite using it would pass against the bug. */
    await act(async () => {
      submit.click();
      submit.click();
      submit.click();
    });

    await waitFor(() => expect(createBodies.length).toBeGreaterThan(0));
    expect(createBodies).toHaveLength(1);
  });
});
