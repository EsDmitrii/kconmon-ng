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

export interface PickerPreset {
  label: string;
  /** SIGNED minutes from now: negative reaches back, positive reaches forward.
   *  One field rather than a direction flag, so applyPreset is one addition. */
  minutes: number;
}

/** PAST_PRESETS are the jumps the spec calls out ("the last hour" and friends)
 *  — one click, no grid, no Apply. The Time Machine's own set. */
export const PAST_PRESETS: readonly PickerPreset[] = [
  { label: "15m ago", minutes: -15 },
  { label: "1h ago", minutes: -60 },
  { label: "6h ago", minutes: -360 },
  { label: "24h ago", minutes: -1440 },
];

/**
 * FUTURE_PRESETS is the allowFuture set (QA round 3, finding #17).
 *
 * The backward presets were shown on every picker, including the maintenance
 * form's two — where the grid disables nothing ahead of today because a change
 * window is DECLARED IN ADVANCE. So the one control offering a shortcut offered
 * four shortcuts that all pointed the wrong way: an operator planning tomorrow's
 * switch upgrade could click "24h ago" and land a day BEFORE the window they
 * came to write. A preset row is a statement about where this field usually
 * goes, and on a forward-looking field it has to point forward.
 *
 * "tomorrow" is +24h rather than "the next midnight" on purpose: every other
 * entry here is an offset from now, and a preset that silently changed KIND
 * would be the one an operator gets wrong.
 */
export const FUTURE_PRESETS: readonly PickerPreset[] = [
  { label: "in 15m", minutes: 15 },
  { label: "in 1h", minutes: 60 },
  { label: "in 6h", minutes: 360 },
  { label: "tomorrow", minutes: 1440 },
];

/**
 * POPOVER_MIN_HEIGHT_PX is the popover's own drawn height, near enough: the
 * preset row, the month header, six calendar rows of 36px, the field row and
 * the button row. It is a CONSTANT rather than a measurement because the
 * direction has to be decided BEFORE the popover exists — measuring it would
 * mean rendering it downward first and flipping it under the cursor.
 */
export const POPOVER_MIN_HEIGHT_PX = 420;

/**
 * pickerDropDirection decides which way the popover opens (QA round 3,
 * finding #16).
 *
 * The popover was unconditionally `top-full`, so a trigger low in the viewport
 * — a maintenance row at the bottom of a long Investigate page, an annotation
 * bar under a chart — put the whole calendar below the fold: Apply and Cancel
 * were unreachable without scrolling the page behind an open popover.
 *
 * Downward is still the DEFAULT and the tie-break, because down is where a
 * dropdown belongs and flipping it is a surprise worth spending only when the
 * alternative is unusable. Non-finite inputs (no layout to measure — jsdom, a
 * detached node) answer "down": the fallback must be the ordinary case, never
 * the exception.
 *
 * QA round 5, finding #3 narrowed the flip. The old rule took the LARGER side
 * once below fell short, so a trigger with 310px below and 390px above opened
 * UP into 390px — and the popover needs 420, so the presets and the month
 * header went off the TOP of the viewport, where nothing can scroll to them
 * (the page cannot scroll past y=0). Downward overflow is recoverable; upward
 * overflow is not. So up is now EARNED by fitting, not won by comparison:
 * spaceBelow short AND spaceAbove genuinely large enough.
 */
export function pickerDropDirection(spaceBelow: number, spaceAbove: number): "down" | "up" {
  if (!Number.isFinite(spaceBelow) || !Number.isFinite(spaceAbove)) return "down";
  if (spaceBelow >= POPOVER_MIN_HEIGHT_PX) return "down";
  return spaceAbove >= POPOVER_MIN_HEIGHT_PX ? "up" : "down";
}

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

/** WheelColumn is one scrollable option list of the two wheel panels (time,
 *  month & year). Options are tabIndex -1 on purpose: the wheels are the
 *  pointer path, the inputs and the grid are the keyboard path, and the
 *  popover's soft Tab trap must not grow 60 stops. */
