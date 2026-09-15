import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AnnotationBar, scopeLabel, useAnnotations } from "@/components/annotations";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { Annotation } from "@/lib/types";

/** The shared annotations hook + bar. */

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const problem = (status: number, title: string, detail?: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

function ann(over: Partial<Annotation> = {}): Annotation {
  return {
    id: "a-1",
    startAt: "2026-08-01T11:30:00Z",
    scope: "",
    text: "rolled the gateway",
    createdBy: "user:ada",
    createdAt: "2026-08-01T11:30:01Z",
    ...over,
  };
}

interface StubOpts {
  permissions?: string[];
  byScope?: Record<string, Annotation[]>;
  onCreate?: (body: unknown) => Response;
  onDelete?: (id: string) => Response;
}

function stubFetch(opts: StubOpts = {}) {
  const { permissions = ["annotations:read", "annotations:write"], byScope = {}, onCreate, onDelete } = opts;
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
    if (href.startsWith("/api/v1/annotations/") && method === "DELETE") {
      const id = decodeURIComponent(href.slice("/api/v1/annotations/".length));
      deleteIds.push(id);
      return Promise.resolve(onDelete ? onDelete(id) : new Response(null, { status: 204 }));
    }
    if (href.startsWith("/api/v1/annotations") && method === "POST") {
      const body: unknown = JSON.parse(String(init?.body ?? "{}"));
      createBodies.push(body);
      return Promise.resolve(onCreate ? onCreate(body) : json(ann({ id: "new" }), { status: 201 }));
    }
    if (href.startsWith("/api/v1/annotations")) {
      const qs = new URLSearchParams(href.split("?")[1] ?? "");
      listCalls.push(qs);
      // A key of "\u0000" stands for "the parameter was absent"; "" is the
      // present-but-empty (global-only) listing, which is a different request.
      const key = qs.has("scope") ? (qs.get("scope") as string) : "\u0000";
      return Promise.resolve(json({ annotations: byScope[key] ?? [], nextCursor: "" }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { fetchMock, listCalls, createBodies, deleteIds };
}

/** A probe that renders the hook's output through the bar — the same pairing
 *  every real surface uses. */
function Harness({ scope, rangeSeconds = 3600 }: { scope: string; rangeSeconds?: number }) {
  const { annotations, error, refresh } = useAnnotations(scope, rangeSeconds);
  return <AnnotationBar scope={scope} annotations={annotations} error={error} onChanged={() => void refresh()} />;
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
  return listCalls.map((qs) => (qs.has("scope") ? `=${qs.get("scope")}` : "absent")).sort();
}

beforeEach(() => {
  window.history.pushState({}, "", "/explore");
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  window.history.pushState({}, "", "/");
});

describe("scopeLabel", () => {
  it('names "" global rather than empty', () => {
    expect(scopeLabel("")).toBe("global");
  });

  it("passes any other scope through", () => {
    expect(scopeLabel("node-a→node-b")).toBe("node-a→node-b");
  });
});

/**
 * Drives one of the form's two DateTimePickers: open it, type the local day and wall-clock into its
 * manual fields.
 */
function pickInstant(triggerName: "Start" | "End", date: string, time: string) {
  fireEvent.click(screen.getByRole("button", { name: triggerName }));
  fireEvent.change(screen.getByLabelText("Date"), { target: { value: date } });
  fireEvent.change(screen.getByLabelText("Time"), { target: { value: time } });
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
}

async function openForm(scope = "") {
  const view = renderHarness(scope);
  fireEvent.click(await screen.findByRole("button", { name: /annotate/i }));
  /* role="form", not role="dialog": the popover is a disclosure — no focus trap, no Escape dismissal. */
  await screen.findByRole("form", { name: "New annotation" });
  return view;
}

describe("useAnnotations scope semantics", () => {
  it("asks ONLY for the global listing on a global surface, with scope present-but-empty", async () => {
    const { listCalls } = stubFetch({ byScope: { "": [ann()] } });
    renderHarness("");
    await screen.findByText(/rolled the gateway/);
    expect(scopesAsked(listCalls)).toEqual(["="]);
  });

  it("asks for BOTH the surface scope and the global one on a scoped surface", async () => {
    const { listCalls } = stubFetch({ byScope: { "": [ann({ id: "g", text: "fleet note" })], "node-a": [ann({ id: "n", scope: "node-a", text: "drained" })] } });
    renderHarness("node-a");
    await screen.findByText(/drained/);
    await screen.findByText(/fleet note/);
    expect(scopesAsked(listCalls)).toEqual(["=", "=node-a"]);
  });

  it("never asks with the scope parameter ABSENT — that would be every scope in the fleet", async () => {
    const { listCalls } = stubFetch({ byScope: { "\u0000": [ann({ id: "other", scope: "node-z", text: "somebody else" })] } });
    renderHarness("node-a");
    await waitFor(() => expect(listCalls.length).toBeGreaterThanOrEqual(2));
    expect(listCalls.every((qs) => qs.has("scope"))).toBe(true);
    expect(screen.queryByText(/somebody else/)).toBeNull();
  });

  it("bounds the fetch to the visible window: from = to - rangeSeconds", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-01T12:00:00Z"));
    const { listCalls } = stubFetch();
    renderHarness("", 900);
    await vi.waitFor(() => expect(listCalls.length).toBeGreaterThan(0));
    expect(listCalls[0].get("to")).toBe("2026-08-01T12:00:00.000Z");
    expect(listCalls[0].get("from")).toBe("2026-08-01T11:45:00.000Z");
  });

  it("anchors the window at t while the Time Machine is engaged", async () => {
    window.history.pushState({}, "", "/explore?at=2026-08-01T09:00:00Z");
    const { listCalls } = stubFetch();
    renderHarness("", 3600);
    await waitFor(() => expect(listCalls.length).toBeGreaterThan(0));
    expect(listCalls[0].get("to")).toBe("2026-08-01T09:00:00.000Z");
    expect(listCalls[0].get("from")).toBe("2026-08-01T08:00:00.000Z");
  });

  it("de-duplicates a mark that came back from both legs", async () => {
    const shared = ann({ id: "same", text: "one note" });
    stubFetch({ byScope: { "": [shared], "node-a": [shared] } });
    renderHarness("node-a");
    await waitFor(() => expect(screen.getAllByText("one note")).toHaveLength(1));
  });

  it("says so, once, when the listing fails", async () => {
    const fetchMock = vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/auth/me")) return Promise.resolve(json({ subject: {}, permissions: [] }));
      return Promise.resolve(problem(503, "database unavailable"));
    });
    vi.stubGlobal("fetch", fetchMock);
    renderHarness("");
    await screen.findByText("Annotations are unavailable.");
  });
});

describe("AnnotationBar affordances", () => {
  it("HIDES the create button without annotations:write", async () => {
    stubFetch({ permissions: ["annotations:read"] });
    renderHarness("");
    await screen.findByText(/0 annotations in this window/);
    expect(screen.queryByRole("button", { name: /annotate/i })).toBeNull();
  });

  it("HIDES the delete button without annotations:write", async () => {
    stubFetch({ permissions: ["annotations:read"], byScope: { "": [ann()] } });
    renderHarness("");
    await screen.findByText(/rolled the gateway/);
    expect(screen.queryByRole("button", { name: /delete annotation/i })).toBeNull();
  });

  it("shows the create button, enabled, with annotations:write while Live", async () => {
    stubFetch();
    renderHarness("");
    const button = await screen.findByRole("button", { name: /annotate/i });
    expect(button).not.toBeDisabled();
  });

  it("keeps create VISIBLE but DISABLED while the Time Machine is engaged", async () => {
    window.history.pushState({}, "", "/explore?at=2026-08-01T09:00:00Z");
    stubFetch();
    renderHarness("");
    const button = await screen.findByRole("button", { name: /annotate/i });
    expect(button).toBeDisabled();
  });

  it("keeps delete VISIBLE but DISABLED while engaged", async () => {
    window.history.pushState({}, "", "/explore?at=2026-08-01T09:00:00Z");
    stubFetch({ byScope: { "": [ann()] } });
    renderHarness("");
    expect(await screen.findByRole("button", { name: /delete annotation/i })).toBeDisabled();
  });

  it("names the surface's scope so an operator knows where a note will land", async () => {
    stubFetch();
    renderHarness("node-a→node-b");
    await screen.findByText(/scope node-a→node-b/);
  });
});

describe("create flow", () => {
  it("POSTs startAt + the surface's fixed scope + text, and OMITS endAt for an instant mark", async () => {
    const { createBodies } = stubFetch();
    await openForm("node-a");
    pickInstant("Start", "2026-08-01", "11:30");
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "drained node-a" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await waitFor(() => expect(createBodies).toHaveLength(1));
    expect(createBodies[0]).toEqual({
      startAt: new Date(2026, 7, 1, 11, 30).toISOString(),
      scope: "node-a",
      text: "drained node-a",
    });
  });

  it("carries endAt when an end was given — that is what makes it a span", async () => {
    const { createBodies } = stubFetch();
    await openForm("");
    pickInstant("Start", "2026-08-01", "11:30");
    pickInstant("End", "2026-08-01", "11:45");
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "maintenance window" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await waitFor(() => expect(createBodies).toHaveLength(1));
    expect(createBodies[0]).toEqual({
      startAt: new Date(2026, 7, 1, 11, 30).toISOString(),
      endAt: new Date(2026, 7, 1, 11, 45).toISOString(),
      scope: "",
      text: "maintenance window",
    });
  });

  it("refetches the window and closes the form after a successful create", async () => {
    const state = { rows: [] as Annotation[] };
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      const href = String(url);
      const method = (init?.method ?? "GET").toUpperCase();
      if (href.includes("/api/v1/auth/me")) {
        return Promise.resolve(json({ subject: { kind: "user", id: "u1" }, permissions: ["annotations:write"] }));
      }
      if (href.startsWith("/api/v1/annotations") && method === "POST") {
        state.rows = [ann({ id: "fresh", text: "just written" })];
        return Promise.resolve(json(state.rows[0], { status: 201 }));
      }
      if (href.startsWith("/api/v1/annotations")) {
        return Promise.resolve(json({ annotations: state.rows, nextCursor: "" }));
      }
      return Promise.resolve(json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    renderHarness("");
    fireEvent.click(await screen.findByRole("button", { name: /annotate/i }));
    fireEvent.change(await screen.findByLabelText("Note"), { target: { value: "just written" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await screen.findByText("just written");
    expect(screen.queryByRole("form", { name: "New annotation" })).toBeNull();
  });

  it("refuses an empty note without going near the network", async () => {
    const { createBodies } = stubFetch();
    await openForm("");
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await screen.findByText("A note is required.");
    expect(createBodies).toHaveLength(0);
  });

  it("caps the note at the length the server enforces", async () => {
    stubFetch();
    await openForm("");
    expect(screen.getByLabelText("Note")).toHaveAttribute("maxlength", "1024");
  });

  it("surfaces a rejected create inline and keeps the form open", async () => {
    stubFetch({ onCreate: () => problem(422, "unprocessable", "text is too long") });
    await openForm("");
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await screen.findByText("text is too long");
    expect(screen.getByRole("form", { name: "New annotation" })).toBeTruthy();
  });

  it("cancel closes the form and posts nothing", async () => {
    const { createBodies } = stubFetch();
    await openForm("");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("form", { name: "New annotation" })).toBeNull());
    expect(createBodies).toHaveLength(0);
  });
});

/* ── QA round 2, finding #13: both edges go through the DateTimePicker ───── */

describe("the annotate form's time controls", () => {
  it("uses the M5 picker rather than a raw datetime-local for either edge", async () => {
    stubFetch();
    const view = await openForm("");
    expect(view.container.querySelector('input[type="datetime-local"]')).toBeNull();
    expect(screen.getByRole("button", { name: "Start" })).toHaveAttribute("aria-haspopup", "dialog");
    expect(screen.getByRole("button", { name: "End" })).toHaveAttribute("aria-haspopup", "dialog");
  });

  /* Wrapping is driven by the width actually available. */
  it("lays the two edges out by available width, not by the viewport breakpoint", async () => {
    stubFetch();
    await openForm("");
    const row = screen.getByRole("button", { name: "Start" }).closest("div.flex.flex-wrap");
    expect(row).not.toBeNull();
    expect(row?.className).not.toContain("sm:grid-cols-2");
  });

  it("opens with NO end at all — absence is what makes a mark an instant", async () => {
    stubFetch();
    await openForm("");
    expect(screen.getByRole("button", { name: "End" })).toHaveTextContent("Not set");
    expect(screen.queryByRole("button", { name: "Clear end" })).toBeNull();
  });

  it("can put the end back to unset, so the optional field is really optional", async () => {
    const { createBodies } = stubFetch();
    await openForm("");
    pickInstant("End", "2026-08-01", "11:45");
    fireEvent.click(screen.getByRole("button", { name: "Clear end" }));
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "back to an instant" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await waitFor(() => expect(createBodies).toHaveLength(1));
    expect(createBodies[0]).not.toHaveProperty("endAt");
  });

  /* An annotation records what HAPPENED. The API imposes no future bound (see
     store.AnnotationInput.Validate), so this is the console's own rule — and
     the reason maintenance keeps allowFuture while this form does not. */
  it("offers no future day on either edge", async () => {
    stubFetch();
    await openForm("");
    fireEvent.click(screen.getByRole("button", { name: "Start" }));
    const tomorrow = new Date();
    tomorrow.setDate(tomorrow.getDate() + 1);
    const label = `Choose ${tomorrow.getDate()} ${tomorrow.toLocaleString("en-US", { month: "long" })} ${tomorrow.getFullYear()}`;
    const day = screen.queryByRole("button", { name: label });
    if (day) expect(day).toBeDisabled();
    expect(screen.getByRole("button", { name: "Next month" })).toBeDisabled();
  });
});

/* ── QA round 2, finding #17: the end/start check happens HERE first ─────── */

describe("end before start", () => {
  it("is refused locally, in the reader's own words, without a POST", async () => {
    const { createBodies } = stubFetch();
    await openForm("");
    pickInstant("Start", "2026-08-01", "11:30");
    pickInstant("End", "2026-08-01", "11:00");
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "backwards" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await screen.findByText("End is before start.");
    expect(createBodies).toHaveLength(0);
  });

  it("still renders the server's own 422 verbatim — the local check is a shortcut, not a replacement", async () => {
    stubFetch({ onCreate: () => problem(422, "unprocessable", "end_at 2026-08-01T09:00:00Z is before start_at") });
    await openForm("");
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await screen.findByText("end_at 2026-08-01T09:00:00Z is before start_at");
  });
});

/* ── QA round 2, finding #20: where focus lands after a submit ───────────── */

describe("focus after the annotate form closes", () => {
  it("returns to the trigger on success", async () => {
    stubFetch();
    await openForm("");
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "done" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await waitFor(() => expect(screen.queryByRole("form", { name: "New annotation" })).toBeNull());
    expect(document.activeElement).toBe(screen.getByRole("button", { name: /annotate/i }));
  });

  it("returns to the trigger on cancel", async () => {
    stubFetch();
    await openForm("");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("form", { name: "New annotation" })).toBeNull());
    expect(document.activeElement).toBe(screen.getByRole("button", { name: /annotate/i }));
  });

  it("goes to the empty note when that is what is wrong", async () => {
    stubFetch();
    await openForm("");
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await screen.findByText("A note is required.");
    expect(document.activeElement).toBe(screen.getByLabelText("Note"));
  });

  it("goes to the End control when the range is backwards", async () => {
    stubFetch();
    await openForm("");
    pickInstant("Start", "2026-08-01", "11:30");
    pickInstant("End", "2026-08-01", "11:00");
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "backwards" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    await screen.findByText("End is before start.");
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "End" }));
  });
});

