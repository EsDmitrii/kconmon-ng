import { render, screen, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { formatAtParam, TimeMachineProvider, useTimeMachine } from "@/lib/timemachine";

/** Probe renders the whole context surface as text plus two buttons. */
function Probe({ engageAt }: { engageAt?: Date }) {
  const { at, isLive, engage, returnToLive } = useTimeMachine();
  return (
    <div>
      <span data-testid="at">{at ? at.toISOString() : "none"}</span>
      <span data-testid="live">{String(isLive)}</span>
      <button onClick={() => engage(engageAt ?? new Date("2026-08-07T10:00:00Z"))}>engage</button>
      <button onClick={() => returnToLive()}>return</button>
    </div>
  );
}

function renderProbe(engageAt?: Date) {
  return render(
    <TimeMachineProvider>
      <Probe engageAt={engageAt} />
    </TimeMachineProvider>,
  );
}

const at = () => screen.getByTestId("at").textContent;
const live = () => screen.getByTestId("live").textContent;

beforeEach(() => {
  window.history.pushState({}, "", "/");
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("formatAtParam", () => {
  it("formats UTC RFC 3339 at seconds precision", () => {
    expect(formatAtParam(new Date("2026-08-07T10:00:00Z"))).toBe("2026-08-07T10:00:00Z");
  });

  it("drops sub-second precision rather than rounding it", () => {
    expect(formatAtParam(new Date("2026-08-07T10:00:00.999Z"))).toBe("2026-08-07T10:00:00Z");
  });

  it("normalizes a non-UTC instant to Z", () => {
    expect(formatAtParam(new Date("2026-08-07T12:00:00+02:00"))).toBe("2026-08-07T10:00:00Z");
  });
});

describe("TimeMachineProvider URL initialization", () => {
  it("is live when ?at= is absent", () => {
    renderProbe();
    expect(live()).toBe("true");
    expect(at()).toBe("none");
  });

  it("engages at a valid RFC 3339 ?at=", () => {
    window.history.pushState({}, "", "/matrix?at=2026-08-07T10:00:00Z");
    renderProbe();
    expect(live()).toBe("false");
    expect(at()).toBe("2026-08-07T10:00:00.000Z");
  });

  it("accepts an offset form and normalizes it to the same instant", () => {
    window.history.pushState({}, "", "/matrix?at=2026-08-07T12:00:00%2B02:00");
    renderProbe();
    expect(at()).toBe("2026-08-07T10:00:00.000Z");
  });

  it("degrades to live with a warning for an unparseable ?at=", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    window.history.pushState({}, "", "/?at=not-a-date");
    renderProbe();
    expect(live()).toBe("true");
    expect(at()).toBe("none");
    expect(warn).toHaveBeenCalled();
  });

  it("degrades to live for a date-only ?at= (RFC 3339 wants a full instant)", () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    window.history.pushState({}, "", "/?at=2026-08-07");
    renderProbe();
    expect(live()).toBe("true");
  });

  it("degrades to live for an empty ?at=", () => {
    window.history.pushState({}, "", "/?at=");
    renderProbe();
    expect(live()).toBe("true");
  });

  it("clamps a future ?at= to now with a warning, never sending the future onward", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    window.history.pushState({}, "", "/?at=2999-01-01T00:00:00Z");
    renderProbe();
    expect(live()).toBe("false");
    const engaged = new Date(at()!).getTime();
    expect(Math.abs(engaged - Date.now())).toBeLessThan(5_000);
    expect(warn).toHaveBeenCalled();
  });
});

