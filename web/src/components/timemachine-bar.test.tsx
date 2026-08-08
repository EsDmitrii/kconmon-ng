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
import { ThemeProvider } from "@/components/theme-provider";
import { TimeMachineProvider } from "@/lib/timemachine";
import { AppShell } from "@/routes";

function renderBar() {
  return render(
    <TimeMachineProvider>
      <TimeMachineBar />
    </TimeMachineProvider>,
  );
}

const atParam = () => new URLSearchParams(window.location.search).get("at");
const asAtParam = (d: Date) => d.toISOString().replace(/\.\d{3}Z$/, "Z");

/* The bar's control is now a calendar (ui/datetime-picker.tsx), so which days
   it offers depends on the clock. Both stateful describes below freeze it; the
   AppShell mount test keeps the real one, since react-query and the router are
   in play there and it asserts nothing about time. */
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

describe("TimeMachineBar while live", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(NOW);
  });
  afterEach(() => vi.useRealTimers());

  it("renders the toggle only — no banner, no popover", () => {
    renderBar();
    expect(screen.getByRole("button", { name: /time machine/i })).toBeInTheDocument();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
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

describe("TimeMachineBar while engaged", () => {
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
    expect(banner).toHaveTextContent(engagedAt.toLocaleString());
    expect(banner).toHaveTextContent(/return to live to act/i);
  });

  it("offers Return to Live, which clears ?at= and drops back to the toggle", () => {
    renderEngaged();
    fireEvent.click(screen.getByRole("button", { name: /return to live/i }));
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(atParam()).toBeNull();
    expect(window.location.pathname).toBe("/matrix");
    expect(screen.getByRole("button", { name: /time machine/i })).toBeInTheDocument();
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
    expect(screen.getByRole("status")).toHaveTextContent(new Date(2026, 7, 6, 15, 34).toLocaleString());
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

/**
 * The bar is only useful if it is actually in the chrome: this pins the mount
 * point (AppShell's flex column, sibling to AnonymousBanner, above <main>) the
 * same way anonymous-banner.test.tsx pins the shell, with a memory-history
 * router so it cannot bleed location state into the rest of the suite.
 */
describe("AppShell mounts the Time Machine bar", () => {
  it("renders the bar above the page frame on every route", async () => {
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
        createRoute({ getParentRoute: () => testRoot, path: "/", component: () => <div>page content</div> }),
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

    expect(await screen.findByRole("button", { name: /time machine/i })).toBeInTheDocument();
    expect(screen.getByText("page content")).toBeInTheDocument();
  });
});