/* ── QA round 2, finding #18: a disabled control says why ────────────────── */

describe("a time-disabled control carries its reason", () => {
  it("titles and describes the create button while engaged", async () => {
    window.history.pushState({}, "", "/explore?at=2026-08-01T09:00:00Z");
    stubFetch();
    renderHarness("");
    const button = await screen.findByRole("button", { name: /annotate/i });
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute("title", "Time Machine is engaged — return to Live to act.");
    const describedBy = button.getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();
    expect(document.getElementById(describedBy as string)).toHaveTextContent(
      "Time Machine is engaged — return to Live to act.",
    );
  });

  it("adds nothing at all while Live", async () => {
    stubFetch();
    renderHarness("");
    const button = await screen.findByRole("button", { name: /annotate/i });
    expect(button).not.toBeDisabled();
    expect(button).not.toHaveAttribute("title");
    expect(button).not.toHaveAttribute("aria-describedby");
  });
});

describe("delete flow", () => {
  it("DELETEs the row's id and refetches the window", async () => {
    const state = { rows: [ann({ id: "doomed", text: "typo note" })] };
    const deleted: string[] = [];
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      const href = String(url);
      const method = (init?.method ?? "GET").toUpperCase();
      if (href.includes("/api/v1/auth/me")) {
        return Promise.resolve(json({ subject: { kind: "user", id: "u1" }, permissions: ["annotations:write"] }));
      }
      if (href.startsWith("/api/v1/annotations/") && method === "DELETE") {
        deleted.push(decodeURIComponent(href.slice("/api/v1/annotations/".length)));
        state.rows = [];
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (href.startsWith("/api/v1/annotations")) {
        return Promise.resolve(json({ annotations: state.rows, nextCursor: "" }));
      }
      return Promise.resolve(json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    renderHarness("");
    fireEvent.click(await screen.findByRole("button", { name: /^delete annotation/i }));
    fireEvent.click(await screen.findByRole("button", { name: /^confirm delete annotation/i }));
    await waitFor(() => expect(deleted).toEqual(["doomed"]));
    await waitFor(() => expect(screen.queryByText("typo note")).toBeNull());
  });

  it("surfaces a failed delete on the row and keeps it", async () => {
    stubFetch({ byScope: { "": [ann({ id: "gone", text: "already deleted" })] }, onDelete: () => problem(404, "not found") });
    renderHarness("");
    fireEvent.click(await screen.findByRole("button", { name: /^delete annotation/i }));
    fireEvent.click(await screen.findByRole("button", { name: /^confirm delete annotation/i }));
    await screen.findByText("not found");
    expect(screen.getByText("already deleted")).toBeTruthy();
  });

  it("asks for a second click before deleting anything", async () => {
    const { deleteIds } = stubFetch({ byScope: { "": [ann({ id: "doomed", text: "typo note" })] } });
    renderHarness("");
    fireEvent.click(await screen.findByRole("button", { name: /^delete annotation/i }));
    expect(deleteIds).toHaveLength(0);
    expect(screen.getByRole("button", { name: /^confirm delete annotation/i })).toBeInTheDocument();
  });

  it("backs out cleanly, leaving the row and its normal Delete", async () => {
    const { deleteIds } = stubFetch({ byScope: { "": [ann({ id: "doomed", text: "typo note" })] } });
    renderHarness("");
    fireEvent.click(await screen.findByRole("button", { name: /^delete annotation/i }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(deleteIds).toHaveLength(0);
    expect(screen.getByText("typo note")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^delete annotation/i })).toBeInTheDocument();
  });
});

/* ── QA round 2, finding #11: the row's own layout ───────────────────────── */

describe("the annotation row", () => {
  it("keeps the stamp narrow and truncating, with the whole thing on hover", async () => {
    stubFetch({ byScope: { "": [ann({ startAt: "2026-08-01T11:30:00Z", text: "rolled the gateway" })] } });
    renderHarness("");
    const row = await screen.findByTestId("annotation-item");
    const stamp = row.querySelector("span");
    expect(stamp?.className).toContain("w-28");
    expect(stamp?.className).toContain("truncate");
    expect(stamp?.getAttribute("title")).toBe(new Date("2026-08-01T11:30:00Z").toLocaleString(undefined, { hour12: false }));
  });
});

/* ── QA round 3 ─────────────────────────────────────────────────────────── */

describe("#11 the note keeps the row's remaining width", () => {
  it("truncates the SCOPE column too, so a pair scope cannot squeeze the text out", async () => {
    stubFetch({ byScope: { "": [ann({ scope: "", text: "rolled the gateway" })] } });
    renderHarness("");
    const row = await screen.findByTestId("annotation-item");
    const text = await screen.findByTestId("annotation-text");
    expect(text.className).toContain("flex-1");
    expect(text.className).toContain("min-w-0");

    const scope = [...row.querySelectorAll("span")].find((s) => s.textContent === "global");
    expect(scope?.className).toContain("truncate");
    expect(scope?.className).toContain("max-w-[7rem]");
    expect(scope?.getAttribute("title")).toBe("global");
  });
});

/* ── polish: the row reflows by the LIST's width, and a chip is for foreign scopes ── */

describe("the row lays itself out by its container, not the viewport", () => {
  it("stacks the note first and full-width under @md, and is the single truncating line from @md", async () => {
    stubFetch({ byScope: { "": [ann({ scope: "", text: "rolled the gateway" })] } });
    renderHarness("");
    const text = await screen.findByTestId("annotation-text");
    // Stacked: the note is its own first line, clamped to two, and may wrap.
    expect(text.className).toContain("order-first");
    expect(text.className).toContain("basis-full");
    expect(text.className).toContain("line-clamp-2");
    expect(text.className).toContain("whitespace-normal");
    // Single line: the flex-1 / min-w-0 / truncate trio, now behind the container variant.
    expect(text.className).toContain("@md:flex-1");
    expect(text.className).toContain("@md:truncate");
    expect(text.className).toContain("@md:order-none");
    // The list is the container the row measures against, and never wider than its parent.
    const list = text.closest("ul")!;
    expect(list.className).toContain("@container");
    expect(list.className).toContain("w-full");
    expect(list.className).toContain("min-w-0");
    // Delete hugs the right of the meta line.
    expect(screen.getByRole("button", { name: /^delete annotation/i }).className).toContain("ml-auto");
  });
});

describe("ownScope: the scope chip names a FOREIGN scope", () => {
  function renderOwn(ownScope: string | undefined) {
    stubFetch();
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <AnnotationBar
            scope="node-a→node-b"
            ownScope={ownScope}
            annotations={[
              ann({ id: "own", scope: "node-a→node-b", text: "pair note" }),
              ann({ id: "fleet", scope: "", text: "fleet note" }),
            ]}
            onChanged={() => {}}
          />
        </TimeMachineProvider>
      </QueryClientProvider>,
    );
  }
  const chips = (row: HTMLElement) => [...row.querySelectorAll("span")].map((s) => s.textContent);

  it("drops the chip on a note filed under the surface's own scope and keeps it on a global one", async () => {
    renderOwn("node-a→node-b");
    const rows = await screen.findAllByTestId("annotation-item");
    const own = rows.find((r) => r.textContent?.includes("pair note"))!;
    const fleet = rows.find((r) => r.textContent?.includes("fleet note"))!;
    expect(chips(own)).not.toContain("node-a→node-b");
    // The full scope is still one hover away, on the row.
    expect(own).toHaveAttribute("title", "node-a→node-b");
    expect(chips(fleet)).toContain("global");
    expect(fleet).not.toHaveAttribute("title");
  });

  it("shows every chip when no ownScope is given — the harness is unchanged", async () => {
    renderOwn(undefined);
    const rows = await screen.findAllByTestId("annotation-item");
    expect(rows).toHaveLength(2);
    expect(chips(rows.find((r) => r.textContent?.includes("pair note"))!)).toContain("node-a→node-b");
    expect(chips(rows.find((r) => r.textContent?.includes("fleet note"))!)).toContain("global");
    rows.forEach((r) => expect(r).not.toHaveAttribute("title"));
  });
});

describe("#15 the form is a disclosure, not a dialog", () => {
  it("carries role=form with its own name, and no dialog role anywhere", async () => {
    stubFetch();
    await openForm("");
    expect(screen.getByRole("form", { name: "New annotation" })).toBeTruthy();
    expect(screen.queryByRole("dialog", { name: "New annotation" })).toBeNull();
  });

  /* The NEGATIVE pin: Escape-to-discard is deliberately absent. */
  it("does NOT clear a typed draft on Escape", async () => {
    stubFetch();
    await openForm("");
    const note = screen.getByLabelText("Note") as HTMLTextAreaElement;
    fireEvent.change(note, { target: { value: "half a thought" } });
    fireEvent.keyDown(note, { key: "Escape" });
    fireEvent.keyDown(document, { key: "Escape" });

    expect(screen.getByRole("form", { name: "New annotation" })).toBeTruthy();
    expect((screen.getByLabelText("Note") as HTMLTextAreaElement).value).toBe("half a thought");
  });
});

describe("#8 the bar notes a create that lands outside a FROZEN window", () => {
  function renderFrozen() {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <AnnotationBar
            scope=""
            annotations={[]}
            onChanged={() => {}}
            frozenWindow={{ from: new Date("2026-08-08T00:00:00Z"), to: new Date("2026-08-08T01:00:00Z") }}
          />
        </TimeMachineProvider>
      </QueryClientProvider>,
    );
  }

  it("says so, once, after a successful create the list will not show", async () => {
    stubFetch();
    renderFrozen();
    fireEvent.click(await screen.findByRole("button", { name: /annotate/i }));
    pickInstant("Start", "2030-01-01", "12:00");
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "started the rollback" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));

    const note = await screen.findByText(/^Created — outside this window \(which ends .+\); press Investigate to reframe\.$/);
    expect(note.getAttribute("role")).toBe("status");
  });

  it("says nothing when there is no frozen window at all — every other surface re-fetches", async () => {
    stubFetch();
    await openForm("");
    pickInstant("Start", "2030-01-01", "12:00");
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "no window here" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));

    await waitFor(() => expect(screen.queryByRole("form", { name: "New annotation" })).toBeNull());
    expect(screen.queryByText(/outside this window/)).toBeNull();
  });
});

