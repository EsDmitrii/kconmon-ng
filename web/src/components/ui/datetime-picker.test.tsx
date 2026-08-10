import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  DateTimePicker,
  FUTURE_PRESETS,
  PAST_PRESETS,
  POPOVER_MIN_HEIGHT_PX,
  composeLocal,
  formatInstant,
  pickerDropDirection,
  toDateInputValue,
  toTimeInputValue,
} from "@/components/ui/datetime-picker";

/* The whole suite runs on a frozen clock: "future days are disabled" and
   "1h ago" are claims about NOW, and a suite that reads the wall clock would
   pass in the morning and fail at 00:30 on the first of a month. */
const NOW = new Date(2026, 7, 8, 12, 0, 0); // Sat 8 August 2026, 12:00 local
const VALUE = new Date(2026, 7, 7, 15, 34, 0); // Fri 7 August 2026, 15:34 local

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(NOW);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

function renderPicker(value: Date | null = VALUE) {
  const onApply = vi.fn();
  render(<DateTimePicker value={value} onApply={onApply} aria-label="Viewing time" />);
  return { onApply };
}

const trigger = () => screen.getByRole("button", { name: /viewing time/i });
const dialog = () => screen.getByRole("dialog", { name: /choose a date and time/i });
const dateField = () => screen.getByLabelText("Date") as HTMLInputElement;
const timeField = () => screen.getByLabelText("Time") as HTMLInputElement;
const day = (name: string) => screen.getByRole("button", { name: `Choose ${name}` }) as HTMLButtonElement;

function open() {
  fireEvent.click(trigger());
}

describe("value helpers", () => {
  it("renders local date and time field values, never UTC", () => {
    expect(toDateInputValue(VALUE)).toBe("2026-08-07");
    expect(toTimeInputValue(VALUE)).toBe("15:34");
  });

  it("composes the local instant the two fields name", () => {
    expect(composeLocal("2026-08-07", "15:34")).toEqual(VALUE);
  });

  it("refuses a half-typed or nonsense pair instead of composing Invalid Date", () => {
    expect(composeLocal("", "15:34")).toBeNull();
    expect(composeLocal("2026-08-07", "")).toBeNull();
    expect(composeLocal("2026-8-7", "15:34")).toBeNull();
    expect(composeLocal("2026-13-07", "15:34")).toBeNull();
    expect(composeLocal("2026-08-07", "25:00")).toBeNull();
  });

  it("formats an instant for the trigger", () => {
    // Asserted loosely on purpose: the order of the parts is the VIEWER's
    // locale, and pinning "Aug 7, 2026, 03:34 PM" would pin en-US.
    const label = formatInstant(VALUE);
    expect(label).toMatch(/2026/);
    expect(label).toMatch(/7/); // the day of month
    expect(label).toMatch(/34/); // the minute
  });

  it("takes the INTERFACE locale rather than a bare undefined (QA scope 3, finding #7)", () => {
    // The custom-range triggers on /investigate sit inside a Russian page; a
    // month abbreviation is a WORD, and words follow the interface.
    expect(formatInstant(VALUE, "ru")).not.toBe(formatInstant(VALUE, "en"));
    expect(formatInstant(VALUE, "ru")).not.toMatch(/AM|PM/);
  });

  it("prints the hour in the locale's natural form, never a padded 08:00 PM (finding #17)", () => {
    // Local 20:05, i.e. 8pm wherever the clock is 12-hour.
    const evening = new Date(2026, 7, 8, 20, 5);
    expect(formatInstant(evening, "en")).not.toMatch(/\b0\d:\d\d\s?(AM|PM)/i);
  });
});

describe("DateTimePicker trigger", () => {
  it("shows the current value and opens the popover", () => {
    renderPicker();
    expect(trigger()).toHaveTextContent(/2026/);
    expect(trigger()).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    open();
    expect(dialog()).toBeInTheDocument();
    expect(trigger()).toHaveAttribute("aria-expanded", "true");
  });

  it("falls back to a prompt when there is no value yet", () => {
    renderPicker(null);
    expect(trigger()).toHaveTextContent(/pick a time/i);
  });

  it("opens on now when there is no value", () => {
    renderPicker(null);
    open();
    expect(dateField().value).toBe("2026-08-08");
    expect(timeField().value).toBe("12:00");
  });
});

