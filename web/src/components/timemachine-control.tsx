import { History } from "lucide-react";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { stampFull, useLocale, useT, type Locale } from "@/lib/i18n";
import { chromeDict } from "@/lib/i18n/dict/chrome";
import { useTimeMachineControls } from "@/lib/timemachine";

/**
 * TIME_MACHINE_TRIGGER_SELECTOR is the seam the command palette engages through: "Toggle Time
 * Machine" needs an INSTANT.
 */
export const TIME_MACHINE_TRIGGER_SELECTOR = "[data-timemachine-trigger] button";

/** The banner and the trigger named the same instant in two formats. Every stamp
 *  here lands INSIDE a translated sentence, so it takes the interface language's
 *  format rather than the runtime default. */
function stamp(d: Date, locale: Locale): string {
  /* stampFull, for the same reason the banner beside it takes stampFull: this chip and that banner
     print the SAME instant, and a bare toLocaleString gave one of them the browser default — 12-hour
     in en-US — so one screen showed "15:34:00" and "3:34:00 PM" for one moment. */
  return stampFull(d, locale);
}

/**
 * TimeMachineControl is the Time Machine's trigger, and it lives in the PAGE HEADER beside the
 * range presets rather than in the chrome.
 *
 * As a strip across the top it was the same two words on every page, next to nothing, and the
 * reader who wanted a deeper window did not connect it to the 15m/1h/6h/24h he was looking at
 * (owner report). The presets pick how long the window is; this picks where it ends, so the two
 * belong in one row.
 */
export function TimeMachineControl() {
  const tm = useTimeMachineControls();
  const t = useT(chromeDict);
  const { locale } = useLocale();

  // No provider above: no machine, so no control — a dead trigger is worse than none.
  if (!tm) return null;

  const at = tm.at;
  return (
    /* `contents` so the marker generates no box: the picker stays a direct flex
       child of the header's action row. */
    <span className="contents" data-timemachine-trigger="" title={t("timemachine.hint")}>
      <DateTimePicker
        value={at}
        onApply={tm.engage}
        label={at ? stamp(at, locale) : t("timemachine.now")}
        aria-label={at ? t("timemachine.change", { at: stamp(at, locale) }) : t("timemachine.trigger")}
        icon={<History aria-hidden="true" className="size-3.5 shrink-0" />}
        variant="outline"
        /* Engaged, the trigger carries the banner's own colour: the header is
           where the reader is looking, and "you are in the past" has to be
           visible from there without scrolling up. */
        className={at ? "border-health-warn/60 bg-health-warn-soft/60" : "text-muted-foreground"}
      />
    </span>
  );
}
