import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * maintenance — components/maintenance.tsx's bar: its count line, its create
 * form, and the row pages/settings.tsx borrows for the windows the bar's own
 * range cannot reach.
 *
 * The deliberate TWIN of dict/annotations.ts, and a separate file for the same
 * reason the two components are separate (maintenance.tsx says it at length):
 * an annotation may be an INSTANT and a window is always a SPAN, one carries
 * `text` and the other `reason`, and they ride different permissions. The short
 * strings the two dictionaries repeat — «Отмена», «Подтвердить удаление»,
 * «Область» — are the README's sanctioned duplication, not an oversight.
 *
 * «Окно работ», matching dict/investigate.ts's own "actions.createMaintenance"
 * («Создать окно работ») and "source.name.maintenance" («Окна работ»), and
 * dict/settings.ts, which said it that way first. The English repeats "window"
 * for both the range and the declared thing; Russian must not, or the count
 * line reads «окно работ в этом окне». So the RANGE is «интервал» everywhere in
 * this zone and «окно» is left to mean the declared window alone.
 *
 * NOT HERE, on purpose:
 *   - the reason text, the scope VALUE, who declared it and both stamps. Data,
 *     and the stamps are toLocaleString in the VIEWER's locale.
 *   - `maintenance:read` / `maintenance:write`, and every problem+json detail a
 *     refused create or delete carries.
 *   - the DateTimePicker's strings: dict/shared.ts owns the picker.
 *   - the create button's label when Investigate passes one. That rail speaks
 *     in verbs and supplies its own word from dict/investigate.ts; only the
 *     default that sits under a chart is here.
 */

const en = {
  /* ── the bar ───────────────────────────────────────────────────────────── */
  "bar.unavailable": "Maintenance windows are unavailable.",
  "bar.count.one": "{count} maintenance window in this window · scope {scope}",
  "bar.count.few": "{count} maintenance windows in this window · scope {scope}",
  "bar.count.many": "{count} maintenance windows in this window · scope {scope}",
  "bar.create": "＋ maintenance",
  "bar.list.aria": "Maintenance windows in this window",

  "scope.global": "global",

  /* ── the create form ───────────────────────────────────────────────────── */
  "form.aria": "New maintenance window",
  "form.scope.before": "Scope",
  "form.scope.after": "— fixed to this view.",
  "form.start": "Start",
  "form.end": "End",
  /* Says WHO refuses, not just that it is refused: the client test mirrors the
     store's CHECK and the server remains the arbiter. */
  "form.end.hint": "Must be after the start — the server refuses anything else.",
  "form.reason": "Reason",
  "form.reason.placeholder": "Core switch firmware upgrade",
  "form.error.reasonRequired": "A reason is required.",
  "form.error.endNotAfterStart": "The end must be after the start.",
  "form.error.createFailed": "Failed to declare the window",
  "form.submit": "Create maintenance window",
  "form.cancel": "Cancel",

  /* ── one row ───────────────────────────────────────────────────────────── */
  "row.delete": "Delete",
  "row.delete.aria": "Delete maintenance window: {reason}",
  "row.confirmDelete": "Confirm delete",
  "row.confirmDelete.aria": "Confirm delete maintenance window: {reason}",
  "row.cancel": "Cancel",
  "row.deleteFailed": "Failed to delete",

  /* ── lib/annotations.ts's outsideWindowNote ────────────────────────────── */
  /* The SAME sentence dict/annotations.ts declares, duplicated rather than
     shared: outsideWindowNote is written once and both bars call it, each
     passing its OWN translator. A test pins that the two tables agree, which is
     the cheap half of the trade the README asks for. */
  "created.outsideWindow": "Created — outside this window (which ends {ends}); press Investigate to reframe.",
} as const;

export type MaintenanceKey = keyof typeof en;

export const maintenanceDict: Dictionary<MaintenanceKey> = defineDict(en, {
  "bar.unavailable": "Окна работ недоступны.",
  "bar.count.one": "{count} окно работ в этом интервале · область {scope}",
  "bar.count.few": "{count} окна работ в этом интервале · область {scope}",
  "bar.count.many": "{count} окон работ в этом интервале · область {scope}",
  "bar.create": "＋ работы",
  "bar.list.aria": "Окна работ в этом интервале",

  "scope.global": "глобальная",

  "form.aria": "Новое окно работ",
  "form.scope.before": "Область",
  "form.scope.after": "задана этим экраном.",
  "form.start": "Начало",
  "form.end": "Конец",
  "form.end.hint": "Должен быть позже начала, иначе сервер откажет.",
  "form.reason": "Причина",
  "form.reason.placeholder": "Обновление прошивки на коммутаторе ядра",
  "form.error.reasonRequired": "Нужна причина.",
  "form.error.endNotAfterStart": "Конец должен быть позже начала.",
  "form.error.createFailed": "Не удалось объявить окно",
  "form.submit": "Создать окно работ",
  "form.cancel": "Отмена",

  "row.delete": "Удалить",
  "row.delete.aria": "Удалить окно работ: {reason}",
  "row.confirmDelete": "Подтвердить удаление",
  "row.confirmDelete.aria": "Подтвердить удаление окна работ: {reason}",
  "row.cancel": "Отмена",
  "row.deleteFailed": "Не удалось удалить",

  "created.outsideWindow": "Создано, но в этот интервал не попадает: он кончается в {ends}. Нажмите «Расследовать», чтобы переставить рамку.",
});

/** countForm picks between `bar.count.one` / `.few` / `.many`. A per-dictionary
 *  copy, as dict/annotations.ts and four others already keep one — lib/i18n
 *  ships no plural machinery, and the README forbids the shared file. */
export function countForm(locale: string, n: number): "one" | "few" | "many" {
  if (locale !== "ru") return n === 1 ? "one" : "many";
  const teen = n % 100;
  if (teen >= 11 && teen <= 14) return "many";
  const last = n % 10;
  if (last === 1) return "one";
  if (last >= 2 && last <= 4) return "few";
  return "many";
}
