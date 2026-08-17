import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Modal } from "@/components/ui/modal";

afterEach(cleanup);

/**
 * modal.test.tsx — the dialog's WAI-ARIA contract, and the layout invariant that keeps its own
 * controls reachable.
 *
 * The layout half is asserted through class names, which this repo otherwise avoids. jsdom has no
 * layout engine: it cannot tell that a panel overflowed its viewport, so the only thing a test can
 * pin here is the STRUCTURE that decides whether it can. That structure is exactly what regressed —
 * a dialog taller than the screen scrolled its own header out of reach, taking the title and the
 * Close button with it (owner report, a route with fifty traces).
 */

function open(children: React.ReactNode = <p>body</p>, onClose = vi.fn()) {
  render(
    <Modal open onClose={onClose} title="Trace detail" description="node-a → node-b" footer={<button>ok</button>}>
      {children}
    </Modal>,
  );
  return onClose;
}

describe("Modal — the dialog contract", () => {
  it("is a labelled, described, modal dialog", () => {
    open();
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(dialog).toHaveAccessibleName("Trace detail");
    expect(dialog).toHaveAccessibleDescription("node-a → node-b");
  });

  it("closes on Escape and on a backdrop click", () => {
    const onClose = open();
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByTestId("modal-backdrop"));
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it("takes focus itself on open, so the title is read before any control", () => {
    open();
    expect(screen.getByRole("dialog")).toHaveFocus();
  });

  it("stops the page behind it from scrolling, and gives it back on close", () => {
    const { unmount } = render(
      <Modal open onClose={() => {}} title="t">
        <p>body</p>
      </Modal>,
    );
    expect(document.body.style.overflow).toBe("hidden");
    unmount();
    expect(document.body.style.overflow).not.toBe("hidden");
  });
});

describe("Modal — the layout that keeps its own controls reachable", () => {
  /* `position: fixed` is relative to the nearest ancestor with a transform, filter or perspective —
     NOT to the viewport. The page shell animates in under `.page-enter`, which holds a transform
     while it runs, so a dialog rendered inside the page anchored itself to the page: a tall one hung
     past the bottom of the screen with its header pushed off the top, and neither the title nor the
     Close button was reachable. At the top of the document there is nothing to be relative to. */
  it("renders at the top of the document, not inside whatever opened it", () => {
    const { container } = render(
      <Modal open onClose={() => {}} title="Trace detail">
        <p>body</p>
      </Modal>,
    );
    const dialog = screen.getByRole("dialog");
    expect(container.contains(dialog)).toBe(false);
    expect(dialog.closest("body")).toBe(document.body);
  });


  it("bounds its OWN height and lays itself out as a column", () => {
    open();
    const dialog = screen.getByRole("dialog");
    /* Without a ceiling of its own the panel grows past the viewport, and a centred flex child that
       overflows does so in BOTH directions: the top ends up above the scroll origin, unreachable at
       any scroll position. dvh, not vh — a mobile browser's vh is the address-bar-collapsed height,
       which is taller than what is actually on screen. */
    expect(dialog.className).toMatch(/max-h-\[calc\(100dvh/);
    expect(dialog.className).toMatch(/\bflex\b/);
    expect(dialog.className).toMatch(/\bflex-col\b/);
  });

  it("does not put the scrollbar on the overlay, which is what used to swallow the header", () => {
    open();
    const overlay = screen.getByTestId("modal-backdrop").parentElement;
    expect(overlay?.className).not.toMatch(/overflow-y-auto/);
  });

  it("scrolls the BODY only — the header and footer are its siblings, not its content", () => {
    open(<p data-testid="long-content">body</p>);
    const dialog = screen.getByRole("dialog");
    const scroller = screen.getByTestId("long-content").parentElement;

    expect(scroller?.className).toMatch(/overflow-auto/);
    // min-h-0 is what lets a flex item shrink below its content; without it the overflow moves back
    // up to the panel and the header goes with it.
    expect(scroller?.className).toMatch(/min-h-0/);

    // The title and the Close button live OUTSIDE the scrolling box, so no amount of content can
    // carry them off the screen.
    const title = screen.getByRole("heading", { name: "Trace detail" });
    const close = screen.getByRole("button", { name: /close/i });
    expect(scroller?.contains(title)).toBe(false);
    expect(scroller?.contains(close)).toBe(false);
    expect(dialog.contains(title)).toBe(true);
    expect(dialog.contains(close)).toBe(true);
  });

  it("keeps the header and footer from being squeezed by a tall body", () => {
    open();
    const title = screen.getByRole("heading", { name: "Trace detail" });
    const header = title.closest("div")?.parentElement;
    const footer = screen.getByRole("button", { name: "ok" }).parentElement;
    expect(header?.className).toMatch(/shrink-0/);
    expect(footer?.className).toMatch(/shrink-0/);
  });
});