describe("DateTimePicker calendar", () => {
  it("renders the month of the given value with the day selected", () => {
    renderPicker();
    open();
    expect(screen.getByText("August 2026")).toBeInTheDocument();
    expect(within(screen.getByRole("gridcell", { selected: true })).getByRole("button")).toHaveTextContent("7");
    // 6 rows × 7 columns, always — the popover must not resize between months.
    expect(screen.getAllByRole("gridcell")).toHaveLength(42);
  });

  it("marks today with aria-current", () => {
    renderPicker();
    open();
    expect(day("8 August 2026")).toHaveAttribute("aria-current", "date");
    expect(day("7 August 2026")).not.toHaveAttribute("aria-current");
  });

  it("walks months with the chevrons", () => {
    renderPicker();
    open();
    fireEvent.click(screen.getByRole("button", { name: /previous month/i }));
    expect(screen.getByText("July 2026")).toBeInTheDocument();
    expect(day("1 July 2026")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /next month/i }));
    expect(screen.getByText("August 2026")).toBeInTheDocument();
  });

  it("the header opens a month & year panel centred on the view", () => {
    renderPicker();
    open();
    fireEvent.click(screen.getByRole("button", { name: /choose the month and year/i }));
    const months = screen.getByRole("listbox", { name: "Month" });
    const years = screen.getByRole("listbox", { name: "Year" });
    expect(within(months).getByRole("option", { name: "August" })).toHaveAttribute("aria-selected", "true");
    expect(within(years).getByRole("option", { name: "2026" })).toHaveAttribute("aria-selected", "true");
  });

  it("picking a year keeps the panel up; picking a month closes it and pages the grid", () => {
    renderPicker();
    open();
    fireEvent.click(screen.getByRole("button", { name: /choose the month and year/i }));
    fireEvent.click(within(screen.getByRole("listbox", { name: "Year" })).getByRole("option", { name: "2024" }));
    expect(screen.getByRole("listbox", { name: "Month" })).toBeInTheDocument();

    fireEvent.click(within(screen.getByRole("listbox", { name: "Month" })).getByRole("option", { name: "March" }));
    expect(screen.queryByRole("listbox", { name: "Month" })).not.toBeInTheDocument();
    expect(screen.getByText("March 2024")).toBeInTheDocument();
    expect(day("14 March 2024")).toBeInTheDocument();
  });

  it("never offers a future month or year without allowFuture", () => {
    renderPicker();
    open();
    fireEvent.click(screen.getByRole("button", { name: /choose the month and year/i }));
    expect(within(screen.getByRole("listbox", { name: "Month" })).getByRole("option", { name: "September" })).toBeDisabled();
    expect(within(screen.getByRole("listbox", { name: "Year" })).queryByRole("option", { name: "2027" })).not.toBeInTheDocument();
  });

  it("clamps the view month down when a year pick would land it in the future", () => {
    renderPicker(new Date(2024, 11, 25, 9, 0, 0)); // view: December 2024
    open();
    fireEvent.click(screen.getByRole("button", { name: /choose the month and year/i }));
    fireEvent.click(within(screen.getByRole("listbox", { name: "Year" })).getByRole("option", { name: "2026" }));
    // December 2026 has not happened; the view lands on today's month instead.
    expect(screen.getByText("August 2026")).toBeInTheDocument();
  });

  it("disables future days and the next-month chevron — the clamp is never offered", () => {
    renderPicker();
    open();
    expect(day("8 August 2026")).toBeEnabled();
    expect(day("9 August 2026")).toBeDisabled();
    expect(day("31 August 2026")).toBeDisabled();
    expect(screen.getByRole("button", { name: /next month/i })).toBeDisabled();
  });

  it("keeps the chosen time of day when a day is clicked", () => {
    const { onApply } = renderPicker();
    open();
    fireEvent.click(day("3 August 2026"));
    expect(timeField().value).toBe("15:34");
    expect(dateField().value).toBe("2026-08-03");

    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    expect(onApply).toHaveBeenCalledTimes(1);
    expect(onApply.mock.calls[0][0]).toEqual(new Date(2026, 7, 3, 15, 34, 0));
  });

  it("follows a day picked in the previous month", () => {
    const { onApply } = renderPicker();
    open();
    fireEvent.click(screen.getByRole("button", { name: /previous month/i }));
    fireEvent.click(day("14 July 2026"));
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    expect(onApply.mock.calls[0][0]).toEqual(new Date(2026, 6, 14, 15, 34, 0));
  });
});

