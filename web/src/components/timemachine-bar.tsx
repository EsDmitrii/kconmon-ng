import { History } from "lucide-react";
import { Button } from "@/components/ui/button";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { localeTag, useLocale, useT, type Locale } from "@/lib/i18n";
import { chromeDict } from "@/lib/i18n/dict/chrome";
import { useTimeMachine } from "@/lib/timemachine";

/**
 * TimeMachineBar is the top-bar control from TIME_MACHINE.md; Live: nothing but a compact toggle —
 * the console's normal state must not pay a banner for a feature it is not using.
 */
/**
 * TIME_MACHINE_TRIGGER_SELECTOR is the seam the command palette engages through: "Toggle Time
 * Machine" needs an INSTANT.
 */
export const TIME_MACHINE_TRIGGER_SELECTOR = "[data-timemachine-trigger] button";

/** The banner and the picker chip sat two inches apart naming the same instant
 *  in two formats. Every stamp here lands INSIDE a translated sentence, so it
 *  takes the interface language's format rather than the runtime default. */
function stamp(d: Date, locale: Locale): string {
  return d.toLocaleString(localeTag(locale));
}

export function TimeMachineBar() {
  const { at, isLive, engage, returnToLive } = useTimeMachine();
  const t = useT(chromeDict);
  const { locale } = useLocale();

  if (isLive) {
    return (
      <div className="flex items-center gap-2 border-b border-border bg-background px-5 py-1 text-[13px]">
        {/* `contents` so the marker element generates no box at all: the picker
            stays a direct flex child of the bar and the layout is byte-for-byte
            what it was before this wrapper existed. */}
        <span className="contents" data-timemachine-trigger="">
          <DateTimePicker
            value={null}
            onApply={engage}
            aria-label={t("timemachine.trigger")}
            label={t("timemachine.label")}
            icon={<History aria-hidden="true" className="size-3.5 shrink-0" />}
            variant="ghost"
            className="text-muted-foreground"
          />
        </span>
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
        {/* {at} is a stamp, not translated prose — but it is formatted in the
            sentence's own language (lib/i18n's localeTag). */}
        <span className="font-medium">{t("timemachine.viewing", { at: stamp(at!, locale) })}</span>{" "}
        <span className="text-muted-foreground">{t("timemachine.viewingHint")}</span>
      </span>
      <DateTimePicker
        value={at}
        onApply={engage}
        // The chip states the instant in the banner's own words, not the
        // picker's default format.
        label={stamp(at!, locale)}
        // The visible label is the instant itself, so the accessible name
        // carries it too (never replaces it) — a name that dropped the value
        // would leave a screen reader with an unlabelled "button".
        aria-label={t("timemachine.change", { at: stamp(at!, locale) })}
        variant="outline"
      />
      <Button variant="outline" size="sm" className="h-7" onClick={() => returnToLive()}>
        {t("timemachine.returnToLive")}
      </Button>
    </div>
  );
}
