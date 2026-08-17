import { History } from "lucide-react";
import { Button } from "@/components/ui/button";
import { stampFull, useLocale, useT, type Locale } from "@/lib/i18n";
import { chromeDict } from "@/lib/i18n/dict/chrome";
import { useTimeMachine } from "@/lib/timemachine";

/**
 * TimeMachineBar is the ENGAGED state's banner and nothing else: "you are in the past, writes are
 * off" is a fact about the whole console, so it belongs in the chrome.
 *
 * Live it renders nothing. The trigger moved to components/timemachine-control.tsx, into the page
 * header beside the range presets — as a permanent top strip it was two words with no context and
 * the reader never connected it to the time filters he was actually looking at (owner report).
 */

/** Both stamps land INSIDE a translated sentence, so they take the interface
 *  language's format rather than the runtime default. */
function stamp(d: Date, locale: Locale): string {
  /* stampFull, not a bare toLocaleString: the default is 12-hour in en-US, so this banner read
     "8/17/2026, 3:17:21 PM" directly above rows the same page stamps "15:17:21". The console has one
     clock and it is the 24-hour one (lib/i18n's HOUSE_CLOCK). */
  return stampFull(d, locale);
}

export function TimeMachineBar() {
  const { at, isLive, returnToLive } = useTimeMachine();
  const t = useT(chromeDict);
  const { locale } = useLocale();

  if (isLive) return null;

  return (
    <div
      role="status"
      className="flex flex-wrap items-center gap-2.5 border-b border-border bg-health-warn-soft/60 px-5 py-1.5 text-[13px] text-foreground"
    >
      <History aria-hidden="true" className="size-3.5 shrink-0 text-health-warn" />
      <span className="min-w-0">
        <span className="font-medium">{t("timemachine.viewing", { at: stamp(at!, locale) })}</span>{" "}
        <span className="text-muted-foreground">{t("timemachine.viewingHint")}</span>
      </span>
      <Button variant="outline" size="sm" className="h-7" onClick={() => returnToLive()}>
        {t("timemachine.returnToLive")}
      </Button>
    </div>
  );
}
