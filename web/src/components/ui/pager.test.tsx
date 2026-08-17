import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { Pager, usePager } from "@/components/ui/pager";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { DEFAULT_PAGE_SIZE } from "@/lib/pagination";

/**
 * The ONE pager every list in this console mounts. The arithmetic under it is
 * pinned in lib/pagination.test.ts; what this file pins is the CHROME — which
 * rows a page shows, what the caption claims, and where the reader lands when
 * the page size changes under them.
 */

/** A harness with the smallest possible surface: numbered rows and the pager. */
function Rows({
  total,
  size,
  subject,
  truncated,
}: {
  total: number;
  size?: 10 | 20 | 50 | 100;
  subject?: string;
  truncated?: boolean;
}) {
  const items = Array.from({ length: total }, (_, i) => `row-${i}`);
  const pager = usePager(items, { size });
  return (
    <div>
      <ul>
        {pager.visible.map((r) => (
          <li key={r} data-testid="row">
            {r}
          </li>
        ))}
      </ul>
      <Pager pager={pager} subject={subject} truncated={truncated} />
    </div>
  );
}

const rows = () => screen.queryAllByTestId("row").map((el) => el.textContent);

beforeEach(() => localStorage.clear());
afterEach(cleanup);

describe("usePager cuts the list the reader is actually shown", () => {
  it("shows the first page-worth and nothing else", () => {
    render(<Rows total={90} size={20} />);
    expect(rows()).toHaveLength(20);
    expect(rows()[0]).toBe("row-0");
    expect(rows()[19]).toBe("row-19");
  });

  it("walks to the next page without reshuffling the order", () => {
    render(<Rows total={90} size={20} />);
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    expect(rows()[0]).toBe("row-20");
    expect(rows()[19]).toBe("row-39");
  });

  it("stops at the last page, which holds the remainder rather than a full size", () => {
    render(<Rows total={90} size={20} />);
    const next = screen.getByRole("button", { name: "Next page" });
    for (let i = 0; i < 6; i++) fireEvent.click(next);
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 5 of 5");
    expect(rows()).toHaveLength(10);
    expect(next).toBeDisabled();
  });

  it("draws no pager at all when the whole list fits in the smallest size", () => {
    render(<Rows total={7} />);
    expect(screen.queryByTestId("pager")).not.toBeInTheDocument();
    expect(rows()).toHaveLength(7);
  });
});

describe("the caption is honest about what is on screen", () => {
  it("counts the rows drawn against the rows there are", () => {
    render(<Rows total={90} size={20} />);
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Showing 20 of 90");
  });

  it("counts the SHORT last page, not a full size-worth", () => {
    render(<Rows total={90} size={20} />);
    const next = screen.getByRole("button", { name: "Next page" });
    for (let i = 0; i < 4; i++) fireEvent.click(next);
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Showing 10 of 90");
  });

  it("names the rows when the surface passes a subject", () => {
    render(<Rows total={90} size={20} subject="pairs" />);
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Showing 20 of 90 pairs");
  });
});

describe("the size selector ANCHORS rather than resetting to page one", () => {
  it("keeps the row that was at the top of the page in view when the size grows", () => {
    render(<Rows total={200} size={10} />);
    const next = screen.getByRole("button", { name: "Next page" });
    // Page 5 of 20 at ten a page: the top row is #40.
    for (let i = 0; i < 4; i++) fireEvent.click(next);
    expect(rows()[0]).toBe("row-40");

    fireEvent.click(screen.getByRole("radio", { name: "50" }));
    // Row 40 lives on page 1 at fifty a page, and page 1 is where the reader is.
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 1 of 4");
    expect(rows()[0]).toBe("row-0");
    expect(rows()).toHaveLength(50);
  });

  it("offers the four product sizes", () => {
    render(<Rows total={200} size={10} />);
    expect(screen.getAllByRole("radio").map((el) => el.textContent)).toEqual(["10", "20", "50", "100"]);
  });
});

describe("a list that shrinks under the reader", () => {
  it("clamps onto the last page that still has rows", () => {
    const { rerender } = render(<Rows total={200} size={10} />);
    const next = screen.getByRole("button", { name: "Next page" });
    for (let i = 0; i < 9; i++) fireEvent.click(next);
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 10 of 20");

    rerender(<Rows total={25} size={10} />);
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 3 of 3");
    expect(rows()).toHaveLength(5);
  });
});

/* ── ten rows is the product's default, not each surface's opinion ────────── */

/**
 * The owner made the page size uniform: every list in the console opens at ten
 * rows and the reader asks for more if they want it. A per-surface `size:` is
 * how that rule rots — one list at 20, another at 50, and no reader can tell
 * whether the number they are looking at is a decision or an accident.
 *
 * The sweep is the open end of it: it reads every page and component through
 * import.meta.glob rather than a hand-kept list, so a surface written next
 * month is judged the day it lands.
 */
const SOURCE_MODULES = import.meta.glob("../../{pages,components}/**/*.{ts,tsx}", {
  eager: true,
  query: "?raw",
  import: "default",
}) as Record<string, string>;

const SURFACES = Object.entries(SOURCE_MODULES).filter(
  ([path]) => !path.includes(".test.") && !path.endsWith("/ui/pager.tsx"),
);

describe("ten rows is the product default", () => {
  it("shows exactly the default when the surface names no size", () => {
    render(<Rows total={90} />);
    expect(DEFAULT_PAGE_SIZE).toBe(10);
    expect(rows()).toHaveLength(10);
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 1 of 9");
  });

  it("reads every page and component, so this is a sweep and not a sample", () => {
    expect(SURFACES.length).toBeGreaterThanOrEqual(40);
  });

  it("finds no surface passing a page size of its own", () => {
    const offenders: string[] = [];
    for (const [path, source] of SURFACES) {
      let from = source.indexOf("usePager(");
      while (from !== -1) {
        // The options object is the second argument and never runs long; 240
        // characters covers the longest call on this codebase (a resetKey built
        // from two node names) without reaching the next statement.
        const call = source.slice(from, from + 240);
        if (/\bsize:/.test(call)) offenders.push(`${path}: ${call.split("\n")[0].trim()}`);
        from = source.indexOf("usePager(", from + 1);
      }
    }
    expect(offenders).toEqual([]);
  });
});

describe("Russian", () => {
  it("says the same three things in the interface language", () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    render(
      <LocaleProvider>
        <Rows total={90} size={20} />
      </LocaleProvider>,
    );
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Показано 20 из 90");
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Страница 1 из 5");
    expect(screen.getByRole("button", { name: "Следующая страница" })).toBeInTheDocument();
  });
});

/*
 * A capped listing must not be presented as a complete one.
 *
 * The walk-to-exhaustion fetcher stops after MAX_COLLECT_PAGES and reports `truncated`, and every
 * caller dropped it: the caption then read "Showing 10 of 5000 targets · Page 1 of 500" over an
 * inventory that was known to be short — the same assertion-of-completeness the fetcher exists to
 * prevent, moved one layer up.
 */
describe("a truncated listing says so", () => {
  it("marks the caption when the listing was capped", () => {
    render(<Rows total={90} size={10} subject="targets" truncated />);
    expect(screen.getByTestId("pager-truncated")).toBeInTheDocument();
  });

  it("says nothing when the listing is whole", () => {
    render(<Rows total={90} size={10} subject="targets" />);
    expect(screen.queryByTestId("pager-truncated")).toBeNull();
  });
});