function WheelColumn({
  label,
  width,
  options,
  onPick,
}: {
  label: string;
  width: string;
  options: { value: number; text: string; selected: boolean; disabled?: boolean }[];
  onPick: (value: number) => void;
}) {
  return (
    <div role="listbox" aria-label={label} className={cn("max-h-44 overflow-y-auto overscroll-contain", width)}>
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="option"
          aria-selected={o.selected}
          tabIndex={-1}
          disabled={o.disabled}
          onClick={() => onPick(o.value)}
          className={cn(
            "block w-full rounded-md px-2 py-1 text-center text-[13px] tabular-nums transition-colors duration-(--dur-fast) ease-(--ease)",
            "disabled:pointer-events-none disabled:opacity-30",
            o.selected
              ? "bg-primary font-medium text-primary-foreground"
              : "text-foreground hover:bg-accent hover:text-accent-foreground",
          )}
        >
          {o.text}
        </button>
      ))}
    </div>
  );
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
  /** Opt OUT of days before today — the mirror image of allowFuture, for a
   *  field that is FUTURE-ONLY (QA round 5, finding #12). The schedules form's
   *  "Run at" is the case: a one-shot run scheduled for last Tuesday is an
   *  instruction nothing can carry out, and the picker offered a whole
   *  calendar of them. Separate from allowFuture rather than folded into a
   *  three-way mode, because the two clamps are independent: the Time Machine
   *  wants the past only, maintenance wants both, Run-at wants the future
   *  only. The Time Machine picker passes neither flag and is UNCHANGED. */
  disablePast?: boolean;
  disabled?: boolean;
  /** Trigger text. Defaults to the formatted value (or "Pick a time"); pass it
   *  when the trigger has a name of its own, as the Live-state bar does. */
  label?: React.ReactNode;
  /** Trigger icon, defaulting to a calendar. */
  icon?: React.ReactNode;
  "aria-label": string;
  /** Marks the TRIGGER invalid, for a caller whose server rejected the instant
   *  this control produced (pages/targets.tsx's schedule form). Forwarded, not
   *  interpreted: the picker has no opinion about what makes an instant wrong,
   *  and the message lives beside it where the caller put it. */
  "aria-invalid"?: boolean;
  variant?: ButtonProps["variant"];
  className?: string;
}