describe("DateTimePicker keyboard navigation", () => {
  it("opens with focus on the selected day", () => {
    renderPicker();
    open();
    expect(document.activeElement).toBe(day("7 August 2026"));
  });

  it("moves focus a day at a time and a week at a time", () => {
    renderPicker();
    open();
    fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" });
    expect(document.activeElement).toBe(day("6 August 2026"));

    fireEvent.keyDown(document.activeElement!, { key: "ArrowUp" });
    expect(document.activeElement).toBe(day("30 July 2026"));
    // Crossing a month boundary pages the grid with the focus.
    expect(screen.getByText("July 2026")).toBeInTheDocument();

    fireEvent.keyDown(document.activeElement!, { key: "ArrowDown" });
    expect(document.activeElement).toBe(day("6 August 2026"));
    fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
    expect(document.activeElement).toBe(day("7 August 2026"));
  });

  it("refuses to walk past today", () => {
    renderPicker();
    open();
    fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" }); // → 8 Aug, today
    expect(document.activeElement).toBe(day("8 August 2026"));
    fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" }); // → 9 Aug, refused
    expect(document.activeElement).toBe(day("8 August 2026"));
  });

  it("jumps to the ends of the week with Home and End", () => {
    renderPicker();
    open();
    fireEvent.keyDown(document.activeElement!, { key: "Home" });
    expect(document.activeElement).toBe(day("3 August 2026")); // Monday
    fireEvent.keyDown(document.activeElement!, { key: "End" });
    // Sunday the 9th is in the future, so End stops where the clamp does.
    expect(document.activeElement).toBe(day("3 August 2026"));
  });

  it("keeps exactly one day in the tab order", () => {
    renderPicker();
    open();
    expect(day("7 August 2026").tabIndex).toBe(0);
    expect(day("6 August 2026").tabIndex).toBe(-1);
    fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" });
    expect(day("6 August 2026").tabIndex).toBe(0);
    expect(day("7 August 2026").tabIndex).toBe(-1);
  });

  it("wraps Tab inside the popover instead of dropping into the page", () => {
    renderPicker();
    open();
    const applyButton = screen.getByRole("button", { name: "Apply" });
    applyButton.focus();
    fireEvent.keyDown(dialog(), { key: "Tab" });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "15m ago" }));
  });
});

