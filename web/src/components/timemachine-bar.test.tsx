import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TimeMachineBar } from "@/components/timemachine-bar";
import { TimeMachineControl } from "@/components/timemachine-control";
import { PageShell } from "@/components/page-shell";
import { ThemeProvider } from "@/components/theme-provider";
import { TimeMachineProvider } from "@/lib/timemachine";
import { AppShell } from "@/routes";

/* The two halves as the app composes them: the banner in the chrome, the trigger
   in the page header (components/page-shell.tsx). Rendering one without the
   other would test a screen that does not exist. */
function renderBar() {
  return render(
    <TimeMachineProvider>
      <TimeMachineBar />
      <TimeMachineControl />
    </TimeMachineProvider>,
  );
}

const atParam = () => new URLSearchParams(window.location.search).get("at");
const asAtParam = (d: Date) => d.toISOString().replace(/\.\d{3}Z$/, "Z");

/* The bar's control is now a calendar (ui/datetime-picker.tsx), so which days it offers depends on the clock. */
const NOW = new Date(2026, 7, 8, 12, 0, 0); // Sat 8 August 2026, 12:00 local

const openPicker = (name: RegExp) => fireEvent.click(screen.getByRole("button", { name }));
const popover = () => screen.getByRole("dialog", { name: /choose a date and time/i });
const dateField = () => within(popover()).getByLabelText("Date");
const timeField = () => within(popover()).getByLabelText("Time");
const applyButton = () => within(popover()).getByRole("button", { name: "Apply" });

beforeEach(() => {
  window.history.pushState({}, "", "/");
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("Time Machine while live", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(NOW);
  });
  afterEach(() => vi.useRealTimers());

  it("renders the trigger only — no banner, no popover", () => {
    renderBar();
    expect(screen.getByRole("button", { name: /time machine/i })).toBeInTheDocument();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  /* Live it names the feature AND the anchor. The bare "Now" chip read as part
     of the range presets and nobody found the Time Machine behind it (M3-11
     reversed the earlier anchor-only wording on live-console evidence). */
  it("reads 'Time Machine: Now', so the idle trigger is findable as the Time Machine", () => {
    renderBar();
    expect(screen.getByRole("button", { name: /time machine/i })).toHaveTextContent("Time Machine: Now");
  });

  it("opens the calendar popover on now", () => {
    renderBar();
    openPicker(/time machine/i);
    expect(popover()).toBeInTheDocument();
    // The default is now, not an arbitrary epoch.
    expect(dateField()).toHaveValue("2026-08-08");
    expect(timeField()).toHaveValue("12:00");
    expect(within(popover()).getByRole("gridcell", { selected: true })).toBeInTheDocument();
  });

  it("engages from a cold start in one preset click, writing ?at= to the URL", () => {
    renderBar();
    openPicker(/time machine/i);
    fireEvent.click(screen.getByRole("button", { name: "1h ago" }));

    expect(screen.getByRole("status")).toHaveTextContent(/you are viewing/i);
    expect(atParam()).toBe(asAtParam(new Date(NOW.getTime() - 3_600_000)));
  });

  it("engages the instant typed into the manual fields on Apply", () => {
    renderBar();
    openPicker(/time machine/i);
    // The manual path the raw datetime-local used to be: type both halves,
    // never touch the grid.
    fireEvent.change(dateField(), { target: { value: "2026-08-07" } });
    fireEvent.change(timeField(), { target: { value: "15:34" } });
    fireEvent.click(applyButton());

    expect(screen.getByRole("status")).toHaveTextContent(/you are viewing/i);
    // The fields carry a LOCAL wall clock; the URL carries the instant they
    // name, in UTC.
    expect(atParam()).toBe(asAtParam(new Date(2026, 7, 7, 15, 34, 0)));
  });

  it("engages at a day clicked in the grid, keeping the time of day", () => {
    renderBar();
    openPicker(/time machine/i);
    fireEvent.click(screen.getByRole("button", { name: "Choose 3 August 2026" }));
    fireEvent.click(applyButton());
    expect(atParam()).toBe(asAtParam(new Date(2026, 7, 3, 12, 0, 0)));
  });

  it("cannot engage from a cleared field — nothing unparseable reaches the store", () => {
    renderBar();
    openPicker(/time machine/i);
    fireEvent.change(dateField(), { target: { value: "" } });
    expect(applyButton()).toBeDisabled();
    fireEvent.click(applyButton());

    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(atParam()).toBeNull();
  });

  it("closes without engaging on Cancel", () => {
    renderBar();
    openPicker(/time machine/i);
    fireEvent.click(within(popover()).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(atParam()).toBeNull();
  });
});

describe("Time Machine while engaged", () => {
  const engagedAt = new Date(2026, 7, 7, 15, 34, 0);

  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(NOW);
  });
  afterEach(() => vi.useRealTimers());

  function renderEngaged() {
    window.history.pushState({}, "", `/matrix?at=${encodeURIComponent(engagedAt.toISOString())}`);
    return renderBar();
  }

  it("renders the amber status banner naming the local datetime", () => {
    renderEngaged();
    const banner = screen.getByRole("status");
    expect(banner).toHaveTextContent(/you are viewing/i);
    expect(banner).toHaveTextContent(engagedAt.toLocaleString(undefined, { hour12: false }));
    expect(banner).toHaveTextContent(/return to live to act/i);
  });

  it("offers Return to Live, which clears ?at= and drops back to the trigger", () => {
    renderEngaged();
    fireEvent.click(screen.getByRole("button", { name: /return to live/i }));
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(atParam()).toBeNull();
    expect(window.location.pathname).toBe("/matrix");
    expect(screen.getByRole("button", { name: /time machine/i })).toBeInTheDocument();
  });

  /* One picker, in one place: the banner used to carry a second one two inches
     from the first, both naming the same instant. */
  it("leaves the picking to the header trigger — the banner is a status, not a control", () => {
    renderEngaged();
    expect(screen.getAllByRole("button", { name: /change the viewing time/i })).toHaveLength(1);
    within(screen.getByRole("status")).getByRole("button", { name: /return to live/i });
    expect(within(screen.getByRole("status")).queryByRole("button", { name: /change the viewing time/i })).toBeNull();
  });

  it("states the instant ONCE, in one format, on both the banner and the chip", () => {
    renderEngaged();
    const adjust = screen.getByRole("button", { name: /change the viewing time/i });

    expect(adjust).toHaveTextContent(engagedAt.toLocaleString(undefined, { hour12: false }));
    expect(screen.getByRole("status")).toHaveTextContent(engagedAt.toLocaleString(undefined, { hour12: false }));
    expect(adjust.getAttribute("aria-label")).toContain(engagedAt.toLocaleString(undefined, { hour12: false }));
  });

  it("shows the engaged instant on the adjust trigger and inside the popover", () => {
    renderEngaged();
    const adjust = screen.getByRole("button", { name: /change the viewing time/i });
    expect(adjust).toHaveTextContent(/2026/);

    fireEvent.click(adjust);
    expect(dateField()).toHaveValue("2026-08-07");
    expect(timeField()).toHaveValue("15:34");
  });

  it("re-engages at the new t when a calendar day is picked", () => {
    renderEngaged();
    openPicker(/change the viewing time/i);
    fireEvent.click(screen.getByRole("button", { name: "Choose 6 August 2026" }));
    fireEvent.click(applyButton());

    // The day moved, the time of day did not.
    expect(atParam()).toBe(asAtParam(new Date(2026, 7, 6, 15, 34, 0)));
    expect(screen.getByRole("status")).toHaveTextContent(new Date(2026, 7, 6, 15, 34).toLocaleString(undefined, { hour12: false }));
  });

  it("clamps a manually typed future instant to now rather than sending the future to the API", () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    renderEngaged();
    openPicker(/change the viewing time/i);
    // The grid disables future days, so this can only arrive by typing — and
    // the clamp still has to hold behind it.
    fireEvent.change(dateField(), { target: { value: "2026-08-15" } });
    fireEvent.click(applyButton());

    expect(Math.abs(new Date(atParam()!).getTime() - Date.now())).toBeLessThan(5_000);
  });
});