describe("the frozen window decides where the form OPENS (finding #5)", () => {
  const FROZEN = { from: new Date("2026-08-08T00:00:00Z"), to: new Date("2026-08-08T01:00:00Z") };

  function renderWith(frozenWindow?: { from: Date; to: Date }) {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <AnnotationBar scope="" annotations={[]} onChanged={() => {}} frozenWindow={frozenWindow} />
        </TimeMachineProvider>
      </QueryClientProvider>,
    );
  }

  it("defaults Start INSIDE the window rather than to a now that is hours past it", async () => {
    // NOW is 2026-08-01 in this file's fake clock, i.e. before the window —
    // every create from a frozen surface used to be born "outside this window".
    stubFetch();
    renderWith(FROZEN);
    fireEvent.click(await screen.findByRole("button", { name: /annotate/i }));
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "in the window" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));

    await waitFor(() => expect(screen.queryByRole("form", { name: "New annotation" })).toBeNull());
    // The default landed inside, so the honest out-of-window note stays silent.
    expect(screen.queryByText(/outside this window/)).toBeNull();
  });

  it("keeps the now-default when the surface is LIVE — there is no window to land in", async () => {
    stubFetch();
    renderWith(undefined);
    fireEvent.click(await screen.findByRole("button", { name: /annotate/i }));
    const trigger = screen.getByLabelText("Start");
    // 2026-08-01 is this file's frozen `now`; a live bar still opens on it.
    expect(trigger.textContent).toContain("2026");
  });
});

