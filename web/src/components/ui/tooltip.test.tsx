import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Tooltip } from "./tooltip";

/**
 * QA scope 2, finding #23: the matrix cell tooltip landed over the very cell
 * the operator was pointing at. The bubble is portalled and positioned from a
 * measured rect, and jsdom measures everything as 0×0 — so both halves are
 * stubbed here, keyed on the element's own role, which is the only thing that
 * separates the trigger from the bubble at measure time.
 */

const BUBBLE = { width: 200, height: 120 };

function stubRects(trigger: { top: number; left: number; width: number; height: number }) {
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    const box =
      this.getAttribute("role") === "tooltip"
        ? { top: 0, left: 0, ...BUBBLE }
        : { ...trigger };
    return {
      ...box,
      right: box.left + box.width,
      bottom: box.top + box.height,
      x: box.left,
      y: box.top,
      toJSON: () => box,
    } as DOMRect;
  });
}

function open(trigger: { top: number; left: number; width: number; height: number }) {
  stubRects(trigger);
  render(
    <Tooltip content="the figures">
      <span>cell</span>
    </Tooltip>,
  );
  fireEvent.mouseEnter(screen.getByText("cell"));
  return screen.getByRole("tooltip");
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Tooltip placement", () => {
  it("keeps its preferred side when the bubble fits above the trigger", () => {
    const bubble = open({ top: 400, left: 300, width: 64, height: 48 });
    expect(bubble).toHaveAttribute("data-side", "top");
    // Bottom edge of the bubble sits a gap ABOVE the trigger, never over it.
    expect(bubble.style.top).toBe("392px");
  });

  it("FLIPS below when there is no room above — the cell stays readable", () => {
    // A first-row cell: 120px of bubble does not fit in the 10px above it.
    const bubble = open({ top: 10, left: 300, width: 64, height: 48 });
    expect(bubble).toHaveAttribute("data-side", "bottom");
    expect(bubble.style.top).toBe("66px"); // trigger bottom (58) + gap
  });

  it("clamps a first-column trigger so half the bubble does not fall off screen", () => {
    // Centred on x=32 a 200px bubble would start at -68; the clamp holds it at
    // half-width plus the margin.
    const bubble = open({ top: 400, left: 0, width: 64, height: 48 });
    expect(bubble.style.left).toBe("108px");
  });

  it("clamps a last-column trigger against the right edge the same way", () => {
    const right = window.innerWidth - 32;
    const bubble = open({ top: 400, left: right, width: 64, height: 48 });
    expect(Number.parseFloat(bubble.style.left)).toBeLessThanOrEqual(window.innerWidth - 8 - BUBBLE.width / 2);
  });
});