/** Both halves are only useful if the real shell actually mounts them. */
describe("AppShell mounts the Time Machine", () => {
  it("puts the trigger in the page header, and no banner while live", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              auth: { mode: "local", role: "", loginPath: "/login" },
              anonymousBanner: false,
              controller: { configured: true },
              prometheus: { configured: true },
              database: { configured: true },
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          ),
        ),
      ),
    );
    const testRoot = createRootRoute({
      component: () => (
        <AppShell>
          <Outlet />
        </AppShell>
      ),
    });
    const testRouter = createRouter({
      routeTree: testRoot.addChildren([
        createRoute({
          getParentRoute: () => testRoot,
          path: "/",
          /* A real page, because the trigger now travels with PageShell rather
             than with the chrome. */
          component: () => (
            <PageShell timeMachine title="Explore" actions={<button type="button">1h</button>}>
              page content
            </PageShell>
          ),
        }),
      ]),
      history: createMemoryHistory({ initialEntries: ["/"] }),
    });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ThemeProvider>
          <RouterProvider router={testRouter} />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    const trigger = await screen.findByRole("button", { name: /time machine/i });
    expect(screen.getByText("page content")).toBeInTheDocument();
    /* Beside the page's own time filter, in one row — that placement IS the
       feature here, so assert the shared parent rather than mere presence. */
    const actionsRow = screen.getByRole("button", { name: "1h" }).parentElement;
    expect(actionsRow).toContainElement(trigger);
    // Live pays no banner. By its words, not by role: the anonymous-mode
    // banner is a role="status" too, and it is a different statement.
    expect(screen.queryByText(/you are viewing/i)).not.toBeInTheDocument();
  });
});
