import { render, screen, cleanup, act, fireEvent, waitFor } from "@testing-library/react";
import {
  createBrowserHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Link,
  Outlet,
  RouterProvider,
  useRouterState,
} from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AtParamSync, formatAtParam, TimeMachineProvider, useTimeMachine, withAtParam } from "@/lib/timemachine";

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

/*
The time context is console-wide and survives a route change on purpose; the URL did not, because
the router builds the next URL from the route alone. The address bar then read Live while every
read still resolved at `t`, and a link copied from that page opened a different console.

These cases drive a REAL TanStack router over REAL browser history and click REAL <Link>s. The
first version of this suite did `pushState(...)` and THEN `rerender(...)`, which is the opposite of
what the router does — it writes the URL in a queued microtask AFTER notifying React. That fake
order made the suite green over a component that never wrote anything on a link click.
*/
describe("AtParamSync", () => {
  /* These cases declare their OWN two-route tree; the app's global router type
     (the Register interface in routes.tsx) knows nothing about it, so the two
     literal paths are cast past a union that describes a different router. */
  const to = (path: string) => path as never;

  /** The chrome's own wiring: the sync fed by the router's full href, the way AppShell feeds it. */
  function Shell() {
    const href = useRouterState({ select: (s) => s.location.href });
    return (
      <TimeMachineProvider>
        <AtParamSync href={href} />
        <nav>
          <Link to={to("/matrix")}>to matrix</Link>
          <Link to={to("/overview")}>to overview</Link>
        </nav>
        <Outlet />
      </TimeMachineProvider>
    );
  }

  async function renderRouter(initial: string) {
    window.history.replaceState(null, "", initial);
    const root = createRootRoute({ component: Shell });
    const tree = root.addChildren([
      createRoute({ getParentRoute: () => root, path: "/matrix", component: () => <div>matrix page</div> }),
      createRoute({ getParentRoute: () => root, path: "/overview", component: () => <div>overview page</div> }),
    ]);
    const testRouter = createRouter({ routeTree: tree, history: createBrowserHistory() });
    const view = render(<RouterProvider router={testRouter} />);
    await screen.findByRole("link", { name: "to overview" });
    return view;
  }

  const atParam = () => new URLSearchParams(window.location.search).get("at");
  const click = (name: string) => fireEvent.click(screen.getByRole("link", { name }));

  it("keeps ?at= across a link click to another page", async () => {
    await renderRouter("/matrix?at=2026-08-07T10:00:00Z");
    expect(atParam()).toBe("2026-08-07T10:00:00Z");

    click("to overview");

    await screen.findByText("overview page");
    await waitFor(() => expect(window.location.pathname).toBe("/overview"));
    await waitFor(() => expect(atParam()).toBe("2026-08-07T10:00:00Z"));
  });

  /* The router compares FULL hrefs, so a link back to the page you are on is a
     real push — and a pathname-keyed effect could not even see it happen. */
  it("keeps ?at= across a link click to the page already open", async () => {
    await renderRouter("/matrix?at=2026-08-07T10:00:00Z&protocol=icmp");

    click("to matrix");

    await waitFor(() => expect(window.location.pathname).toBe("/matrix"));
    await waitFor(() => expect(atParam()).toBe("2026-08-07T10:00:00Z"));
  });

  it("adds nothing at all while Live", async () => {
    await renderRouter("/matrix");

    click("to overview");

    await screen.findByText("overview page");
    await waitFor(() => expect(window.location.pathname).toBe("/overview"));
    expect(atParam()).toBeNull();
  });

  /* replaceState, not push: the reader did not make this correction and must
     not have to press Back through one extra entry per navigation. */
  it("corrects the URL in place, and keeps the router's own history state", async () => {
    await renderRouter("/matrix?at=2026-08-07T10:00:00Z");
    const before = window.history.length;
    const stateBefore = window.history.state;

    click("to overview");
    await waitFor(() => expect(atParam()).toBe("2026-08-07T10:00:00Z"));

    // One entry for the router's push, none for our correction.
    expect(window.history.length).toBe(before + 1);
    // The router keeps its entry index in history.state; a `{}` would wipe it.
    expect(window.history.state).not.toBeNull();
    expect(typeof window.history.state).toBe(typeof stateBefore);
  });

  it("survives going Back to another engaged page, rather than dropping into Live", async () => {
    await renderRouter("/matrix?at=2026-08-07T10:00:00Z");
    click("to overview");
    await screen.findByText("overview page");
    await waitFor(() => expect(atParam()).toBe("2026-08-07T10:00:00Z"));

    window.history.back();

    await waitFor(() => expect(window.location.pathname).toBe("/matrix"));
    await waitFor(() => expect(atParam()).toBe("2026-08-07T10:00:00Z"));
  });

  /*
  NOT PINNED HERE, on purpose. Engaging PUSHES an entry, so Back is how the reader leaves the past —
  and the sync used to re-stamp the instant React was still holding onto the Live entry it landed
  on, with replaceState: the console stayed engaged AND that entry was destroyed, so Back could
  never leave the Time Machine again. It happens because the queued write runs BEFORE the
  provider's own popstate handler, and that ordering is the browser's, not jsdom's — here the
  provider wins the race and the write is a no-op either way, so a test asserting the outcome
  passes with and without the guard. Writing one would only prove jsdom's ordering.

  The guard is in lib/timemachine.tsx (`traversing`), and it is verified on the running stand:
  engage, press Back, and the address bar and the banner both return to Live.
  */

  it("pushes the NEXT history index rather than erasing the router's own bookkeeping", () => {
    window.history.replaceState({ __TSR_index: 4, key: "abc" }, "", "/matrix");
    const view = render(
      <TimeMachineProvider>
        <Probe />
      </TimeMachineProvider>,
    );
    act(() => void screen.getByText("engage").click());

    const state = window.history.state as { __TSR_index?: number; key?: string };
    expect(state.__TSR_index).toBe(5);
    // Everything else the router put there travels with it.
    expect(state.key).toBe("abc");
    view.unmount();
  });
});

/*
Six in-app links are plain anchors rather than router <Link>s, and a raw anchor is a full document
load: the provider unmounts, the fresh page reads no ?at=, and the reader was silently dropped back
into Live in the middle of an investigation. The instant travels with the href instead.
*/
describe("withAtParam", () => {
  it("adds nothing while Live", () => {
    window.history.pushState({}, "", "/overview");
    expect(withAtParam("/live")).toBe("/live");
  });

  it("carries the engaged instant onto a plain path", () => {
    window.history.pushState({}, "", "/overview?at=2026-08-07T10:00:00Z");
    expect(new URLSearchParams(withAtParam("/live").split("?")[1]).get("at")).toBe("2026-08-07T10:00:00Z");
  });

  it("keeps the link's own query and hash", () => {
    window.history.pushState({}, "", "/overview?at=2026-08-07T10:00:00Z");
    const href = withAtParam("/matrix?protocol=icmp#zone");
    const params = new URLSearchParams(href.split("?")[1].split("#")[0]);
    expect(href.startsWith("/matrix?")).toBe(true);
    expect(params.get("protocol")).toBe("icmp");
    expect(params.get("at")).toBe("2026-08-07T10:00:00Z");
    expect(href.endsWith("#zone")).toBe(true);
  });

  /* A ?at= the console itself would refuse is not one to hand onwards. */
  it("adds nothing for a URL whose ?at= is not a real instant", () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    window.history.pushState({}, "", "/overview?at=yesterday");
    expect(withAtParam("/live")).toBe("/live");
  });
});
