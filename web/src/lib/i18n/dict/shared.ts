import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * shared — the strings of the components that render on EVERY page, whichever
 * route is on screen: ui/datetime-picker.tsx (the one instant-picker the whole
 * console asks with) and lib/timemachine.tsx's write-guard sentence.
 *
 * "Shared" here means ONE COMPONENT WITH MANY MOUNTS, not "strings several
 * pages happen to use". lib/i18n/README.md forbids a common.ts for the second
 * kind, and it is right to: "Cancel" next to a Delete on /targets and "Cancel"
 * inside this picker are two different buttons that happen to agree in English,
 * and a table shared between them is a merge conflict waiting for the first
 * page that needs «Отмена» to become «Отменить». Every page's own Cancel lives
 * in that page's own dictionary. The two below belong to the PICKER.
 *
 * NOT HERE:
 *   - the trigger's formatted instant. formatInstant is toLocaleString in the
 *     VIEWER's locale and stays that way — lib/i18n's module doc says why.
 *   - PAST_PRESETS / FUTURE_PRESETS' `label` fields. Those constants are
 *     exported and pinned as data; the picker looks the ru label up by the
 *     preset's `minutes`, so the exported shape never had to change.
 */

const en = {
  /* ── the Time Machine's write guard ────────────────────────────────────── */
  /* The sentence a time-disabled control gives for itself, and the one the
     provider mounts once for every aria-describedby pointing at it.
     lib/timemachine.tsx re-exports this very string as
     TIME_MACHINE_DISABLED_REASON so the constant and the table cannot drift. */
  "timemachine.disabledReason": "Time Machine is engaged — return to Live to act.",

  /* ── ui/datetime-picker.tsx ────────────────────────────────────────────── */
  "picker.triggerDefault": "Pick a time",
  "picker.dialog": "Choose a date and time",
  "picker.quickRanges": "Quick ranges",
  "picker.prevMonth": "Previous month",
  "picker.nextMonth": "Next month",
  "picker.monthYearTrigger": "Choose the month and year",
  "picker.monthYearPanel": "Pick a month and year",
  "picker.monthColumn": "Month",
  "picker.yearColumn": "Year",
  "picker.calendar": "Calendar",
  /* The day cell's accessible name, in two halves so Russian can put the month
     in the genitive where the sentence needs it («8 августа 2026») without the
     component knowing that grammar exists. */
  "picker.dayLabel": "{day} {month} {year}",
  "picker.chooseDay": "Choose {date}",
  "picker.dateField": "Date",
  "picker.timeField": "Time",
  /* The time WHEEL's group label. Same English words as picker.triggerDefault
     and a separate key on purpose: one names a control that has no value yet,
     the other names an open panel of hours and minutes. */
  "picker.timePanel": "Pick a time",
  "picker.hourColumn": "Hour",
  "picker.minuteColumn": "Minute",
  "picker.nowAria": "Set the date and time to now",
  "picker.now": "Now",
  "picker.futureHint": "In the future — will engage at now.",
  "picker.cancel": "Cancel",
  "picker.apply": "Apply",

  /* Quick ranges, keyed by what they DO rather than by their text, so the
     Russian is free to say «15 мин назад» where English says "15m ago". */
  "picker.preset.ago15m": "15m ago",
  "picker.preset.ago1h": "1h ago",
  "picker.preset.ago6h": "6h ago",
  "picker.preset.ago24h": "24h ago",
  "picker.preset.in15m": "in 15m",
  "picker.preset.in1h": "in 1h",
  "picker.preset.in6h": "in 6h",
  "picker.preset.tomorrow": "tomorrow",

  /* Month names come in TWO sets because Russian declines them and English
     does not: the header and the wheel say «Август 2026» (nominative), the day
     cell says «8 августа 2026» (genitive). English fills both with the same
     twelve words, which is exactly why one table would have looked sufficient
     right up until the aria-labels read "Выбрать 8 Август 2026". */
  "month.1": "January",
  "month.2": "February",
  "month.3": "March",
  "month.4": "April",
  "month.5": "May",
  "month.6": "June",
  "month.7": "July",
  "month.8": "August",
  "month.9": "September",
  "month.10": "October",
  "month.11": "November",
  "month.12": "December",

  "monthOf.1": "January",
  "monthOf.2": "February",
  "monthOf.3": "March",
  "monthOf.4": "April",
  "monthOf.5": "May",
  "monthOf.6": "June",
  "monthOf.7": "July",
  "monthOf.8": "August",
  "monthOf.9": "September",
  "monthOf.10": "October",
  "monthOf.11": "November",
  "monthOf.12": "December",

  /* Monday-first, ISO 8601. `short` is the header cell, `long` is its `abbr`
     so a screen reader says the whole word. */
  "weekday.mon.short": "Mo",
  "weekday.tue.short": "Tu",
  "weekday.wed.short": "We",
  "weekday.thu.short": "Th",
  "weekday.fri.short": "Fr",
  "weekday.sat.short": "Sa",
  "weekday.sun.short": "Su",
  "weekday.mon.long": "Monday",
  "weekday.tue.long": "Tuesday",
  "weekday.wed.long": "Wednesday",
  "weekday.thu.long": "Thursday",
  "weekday.fri.long": "Friday",
  "weekday.sat.long": "Saturday",
  "weekday.sun.long": "Sunday",
} as const;

export type SharedKey = keyof typeof en;

