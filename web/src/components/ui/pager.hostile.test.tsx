import { useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { Pager, usePager } from "@/components/ui/pager";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { PAGE_SIZES } from "@/lib/pagination";

/**
 * The pager, clicked the way an operator clicks it: every arrow to its end,
 * every size at every boundary, and the list replaced underneath while a page
 * of it is on screen.
 *
 * components/ui/pager.test.tsx pins what the control DOES. This file pins what
 * it must never do — leave a reader on a blank page, claim a count no row backs,
 * or land them somewhere they did not ask to be after a filter changed.
 */

function Rows({ total, subject, resetKey }: { total: number; subject?: string; resetKey?: string }) {
  const items = Array.from({ length: total }, (_, i) => `row-${i}`);
  const pager = usePager(items, { resetKey });
  return (
    <div>
      <ul>
        {pager.visible.map((r) => (
          <li key={r} data-testid="row">
            {r}
          </li>
        ))}
      </ul>
      <Pager pager={pager} subject={subject} />
    </div>
  );
}

/** A list whose LENGTH changes without the component remounting — "Load older"
 *  growing it, a filter cutting it, the Time Machine slicing it. */
function ShrinkingRows({ from, to, resetKey }: { from: number; to: number; resetKey?: boolean }) {
  const [total, setTotal] = useState(from);
  const [key, setKey] = useState("a");
  const items = Array.from({ length: total }, (_, i) => `row-${i}`);
  const pager = usePager(items, { resetKey: resetKey ? key : undefined });
  return (
    <div>
      <button
        type="button"
        onClick={() => {
          setTotal(to);
          setKey("b");
        }}
      >
        replace
      </button>
      <ul>
        {pager.visible.map((r) => (
          <li key={r} data-testid="row">
            {r}
          </li>
        ))}
      </ul>
      <Pager pager={pager} />
    </div>
  );
}

const rows = () => screen.queryAllByTestId("row").map((el) => el.textContent);
const next = () => screen.getByRole("button", { name: "Next page" });
const prev = () => screen.getByRole("button", { name: "Previous page" });

beforeEach(() => localStorage.clear());
afterEach(cleanup);

/* ── the arrows, held down ────────────────────────────────────────────────── */

describe("clicking past the ends", () => {
  it("cannot be walked off the back of the list", () => {
    render(<Rows total={95} />);
    for (let i = 0; i < 40; i++) fireEvent.click(next());
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 10 of 10");
    // The remainder page, and it is not empty — five rows, all real.
    expect(rows()).toEqual(["row-90", "row-91", "row-92", "row-93", "row-94"]);
    expect(next()).toBeDisabled();
  });

  it("cannot be walked off the front of it either", () => {
    render(<Rows total={95} />);
    for (let i = 0; i < 5; i++) fireEvent.click(next());
    for (let i = 0; i < 40; i++) fireEvent.click(prev());
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 1 of 10");
    expect(rows()[0]).toBe("row-0");
    expect(prev()).toBeDisabled();
  });

  it("never shows a page with no rows on it, at any page of any size", () => {
    for (const total of [11, 12, 20, 21, 100, 101]) {
      cleanup();
      render(<Rows total={total} />);
      for (const size of PAGE_SIZES) {
        fireEvent.click(screen.getByRole("radio", { name: String(size) }));
        // Walk the whole list at this size and check every page carries rows.
        fireEvent.click(screen.getByRole("button", { name: "Previous page" }));
        while (!next().hasAttribute("disabled")) {
          fireEvent.click(next());
          expect(rows().length, `total=${total} size=${size}`).toBeGreaterThan(0);
        }
      }
    }
  });
});

/* ── the size selector, at every boundary ─────────────────────────────────── */

describe("the size switch keeps the reader where they were", () => {
  it("keeps the first visible row on screen for every size pairing", () => {
    for (const from of PAGE_SIZES) {
      for (const to of PAGE_SIZES) {
        cleanup();
        render(<Rows total={457} />);
        fireEvent.click(screen.getByRole("radio", { name: String(from) }));
        // Two pages in, so the anchor is never trivially row 0.
        fireEvent.click(next());
        fireEvent.click(next());
        const anchor = rows()[0];
        fireEvent.click(screen.getByRole("radio", { name: String(to) }));
        expect(rows(), `${from} → ${to} lost row ${anchor}`).toContain(anchor);
      }
    }
  });

  it("lands on a page that exists when the size grows past the whole list", () => {
    render(<Rows total={45} />);
    for (let i = 0; i < 4; i++) fireEvent.click(next());
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 5 of 5");
    fireEvent.click(screen.getByRole("radio", { name: "100" }));
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 1 of 1");
    expect(rows()).toHaveLength(45);
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Showing 45 of 45");
    expect(next()).toBeDisabled();
    expect(prev()).toBeDisabled();
  });

  it("counts the short last page honestly at every size", () => {
    render(<Rows total={45} />);
    for (const size of PAGE_SIZES) {
      fireEvent.click(screen.getByRole("radio", { name: String(size) }));
      while (!next().hasAttribute("disabled")) fireEvent.click(next());
      const shown = rows().length;
      expect(screen.getByTestId("pager-showing"), `size=${size}`).toHaveTextContent(`Showing ${shown} of 45`);
    }
  });
});

/* ── a list that is exactly one page, and one that is none ────────────────── */

describe("the degenerate lists", () => {
  it("draws nothing at all for an empty list", () => {
    render(<Rows total={0} />);
    expect(screen.queryByTestId("pager")).not.toBeInTheDocument();
    expect(rows()).toEqual([]);
  });

  it("draws nothing for exactly the floor, and everything above it", () => {
    render(<Rows total={10} />);
    expect(screen.queryByTestId("pager")).not.toBeInTheDocument();
    expect(rows()).toHaveLength(10);
    cleanup();
    render(<Rows total={11} />);
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 1 of 2");
    fireEvent.click(next());
    expect(rows()).toEqual(["row-10"]);
  });
});

/* ── the list changing under the page ─────────────────────────────────────── */

describe("the list is replaced while a page of it is on screen", () => {
  it("clamps onto a page that still has rows rather than going blank", () => {
    render(<ShrinkingRows from={200} to={12} />);
    for (let i = 0; i < 9; i++) fireEvent.click(next());
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 10 of 20");
    fireEvent.click(screen.getByRole("button", { name: "replace" }));
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 2 of 2");
    expect(rows()).toEqual(["row-10", "row-11"]);
  });

  it("shows the whole of a list that shrank below the floor, with no chrome left to explain it", () => {
    render(<ShrinkingRows from={200} to={4} />);
    for (let i = 0; i < 9; i++) fireEvent.click(next());
    fireEvent.click(screen.getByRole("button", { name: "replace" }));
    expect(screen.queryByTestId("pager")).not.toBeInTheDocument();
    expect(rows()).toEqual(["row-0", "row-1", "row-2", "row-3"]);
  });

  it("goes back to page one when the surface says these are DIFFERENT rows", () => {
    // What a filter change is, and the whole reason usePager takes a resetKey:
    // clamping is right for a list that grew or shrank, and wrong for one that
    // was replaced — page 10 of the old list addresses nothing in the new one.
    render(<ShrinkingRows from={200} to={120} resetKey />);
    for (let i = 0; i < 9; i++) fireEvent.click(next());
    fireEvent.click(screen.getByRole("button", { name: "replace" }));
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 1 of 12");
    expect(rows()[0]).toBe("row-0");
  });

  it("stays put when the SAME list merely grew — a page appended is not a page moved", () => {
    render(<ShrinkingRows from={95} to={200} />);
    for (let i = 0; i < 3; i++) fireEvent.click(next());
    expect(rows()[0]).toBe("row-30");
    fireEvent.click(screen.getByRole("button", { name: "replace" }));
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 4 of 20");
    expect(rows()[0]).toBe("row-30");
  });
});

/* ── the caption ─────────────────────────────────────────────────────────── */

describe("the caption never claims a row that is not there", () => {
  it("matches the rendered row count on every page of every size", () => {
    render(<Rows total={137} subject="pairs" />);
    for (const size of PAGE_SIZES) {
      fireEvent.click(screen.getByRole("radio", { name: String(size) }));
      let guard = 0;
      for (;;) {
        expect(screen.getByTestId("pager-showing")).toHaveTextContent(`Showing ${rows().length} of 137 pairs`);
        if (next().hasAttribute("disabled") || guard++ > 30) break;
        fireEvent.click(next());
      }
    }
  });

  it("says it in Russian too, with the subject in front of the numbers", () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    render(
      <LocaleProvider>
        <Rows total={137} subject="Пары" />
      </LocaleProvider>,
    );
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Пары: показано 10 из 137");
    fireEvent.click(screen.getByRole("radio", { name: "50" }));
    fireEvent.click(screen.getByRole("button", { name: "Следующая страница" }));
    fireEvent.click(screen.getByRole("button", { name: "Следующая страница" }));
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Страница 3 из 3");
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Пары: показано 37 из 137");
  });
});
