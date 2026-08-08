import * as React from "react";
import { CalendarDays, ChevronLeft, ChevronRight } from "lucide-react";
import { Button, type ButtonProps } from "@/components/ui/button";
import { cn } from "@/lib/utils";

/**
 * DateTimePicker — a hand-rolled calendar popover for choosing a past instant.
 *
 * It exists because `<input type="datetime-local">` is a spinner: correct, and
 * miserable to aim at a day three weeks back. The repo carries no date library
 * and gains none for this; the grid below is 40 lines of arithmetic on the
 * local-time Date getters and nothing more.
 *
 * The component is CONTROLLED and dumb: it holds a draft while open, emits one
 * Date through onApply, and knows nothing about the Time Machine, `?at=` or the
 * future-clamp. The caller owns all of that (lib/timemachine.tsx's engage()).
 *
 * Two ways in, on purpose:
 *   - point: presets, then the month grid (mouse, and arrow keys in the grid).
 *   - type: the date and time fields at the bottom are plain, editable inputs —
 *     a keyboard user can compose the whole instant without ever entering the
 *     grid. That is the manual path the raw input used to be, kept intact.
 *
 * Every surface is a token (popover/surface-2/accent/primary/ring), so light
 * and dark come from index.css and never from a literal colour here.
 */

const pad = (n: number) => String(n).padStart(2, "0");

/** toDateInputValue renders the LOCAL calendar day an `<input type="date">`
 *  speaks. Built from the local getters, never from toISOString(), which would
 *  hand the field a UTC day and shift the user's pick by a day near midnight. */
