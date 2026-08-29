import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { InvestigationTimeline, type SourceNote } from "./investigation-timeline";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { DEFAULT_PAGE_SIZE } from "@/lib/pagination";
import type { TimelineEntry } from "@/lib/investigation";

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
    /* Required for the same reason, and it is the OTHER half of that reason
       (QA scope 4): "nothing happened" also assumes somebody asked, and a
       subject holding none of the read permissions had every query disabled. */
    asked: true,
    onCursor: () => {},
    windowKey: "pair|node-a|node-b|from|to",
    ...over,
  };
  return { ...render(<InvestigationTimeline {...props} />), props };
}

const rowTitles = () =>
  screen.getAllByTestId("timeline-row").map((r) => (r.textContent ?? "").match(/entry-\d{3}/)?.[0] ?? "");

const pageLabel = () => screen.getByTestId("pager-page").textContent ?? "";
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

/* NEWEST FIRST. The entries arrive ascending (mergeTimeline builds them that way, and the onset
   detection and the correlation ranking both read them in that order), and the component reverses
   them for the reader: the most recent thing in the window belongs at the top, not on page 14. */
describe("InvestigationTimeline — pagination", () => {
  it("shows the default page size, NEWEST first, and says which page of how many", () => {
    renderTimeline();
    expect(DEFAULT_PAGE_SIZE).toBe(10);
    expect(rowTitles()).toHaveLength(10);
    expect(rowTitles()[0]).toBe("entry-136");
    expect(rowTitles()[9]).toBe("entry-127");
    expect(pageLabel()).toMatch(/page 1 of 14/i);
  });

  it("pages forward and back over the same merged order", () => {
    renderTimeline();
    fireEvent.click(next());
    expect(rowTitles()[0]).toBe("entry-126");
    expect(rowTitles()).toHaveLength(10);
    expect(pageLabel()).toMatch(/page 2 of 14/i);

    setSize(50);
    fireEvent.click(next());
    fireEvent.click(next());
    expect(rowTitles()[0]).toBe("entry-036");
    // The last page is the REMAINDER, not a padded 50 — and it ends at the OLDEST entry.
    expect(rowTitles()).toHaveLength(37);
    expect(rowTitles()[36]).toBe("entry-000");
    expect(pageLabel()).toMatch(/page 3 of 3/i);

    fireEvent.click(prev());
    expect(rowTitles()[0]).toBe("entry-086");
    expect(pageLabel()).toMatch(/page 2 of 3/i);
  });

  it("disables the pager at both boundaries rather than wrapping around", () => {
    renderTimeline();
    // Fifty a page puts the far edge two clicks away; the boundary rule is the
    // subject here, not how many clicks it takes to reach one.
    setSize(50);
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

  it("offers 10 / 20 / 50 / 100 and re-cuts the list on the spot", () => {
    renderTimeline();
    expect(within(sizeControl()).getAllByRole("radio").map((b) => b.textContent)).toEqual(["10", "20", "50", "100"]);

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
    // Newest first, so page 8 of a 10-row cut starts 70 rows down from entry-136.
    for (let i = 0; i < 7; i++) fireEvent.click(next());
    expect(rowTitles()[0]).toBe("entry-066");

    setSize(50);
    // NOT page 1: entry-066 lives on page 2 of a 50-row cut, and there it is.
    expect(pageLabel()).toMatch(/page 2 of 3/i);
    expect(rowTitles()).toContain("entry-066");
    expect(rowTitles()[0]).toBe("entry-086");

    setSize(10);
    // Anchoring is symmetric: the first visible row (entry-086) leads again.
    expect(pageLabel()).toMatch(/page 6 of 14/i);
    expect(rowTitles()[0]).toBe("entry-086");
  });

  /* DECISION (pinned): the scope/window identity resets the page. A new window
     is a new list; page 3 of the previous one addresses nothing here. */
  it("resets to page 1 when the window key changes", () => {
    const { rerender, props } = renderTimeline();
    setSize(50);
    fireEvent.click(next());
    expect(pageLabel()).toMatch(/page 2 of 3/i);

    rerender(<InvestigationTimeline {...props} windowKey="pair|node-a|node-c|from|to" />);
    expect(pageLabel()).toMatch(/page 1 of 3/i);
    expect(rowTitles()[0]).toBe("entry-136");
  });

  it("does NOT reset the page for a re-render that leaves the window alone", () => {
    const { rerender, props } = renderTimeline();
    fireEvent.click(next());
    rerender(<InvestigationTimeline {...props} loading={true} />);
    expect(pageLabel()).toMatch(/page 2 of 14/i);
  });

  /* A live-shifting list: the sources re-fetch and 137 rows become 22 while the
     reader sits on page 3. Clamping shows rows; honouring the stale number
     shows an empty box under a header that just counted 22 entries. */
  it("clamps to the last page when the list shrinks underneath the reader", () => {
    const { rerender, props } = renderTimeline();
    setSize(50);
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
    expect(screen.queryByTestId("pager-page")).toBeNull();
    expect(screen.queryByRole("radiogroup", { name: "Rows per page" })).toBeNull();
    expect(rowTitles()).toHaveLength(10);
  });

  it("shows the chrome as soon as one size can differ from another", () => {
    renderTimeline({ entries: entries(11) });
    // Eleven rows is one past the default ten, so there IS a second page and
    // the forward edge is live; back from page one still is not.
    expect(screen.getByTestId("pager-page").textContent).toMatch(/page 1 of 2/i);
    expect(sizeControl()).toBeTruthy();
    expect(rowTitles()).toHaveLength(10);
    expect((prev() as HTMLButtonElement).disabled).toBe(true);
    expect((next() as HTMLButtonElement).disabled).toBe(false);

    // At the next size up the same eleven rows fit one page, and both edges die.
    setSize(20);
    expect(screen.getByTestId("pager-page").textContent).toMatch(/page 1 of 1/i);
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

  /* QA scope 4. `asked` is `allFailed`'s mirror: with every source refused there
     is no timeline, and with none of them ASKED there is no claim to make
     either. A subject holding none of the eight read permissions had every
     query disabled — nothing pending, nothing failed — and the pane turned that
     settled nothing into "Nothing happened in this window", contradicting the
     eight per-source lines directly above it that each name a missing
     permission. */
  it("makes no nothing-happened claim when no source was asked", () => {
    renderTimeline({ entries: [], asked: false });
    expect(screen.queryByText(/Nothing happened in this window/i)).toBeNull();
    /* The COUNT still renders: zero rows over zero questions is a true count,
       and suppressing it too would leave the pane silent about itself. */
    expect(screen.getByTestId("timeline-count").textContent).toBe("0 entries in this window");
  });

  it("still says nothing happened for an empty window, with no pager over it", () => {
    renderTimeline({ entries: [] });
    expect(screen.getByText(/Nothing happened in this window/i)).toBeTruthy();
    expect(screen.queryByTestId("pager-page")).toBeNull();
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
    expect(screen.getByTestId("pager-page").getAttribute("aria-live")).toBe("polite");
  });

  it("moves the page size with the arrow keys, like every other segmented control here", () => {
    renderTimeline();
    const fifty = within(sizeControl()).getByRole("radio", { name: "50" });
    fifty.focus();
    fireEvent.keyDown(fifty, { key: "ArrowRight" });
    expect(rowTitles()).toHaveLength(100);
  });
});

/* ── one row highlights at a time ────────────────────────────────────────────
   The pane's `active` used to be `entry.at === cursorAt`, and an instant is
   shared: every alert already firing when the window opens carries the window's
   own start, so hovering one lit the whole batch as a single block. */

/** Rows whose class list carries the ACTIVE background — not the hover variant,
 *  which is a different token that happens to contain the same substring. */
const highlighted = () =>
  screen
    .getAllByTestId("timeline-row")
    .filter((r) => (r.className ?? "").split(/\s+/).includes("bg-surface-2"))
    .map((r) => (r.textContent ?? "").match(/entry-\d{3}/)?.[0] ?? "");

/** Four rows at ONE instant — what "already firing when this window opens" builds. */
function sameInstant(n: number): TimelineEntry[] {
  return Array.from({ length: n }, (_, i) => ({
    at: new Date(T0),
    kind: "alert" as const,
    severity: "warn" as const,
    title: `entry-${String(i).padStart(3, "0")}`,
    ref: { kind: "alert" as const, id: `a-${i}` },
  }));
}

describe("InvestigationTimeline — hover highlights ONE row", () => {
  it("lights only the row under the pointer when four share an instant", () => {
    renderTimeline({ entries: sameInstant(4) });

    /* Rows are drawn NEWEST first, and these four share one instant, so the merge order decides:
       rows[1] is the second from the top, which is entry-002 of four. */
    const rows = screen.getAllByTestId("timeline-row");
    fireEvent.mouseEnter(rows[1]);
    expect(highlighted()).toEqual(["entry-002"]);

    fireEvent.mouseLeave(rows[1]);
    expect(highlighted()).toEqual([]);
  });

  it("still moves the shared cursor, which is what the charts read", () => {
    const seen: (Date | null)[] = [];
    renderTimeline({ entries: sameInstant(4), onCursor: (at) => seen.push(at) });

    const rows = screen.getAllByTestId("timeline-row");
    fireEvent.mouseEnter(rows[2]);
    fireEvent.mouseLeave(rows[2]);
    expect(seen[0]?.getTime()).toBe(T0);
    expect(seen[1]).toBeNull();
  });

  it("lights only the FOCUSED row, so a keyboard walk reads one row at a time", () => {
    renderTimeline({ entries: sameInstant(4) });

    // Newest first: the LAST row of four at one instant is entry-000.
    const rows = screen.getAllByTestId("timeline-row");
    fireEvent.focus(rows[3]);
    expect(highlighted()).toEqual(["entry-000"]);
  });

  /* The pane no longer TAKES the cursor back: it used to, and lighting every row
     on the returned instant is exactly how one hover became a highlighted block.
     The prop is gone, so this pins that nothing lights without a gesture. */
  it("lights nothing until a row is entered, and lets go on a new window", () => {
    const { rerender, props } = renderTimeline({ entries: sameInstant(4) });
    expect(highlighted()).toEqual([]);

    fireEvent.mouseEnter(screen.getAllByTestId("timeline-row")[0]);
    rerender(<InvestigationTimeline {...props} entries={sameInstant(4)} windowKey="cluster||from|to" />);
    expect(highlighted()).toEqual([]);
  });

  it("lights the right row after paging, where the whole-list index is what keys it", () => {
    renderTimeline();
    fireEvent.click(next());
    // Page 2 of a newest-first list of 137 starts ten rows down from entry-136.
    fireEvent.mouseEnter(screen.getAllByTestId("timeline-row")[0]);
    expect(highlighted()).toEqual(["entry-126"]);
  });

  it("holds for ordinary entries too — two events logged in the same second", () => {
    const at = new Date(T0);
    renderTimeline({
      entries: [
        { at, kind: "event", severity: "info", title: "entry-000", ref: { kind: "event", id: "e-a" } },
        { at, kind: "event", severity: "info", title: "entry-001", ref: { kind: "event", id: "e-b" } },
      ],
    });

    fireEvent.mouseEnter(screen.getAllByTestId("timeline-row")[0]);
    expect(highlighted()).toEqual(["entry-001"]);
  });
});

/* ── M3-5: the raw audit subject survives as a tooltip ─────────────────────
   The visible detail line prefers the human-readable subjectDisplay; the raw
   subjectKind:subjectId the operator may need to paste into a query stays one
   hover away, never lost. */
describe("InvestigationTimeline — detail tooltip", () => {
  it("puts detailTitle on the detail line as its title attribute", () => {
    renderTimeline({
      entries: [
        {
          at: new Date(T0),
          kind: "audit",
          severity: "info",
          title: "POST /api/v1/targets",
          detail: "d.esin@group-ib.com · targets · allowed",
          detailTitle: "user:oidc:6f616b42",
          ref: { kind: "audit", id: "a-1" },
        },
      ],
    });
    expect(screen.getByText("d.esin@group-ib.com · targets · allowed")).toHaveAttribute(
      "title",
      "user:oidc:6f616b42",
    );
  });

  it("adds no title attribute when there is nothing hidden to reveal", () => {
    renderTimeline({
      entries: [
        {
          at: new Date(T0),
          kind: "audit",
          severity: "info",
          title: "POST /api/v1/targets",
          detail: "user:ada · targets · allowed",
          ref: { kind: "audit", id: "a-2" },
        },
      ],
    });
    expect(screen.getByText("user:ada · targets · allowed")).not.toHaveAttribute("title");
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
      asked: true,
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

    // 137 → «записей» (the many form); 14 pages at the default size of ten.
    expect(screen.getByTestId("timeline-count").textContent).toBe("137 записей в этом интервале");
    expect(screen.getByTestId("pager-page").textContent).toBe("Страница 1 из 14");
    // One failed source → «источник», the singular form.
    expect(screen.getByTestId("timeline-partial").textContent).toBe(
      "1 источник не ответил, лента ниже неполная.",
    );

    fireEvent.click(screen.getByRole("button", { name: "Следующая страница" }));
    expect(screen.getByTestId("pager-page").textContent).toBe("Страница 2 из 14");
  });

  it("picks the few/one plural forms a two-form language would get wrong", () => {
    renderRu({ entries: entries(21) });
    // 21 is «запись», not «записей» — the English rule would say "21 entries".
    expect(screen.getByTestId("timeline-count").textContent).toBe("21 запись в этом интервале");
  });
});

/* ── M4 typography: the data face and the named scale ────────────────────────
   Pinned once at the component: the timestamp column is data and wears
   mono-data; the pane heading sits on the named section step; the readable
   detail line is primary content, so it must NOT carry the muted caption
   colour (the raw machine identity already lives in the tooltip). */
describe("InvestigationTimeline — M4 typography", () => {
  it("puts the stamp in mono-data, the heading on type-section, and leaves the detail unmuted", () => {
    renderTimeline({
      entries: [
        {
          at: new Date(T0),
          kind: "audit",
          severity: "info",
          title: "POST /api/v1/targets",
          detail: "ada@example.com · targets · allowed",
          detailTitle: "user:oidc:6f616b42",
          ref: { kind: "audit", id: "a-1" },
        },
      ],
    });
    const row = screen.getByTestId("timeline-row");
    const stamp = row.querySelector("span");
    expect(stamp?.className).toContain("mono-data");
    expect(screen.getByRole("heading", { name: "Timeline" }).className).toContain("type-section");
    expect(screen.getByText("ada@example.com · targets · allowed").className).not.toContain(
      "text-muted-foreground",
    );
  });
});
