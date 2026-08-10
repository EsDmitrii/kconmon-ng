import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { InvestigationTimeline, type SourceNote } from "./investigation-timeline";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { TIMELINE_DEFAULT_PAGE_SIZE, type TimelineEntry } from "@/lib/investigation";

/**
 * investigation-timeline.test.tsx — the centre pane's PAGINATION, pinned at the
 * component rather than through the page.
 *
 * The page test owns the honesty rules that depend on real sources (which
 * caption appears for which permission); everything here is arithmetic and
 * chrome, and 137 entries are a prop rather than nine mocked fetches.
 */

const T0 = Date.parse("2026-08-08T00:00:00.000Z");

/** `n` rows, one minute apart, titled entry-000 … so a test can name the exact
 *  row it expects at the top of a page. */
function entries(n: number): TimelineEntry[] {
  return Array.from({ length: n }, (_, i) => ({
    at: new Date(T0 + i * 60_000),
    kind: "event" as const,
    severity: "info" as const,
    title: `entry-${String(i).padStart(3, "0")}`,
    ref: { kind: "event" as const, id: `e-${i}` },
  }));
}

/* The three captions the owner pinned as load-bearing, in miniature: they are
   ordinary notes as far as this component is concerned, which is exactly why a
   pager must not be able to scroll them away. */
const NOTES: SourceNote[] = [
  { id: "audit-window", text: "Config changes come from the newest 200 audit rows filtered to this window here." },
  { id: "runs-window", text: "Runs are the newest 100, narrowed to this window." },
  { id: "alerts-live", text: "Alerts are the set firing NOW; resolutions are not recorded." },
];

function renderTimeline(over: Partial<Parameters<typeof InvestigationTimeline>[0]> = {}) {
  const props = {
    entries: entries(137),
    notes: NOTES,
    loading: false,
    /* Required rather than defaulted on the component (QA scope 3, finding #1):
       "no source failed" is a claim, and a component that assumes it silently is
       how the honest-empty copy came to be drawn over six dead requests. */
    allFailed: false,
    cursorAt: null,
    onCursor: () => {},
    windowKey: "pair|node-a|node-b|from|to",
    ...over,
  };
  return { ...render(<InvestigationTimeline {...props} />), props };
}

const rowTitles = () =>
  screen.getAllByTestId("timeline-row").map((r) => (r.textContent ?? "").match(/entry-\d{3}/)?.[0] ?? "");

const pageLabel = () => screen.getByTestId("timeline-page-label").textContent ?? "";
const sizeControl = () => screen.getByRole("radiogroup", { name: "Rows per page" });
const setSize = (n: number) => fireEvent.click(within(sizeControl()).getByRole("radio", { name: String(n) }));
const prev = () => screen.getByRole("button", { name: "Previous page" });
const next = () => screen.getByRole("button", { name: "Next page" });