export function toDateInputValue(d: Date): string {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** toTimeInputValue renders the LOCAL wall clock an `<input type="time">`
 *  speaks, to the minute (the Time Machine's own precision is the second, and
 *  a seconds spinner buys nothing an operator wants to aim at). */
export function toTimeInputValue(d: Date): string {
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;
const TIME_RE = /^\d{2}:\d{2}(:\d{2})?$/;

/**
 * composeLocal turns the two field values into the instant they name, in LOCAL
 * time. Assembled through the Date(y, m, d, h, min) constructor rather than
 * `new Date("2026-08-08T01:23")`: the string form is parsed as UTC by some
 * engines for date-only shapes and locally for others, and a picker that lands
 * on a different hour per browser is worse than no picker.
 *
 * null for anything incomplete — a half-typed field must not compose an
 * "Invalid Date" and travel any further.
 */
export function composeLocal(date: string, time: string): Date | null {
  if (!DATE_RE.test(date) || !TIME_RE.test(time)) return null;
  const [y, m, d] = date.split("-").map(Number);
  const [hh, mm] = time.split(":").map(Number);
  if (m < 1 || m > 12 || d < 1 || d > 31 || hh > 23 || mm > 59) return null;
  const out = new Date(y, m - 1, d, hh, mm, 0, 0);
  return Number.isNaN(out.getTime()) ? null : out;
}

/** formatInstant is the trigger's label: "Aug 8, 2026, 01:23" in the viewer's
 *  own locale. Short by design — the trigger sits in a one-line bar. */
export function formatInstant(d: Date): string {
  return d.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

const MONTHS = [
  "January",
  "February",
  "March",
  "April",
  "May",
  "June",
  "July",
  "August",
  "September",
  "October",
  "November",
  "December",
];

/* Monday-first, matching ISO 8601 and the week an operator reading a duty
   roster already has in their head. Header cells carry the full name in
   `abbr` so a screen reader announces "Monday", not "Mo". */
const WEEKDAYS = [
  { short: "Mo", long: "Monday" },
  { short: "Tu", long: "Tuesday" },
  { short: "We", long: "Wednesday" },
  { short: "Th", long: "Thursday" },
  { short: "Fr", long: "Friday" },
  { short: "Sa", long: "Saturday" },
  { short: "Su", long: "Sunday" },
];

/** PRESETS are the jumps the spec calls out ("the last hour" and friends) —
 *  one click, no grid, no Apply. Minutes back from now. */
const PRESETS: readonly { label: string; minutes: number }[] = [
  { label: "15m ago", minutes: 15 },
  { label: "1h ago", minutes: 60 },
  { label: "6h ago", minutes: 360 },
  { label: "24h ago", minutes: 1440 },
];

function startOfDay(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

function addDays(d: Date, n: number): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + n);
}

function sameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

function dayLabel(d: Date): string {
  return `${d.getDate()} ${MONTHS[d.getMonth()]} ${d.getFullYear()}`;
}

/**
 * monthGrid returns the 42 days (6 rows × 7 columns) the month is drawn on,
 * including the leading/trailing days of the neighbouring months. Always 42, so
 * the popover never changes height between February and a 31-day month that
 * starts on a Sunday — a calendar that resizes under the cursor is the exact
 * jank this component was asked to remove.
 */
function monthGrid(view: Date): Date[] {
  const first = new Date(view.getFullYear(), view.getMonth(), 1);
  const offset = (first.getDay() + 6) % 7; // Sunday is 0 in JS; the week starts Monday here.
  const start = addDays(first, -offset);
  return Array.from({ length: 42 }, (_, i) => addDays(start, i));
}

export interface DateTimePickerProps {
  /** The instant the trigger shows and the popover opens on. null means "no
   *  instant chosen yet" — the popover then opens on now. */
  value: Date | null;
  /** Called with the composed instant on Apply or on a preset click. Never
   *  called by Cancel, Escape or a click outside. */
  onApply: (d: Date) => void;
  /** Opt IN to days after today. Default false, because the control was built
   *  for the Time Machine, where a future instant is a state nothing has
   *  measured yet and offering it is a promise the console cannot keep.
   *  M6's maintenance form is the exception the flag exists for: a change
   *  window is normally DECLARED IN ADVANCE, so clamping it to the past would
   *  make the common case unexpressible. */
  allowFuture?: boolean;
  disabled?: boolean;
  /** Trigger text. Defaults to the formatted value (or "Pick a time"); pass it
   *  when the trigger has a name of its own, as the Live-state bar does. */
  label?: React.ReactNode;
  /** Trigger icon, defaulting to a calendar. */
  icon?: React.ReactNode;
  "aria-label": string;
  variant?: ButtonProps["variant"];
  className?: string;
}

export function DateTimePicker({
  value,
  onApply,
  allowFuture = false,
  disabled,
  label,
  icon,
  "aria-label": ariaLabel,
  variant = "outline",
  className,
}: DateTimePickerProps) {
  const [open, setOpen] = React.useState(false);
  const rootRef = React.useRef<HTMLDivElement>(null);
  const popRef = React.useRef<HTMLDivElement>(null);
  const triggerRef = React.useRef<HTMLButtonElement>(null);
  const focusedDayRef = React.useRef<HTMLButtonElement>(null);
  /* Set by the gestures that OWN the focus (opening, the arrow keys) and read
     by the effect below. A ref, not state: focus is an imperative fact about
     the DOM, and re-rendering to record "please focus" would be a render for
     nobody. Clicking a day deliberately does NOT set it — the browser already
     put focus on the button that was clicked. */
  const pendingFocusRef = React.useRef(false);

  /* The instant the popover opens on: the current value, or now when there
     isn't one yet. A function, so the `new Date()` only runs in the lazy
     initialisers below and on an actual open — not on every render. */
  const seed = () => value ?? new Date();

  // The draft is two strings, not a Date: the fields below are the manual path
  // and must survive being half-typed. A Date would round-trip "2026-08-" into
  // Invalid and eat the keystroke.
  const [dateStr, setDateStr] = React.useState(() => toDateInputValue(seed()));
  const [timeStr, setTimeStr] = React.useState(() => toTimeInputValue(seed()));
  const [view, setView] = React.useState(() => startOfDay(seed()));
  const [focusedDay, setFocusedDay] = React.useState(() => startOfDay(seed()));

  const today = startOfDay(new Date());

  const closeAndRefocus = React.useCallback(() => {
    setOpen(false);
    triggerRef.current?.focus();
  }, []);

  function openPopover() {
    // Reseed on every open: a draft abandoned three minutes ago is not what
    // the user means by "the current value" now.
    const base = seed();
    setDateStr(toDateInputValue(base));
    setTimeStr(toTimeInputValue(base));
    setView(startOfDay(base));
    setFocusedDay(startOfDay(base));
    pendingFocusRef.current = true;
    setOpen(true);
  }

  React.useEffect(() => {
    if (!open) return;
    function onPointerDown(e: MouseEvent) {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    }
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") {
        setOpen(false);
        triggerRef.current?.focus();
      }
    }
    document.addEventListener("mousedown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  /* Opening moves focus into the grid, at the selected day (leaving it on the
     trigger behind the popover would make Tab walk the page instead of the
     calendar), and the arrow keys then carry it — including across a month
     boundary, where the old cell is unmounted and focus would otherwise fall
     back to <body>. */
  React.useEffect(() => {
    if (!open || !pendingFocusRef.current) return;
    pendingFocusRef.current = false;
    focusedDayRef.current?.focus();
  }, [focusedDay, open]);

  /* A soft trap, not a modal one: Tab cycles inside the popover so the
     calendar can be driven to Apply without falling into the page behind it,
     while the page itself stays live (this is a dropdown, not a modal). */
  function onDialogKeyDown(e: React.KeyboardEvent) {
    if (e.key !== "Tab" || !popRef.current) return;
    const nodes = Array.from(
      popRef.current.querySelectorAll<HTMLElement>("button:not([disabled]), input:not([disabled])"),
    ).filter((n) => n.tabIndex >= 0);
    if (nodes.length === 0) return;
    const first = nodes[0];
    const last = nodes[nodes.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }

  const composed = composeLocal(dateStr, timeStr);
  const selectedDay = composeLocal(dateStr, "00:00");

  function pickDay(d: Date) {
    // The time of day is deliberately untouched: an operator narrowing down an
    // incident picks 02:15 once and then walks days looking for it.
    setDateStr(toDateInputValue(d));
    setFocusedDay(d);
    setView(startOfDay(d));
  }

  function onManualDate(next: string) {
    setDateStr(next);
    const parsed = composeLocal(next, "00:00");
    if (parsed) {
      setView(startOfDay(parsed));
      setFocusedDay(startOfDay(parsed));
    }
  }

  function applyPreset(minutes: number) {
    onApply(new Date(Date.now() - minutes * 60_000));
    closeAndRefocus();
  }

  function apply() {
    if (!composed) return;
    onApply(composed);
    closeAndRefocus();
  }

  function moveFocus(delta: number) {
    const next = addDays(focusedDay, delta);
    // Never walk into a day the Time Machine would clamp away — offering it and
    // then silently correcting it is the confusing half of the old control.
    // A picker opened with allowFuture has no such clamp to respect.
    if (!allowFuture && next > today) return;
    pendingFocusRef.current = true;
    setFocusedDay(next);
    setView(startOfDay(next));
  }

  function onGridKeyDown(e: React.KeyboardEvent, d: Date) {
    switch (e.key) {
      case "ArrowLeft":
        e.preventDefault();
        moveFocus(-1);
        break;
      case "ArrowRight":
        e.preventDefault();
        moveFocus(1);
        break;
      case "ArrowUp":
        e.preventDefault();
        moveFocus(-7);
        break;
      case "ArrowDown":
        e.preventDefault();
        moveFocus(7);
        break;
      case "Home":
        e.preventDefault();
        moveFocus(-((d.getDay() + 6) % 7));
        break;
      case "End":
        e.preventDefault();
        moveFocus(6 - ((d.getDay() + 6) % 7));
        break;
      default:
        break;
    }
  }

  const days = monthGrid(view);
  const atLastMonth =
    !allowFuture && view.getFullYear() === today.getFullYear() && view.getMonth() === today.getMonth();
  const triggerText = label ?? (value ? formatInstant(value) : "Pick a time");

  return (
    <div ref={rootRef} className="relative">
      <Button
        ref={triggerRef}
        type="button"
        variant={variant}
        size="sm"
        disabled={disabled}
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-label={ariaLabel}
        onClick={() => (open ? closeAndRefocus() : openPopover())}
        className={cn("h-7 gap-1.5 px-2 text-[13px]", className)}
      >
        {icon ?? <CalendarDays aria-hidden="true" className="size-3.5 shrink-0" />}
        {triggerText}
      </Button>

      {open ? (
        <div
          ref={popRef}
          role="dialog"
          aria-label="Choose a date and time"
          onKeyDown={onDialogKeyDown}
          className="absolute left-0 top-full z-50 mt-1.5 w-[17.5rem] rounded-lg border border-border bg-popover p-3 text-popover-foreground shadow-card"
        >
          <div role="group" aria-label="Quick ranges" className="flex flex-wrap gap-1">
            {PRESETS.map((p) => (
              <button
                key={p.label}
                type="button"
                onClick={() => applyPreset(p.minutes)}
                className="rounded-full bg-surface-2 px-2.5 py-1 text-[11px] font-medium text-muted-foreground transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                {p.label}
              </button>
            ))}
          </div>

          <div className="mt-3 flex items-center justify-between">
            <button
              type="button"
              aria-label="Previous month"
              onClick={() => setView(new Date(view.getFullYear(), view.getMonth() - 1, 1))}
              className="grid size-7 place-items-center rounded-md text-muted-foreground transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              <ChevronLeft aria-hidden="true" className="size-4" />
            </button>
            <span aria-live="polite" className="text-[13px] font-medium text-foreground">
              {MONTHS[view.getMonth()]} {view.getFullYear()}
            </span>
            <button
              type="button"
              aria-label="Next month"
              disabled={atLastMonth}
              onClick={() => setView(new Date(view.getFullYear(), view.getMonth() + 1, 1))}
              className="grid size-7 place-items-center rounded-md text-muted-foreground transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-30"
            >
              <ChevronRight aria-hidden="true" className="size-4" />
            </button>
          </div>

          <table role="grid" aria-label="Calendar" className="mt-1.5 w-full border-collapse">
            <thead>
              <tr>
                {WEEKDAYS.map((w) => (
                  <th
                    key={w.short}
                    scope="col"
                    abbr={w.long}
                    className="pb-1 text-[10px] font-medium uppercase tracking-wide text-muted-foreground"
                  >
                    {w.short}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {Array.from({ length: 6 }, (_, row) => (
                <tr key={row}>
                  {days.slice(row * 7, row * 7 + 7).map((d) => {
                    const outside = d.getMonth() !== view.getMonth();
                    const isFuture = !allowFuture && d > today;
                    const isToday = sameDay(d, today);
                    const isSelected = selectedDay !== null && sameDay(d, selectedDay);
                    const isFocused = sameDay(d, focusedDay);
                    return (
                      <td key={d.getTime()} role="gridcell" aria-selected={isSelected} className="p-0 text-center">
                        <button
                          type="button"
                          ref={isFocused ? focusedDayRef : undefined}
                          tabIndex={isFocused ? 0 : -1}
                          disabled={isFuture}
                          aria-label={`Choose ${dayLabel(d)}`}
                          aria-current={isToday ? "date" : undefined}
                          onClick={() => pickDay(d)}
                          onKeyDown={(e) => onGridKeyDown(e, d)}
                          className={cn(
                            "m-0.5 size-8 rounded-md text-[13px] tabular-nums transition-colors duration-(--dur-fast) ease-(--ease)",
                            "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                            "disabled:pointer-events-none disabled:opacity-30",
                            isSelected
                              ? "bg-primary font-medium text-primary-foreground"
                              : "hover:bg-accent hover:text-accent-foreground",
                            !isSelected && isToday && "ring-1 ring-inset ring-border-strong",
                            !isSelected && outside && "text-muted-foreground/60",
                            !isSelected && !outside && "text-foreground",
                          )}
                        >
                          {d.getDate()}
                        </button>
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>

          <div className="mt-2.5 flex items-center gap-1.5 border-t border-border pt-2.5">
            <input
              type="date"
              aria-label="Date"
              value={dateStr}
              max={allowFuture ? undefined : toDateInputValue(today)}
              onChange={(e) => onManualDate(e.target.value)}
              className="h-8 min-w-0 flex-1 rounded-md bg-surface-2 px-2 text-[13px] text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            />
            <input
              type="time"
              aria-label="Time"
              value={timeStr}
              onChange={(e) => setTimeStr(e.target.value)}
              className="h-8 w-[5.5rem] rounded-md bg-surface-2 px-2 text-[13px] tabular-nums text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            />
            <Button
              type="button"
              variant="ghost"
              size="sm"
              aria-label="Set the time to now"
              onClick={() => setTimeStr(toTimeInputValue(new Date()))}
              className="h-8 px-2 text-muted-foreground"
            >
              Now
            </Button>
          </div>

          <div className="mt-2.5 flex items-center justify-end gap-2">
            <Button type="button" variant="ghost" size="sm" className="h-8" onClick={closeAndRefocus}>
              Cancel
            </Button>
            <Button type="button" size="sm" className="h-8" disabled={composed === null} onClick={apply}>
              Apply
            </Button>
          </div>
        </div>
      ) : null}
    </div>
  );
}
