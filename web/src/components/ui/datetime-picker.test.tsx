import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  DateTimePicker,
  composeLocal,
  formatInstant,
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
