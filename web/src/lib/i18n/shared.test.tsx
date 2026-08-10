import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Button } from "@/components/ui/button";
import { DateTimePicker, FUTURE_PRESETS, PAST_PRESETS } from "@/components/ui/datetime-picker";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { PRESET_KEYS, sharedDict } from "@/lib/i18n/dict/shared";
import {
  TIME_MACHINE_DISABLED_REASON,
  TIME_MACHINE_REASON_ID,
  TimeMachineProvider,
  useWriteGuard,
} from "@/lib/timemachine";

/**
 * The two SHARED surfaces, in both languages.
 *
 * They are the ones worth their own file because they are on EVERY page: the
 * instant picker the whole console asks with, and the sentence a time-disabled
 * control gives for itself. A page agent can forget a string on their own page
 * and only that page is wrong; a string missed here is wrong everywhere.
 */

const VALUE = new Date(2026, 7, 8, 15, 34); // 8 August 2026, local.

function renderPicker({ locale, ...props }: { locale?: "en" | "ru"; allowFuture?: boolean } = {}) {
  if (locale) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  render(
    <LocaleProvider>
      <DateTimePicker value={VALUE} onApply={vi.fn()} aria-label="Viewing time" {...props} />
    </LocaleProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Viewing time" }));
}

afterEach(() => {
  cleanup();
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

describe("the picker in English, with the provider mounted", () => {
  /* NOT a duplicate of datetime-picker.test.tsx: that file renders with no
     LocaleProvider at all and passes, which is the property it pins. This one
     mounts the provider and asserts the bytes did not move — a t() that
     reworded a label would be caught here. */
  it("still says exactly what it always said", () => {
    renderPicker({ locale: "en" });
    expect(screen.getByRole("dialog", { name: "Choose a date and time" })).toBeInTheDocument();
    expect(screen.getByText("August 2026")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Choose 8 August 2026" })).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Quick ranges" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Apply" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
  });
});

describe("the picker in Russian", () => {
  it("names its dialog, its grid and its two buttons", () => {
    renderPicker({ locale: "ru" });
    expect(screen.getByRole("dialog", { name: sharedDict.ru["picker.dialog"] })).toBeInTheDocument();
    expect(screen.getByRole("grid", { name: sharedDict.ru["picker.calendar"] })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: sharedDict.ru["picker.apply"] })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: sharedDict.ru["picker.cancel"] })).toBeInTheDocument();
  });

  it("writes the month header in the NOMINATIVE and the day label in the GENITIVE", () => {
    renderPicker({ locale: "ru" });
    // «Август 2026» in the header…
    expect(screen.getByText("Август 2026")).toBeInTheDocument();
    // …and «8 августа 2026» in the cell's spoken name. One word, two forms —
    // the reason there are two month tables at all.
    expect(screen.getByRole("button", { name: "Выбрать 8 августа 2026" })).toBeInTheDocument();
  });

  it("translates the weekday headers, keeping Monday first", () => {
    renderPicker({ locale: "ru" });
    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).toEqual(["Пн", "Вт", "Ср", "Чт", "Пт", "Сб", "Вс"]);
  });

  it("spells the full weekday name in `abbr`, so a screen reader says the word", () => {
    renderPicker({ locale: "ru" });
    expect(screen.getAllByRole("columnheader")[0]).toHaveAttribute("abbr", sharedDict.ru["weekday.mon.long"]);
  });

  it("translates the past presets", () => {
    renderPicker({ locale: "ru" });
    const quick = within(screen.getByRole("group", { name: sharedDict.ru["picker.quickRanges"] }));
    expect(quick.getByRole("button", { name: sharedDict.ru["picker.preset.ago15m"] })).toBeInTheDocument();
    expect(quick.getByRole("button", { name: sharedDict.ru["picker.preset.ago24h"] })).toBeInTheDocument();
  });

  it("translates the FORWARD presets on a forward-looking field", () => {
    renderPicker({ locale: "ru", allowFuture: true });
    const quick = within(screen.getByRole("group", { name: sharedDict.ru["picker.quickRanges"] }));
    expect(quick.getByRole("button", { name: sharedDict.ru["picker.preset.in15m"] })).toBeInTheDocument();
    expect(quick.getByRole("button", { name: sharedDict.ru["picker.preset.tomorrow"] })).toBeInTheDocument();
  });

  it("translates the month and year wheels", () => {
    renderPicker({ locale: "ru" });
    fireEvent.click(screen.getByRole("button", { name: sharedDict.ru["picker.monthYearTrigger"] }));
    const months = within(screen.getByRole("listbox", { name: sharedDict.ru["picker.monthColumn"] }));
    expect(months.getByRole("option", { name: "Август" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("listbox", { name: sharedDict.ru["picker.yearColumn"] })).toBeInTheDocument();
  });

  it("translates the two typed fields and the Now escape hatch", () => {
    renderPicker({ locale: "ru" });
    expect(screen.getByLabelText(sharedDict.ru["picker.dateField"])).toBeInTheDocument();
    expect(screen.getByLabelText(sharedDict.ru["picker.timeField"])).toBeInTheDocument();
    expect(screen.getByRole("button", { name: sharedDict.ru["picker.nowAria"] })).toBeInTheDocument();
  });
});

describe("PRESET_KEYS", () => {
  /* The presets stay pure exported DATA with English labels; the picker looks
     the Russian up by the offset. This is what keeps the two in step. */
  it("has a key for every preset in both sets, keyed by its own offset", () => {
    for (const p of [...PAST_PRESETS, ...FUTURE_PRESETS]) {
      const key = PRESET_KEYS[p.minutes];
      expect(key, String(p.minutes)).toBeDefined();
      expect(sharedDict.en[key]).toBe(p.label);
      expect(sharedDict.ru[key]).toBeTruthy();
    }
  });
});

/* ── the write guard ──────────────────────────────────────────────────────── */

function Guarded() {
  const guard = useWriteGuard();
  return <Button {...guard}>save</Button>;
}

function renderGuard({ locale, engaged }: { locale?: "en" | "ru"; engaged: boolean }) {
  if (locale) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  window.history.pushState({}, "", engaged ? "/?at=2026-08-07T10:00:00Z" : "/");
  render(
    <LocaleProvider>
      <TimeMachineProvider>
        <Guarded />
      </TimeMachineProvider>
    </LocaleProvider>,
  );
}

describe("the write guard's reason", () => {
  it("is the dictionary's English entry, so the exported constant cannot drift from the table", () => {
    expect(TIME_MACHINE_DISABLED_REASON).toBe(sharedDict.en["timemachine.disabledReason"]);
  });

  it("travels with the control in English, exactly as it always did", () => {
    renderGuard({ locale: "en", engaged: true });
    const control = screen.getByRole("button", { name: "save" });
    expect(control).toHaveAttribute("title", TIME_MACHINE_DISABLED_REASON);
    expect(control).toHaveAttribute("aria-describedby", TIME_MACHINE_REASON_ID);
    expect(document.getElementById(TIME_MACHINE_REASON_ID)?.textContent).toBe(TIME_MACHINE_DISABLED_REASON);
  });

  it("travels in Russian — the tooltip AND the one sr-only node it points at", () => {
    renderGuard({ locale: "ru", engaged: true });
    const ru = sharedDict.ru["timemachine.disabledReason"];
    expect(screen.getByRole("button", { name: "save" })).toHaveAttribute("title", ru);
    expect(document.getElementById(TIME_MACHINE_REASON_ID)?.textContent).toBe(ru);
  });

  it("says the way OUT in the same words the Time Machine bar's own button uses", () => {
    // «вернитесь в реальное время» / «Вернуться в реальное время» — one concept,
    // one wording, wherever the operator meets it.
    expect(sharedDict.ru["timemachine.disabledReason"]).toContain("реальное время");
  });

  it("adds nothing at all while Live, in either language", () => {
    renderGuard({ locale: "ru", engaged: false });
    const control = screen.getByRole("button", { name: "save" });
    expect(control).not.toHaveAttribute("title");
    expect(control).not.toHaveAttribute("aria-describedby");
    expect(document.getElementById(TIME_MACHINE_REASON_ID)).toBeNull();
  });
});
