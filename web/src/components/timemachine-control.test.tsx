import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PageShell } from "@/components/page-shell";
import { TimeMachineControl } from "@/components/timemachine-control";
import { TimeMachineProvider } from "@/lib/timemachine";

const NOW = new Date(2026, 7, 8, 12, 0, 0);
const atParam = () => new URLSearchParams(window.location.search).get("at");

beforeEach(() => {
  window.history.pushState({}, "", "/");
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(NOW);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("TimeMachineControl", () => {
  /* The load-bearing one: this control is mounted by PageShell, which the page
     tests render on their own. A throw here would take all of them down, and a
     trigger whose engage() does nothing would be worse — it would look live. */
  it("renders nothing at all with no TimeMachineProvider above it", () => {
    const { container } = render(<TimeMachineControl />);
    expect(container).toBeEmptyDOMElement();
  });

  it("says where the window ends, and offers the past when asked", () => {
    render(
      <TimeMachineProvider>
        <TimeMachineControl />
      </TimeMachineProvider>,
    );
    const trigger = screen.getByRole("button", { name: /time machine/i });
    expect(trigger).toHaveTextContent("Now");

    fireEvent.click(trigger);
    fireEvent.click(screen.getByRole("button", { name: "1h ago" }));
    expect(atParam()).toBe(new Date(NOW.getTime() - 3_600_000).toISOString().replace(/\.\d{3}Z$/, "Z"));
  });

  /* The whole point of the move: the reader hunting for "deeper than 24h" is
     looking at the range presets, so the control has to be in that row. */
  it("sits in PageShell's action row, beside the page's own range presets", () => {
    render(
      <TimeMachineProvider>
        <PageShell timeMachine title="Explore" actions={<button type="button">24h</button>}>
          body
        </PageShell>
      </TimeMachineProvider>,
    );
    const row = screen.getByRole("button", { name: "24h" }).parentElement;
    expect(row).toContainElement(screen.getByRole("button", { name: /time machine/i }));
  });

  it("is still there on a time-aware page that declares no actions of its own", () => {
    render(
      <TimeMachineProvider>
        <PageShell timeMachine title="Topology">
          body
        </PageShell>
      </TimeMachineProvider>,
    );
    expect(screen.getByRole("button", { name: /time machine/i })).toBeInTheDocument();
  });

  /* Targets, Alerting and Settings ignore ?at= entirely; offering to move them
     into the past was offering something that does not happen (owner report). */
  it("is absent from a page that does not opt in", () => {
    render(
      <TimeMachineProvider>
        <PageShell title="Settings" actions={<button type="button">Save</button>}>
          body
        </PageShell>
      </TimeMachineProvider>,
    );
    expect(screen.queryByRole("button", { name: /time machine/i })).toBeNull();
  });

  it("carries the instant, and the engaged colour, once engaged", () => {
    const at = new Date(2026, 7, 7, 15, 34, 0);
    window.history.pushState({}, "", `/matrix?at=${encodeURIComponent(at.toISOString())}`);
    render(
      <TimeMachineProvider>
        <TimeMachineControl />
      </TimeMachineProvider>,
    );
    const trigger = screen.getByRole("button", { name: /change the viewing time/i });
    expect(trigger).toHaveTextContent(at.toLocaleString(undefined, { hour12: false }));
    expect(trigger.className).toContain("health-warn");
  });
});
