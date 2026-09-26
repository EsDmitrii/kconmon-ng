import { History } from "lucide-react";
import { TIME_MACHINE_TRIGGER_SELECTOR } from "@/components/timemachine-control";
import { Button } from "@/components/ui/button";
import { stampFull, useLocale, useT, type Locale } from "@/lib/i18n";
import { chromeDict } from "@/lib/i18n/dict/chrome";
import { useTimeMachine } from "@/lib/timemachine";

/**
 * TimeMachineBar is the ENGAGED state's banner and nothing else: "you are in the past, writes are
 * off" is a fact about the whole console, so it belongs in the chrome.
 *
 * Live it renders nothing. The trigger is components/timemachine-control.tsx, in the page header
 * beside the range presets it belongs with.
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
      /* Below sm this is a two-column grid (icon | sentence, button on its own row at the end):
         a wrapping flex row put the icon alone on the first line whenever the sentence did not fit
         beside it (the Russian one is 4px too wide), and with a 0 basis the wide Russian button
         squeezed the sentence into a 74px column. From sm up it is one flex row. */
      className="grid grid-cols-[auto_minmax(0,1fr)] items-center gap-2.5 border-b border-border bg-health-warn-soft/60 px-4 py-1.5 text-[13px] text-foreground sm:flex sm:flex-wrap sm:px-5"
    >
      <History aria-hidden="true" className="size-3.5 shrink-0 text-health-warn" />
      <span className="min-w-0 sm:grow">
        <span className="font-medium">{t("timemachine.viewing", { at: stamp(at!, locale) })}</span>{" "}
        {/* The hint clause is what pushed a 375px banner to three rows; the button says it. */}
        <span className="hidden text-muted-foreground sm:inline">{t("timemachine.viewingHint")}</span>
      </span>
      <Button
        variant="outline"
        size="sm"
        className="col-span-2 h-7 justify-self-end sm:col-auto sm:ml-auto"
        onClick={() => {
          /* This button goes away with the banner, so focus moves first: to the page's own Time
             Machine trigger, or to the page when it has none. */
          const next =
            document.querySelector<HTMLElement>(TIME_MACHINE_TRIGGER_SELECTOR) ?? document.getElementById("main-content");
          next?.focus({ preventScroll: true });
          returnToLive();
        }}
      >
        {t("timemachine.returnToLive")}
      </Button>
    </div>
  );
}