describe("engage / returnToLive", () => {
  it("writes ?at= into the URL, preserves the pathname and other params", () => {
    window.history.pushState({}, "", "/matrix?zone=eu-1");
    renderProbe();

    act(() => void screen.getByText("engage").click());

    expect(live()).toBe("false");
    expect(at()).toBe("2026-08-07T10:00:00.000Z");
    expect(window.location.pathname).toBe("/matrix");
    const params = new URLSearchParams(window.location.search);
    expect(params.get("at")).toBe("2026-08-07T10:00:00Z");
    expect(params.get("zone")).toBe("eu-1");
  });

  it("truncates sub-second precision so state and URL name the same instant", () => {
    renderProbe(new Date("2026-08-07T10:00:00.750Z"));
    act(() => void screen.getByText("engage").click());
    expect(at()).toBe("2026-08-07T10:00:00.000Z");
    expect(new URLSearchParams(window.location.search).get("at")).toBe("2026-08-07T10:00:00Z");
  });

  it("clamps a future engage() to now", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    renderProbe(new Date(Date.now() + 86_400_000));
    act(() => void screen.getByText("engage").click());
    expect(Math.abs(new Date(at()!).getTime() - Date.now())).toBeLessThan(5_000);
    expect(warn).toHaveBeenCalled();
  });

  it("ignores an invalid engage() date rather than corrupting the URL", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    renderProbe(new Date("nonsense"));
    act(() => void screen.getByText("engage").click());
    expect(live()).toBe("true");
    expect(window.location.search).toBe("");
    expect(warn).toHaveBeenCalled();
  });

  it("pushes a history entry so back/forward move the time context", () => {
    const before = window.history.length;
    renderProbe();
    act(() => void screen.getByText("engage").click());
    expect(window.history.length).toBe(before + 1);
  });

  it("removes ?at= on returnToLive, keeping the other params", () => {
    window.history.pushState({}, "", "/matrix?zone=eu-1&at=2026-08-07T10:00:00Z");
    renderProbe();
    expect(live()).toBe("false");

    act(() => void screen.getByText("return").click());

    expect(live()).toBe("true");
    expect(at()).toBe("none");
    expect(window.location.pathname).toBe("/matrix");
    const params = new URLSearchParams(window.location.search);
    expect(params.has("at")).toBe(false);
    expect(params.get("zone")).toBe("eu-1");
  });

  it("pushes a history entry on returnToLive when engaged", () => {
    window.history.pushState({}, "", "/?at=2026-08-07T10:00:00Z");
    renderProbe();
    const before = window.history.length;
    act(() => void screen.getByText("return").click());
    expect(window.history.length).toBe(before + 1);
  });

  it("is a no-op when returnToLive is called while already live", () => {
    renderProbe();
    const before = window.history.length;
    act(() => void screen.getByText("return").click());
    expect(window.history.length).toBe(before);
    expect(live()).toBe("true");
  });

  it("round-trips engage then returnToLive", () => {
    renderProbe();
    act(() => void screen.getByText("engage").click());
    expect(at()).toBe("2026-08-07T10:00:00.000Z");
    act(() => void screen.getByText("return").click());
    expect(at()).toBe("none");
    expect(live()).toBe("true");
  });
});

describe("popstate", () => {
  it("re-reads ?at= so back/forward honestly move the time context", () => {
    renderProbe();
    expect(live()).toBe("true");

    // jsdom's history.back() does not synchronously fire popstate; the browser
    // sequence (location changes, then popstate) is reproduced by hand.
    act(() => {
      window.history.pushState({}, "", "/?at=2026-08-07T10:00:00Z");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    expect(live()).toBe("false");
    expect(at()).toBe("2026-08-07T10:00:00.000Z");

    act(() => {
      window.history.pushState({}, "", "/");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    expect(live()).toBe("true");
    expect(at()).toBe("none");
  });

  it("degrades to live when a popped URL carries a broken ?at=", () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    window.history.pushState({}, "", "/?at=2026-08-07T10:00:00Z");
    renderProbe();
    expect(live()).toBe("false");

    act(() => {
      window.history.pushState({}, "", "/?at=whenever");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    expect(live()).toBe("true");
  });

  it("stops listening once unmounted", () => {
    const { unmount } = renderProbe();
    unmount();
    // No listener, no state update on an unmounted tree: React would log an
    // error, so a clean run of this assertion is the point.
    window.history.pushState({}, "", "/?at=2026-08-07T10:00:00Z");
    window.dispatchEvent(new PopStateEvent("popstate"));
    expect(window.location.search).toContain("at=");
  });
});

/* The URL is the shareable statement of what is on screen, so a param we
   ignored or clamped cannot be left standing. */
describe("?at= normalization rewrites the URL", () => {
  it("drops an unparseable ?at= rather than leaving Live under a time-shaped URL", () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    window.history.pushState({}, "", "/matrix?zone=eu-1&at=not-a-date");
    renderProbe();

    expect(live()).toBe("true");
    const params = new URLSearchParams(window.location.search);
    expect(params.has("at")).toBe(false);
    expect(params.get("zone")).toBe("eu-1");
    expect(window.location.pathname).toBe("/matrix");
  });

  it("drops an empty ?at=", () => {
    window.history.pushState({}, "", "/?at=");
    renderProbe();
    expect(new URLSearchParams(window.location.search).has("at")).toBe(false);
  });

  it("writes the CLAMPED instant back, so a reload lands on the same moment", () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    window.history.pushState({}, "", "/?at=2999-01-01T00:00:00Z");
    renderProbe();

    const param = new URLSearchParams(window.location.search).get("at");
    expect(param).not.toBe("2999-01-01T00:00:00Z");
    expect(param).toBe(formatAtParam(new Date(at()!)));
  });

  it("rewrites in place — no history entry for a correction the operator did not make", () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    window.history.pushState({}, "", "/?at=2999-01-01T00:00:00Z");
    const before = window.history.length;
    renderProbe();
    expect(window.history.length).toBe(before);
  });

  it("leaves a good ?at= byte-for-byte alone", () => {
    window.history.pushState({}, "", "/matrix?at=2026-08-07T10:00:00Z");
    renderProbe();
    expect(window.location.search).toBe("?at=2026-08-07T10:00:00Z");
  });
});

describe("useTimeMachine outside a provider", () => {
  it("throws the standard context guard", () => {
    const err = vi.spyOn(console, "error").mockImplementation(() => {});
    expect(() => render(<Probe />)).toThrow(/TimeMachineProvider/);
    err.mockRestore();
  });
});