describe("the created-outside note does not outlive what it describes (finding #4)", () => {
  const FROZEN = { from: new Date("2026-08-08T00:00:00Z"), to: new Date("2026-08-08T01:00:00Z") };

  function Harness({ frozen, scope }: { frozen: { from: Date; to: Date }; scope: string }) {
    return (
      <AnnotationBar scope={scope} annotations={[]} onChanged={() => {}} frozenWindow={frozen} />
    );
  }

  async function createOutside() {
    fireEvent.click(await screen.findByRole("button", { name: /annotate/i }));
    pickInstant("Start", "2030-01-01", "12:00");
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "well past it" } });
    fireEvent.click(screen.getByRole("button", { name: "Create annotation" }));
    return screen.findByText(/outside this window/);
  }

  it("is cleared when the window is recommitted underneath it", async () => {
    stubFetch();
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { rerender } = render(
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <Harness frozen={FROZEN} scope="" />
        </TimeMachineProvider>
      </QueryClientProvider>,
    );
    await createOutside();

    rerender(
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <Harness frozen={{ from: new Date("2026-08-08T02:00:00Z"), to: new Date("2026-08-08T03:00:00Z") }} scope="" />
        </TimeMachineProvider>
      </QueryClientProvider>,
    );
    expect(screen.queryByText(/outside this window/)).toBeNull();
  });

  it("is cleared when the SCOPE changes — a note about one scope is not about another", async () => {
    stubFetch();
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { rerender } = render(
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <Harness frozen={FROZEN} scope="" />
        </TimeMachineProvider>
      </QueryClientProvider>,
    );
    await createOutside();

    rerender(
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <Harness frozen={FROZEN} scope="node-a" />
        </TimeMachineProvider>
      </QueryClientProvider>,
    );
    expect(screen.queryByText(/outside this window/)).toBeNull();
  });
});