export const sharedDict: Dictionary<SharedKey> = defineDict(en, {
  /* «Машина времени включена» and the way out in the same words chrome.ts's
     bar uses — one concept, one wording, wherever the operator meets it. */
  "timemachine.disabledReason": "Машина времени включена. Чтобы что-то менять, вернитесь в реальное время.",

  "picker.triggerDefault": "Выбрать момент",
  "picker.dialog": "Выбор даты и времени",
  "picker.quickRanges": "Быстрый выбор",
  "picker.prevMonth": "Предыдущий месяц",
  "picker.nextMonth": "Следующий месяц",
  "picker.monthYearTrigger": "Выбрать месяц и год",
  "picker.monthYearPanel": "Месяц и год",
  "picker.monthColumn": "Месяц",
  "picker.yearColumn": "Год",
  "picker.calendar": "Календарь",
  "picker.dayLabel": "{day} {month} {year}",
  "picker.chooseDay": "Выбрать {date}",
  "picker.dateField": "Дата",
  "picker.timeField": "Время",
  "picker.timePanel": "Выбор времени",
  "picker.hourColumn": "Часы",
  "picker.minuteColumn": "Минуты",
  "picker.nowAria": "Поставить текущие дату и время",
  "picker.now": "Сейчас",
  "picker.futureHint": "Момент в будущем, поэтому включимся на текущем.",
  "picker.cancel": "Отмена",
  "picker.apply": "Применить",

  "picker.preset.ago15m": "15 мин назад",
  "picker.preset.ago1h": "1 ч назад",
  "picker.preset.ago6h": "6 ч назад",
  "picker.preset.ago24h": "24 ч назад",
  "picker.preset.in15m": "через 15 мин",
  "picker.preset.in1h": "через 1 ч",
  "picker.preset.in6h": "через 6 ч",
  "picker.preset.tomorrow": "завтра",

  "month.1": "Январь",
  "month.2": "Февраль",
  "month.3": "Март",
  "month.4": "Апрель",
  "month.5": "Май",
  "month.6": "Июнь",
  "month.7": "Июль",
  "month.8": "Август",
  "month.9": "Сентябрь",
  "month.10": "Октябрь",
  "month.11": "Ноябрь",
  "month.12": "Декабрь",

  "monthOf.1": "января",
  "monthOf.2": "февраля",
  "monthOf.3": "марта",
  "monthOf.4": "апреля",
  "monthOf.5": "мая",
  "monthOf.6": "июня",
  "monthOf.7": "июля",
  "monthOf.8": "августа",
  "monthOf.9": "сентября",
  "monthOf.10": "октября",
  "monthOf.11": "ноября",
  "monthOf.12": "декабря",

  "weekday.mon.short": "Пн",
  "weekday.tue.short": "Вт",
  "weekday.wed.short": "Ср",
  "weekday.thu.short": "Чт",
  "weekday.fri.short": "Пт",
  "weekday.sat.short": "Сб",
  "weekday.sun.short": "Вс",
  "weekday.mon.long": "Понедельник",
  "weekday.tue.long": "Вторник",
  "weekday.wed.long": "Среда",
  "weekday.thu.long": "Четверг",
  "weekday.fri.long": "Пятница",
  "weekday.sat.long": "Суббота",
  "weekday.sun.long": "Воскресенье",
});

/** MONTH_KEYS and MONTH_OF_KEYS are indexed by Date#getMonth() (0-11), so the
 *  picker never does the off-by-one itself. */
export const MONTH_KEYS: readonly SharedKey[] = [
  "month.1", "month.2", "month.3", "month.4", "month.5", "month.6",
  "month.7", "month.8", "month.9", "month.10", "month.11", "month.12",
];

export const MONTH_OF_KEYS: readonly SharedKey[] = [
  "monthOf.1", "monthOf.2", "monthOf.3", "monthOf.4", "monthOf.5", "monthOf.6",
  "monthOf.7", "monthOf.8", "monthOf.9", "monthOf.10", "monthOf.11", "monthOf.12",
];

/** WEEKDAY_KEYS is Monday-first, matching the grid's own offset arithmetic. */
export const WEEKDAY_KEYS: readonly { short: SharedKey; long: SharedKey }[] = [
  { short: "weekday.mon.short", long: "weekday.mon.long" },
  { short: "weekday.tue.short", long: "weekday.tue.long" },
  { short: "weekday.wed.short", long: "weekday.wed.long" },
  { short: "weekday.thu.short", long: "weekday.thu.long" },
  { short: "weekday.fri.short", long: "weekday.fri.long" },
  { short: "weekday.sat.short", long: "weekday.sat.long" },
  { short: "weekday.sun.short", long: "weekday.sun.long" },
];

/**
 * PRESET_KEYS maps a preset's SIGNED MINUTES onto its key.
 *
 * By minutes rather than by label because PAST_PRESETS and FUTURE_PRESETS are
 * exported constants that tests read as data ("the past set all points
 * backward"), and threading a translation key through them would have made a
 * pure data table depend on the i18n module. The offsets are unique across
 * both sets, so they are already the identity each row has.
 */
export const PRESET_KEYS: Readonly<Record<number, SharedKey>> = {
  [-15]: "picker.preset.ago15m",
  [-60]: "picker.preset.ago1h",
  [-360]: "picker.preset.ago6h",
  [-1440]: "picker.preset.ago24h",
  [15]: "picker.preset.in15m",
  [60]: "picker.preset.in1h",
  [360]: "picker.preset.in6h",
  [1440]: "picker.preset.tomorrow",
};
