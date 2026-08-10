import { cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/**
 * The one mount point every chart in this console goes through.
 *
 * echarts itself is stubbed: jsdom implements no 2d canvas context, so a real
 * echarts.init throws, which is why every page test in this repo mocks the
 * component wholesale. This file mocks the LIBRARY instead, which leaves the
 * component's own wiring — init, setOption, resize, dispose — observable.
 */

const resize = vi.fn();
const setOption = vi.fn();
const dispose = vi.fn();
const init = vi.fn(() => ({ resize, setOption, dispose }));

vi.mock("echarts", () => ({ init: (...args: unknown[]) => init(...(args as [])) }));

/** The observers the component constructs, so a test can fire one by hand. */
const observers: { cb: () => void; targets: Element[] }[] = [];

beforeEach(() => {
  observers.length = 0;
  vi.clearAllMocks();
  // vitest.setup.ts installs a no-op double; this one records, so the callback
  // the component registered can actually be invoked.
  Object.defineProperty(globalThis, "ResizeObserver", {
    configurable: true,
    value: class {
      private entry: { cb: () => void; targets: Element[] };
      constructor(cb: () => void) {
        this.entry = { cb, targets: [] };
        observers.push(this.entry);
      }
      observe(el: Element) {
        this.entry.targets.push(el);
      }
      unobserve() {}
      disconnect() {
        observers.splice(observers.indexOf(this.entry), 1);
      }
    },
  });
});

afterEach(cleanup);

const { EChart } = await import("@/components/echart");

describe("EChart resizes with its CONTAINER, not just with the window (QA scope 3, finding #12)", () => {
  it("observes the host element it drew into", () => {
    const { container } = render(<EChart option={{}} />);
    expect(observers.length).toBe(1);
    expect(observers[0].targets[0]).toBe(container.firstChild);
  });

  it("resizes the chart when the box changes with the viewport standing still", () => {
    render(<EChart option={{}} />);
    resize.mockClear();
    // A sidebar collapsing, a rail wrapping, the grid dropping from two columns
    // to one at lg — none of these fire a window resize, and every one of them
    // used to leave the canvas drawn at its old width inside a moved box.
    observers[0].cb();
    expect(resize).toHaveBeenCalledTimes(1);
  });

  it("still answers a window resize — the two are belt and braces, not a swap", () => {
    render(<EChart option={{}} />);
    resize.mockClear();
    window.dispatchEvent(new Event("resize"));
    expect(resize).toHaveBeenCalledTimes(1);
  });

  it("disconnects the observer and disposes the chart on unmount", () => {
    const { unmount } = render(<EChart option={{}} />);
    unmount();
    expect(observers.length).toBe(0);
    expect(dispose).toHaveBeenCalledTimes(1);
  });

  it("mounts at all where ResizeObserver does not exist", () => {
    // Older embedded browsers, and any environment that never defined it: the
    // window listener alone must still carry the chart.
    Object.defineProperty(globalThis, "ResizeObserver", { configurable: true, value: undefined });
    expect(() => render(<EChart option={{}} />)).not.toThrow();
  });
});