describe("DateTimePicker presets", () => {
  it.each([
    ["15m ago", 15],
    ["1h ago", 60],
    ["6h ago", 360],
    ["24h ago", 1440],
  ])("%s applies that instant in one click and closes", (label, minutes) => {
    const { onApply } = renderPicker();
    open();
    fireEvent.click(screen.getByRole("button", { name: label }));
    expect(onApply).toHaveBeenCalledTimes(1);
    expect(onApply.mock.calls[0][0].getTime()).toBe(NOW.getTime() - minutes * 60_000);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  /* ── QA round 3, finding #17: a forward field gets forward presets ────── */

  it("is the BACKWARD set by default — the Time Machine's own", () => {
    expect(PAST_PRESETS.map((p) => p.label)).toEqual(["15m ago", "1h ago", "6h ago", "24h ago"]);
    expect(PAST_PRESETS.every((p) => p.minutes < 0)).toBe(true);
    renderPicker();
    open();
    const quick = within(screen.getByRole("group", { name: "Quick ranges" }));
    expect(quick.getAllByRole("button").map((b) => b.textContent)).toEqual([
      "15m ago",
      "1h ago",
      "6h ago",
      "24h ago",
    ]);
  });

  it("is the FORWARD set on an allowFuture picker — a window declared in advance never reaches back", () => {
    expect(FUTURE_PRESETS.map((p) => p.label)).toEqual(["in 15m", "in 1h", "in 6h", "tomorrow"]);
    expect(FUTURE_PRESETS.every((p) => p.minutes > 0)).toBe(true);
    render(<DateTimePicker value={VALUE} onApply={vi.fn()} allowFuture aria-label="End" />);
    fireEvent.click(screen.getByRole("button", { name: "End" }));
    const quick = within(screen.getByRole("group", { name: "Quick ranges" }));
    expect(quick.getAllByRole("button").map((b) => b.textContent)).toEqual(["in 15m", "in 1h", "in 6h", "tomorrow"]);
  });

  it.each([
    ["in 15m", 15],
    ["in 1h", 60],
    ["in 6h", 360],
    ["tomorrow", 1440],
  ])("%s applies an instant AHEAD of now", (label, minutes) => {
    const onApply = vi.fn();
    render(<DateTimePicker value={VALUE} onApply={onApply} allowFuture aria-label="End" />);
    fireEvent.click(screen.getByRole("button", { name: "End" }));
    fireEvent.click(screen.getByRole("button", { name: label }));
    expect(onApply.mock.calls[0][0].getTime()).toBe(NOW.getTime() + minutes * 60_000);
  });
});

/* ── QA round 3, finding #16: which way the popover opens ────────────────── */

describe("pickerDropDirection", () => {
  it("opens downward whenever there is room below, however much room is above", () => {
    expect(pickerDropDirection(POPOVER_MIN_HEIGHT_PX, 5000)).toBe("down");
    expect(pickerDropDirection(POPOVER_MIN_HEIGHT_PX + 1, 5000)).toBe("down");
  });

  it("flips upward only when there is not room below AND the popover actually FITS above", () => {
    expect(pickerDropDirection(10, 700)).toBe("up");
    expect(pickerDropDirection(POPOVER_MIN_HEIGHT_PX - 1, POPOVER_MIN_HEIGHT_PX)).toBe("up");
  });

  it("stays downward when neither side fits — down is the tie-break, not a measurement", () => {
    expect(pickerDropDirection(100, 100)).toBe("down");
    expect(pickerDropDirection(100, 40)).toBe("down");
  });

  /* Up must be earned by FITTING, not by winning a comparison; down at least scrolls. */
  it("stays downward when above is merely larger but still too small (#3)", () => {
    expect(pickerDropDirection(310, 390)).toBe("down");
    expect(pickerDropDirection(0, POPOVER_MIN_HEIGHT_PX - 1)).toBe("down");
  });

  it("goes up at exactly the needed height above, never one pixel under", () => {
    expect(pickerDropDirection(0, POPOVER_MIN_HEIGHT_PX)).toBe("up");
    expect(pickerDropDirection(0, POPOVER_MIN_HEIGHT_PX - 1)).toBe("down");
  });

  it("answers down for a box it cannot measure", () => {
    expect(pickerDropDirection(NaN, 900)).toBe("down");
    expect(pickerDropDirection(10, Number.POSITIVE_INFINITY)).toBe("down");
  });

  it("renders the direction as the class the popover is anchored by", () => {
    /* jsdom measures every box as zero, so the trigger's bottom is 0 and the
       whole viewport counts as space below: the guarded fallback IS the
       downward case, which is what this asserts. */
    renderPicker();
    open();
    expect(dialog().getAttribute("data-drop")).toBe("down");
    expect(dialog().className).toContain("top-full");
    expect(dialog().className).not.toContain("bottom-full");
  });

  it("anchors to bottom-full when the trigger measures as being at the fold", () => {
    renderPicker();
    /* One stubbed measurement, the seam the pure helper exists for: a trigger
       whose bottom is 8px above the viewport floor has no room below. */
    vi.spyOn(trigger(), "getBoundingClientRect").mockReturnValue({
      top: window.innerHeight - 40,
      bottom: window.innerHeight - 8,
      left: 0,
      right: 100,
      width: 100,
      height: 32,
      x: 0,
      y: window.innerHeight - 40,
      toJSON: () => ({}),
    } as DOMRect);
    open();
    expect(dialog().getAttribute("data-drop")).toBe("up");
    expect(dialog().className).toContain("bottom-full");
    expect(dialog().className).not.toContain("top-full");
  });

  it("re-measures on window resize while it is open (#3)", () => {
    renderPicker();
    vi.spyOn(trigger(), "getBoundingClientRect").mockReturnValue({
      top: window.innerHeight - 40,
      bottom: window.innerHeight - 8,
      left: 0,
      right: 100,
      width: 100,
      height: 32,
      x: 0,
      y: window.innerHeight - 40,
      toJSON: () => ({}),
    } as DOMRect);
    open();
    expect(dialog().getAttribute("data-drop")).toBe("up");

    // The viewport grows: the same trigger box now has room below it.
    window.innerHeight = window.innerHeight + 2000;
    fireEvent(window, new Event("resize"));
    expect(dialog().getAttribute("data-drop")).toBe("down");
  });
});

/* ── QA round 5, finding #12: a future-only field refuses the past ────────── */

describe("DateTimePicker disablePast", () => {
  function renderFuture() {
    render(<DateTimePicker value={null} onApply={vi.fn()} allowFuture disablePast aria-label="Run at" />);
    fireEvent.click(screen.getByRole("button", { name: /run at/i }));
  }

  it("disables every day BEFORE today in the grid, and leaves today live", () => {
    renderFuture();
    expect(day("7 August 2026")).toBeDisabled();
    expect(day("1 August 2026")).toBeDisabled();
    expect(day("8 August 2026")).toBeEnabled();
    expect(day("9 August 2026")).toBeEnabled();
  });

  it("blocks the backward chevron at the current month and disables past months in the wheel", () => {
    renderFuture();
    expect(screen.getByRole("button", { name: "Previous month" })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: /choose the month and year/i }));
    const months = within(screen.getByRole("listbox", { name: "Month" }));
    expect(months.getByRole("option", { name: "July" })).toBeDisabled();
    expect(months.getByRole("option", { name: "August" })).toBeEnabled();
    expect(months.getByRole("option", { name: "September" })).toBeEnabled();
  });

  it("shows only forward presets — a backward shortcut on a future-only field is a trap", () => {
    renderFuture();
    const quick = within(screen.getByRole("group", { name: "Quick ranges" }));
    expect(quick.getAllByRole("button").map((b) => b.textContent)).toEqual(["in 15m", "in 1h", "in 6h", "tomorrow"]);
  });

  it("refuses to walk the arrow keys back past today", () => {
    renderFuture();
    // Focus opens on today (value is null, so the popover seeds on now); the
    // grid keeps exactly one tab stop, and it must not walk into a dead day.
    fireEvent.keyDown(day("8 August 2026"), { key: "ArrowLeft" });
    expect(day("8 August 2026")).toHaveAttribute("tabindex", "0");
    fireEvent.keyDown(day("8 August 2026"), { key: "ArrowRight" });
    expect(day("9 August 2026")).toHaveAttribute("tabindex", "0");
  });

  it("floors the date field at today", () => {
    renderFuture();
    expect(screen.getByLabelText("Date")).toHaveAttribute("min", "2026-08-08");
  });

  it("leaves the Time Machine picker untouched — the past is the whole point there", () => {
    renderPicker();
    open();
    expect(day("7 August 2026")).toBeEnabled();
    expect(screen.getByRole("button", { name: "Previous month" })).toBeEnabled();
    expect(screen.getByLabelText("Date")).not.toHaveAttribute("min");
  });
});

