import { History } from "lucide-react";
import { Button } from "@/components/ui/button";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { useTimeMachine } from "@/lib/timemachine";

/**
 * TimeMachineBar is the top-bar control from TIME_MACHINE.md, mounted in
 * AppShell's flex column as a sibling of AnonymousBanner.
 *
 * Live: nothing but a compact toggle — the console's normal state must not pay
 * a banner for a feature it is not using. The toggle IS the picker's trigger,
 * so one click lands straight in the calendar popover, where "1h ago" is one
 * more click and nothing has been typed.
 * Engaged: the amber banner, verbatim from the spec ("You are viewing … —
 * return to Live to act"), carrying its own escape hatch (Return to Live) and
 * the same picker, now showing the engaged instant, so the explanation and the
 * fix sit in the same row.
 *
 * role="status" (polite), not role="alert": engaging Time Machine is a thing
 * the user just did on purpose, not an interruption. AnonymousBanner keeps
 * role="alert" — it reports a console-wide security posture nobody chose in
 * this session.
 *
 * The raw <input type="datetime-local"> this bar used to carry is gone by owner
 * request: it read as a debug field, and aiming its spinner at a day last week
 * was work. Manual entry did not go with it — it moved INSIDE the popover, as
 * a plain date field next to the time field (ui/datetime-picker.tsx), so a
 * keyboard user still types the whole instant and never touches the grid.
 * Engaging is now one explicit Apply (or one preset chip) rather than a change
 * event per keystroke, which also stops half-typed years from engaging.
 */
/**
 * TIME_MACHINE_TRIGGER_LABEL is the Live-state trigger's accessible name, and
 * the seam the command palette engages through: "Toggle Time Machine" needs an
 * INSTANT, which a palette has no honest way to choose, so it opens THIS
 * control instead of guessing one (components/command-palette.tsx). Exported
 * rather than duplicated so the selector and the label can never drift apart.
 */
export const TIME_MACHINE_TRIGGER_LABEL = "Time Machine — view the console at a past time";

/**
 * stamp is the bar's ONE clock (QA round 1, finding #15).
 *
 * The banner and the picker chip sat two inches apart naming the same instant
 * in two formats — "8/8/2026, 3:04:05 PM" beside "Aug 8, 2026, 03:04 PM" — and
 * a reader comparing them has to work out whether they even agree. They are
 * now the same string from the same call. toLocaleString with no options is
 * deliberate: it is the viewer's own full date and time, seconds included,
 * and the console has no business overriding a locale's own answer to "what
 * time is it".
 *
 * ui/datetime-picker's formatInstant keeps its callers — it is the trigger's
 * DEFAULT label, sized for a control that has no instant of its own to state.
 */
function stamp(d: Date): string {
  return d.toLocaleString();
}

export function TimeMachineBar() {
  const { at, isLive, engage, returnToLive } = useTimeMachine();

  if (isLive) {
    return (
      <div className="flex items-center gap-2 border-b border-border bg-background px-5 py-1 text-[13px]">
        <DateTimePicker
          value={null}
          onApply={engage}
          aria-label={TIME_MACHINE_TRIGGER_LABEL}
          label="Time Machine"
          icon={<History aria-hidden="true" className="size-3.5 shrink-0" />}
          variant="ghost"
          className="text-muted-foreground"
        />
      </div>
    );
  }

  return (
    <div
      role="status"
      className="flex flex-wrap items-center gap-2.5 border-b border-border bg-health-warn-soft/60 px-5 py-1.5 text-[13px] text-foreground"
    >
      <History aria-hidden="true" className="size-3.5 shrink-0 text-health-warn" />
      <span className="min-w-0">
        <span className="font-medium">You are viewing {stamp(at!)}</span>{" "}
        <span className="text-muted-foreground">— return to Live to act.</span>
      </span>
      <DateTimePicker
        value={at}
        onApply={engage}
        // The chip states the instant in the banner's own words, not the
        // picker's default format.
        label={stamp(at!)}
        // The visible label is the instant itself, so the accessible name
        // carries it too (never replaces it) — a name that dropped the value
        // would leave a screen reader with an unlabelled "button".
        aria-label={`Change the viewing time — currently ${stamp(at!)}`}
        variant="outline"
      />
      <Button variant="outline" size="sm" className="h-7" onClick={() => returnToLive()}>
        Return to Live
      </Button>
    </div>
  );
}