/* ── one window for the chart and the bar under it ───────────────────────── */

/*
 * useMaintenance was given a shared anchor after QA scope 2 #20 (a bar counting a window the chart
 * had already excluded); useAnnotations was left computing its own `now` on every 60s poll. On a
 * pair card the chart resolves [T-1h, T] once at mount and never moves, so after twenty minutes the
 * annotation bar was asking about [now-1h, now]: notes made since the page opened were drawn as
 * markLines past the end of the data, and notes from the chart's own first twenty minutes vanished
 * from the bar while the chart still drew that time.
 */
describe("useAnnotations takes the surface's shared anchor", () => {
  it("asks for the anchor's window, not for one ending now", async () => {
    const asked: URLSearchParams[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        asked.push(new URLSearchParams(String(url).split("?")[1] ?? ""));
        return Promise.resolve(
          new Response(JSON.stringify({ annotations: [], nextCursor: "" }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          }),
        );
      }),
    );

    const range = {
      from: new Date("2026-08-01T09:00:00Z"),
      to: new Date("2026-08-01T10:00:00Z"),
    };
    function Probe() {
      useAnnotations("node-a", 3600, range);
      return null;
    }
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <Probe />
      </QueryClientProvider>,
    );

    await waitFor(() => expect(asked.length).toBeGreaterThan(0));
    for (const params of asked) {
      expect(params.get("from")).toBe(range.from.toISOString());
      expect(params.get("to")).toBe(range.to.toISOString());
    }
  });
});

/* ── the keyboard across the confirm step ────────────────────────────────── */

/*
 * The row swaps one button for two, and React destroys the focused node when it does. Focus fell to
 * <body>, so the next Tab restarted at the skip link: a keyboard user had to walk the sidebar, the
 * header and every row above to reach the confirm button they had just summoned. And nothing was
 * announced, so a screen reader read the press as "nothing happened".
 */
describe("the delete confirm step keeps the keyboard", () => {
  it("moves focus onto Confirm delete, and back to Delete on cancel", async () => {
    stubFetch({ byScope: { "": [ann()] } });
    renderHarness("");

    const del = await screen.findByRole("button", { name: /delete annotation/i });
    del.focus();
    fireEvent.click(del);

    const confirm = await screen.findByRole("button", { name: /confirm delete/i });
    expect(document.activeElement).toBe(confirm);
    expect(document.activeElement).not.toBe(document.body);

    // And the step is SPOKEN, not only drawn: a reader hearing nothing reads the press as
    // "Delete did nothing".
    expect(screen.getByRole("status")).toHaveTextContent(/confirm delete/i);

    fireEvent.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(document.activeElement).toBe(await screen.findByRole("button", { name: /delete annotation/i }));
  });
});