describe("DateTimePicker manual path", () => {
  it("composes the instant from typed date and time alone", () => {
    const { onApply } = renderPicker();
    open();
    fireEvent.change(dateField(), { target: { value: "2026-07-04" } });
    fireEvent.change(timeField(), { target: { value: "23:45" } });
    // Typing a date in another month pages the grid to it.
    expect(screen.getByText("July 2026")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    expect(onApply.mock.calls[0][0]).toEqual(new Date(2026, 6, 4, 23, 45, 0));
  });

  it("lets a future instant through — clamping belongs to the caller, not here", () => {
    const { onApply } = renderPicker();
    open();
    fireEvent.change(dateField(), { target: { value: "2027-01-01" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    expect(onApply.mock.calls[0][0]).toEqual(new Date(2027, 0, 1, 15, 34, 0));
  });

  it("resets both the date and the time with the Now button, wherever the draft wandered", () => {
    const { onApply } = renderPicker();
    open();
    // Wander far from today on both axes first, so the reset is observable.
    fireEvent.change(dateField(), { target: { value: "2026-07-01" } });
    fireEvent.change(timeField(), { target: { value: "03:15" } });
    fireEvent.click(screen.getByRole("button", { name: /set the date and time to now/i }));
    expect(dateField().value).toBe("2026-08-08");
    expect(timeField().value).toBe("12:00");
    // The grid follows the reset: back to today's month, today selected.
    expect(screen.getByText("August 2026")).toBeInTheDocument();
    expect(within(screen.getByRole("gridcell", { selected: true })).getByRole("button")).toHaveTextContent("8");
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    expect(onApply.mock.calls[0][0]).toEqual(new Date(2026, 7, 8, 12, 0, 0));
  });

  it("opens the time wheel on the time field, scrolled to the draft's hour and minute", () => {
    renderPicker();
    open();
    fireEvent.focus(timeField());
    const hours = screen.getByRole("listbox", { name: "Hour" });
    const minutes = screen.getByRole("listbox", { name: "Minute" });
    expect(within(hours).getByRole("option", { name: "15" })).toHaveAttribute("aria-selected", "true");
    expect(within(minutes).getByRole("option", { name: "34" })).toHaveAttribute("aria-selected", "true");
  });

  it("picking an hour keeps the wheel open; picking a minute closes it", () => {
    const { onApply } = renderPicker();
    open();
    fireEvent.focus(timeField());
    fireEvent.click(within(screen.getByRole("listbox", { name: "Hour" })).getByRole("option", { name: "02" }));
    expect(timeField().value).toBe("02:34");
    expect(screen.getByRole("listbox", { name: "Minute" })).toBeInTheDocument();

    fireEvent.click(within(screen.getByRole("listbox", { name: "Minute" })).getByRole("option", { name: "05" }));
    expect(timeField().value).toBe("02:05");
    expect(screen.queryByRole("listbox", { name: "Hour" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    expect(onApply.mock.calls[0][0]).toEqual(new Date(2026, 7, 7, 2, 5, 0));
  });

  it("Escape closes the wheel and only the wheel", () => {
    renderPicker();
    open();
    fireEvent.focus(timeField());
    fireEvent.keyDown(timeField(), { key: "Escape" });
    expect(screen.queryByRole("listbox", { name: "Hour" })).not.toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: /choose a date and time/i })).toBeInTheDocument();
  });

  it("a click elsewhere in the popover puts the wheel away", () => {
    renderPicker();
    open();
    fireEvent.focus(timeField());
    fireEvent.mouseDown(day("3 August 2026"));
    expect(screen.queryByRole("listbox", { name: "Hour" })).not.toBeInTheDocument();
  });

  it("cannot apply a cleared field", () => {
    const { onApply } = renderPicker();
    open();
    fireEvent.change(dateField(), { target: { value: "" } });
    const applyButton = screen.getByRole("button", { name: "Apply" });
    expect(applyButton).toBeDisabled();
    fireEvent.click(applyButton);
    expect(onApply).not.toHaveBeenCalled();
  });
});

describe("DateTimePicker dismissal", () => {
  it("Cancel closes and emits nothing", () => {
    const { onApply } = renderPicker();
    open();
    fireEvent.click(day("3 August 2026"));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(onApply).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(trigger());
  });

  it("Escape closes and returns focus to the trigger", () => {
    const { onApply } = renderPicker();
    open();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(onApply).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(trigger());
  });

  it("a click outside closes it", () => {
    const { onApply } = renderPicker();
    open();
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(onApply).not.toHaveBeenCalled();
  });

  it("reopens on the current value, not on an abandoned draft", () => {
    renderPicker();
    open();
    fireEvent.change(timeField(), { target: { value: "03:00" } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    open();
    expect(timeField().value).toBe("15:34");
  });

  it("honours disabled — no popover at all", () => {
    render(<DateTimePicker value={VALUE} onApply={vi.fn()} aria-label="Viewing time" disabled />);
    expect(trigger()).toBeDisabled();
    fireEvent.click(trigger());
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});

/* ── QA round 1: findings #6, #7, #11 ────────────────────────────────────── */

describe("DateTimePicker — the time wheel opens upward (#6)", () => {
  it("hangs the wheel above the field so it never covers Cancel and Apply", () => {
    renderPicker();
    open();
    fireEvent.click(timeField());

    const wheel = screen.getByRole("group", { name: "Pick a time" });
    expect(wheel.className).toContain("bottom-full");
    expect(wheel.className).not.toContain("top-full");
  });

  it("leaves the month & year panel opening downward — it only covers the grid", () => {
    renderPicker();
    open();
    fireEvent.click(screen.getByRole("button", { name: /choose the month and year/i }));

    expect(screen.getByRole("group", { name: "Pick a month and year" }).className).toContain("top-full");
  });
});

describe("DateTimePicker — paging keeps the grid reachable (#7)", () => {
  /** Every day cell currently in the tab order. Exactly one, always: that is
   *  what makes the grid a single tab stop the arrows then drive. */
  function tabbableDays(): HTMLElement[] {
    return within(screen.getByRole("grid", { name: "Calendar" }))
      .getAllByRole("button")
      .filter((b) => b.tabIndex === 0);
  }

  it("keeps exactly one tabbable cell after paging past the focused day", () => {
    renderPicker();
    open();
    expect(tabbableDays()).toHaveLength(1);

    fireEvent.click(screen.getByRole("button", { name: /previous month/i }));
    expect(tabbableDays()).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: /previous month/i }));

    const cells = tabbableDays();
    expect(cells).toHaveLength(1);
    // The 1st of the month now on screen, not a day two months away.
    expect(cells[0]).toHaveAccessibleName("Choose 1 June 2026");
  });

  it("the arrows work from the cell paging left behind", () => {
    renderPicker();
    open();
    fireEvent.click(screen.getByRole("button", { name: /previous month/i }));
    fireEvent.click(screen.getByRole("button", { name: /previous month/i }));

    tabbableDays()[0].focus();
    fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
    expect(document.activeElement).toBe(day("2 June 2026"));
  });

  it("does not yank focus out of the chevron the pointer is clicking", () => {
    renderPicker();
    open();
    const prev = screen.getByRole("button", { name: /previous month/i });
    // A real click focuses the button it lands on; jsdom's does not, so the
    // pointer's focus is staged explicitly.
    prev.focus();
    fireEvent.click(prev);
    fireEvent.click(prev);

    expect(document.activeElement).toBe(prev);
  });

  it("still carries focus across a month boundary when the GRID owns it", () => {
    renderPicker();
    open();
    // Focus starts on the selected day (7 August); walking left off the start
    // of the month is the case the arrows already owned.
    for (let i = 0; i < 7; i++) fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" });
    expect(document.activeElement).toBe(day("31 July 2026"));
  });
});

describe("DateTimePicker — a future draft says so (#11)", () => {
  it("warns that the instant will be clamped, and keeps Apply live", () => {
    renderPicker();
    open();
    // Today at 23:00, with the frozen clock at 12:00.
    fireEvent.change(dateField(), { target: { value: "2026-08-08" } });
    fireEvent.change(timeField(), { target: { value: "23:00" } });

    expect(screen.getByTestId("future-hint")).toHaveTextContent("In the future — will engage at now.");
    expect(screen.getByRole("button", { name: "Apply" })).toBeEnabled();
  });

  it("says nothing for a past instant", () => {
    renderPicker();
    open();
    expect(screen.queryByTestId("future-hint")).toBeNull();
  });

  it("says nothing when the caller allows the future — a maintenance window is declared ahead", () => {
    render(<DateTimePicker value={VALUE} onApply={vi.fn()} aria-label="Window start" allowFuture />);
    fireEvent.click(screen.getByRole("button", { name: /window start/i }));
    fireEvent.change(screen.getAllByLabelText("Date")[0], { target: { value: "2026-09-01" } });

    expect(screen.queryByTestId("future-hint")).toBeNull();
  });
});
