import { afterEach, describe, expect, it, vi } from "vitest";
import { subscribeToLocation } from "./location";

/* QA round 3, finding #10. The three ways a URL changes without a page load,
   and the one property that matters: every one of them reaches a subscriber. */

const unsubscribes: (() => void)[] = [];

function subscribe(fn: () => void) {
  const off = subscribeToLocation(fn);
  unsubscribes.push(off);
  return off;
}

afterEach(() => {
  while (unsubscribes.length > 0) unsubscribes.pop()?.();
  window.history.replaceState({}, "", "/");
});

describe("subscribeToLocation", () => {
  it("fires on pushState — the in-app navigation popstate never reports", () => {
    const seen = vi.fn();
    subscribe(seen);
    window.history.pushState({}, "", "/investigate?kind=node&scope=node-a");
    expect(seen).toHaveBeenCalledTimes(1);
    expect(window.location.search).toBe("?kind=node&scope=node-a");
  });

  it("fires on replaceState", () => {
    const seen = vi.fn();
    subscribe(seen);
    window.history.replaceState({}, "", "/matrix?protocol=udp");
    expect(seen).toHaveBeenCalledTimes(1);
  });

  it("fires on popstate — Back and Forward", () => {
    const seen = vi.fn();
    subscribe(seen);
    window.dispatchEvent(new PopStateEvent("popstate"));
    expect(seen).toHaveBeenCalledTimes(1);
  });

  it("reaches every subscriber", () => {
    const a = vi.fn();
    const b = vi.fn();
    subscribe(a);
    subscribe(b);
    window.history.pushState({}, "", "/investigate");
    expect(a).toHaveBeenCalledTimes(1);
    expect(b).toHaveBeenCalledTimes(1);
  });

  it("restores the original history methods once the last subscriber leaves", () => {
    const before = window.history.pushState;
    const off = subscribeToLocation(() => {});
    expect(window.history.pushState).not.toBe(before);
    off();
    expect(window.history.pushState).toBe(before);
  });

  it("keeps the wrapper while a second subscriber is still listening", () => {
    const seen = vi.fn();
    const first = subscribeToLocation(() => {});
    subscribe(seen);
    first();
    window.history.pushState({}, "", "/investigate?kind=cluster");
    expect(seen).toHaveBeenCalledTimes(1);
  });

  it("still performs the navigation it is wrapping", () => {
    subscribe(() => {});
    window.history.pushState({}, "", "/investigate?kind=pair&scope=a");
    expect(window.location.pathname).toBe("/investigate");
    expect(window.location.search).toBe("?kind=pair&scope=a");
  });
});