export function DateTimePicker({
  value,
  onApply,
  allowFuture = false,
  disablePast = false,
  disabled,
  label,
  icon,
  "aria-label": ariaLabel,
  "aria-invalid": ariaInvalid,
  variant = "outline",
  className,
}: DateTimePickerProps) {
  const [open, setOpen] = React.useState(false);
  const rootRef = React.useRef<HTMLDivElement>(null);
  const popRef = React.useRef<HTMLDivElement>(null);
  const triggerRef = React.useRef<HTMLButtonElement>(null);
  const focusedDayRef = React.useRef<HTMLButtonElement>(null);
  /* The calendar table, so changeView can ask whether the grid currently owns
     DOM focus before it moves any. */
  const gridRef = React.useRef<HTMLTableElement>(null);
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
  /* The time wheel: two scrollable columns that open on the time field itself.
     The native indicator (and the browser dropdown behind it) is hidden — one
     popover should not sprout a second, foreign-looking one — so this panel is
     the pointer path for the wall clock, and typing in the field remains the
     keyboard path. */
  const [timeOpen, setTimeOpen] = React.useState(false);
  const timeWrapRef = React.useRef<HTMLDivElement>(null);
  const timeInputRef = React.useRef<HTMLInputElement>(null);
  /* The month & year panel: same wheel pattern, hung off the header, so a
     date three years back is two clicks instead of forty chevrons. */
  const [monthYearOpen, setMonthYearOpen] = React.useState(false);
  const monthYearWrapRef = React.useRef<HTMLDivElement>(null);
  /* pickMinute hands focus back to the field; without this flag that very
     focus would re-open the wheel the pick just closed. */
  const suppressWheelRef = React.useRef(false);
  /* Which way the popover opens, decided ONCE at open time from the trigger's
     box (QA round 3, finding #16). State, not a ref: the class it produces is
     rendered, so the render that mounts the popover has to see it. */
  const [drop, setDrop] = React.useState<"down" | "up">("down");

  const today = startOfDay(new Date());

  const closeAndRefocus = React.useCallback(() => {
    setOpen(false);
    triggerRef.current?.focus();
  }, []);

  /* The measurement half of finding #16, kept next to the decision it feeds.
     Guarded on every step: jsdom implements getBoundingClientRect as all-zeros
     and may not define visualViewport at all, and a picker that throws while
     deciding which way to open would be a worse bug than opening the wrong
     way. Every failure lands on "down", the ordinary case. */
  const measureDropDirection = React.useCallback((): "down" | "up" => {
    const trigger = triggerRef.current;
    if (!trigger || typeof trigger.getBoundingClientRect !== "function") return "down";
    const rect = trigger.getBoundingClientRect();
    const viewportHeight = window.innerHeight || document.documentElement?.clientHeight || 0;
    if (viewportHeight <= 0) return "down";
    return pickerDropDirection(viewportHeight - rect.bottom, rect.top);
  }, []);

  /* The direction is decided at open time AND kept honest afterwards (QA round
     5, finding #3): a window resized — or a phone rotated — under an open
     popover changes which side has room, and an anchoring measured against the
     old viewport is exactly the off-screen popover this listener exists to
     avoid. One cheap listener, only while open, no rAF: resize already
     coalesces and the handler is two reads and a compare. */
  React.useEffect(() => {
    if (!open) return;
    function remeasure() {
      setDrop(measureDropDirection());
    }
    window.addEventListener("resize", remeasure);
    return () => window.removeEventListener("resize", remeasure);
  }, [open, measureDropDirection]);

  function openPopover() {
    // Reseed on every open: a draft abandoned three minutes ago is not what
    // the user means by "the current value" now.
    const base = seed();
    setDateStr(toDateInputValue(base));
    setTimeStr(toTimeInputValue(base));
    setView(startOfDay(base));
    setFocusedDay(startOfDay(base));
    setTimeOpen(false);
    setMonthYearOpen(false);
    setDrop(measureDropDirection());
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

  /* The wheels close like any menu: a press anywhere they don't own, or
     Escape (handled at their own element, where it stops short of the
     popover's own Escape — one key, one layer). */
  React.useEffect(() => {
    if (!timeOpen && !monthYearOpen) return;
    function onPointerDown(e: MouseEvent) {
      const t = e.target as Node;
      if (timeOpen && timeWrapRef.current && !timeWrapRef.current.contains(t)) setTimeOpen(false);
      if (monthYearOpen && monthYearWrapRef.current && !monthYearWrapRef.current.contains(t)) setMonthYearOpen(false);
    }
    document.addEventListener("mousedown", onPointerDown);
    return () => document.removeEventListener("mousedown", onPointerDown);
  }, [timeOpen, monthYearOpen]);

  /* Opening centres each column on its selected row — a wheel that greets the
     user at 00 when the draft says 15:34 is a list, not a wheel. Guarded:
     jsdom has no scrollIntoView. */
  React.useEffect(() => {
    if (!timeOpen && !monthYearOpen) return;
    for (const wrap of [timeWrapRef.current, monthYearWrapRef.current]) {
      const selected = wrap?.querySelectorAll<HTMLElement>('[aria-selected="true"]') ?? [];
      for (const el of selected) el.scrollIntoView?.({ block: "center" });
    }
  }, [timeOpen, monthYearOpen]);

  const composed = composeLocal(dateStr, timeStr);
  const selectedDay = composeLocal(dateStr, "00:00");
  const timeParts = TIME_RE.test(timeStr) ? timeStr.split(":").map(Number) : null;

  function pickHour(h: number) {
    // Hour first, minute second is the natural reading order; the wheel stays
    // up between the two so the pick is one gesture, not two round trips.
    setTimeStr(`${pad(h)}:${pad(timeParts ? timeParts[1] : 0)}`);
  }

  function pickMinute(m: number) {
    setTimeStr(`${pad(timeParts ? timeParts[0] : 0)}:${pad(m)}`);
    setTimeOpen(false);
    suppressWheelRef.current = true;
    timeInputRef.current?.focus();
  }

  const thisMonthStart = new Date(today.getFullYear(), today.getMonth(), 1);

  /**
   * changeView is the ONE way the visible month moves — the chevrons, the
   * month wheel and the year wheel all go through it — because paging has a
   * second half that is easy to forget and was (QA round 1, finding #7).
   *
   * The grid keeps exactly one cell in the tab order: the one matching
   * focusedDay. Page two months away and focusedDay is no longer drawn at all,
   * so the grid has NO tab stop and a keyboard user who reaches it lands
   * nowhere — the arrows have nothing to move from. So a view change that
   * leaves focusedDay behind carries it to the 1st of the new month.
   *
   * DOM focus is a separate question and moves only if the grid already had
   * it. Clicking a chevron puts focus on the chevron, and a control that
   * yanked focus into the grid on every click would make repeated paging
   * impossible with the mouse and surprising with the keyboard.
   */
  function changeView(next: Date) {
    setView(next);
    if (focusedDay.getFullYear() === next.getFullYear() && focusedDay.getMonth() === next.getMonth()) return;
    const first = new Date(next.getFullYear(), next.getMonth(), 1);
    // The 1st of the CURRENT month is never in the future, but the clamp is
    // stated rather than assumed: a caller with allowFuture off must never end
    // up focused on a day the grid has disabled. disablePast is the mirror —
    // the 1st of THIS month is in the past for all but one day a month, and
    // landing the only tab stop on a dead cell is the same bug reflected.
    const landing = !allowFuture && first > today ? today : disablePast && first < today ? today : first;
    const grid = gridRef.current;
    if (grid && document.activeElement instanceof Node && grid.contains(document.activeElement)) {
      pendingFocusRef.current = true;
    }
    setFocusedDay(landing);
  }

  function pickViewMonth(m: number) {
    // Year first, month second is how "March 2024" is aimed at; the month is
    // the closing pick, mirroring the time wheel's hour-then-minute.
    changeView(new Date(view.getFullYear(), m, 1));
    setMonthYearOpen(false);
  }

  function pickViewYear(y: number) {
    const candidate = new Date(y, view.getMonth(), 1);
    // A year hop can strand the view in a month that hasn't happened
    // (December 2024 -> 2026): land on today's month instead of an all-dead grid.
    // Symmetrically for disablePast: January 2027 -> 2026 is an all-dead grid too.
    const stranded =
      (!allowFuture && candidate > thisMonthStart) || (disablePast && candidate < thisMonthStart);
    changeView(stranded ? thisMonthStart : candidate);
  }

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

  /* Signed minutes: PAST_PRESETS carry negatives, FUTURE_PRESETS positives, so
     one addition serves both sets. */
  function applyPreset(minutes: number) {
    onApply(new Date(Date.now() + minutes * 60_000));
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
    // …and never behind today on a future-only field (#12): the arrows must
    // stop where the grid's disabled cells start, or the one tab stop lands on
    // a button that cannot be pressed.
    if (disablePast && next < today) return;
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

  /* Ten years of history is beyond any retention this console will hold;
     allowFuture (maintenance windows) adds two years of advance planning. A
     future-only field gets only the forward half — a column of nine dead years
     is a wheel that scrolls past nothing. */
  const yearRange = disablePast
    ? Array.from({ length: 3 }, (_, i) => today.getFullYear() + i)
    : Array.from({ length: 10 + (allowFuture ? 2 : 0) }, (_, i) => today.getFullYear() - 9 + i);

  /* Compared against a fresh now rather than `today`: the clamp the caller
     applies is to the INSTANT, and 23:00 on today is in the future while
     today's date is not. */
  const futureDraft = !allowFuture && composed !== null && composed.getTime() > Date.now();

  /* The preset row follows the field's DIRECTION, not the component's default
     (finding #17): a picker that accepts future instants is one an operator
     opens to reach forward. disablePast then drops anything that still points
     backward (finding #12) — belt and braces, so a caller that sets the flag
     without allowFuture cannot end up with four one-click trips into the dead
     half of its own calendar. */
  const presets = (allowFuture ? FUTURE_PRESETS : PAST_PRESETS).filter((p) => !disablePast || p.minutes >= 0);

  const days = monthGrid(view);
  const atLastMonth =
    !allowFuture && view.getFullYear() === today.getFullYear() && view.getMonth() === today.getMonth();
  const atFirstMonth =
    disablePast && view.getFullYear() === today.getFullYear() && view.getMonth() === today.getMonth();
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
        aria-invalid={ariaInvalid ? true : undefined}
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
          data-drop={drop}
          className={cn(
            "absolute left-0 z-50 w-[17.5rem] rounded-lg border border-border bg-popover p-3 text-popover-foreground shadow-card",
            /* One of the two, never both — the class IS the direction, so a
               test can read it without a layout engine (finding #16). */
            drop === "up" ? "bottom-full mb-1.5" : "top-full mt-1.5",
          )}
        >
          <div role="group" aria-label="Quick ranges" className="flex flex-wrap gap-1">
            {presets.map((p) => (
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
              disabled={atFirstMonth}
              onClick={() => changeView(new Date(view.getFullYear(), view.getMonth() - 1, 1))}
              className="grid size-7 place-items-center rounded-md text-muted-foreground transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-30"
            >
              <ChevronLeft aria-hidden="true" className="size-4" />
            </button>
            <div ref={monthYearWrapRef} className="relative">
              <button
                type="button"
                aria-haspopup="listbox"
                aria-expanded={monthYearOpen}
                aria-label="Choose the month and year"
                onClick={() => setMonthYearOpen((v) => !v)}
                onKeyDown={(e) => {
                  if (e.key === "Escape" && monthYearOpen) {
                    e.stopPropagation();
                    setMonthYearOpen(false);
                  }
                }}
                className="rounded-md px-2 py-0.5 text-[13px] font-medium text-foreground transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                <span aria-live="polite">
                  {MONTHS[view.getMonth()]} {view.getFullYear()}
                </span>
              </button>
              {monthYearOpen ? (
                <div
                  role="group"
                  aria-label="Pick a month and year"
                  className="absolute left-1/2 top-full z-50 mt-1 flex -translate-x-1/2 gap-1 rounded-lg border border-border bg-popover p-1 shadow-card"
                >
                  <WheelColumn
                    label="Month"
                    width="w-26"
                    options={MONTHS.map((name, m) => ({
                      value: m,
                      text: name,
                      selected: m === view.getMonth(),
                      disabled:
                        (!allowFuture && new Date(view.getFullYear(), m, 1) > thisMonthStart) ||
                        (disablePast && new Date(view.getFullYear(), m, 1) < thisMonthStart),
                    }))}
                    onPick={pickViewMonth}
                  />
                  <WheelColumn
                    label="Year"
                    width="w-16"
                    options={yearRange.map((y) => ({
                      value: y,
                      text: String(y),
                      selected: y === view.getFullYear(),
                    }))}
                    onPick={pickViewYear}
                  />
                </div>
              ) : null}
            </div>
            <button
              type="button"
              aria-label="Next month"
              disabled={atLastMonth}
              onClick={() => changeView(new Date(view.getFullYear(), view.getMonth() + 1, 1))}
              className="grid size-7 place-items-center rounded-md text-muted-foreground transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-30"
            >
              <ChevronRight aria-hidden="true" className="size-4" />
            </button>
          </div>

          <table ref={gridRef} role="grid" aria-label="Calendar" className="mt-1.5 w-full border-collapse">
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
                    const isPast = disablePast && d < today;
                    const isToday = sameDay(d, today);
                    const isSelected = selectedDay !== null && sameDay(d, selectedDay);
                    const isFocused = sameDay(d, focusedDay);
                    return (
                      <td key={d.getTime()} role="gridcell" aria-selected={isSelected} className="p-0 text-center">
                        <button
                          type="button"
                          ref={isFocused ? focusedDayRef : undefined}
                          tabIndex={isFocused ? 0 : -1}
                          disabled={isFuture || isPast}
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
            {/* The native indicators are hidden on BOTH fields: the date one
                opens the browser's own calendar right under ours — two
                calendars for one instant. And with the icons gone the fields
                are sized to their digits — a box wider than its text reads as
                a control with something missing. The grid above is the pointer
                path for the date, the wheel below for the time; these fields
                are the typing path, and they stay plain text boxes. */}
            <input
              type="date"
              aria-label="Date"
              value={dateStr}
              max={allowFuture ? undefined : toDateInputValue(today)}
              min={disablePast ? toDateInputValue(today) : undefined}
              onChange={(e) => onManualDate(e.target.value)}
              className="h-8 w-[6.75rem] rounded-md bg-surface-2 px-2 text-center text-[13px] tabular-nums text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring [&::-webkit-calendar-picker-indicator]:hidden"
            />
            <div ref={timeWrapRef} className="relative">
              <input
                ref={timeInputRef}
                type="time"
                aria-label="Time"
                value={timeStr}
                onChange={(e) => setTimeStr(e.target.value)}
                onFocus={() => {
                  if (suppressWheelRef.current) {
                    suppressWheelRef.current = false;
                    return;
                  }
                  setTimeOpen(true);
                }}
                onClick={() => setTimeOpen(true)}
                onKeyDown={(e) => {
                  if (e.key === "Escape" && timeOpen) {
                    e.stopPropagation();
                    setTimeOpen(false);
                  }
                }}
                className="h-8 w-[4.5rem] rounded-md bg-surface-2 px-2 text-center text-[13px] tabular-nums text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring [&::-webkit-calendar-picker-indicator]:hidden"
              />
              {timeOpen ? (
                <div
                  role="group"
                  aria-label="Pick a time"
                  /* UPWARD (QA round 1, finding #6). The time field is the
                     second-to-last row of the popover, so a downward panel
                     landed squarely on Cancel and Apply — the two controls
                     that end the interaction, unreachable by pointer until
                     the wheel was dismissed. Opening up overlays the grid,
                     which is this popover's own surface and costs nothing:
                     the day is already chosen by the time the clock is being
                     aimed at. The month & year panel above stays downward for
                     the same reason — it covers only the grid. */
                  className="absolute bottom-full right-0 z-50 mb-1 flex gap-1 rounded-lg border border-border bg-popover p-1 shadow-card"
                >
                  {(
                    [
                      { label: "Hour", count: 24, selected: timeParts?.[0], pick: pickHour },
                      { label: "Minute", count: 60, selected: timeParts?.[1], pick: pickMinute },
                    ] as const
                  ).map((col) => (
                    <WheelColumn
                      key={col.label}
                      label={col.label}
                      width="w-12"
                      options={Array.from({ length: col.count }, (_, n) => ({
                        value: n,
                        text: pad(n),
                        selected: col.selected === n,
                      }))}
                      onPick={col.pick}
                    />
                  ))}
                </div>
              ) : null}
            </div>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              aria-label="Set the date and time to now"
              onClick={() => {
                // The whole instant, not just the wall clock: "Now" is an
                // escape hatch back to the present, and a draft parked three
                // weeks deep in the grid must come home on both axes.
                const now = new Date();
                setDateStr(toDateInputValue(now));
                setTimeStr(toTimeInputValue(now));
                setView(startOfDay(now));
                setFocusedDay(startOfDay(now));
              }}
              className="ml-auto h-8 px-2 text-muted-foreground"
            >
              Now
            </Button>
          </div>

          {/* The clamp, said BEFORE it happens (QA round 1, finding #11). The
              grid disables future days, but the time field cannot: 23:00 typed
              on today is a legal wall clock and a future instant, and the
              store silently rewrote it to now on Apply. Apply stays enabled —
              the clamp is real and the result is a usable view, so refusing
              the click would trade a surprise for a dead end. */}
          {futureDraft ? (
            <p data-testid="future-hint" className="mt-2 text-[11px] leading-relaxed text-muted-foreground">
              In the future — will engage at now.
            </p>
          ) : null}

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