afterEach(() => {
  cleanup();
  /* vitest.setup.ts backs localStorage with one Map per test FILE — a locale
     left behind would flip every later case in this one. */
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

describe("InvestigationTimeline — pagination", () => {
  it("shows the default page size and says which page of how many", () => {
    renderTimeline();
    expect(TIMELINE_DEFAULT_PAGE_SIZE).toBe(50);
    expect(rowTitles()).toHaveLength(50);
    expect(rowTitles()[0]).toBe("entry-000");
    expect(rowTitles()[49]).toBe("entry-049");
    expect(pageLabel()).toMatch(/page 1 of 3/i);
  });

  it("pages forward and back over the same merged order", () => {
    renderTimeline();
    fireEvent.click(next());
    expect(rowTitles()[0]).toBe("entry-050");
    expect(rowTitles()).toHaveLength(50);
    expect(pageLabel()).toMatch(/page 2 of 3/i);

    fireEvent.click(next());
    expect(rowTitles()[0]).toBe("entry-100");
    // The last page is the REMAINDER, not a padded 50.
    expect(rowTitles()).toHaveLength(37);
    expect(rowTitles()[36]).toBe("entry-136");
    expect(pageLabel()).toMatch(/page 3 of 3/i);

    fireEvent.click(prev());
    expect(rowTitles()[0]).toBe("entry-050");
    expect(pageLabel()).toMatch(/page 2 of 3/i);
  });

  it("disables the pager at both boundaries rather than wrapping around", () => {
    renderTimeline();
    expect((prev() as HTMLButtonElement).disabled).toBe(true);
    expect((next() as HTMLButtonElement).disabled).toBe(false);

    fireEvent.click(next());
    fireEvent.click(next());
    expect(pageLabel()).toMatch(/page 3 of 3/i);
    expect((prev() as HTMLButtonElement).disabled).toBe(false);
    expect((next() as HTMLButtonElement).disabled).toBe(true);

    // A click on the disabled edge is not a wrap to page 1.
    fireEvent.click(next());
    expect(pageLabel()).toMatch(/page 3 of 3/i);
  });

  it("offers 10 / 50 / 100 and re-cuts the list on the spot", () => {
    renderTimeline();
    expect(within(sizeControl()).getAllByRole("radio").map((b) => b.textContent)).toEqual(["10", "50", "100"]);

    setSize(10);
    expect(rowTitles()).toHaveLength(10);
    expect(pageLabel()).toMatch(/page 1 of 14/i);

    setSize(100);
    expect(rowTitles()).toHaveLength(100);
    expect(pageLabel()).toMatch(/page 1 of 2/i);
  });

  /* DECISION (pinned): a page-size change ANCHORS on the first visible entry
     instead of resetting to page 1. The reader paged down for a reason; growing
     the page and throwing them back to the newest rows loses the position they
     worked to find, and the arithmetic to keep it is one floor division. */
  it("keeps the first visible entry in view when the page size changes", () => {
    renderTimeline();
    setSize(10);
    for (let i = 0; i < 7; i++) fireEvent.click(next()); // page 8 → first row is entry-070
    expect(rowTitles()[0]).toBe("entry-070");

    setSize(50);
    // NOT page 1: entry-070 lives on page 2 of a 50-row cut, and there it is.
    expect(pageLabel()).toMatch(/page 2 of 3/i);
    expect(rowTitles()).toContain("entry-070");
    expect(rowTitles()[0]).toBe("entry-050");

    setSize(10);
    // Anchoring is symmetric: the first visible row (entry-050) leads again.
    expect(pageLabel()).toMatch(/page 6 of 14/i);
    expect(rowTitles()[0]).toBe("entry-050");
  });

  /* DECISION (pinned): the scope/window identity resets the page. A new window
     is a new list; page 3 of the previous one addresses nothing here. */
  it("resets to page 1 when the window key changes", () => {
    const { rerender, props } = renderTimeline();
    fireEvent.click(next());
    expect(pageLabel()).toMatch(/page 2 of 3/i);

    rerender(<InvestigationTimeline {...props} windowKey="pair|node-a|node-c|from|to" />);
    expect(pageLabel()).toMatch(/page 1 of 3/i);
    expect(rowTitles()[0]).toBe("entry-000");
  });

  it("does NOT reset the page for a re-render that leaves the window alone", () => {
    const { rerender, props } = renderTimeline();
    fireEvent.click(next());
    rerender(<InvestigationTimeline {...props} loading={true} />);
    expect(pageLabel()).toMatch(/page 2 of 3/i);
  });

  /* A live-shifting list: the sources re-fetch and 137 rows become 22 while the
     reader sits on page 3. Clamping shows rows; honouring the stale number
     shows an empty box under a header that just counted 22 entries. */
  it("clamps to the last page when the list shrinks underneath the reader", () => {
    const { rerender, props } = renderTimeline();
    fireEvent.click(next());
    fireEvent.click(next());
    expect(pageLabel()).toMatch(/page 3 of 3/i);

    rerender(<InvestigationTimeline {...props} entries={entries(22)} />);
    expect(pageLabel()).toMatch(/page 1 of 1/i);
    expect(rowTitles()).toHaveLength(22);
  });

  it("steps from the CLAMPED page, so prev after a shrink goes somewhere real", () => {
    const { rerender, props } = renderTimeline();
    setSize(10);
    for (let i = 0; i < 9; i++) fireEvent.click(next()); // page 10 of 14
    expect(pageLabel()).toMatch(/page 10 of 14/i);

    rerender(<InvestigationTimeline {...props} entries={entries(35)} />);
    expect(pageLabel()).toMatch(/page 4 of 4/i); // clamped
    fireEvent.click(prev());
    expect(pageLabel()).toMatch(/page 3 of 4/i); // …and prev stepped from 4, not from 10
  });

  it("shows no pagination chrome at all for a list the smallest size already fits", () => {
    renderTimeline({ entries: entries(10) });
    expect(screen.queryByTestId("timeline-page-label")).toBeNull();
    expect(screen.queryByRole("radiogroup", { name: "Rows per page" })).toBeNull();
    expect(rowTitles()).toHaveLength(10);
  });

  it("shows the chrome as soon as one size can differ from another", () => {
    renderTimeline({ entries: entries(11) });
    expect(screen.getByTestId("timeline-page-label").textContent).toMatch(/page 1 of 1/i);
    expect(sizeControl()).toBeTruthy();
    // 11 rows still fit the default 50 — both edges are dead, and say so.
    expect((prev() as HTMLButtonElement).disabled).toBe(true);
    expect((next() as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("InvestigationTimeline — the honesty rules survive paging", () => {
  it("counts the WINDOW, not the page", () => {
    renderTimeline();
    expect(screen.getByTestId("timeline-count").textContent).toMatch(/137 entries/);
    fireEvent.click(next());
    expect(screen.getByTestId("timeline-count").textContent).toMatch(/137 entries/);
    setSize(10);
    expect(screen.getByTestId("timeline-count").textContent).toMatch(/137 entries/);
    expect(rowTitles()).toHaveLength(10);
  });

  it("keeps every source caption above the rows on EVERY page", () => {
    renderTimeline();
    const captions = () =>
      within(screen.getByRole("list", { name: "Timeline sources" }))
        .getAllByRole("listitem")
        .map((li) => li.textContent ?? "");

    for (const page of [1, 2, 3]) {
      if (page > 1) fireEvent.click(next());
      const texts = captions().join(" ");
      expect(texts).toContain("newest 200 audit rows");
      expect(texts).toContain("Runs are the newest 100");
      expect(texts).toContain("firing NOW");
    }
  });

  it("keeps the partial-sources banner on every page — a page is not a fresh claim", () => {
    renderTimeline({
      notes: [...NOTES, { id: "events", text: "Cluster events: 500 upstream.", failed: true }],
    });
    expect(screen.getByTestId("timeline-partial").textContent).toMatch(/1 source failed/i);
    fireEvent.click(next());
    expect(screen.getByTestId("timeline-partial").textContent).toMatch(/1 source failed/i);
  });

  it("still says nothing happened for an empty window, with no pager over it", () => {
    renderTimeline({ entries: [] });
    expect(screen.getByText(/Nothing happened in this window/i)).toBeTruthy();
    expect(screen.queryByTestId("timeline-page-label")).toBeNull();
    /* The count STAYS at nought (QA scope 3, finding #15). It used to vanish,
       which left "did it count and find nothing, or never count at all?" for the
       reader to work out from the absence of a number. */
    expect(screen.getByTestId("timeline-count").textContent).toBe("0 entries in this window");
  });

  it("still counts while a REFETCH is in flight, and only hides the number on the first load", () => {
    // A number over rows that are already on screen is a count; a number over a
    // pane that has never fetched would be a claim.
    renderTimeline({ entries: [], loading: true });
    expect(screen.queryByTestId("timeline-count")).toBeNull();
  });
});

describe("InvestigationTimeline — pager accessibility", () => {
  it("is two real buttons and a labelled selector, all keyboard-reachable", () => {
    renderTimeline();
    expect(prev().tagName).toBe("BUTTON");
    expect(next().tagName).toBe("BUTTON");
    expect(prev().getAttribute("type")).toBe("button");
    expect(next().getAttribute("type")).toBe("button");
    expect(sizeControl().getAttribute("aria-label")).toBe("Rows per page");

    // The live region is what a screen reader hears after a page click.
    expect(screen.getByTestId("timeline-page-label").getAttribute("aria-live")).toBe("polite");
  });

  it("moves the page size with the arrow keys, like every other segmented control here", () => {
    renderTimeline();
    const fifty = within(sizeControl()).getByRole("radio", { name: "50" });
    fifty.focus();
    fireEvent.keyDown(fifty, { key: "ArrowRight" });
    expect(rowTitles()).toHaveLength(100);
  });
});

/* ── the Russian is wired ────────────────────────────────────────────────────
   ONE smoke pin. Every case above renders with no <LocaleProvider>, which
   lib/i18n defines as English, so none of them moved. This one proves the
   pane's three count-bearing strings — the window count, the partial banner
   and the pager label — come from dict/investigate.ts, and that the Russian
   picks the right one of its three plural forms. */
describe("InvestigationTimeline — Russian", () => {
  function renderRu(over: Partial<Parameters<typeof InvestigationTimeline>[0]> = {}) {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    const props = {
      entries: entries(137),
      notes: NOTES,
      loading: false,
      allFailed: false,
      cursorAt: null,
      onCursor: () => {},
      windowKey: "pair|node-a|node-b|from|to",
      ...over,
    };
    return render(
      <LocaleProvider>
        <InvestigationTimeline {...props} />
      </LocaleProvider>,
    );
  }

  it("counts, pages and warns about failed sources in Russian", () => {
    renderRu({ notes: [...NOTES, { id: "boom", text: "События: 500", failed: true }] });

    // 137 → «записей» (the many form); page 3 of 3 at the default size of 50.
    expect(screen.getByTestId("timeline-count").textContent).toBe("137 записей в этом интервале");
    expect(screen.getByTestId("timeline-page-label").textContent).toBe("Страница 1 из 3");
    // One failed source → «источник», the singular form.
    expect(screen.getByTestId("timeline-partial").textContent).toBe(
      "1 источник не ответил, лента ниже неполная.",
    );

    fireEvent.click(screen.getByRole("button", { name: "Следующая страница" }));
    expect(screen.getByTestId("timeline-page-label").textContent).toBe("Страница 2 из 3");
  });

  it("picks the few/one plural forms a two-form language would get wrong", () => {
    renderRu({ entries: entries(21) });
    // 21 is «запись», not «записей» — the English rule would say "21 entries".
    expect(screen.getByTestId("timeline-count").textContent).toBe("21 запись в этом интервале");
  });
});
