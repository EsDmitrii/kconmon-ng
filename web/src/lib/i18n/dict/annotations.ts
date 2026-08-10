import { defineDict, translate, type Dictionary, type Translate } from "@/lib/i18n";

/**
 * annotations — components/annotations.tsx's bar (its count line, its create
 * form and its rows) plus the ONE sentence lib/annotations.ts writes for it.
 *
 * A SHARED BAR, not a page: it is mounted under the chart on /explore, in the
 * Investigate page's notes column, and on the node and target cards. That is
 * why it has a dictionary of its own rather than a corner of any of theirs —
 * four surfaces reading one component must read one wording.
 *
 * components/maintenance.tsx is its deliberate TWIN and has dict/maintenance.ts
 * next door. The two files repeat several short strings («Отмена», «Подтвердить
 * удаление», «Область») and that is the README's rule working as intended: a
 * note and a declared window are different objects, and the day one of them
 * needs a different verb the other must not move with it.
 *
 * NOT HERE, on purpose:
 *   - the note's own text, the scope VALUE (a node, a pair, a target name), the
 *     author and every stamp. Rows are data; only the words around them are
 *     ours. Every stamp is toLocaleString in the VIEWER's locale and stays so.
 *   - `annotations:write` and every problem+json detail a refused create or
 *     delete carries. The server wrote that sentence.
 *   - the DateTimePicker's own strings: dict/shared.ts owns the picker.
 */

const en = {
  /* ── the bar ───────────────────────────────────────────────────────────── */
  "bar.unavailable": "Annotations are unavailable.",
  /* Three forms for one count — lib/i18n ships no plural machinery on purpose
     (see README). English fills `few` with its own plural and countForm below
     never asks for it. */
  "bar.count.one": "{count} annotation in this window · scope {scope}",
  "bar.count.few": "{count} annotations in this window · scope {scope}",
  "bar.count.many": "{count} annotations in this window · scope {scope}",
  /* The ＋ is a glyph, not a word, and stays in both languages — it is what the
     twin's "＋ maintenance" and the rest of the console's compact create
     affordances look like. */
  "bar.create": "＋ annotate",
  "bar.list.aria": "Annotations in this window",

  /* scopeLabel's one WORD. The scope VALUE is a name and never translates; ""
     is the global scope and "global" is this console's name for it, not the
     server's. */
  "scope.global": "global",

  /* ── the create form ───────────────────────────────────────────────────── */
  "form.aria": "New annotation",
  /* Two halves because the scope's name sits between them in its own bold
     span — the one shape interpolation cannot express. */
  "form.scope.before": "Scope",
  "form.scope.after": "— fixed to this view.",
  /* "Start" labels the field AND names the picker inside it: one control, one
     word. "End" does not, because the field says the edge is optional and the
     control just says which edge it is. */
  "form.start": "Start",
  "form.end.label": "End (optional)",
  "form.end": "End",
  "form.end.hint": "Leave unset for a mark at a single moment.",
  "form.end.unset": "Not set",
  "form.end.clear.aria": "Clear end",
  "form.end.clear": "Clear",
  "form.note": "Note",
  "form.note.placeholder": "Rolled the gateway",
  "form.error.noteRequired": "A note is required.",
  "form.error.endBeforeStart": "End is before start.",
  "form.error.createFailed": "Failed to create the annotation",
  "form.submit": "Create annotation",
  "form.cancel": "Cancel",

  /* ── one row ───────────────────────────────────────────────────────────── */
  "row.delete": "Delete",
  "row.delete.aria": "Delete annotation: {text}",
  "row.confirmDelete": "Confirm delete",
  "row.confirmDelete.aria": "Confirm delete annotation: {text}",
  "row.cancel": "Cancel",
  "row.deleteFailed": "Failed to delete",

  /* ── lib/annotations.ts's outsideWindowNote ────────────────────────────── */
  /* The line a create shows when what it just stored will not appear in the
     list it was created from. It names the page's own commit button, so the
     Russian must be the word dict/investigate.ts's "form.submit" uses —
     «Расследовать» — or the sentence points at a control that does not exist. */
  "created.outsideWindow": "Created — outside this window (which ends {ends}); press Investigate to reframe.",
} as const;

export type AnnotationsKey = keyof typeof en;

export const annotationsDict: Dictionary<AnnotationsKey> = defineDict(en, {
  "bar.unavailable": "Заметки недоступны.",
  "bar.count.one": "{count} заметка в этом интервале · область {scope}",
  "bar.count.few": "{count} заметки в этом интервале · область {scope}",
  "bar.count.many": "{count} заметок в этом интервале · область {scope}",
  "bar.create": "＋ заметка",
  "bar.list.aria": "Заметки в этом интервале",

  "scope.global": "глобальная",

  "form.aria": "Новая заметка",
  "form.scope.before": "Область",
  "form.scope.after": "задана этим экраном.",
  "form.start": "Начало",
  "form.end.label": "Конец (необязательно)",
  "form.end": "Конец",
  "form.end.hint": "Оставьте пустым, если отмечаете один момент.",
  "form.end.unset": "Не задан",
  "form.end.clear.aria": "Очистить конец",
  "form.end.clear": "Очистить",
  "form.note": "Заметка",
  "form.note.placeholder": "Перекатили шлюз",
  "form.error.noteRequired": "Нужен текст заметки.",
  "form.error.endBeforeStart": "Конец раньше начала.",
  "form.error.createFailed": "Не удалось создать заметку",
  "form.submit": "Создать заметку",
  "form.cancel": "Отмена",

  "row.delete": "Удалить",
  "row.delete.aria": "Удалить заметку: {text}",
  "row.confirmDelete": "Подтвердить удаление",
  "row.confirmDelete.aria": "Подтвердить удаление заметки: {text}",
  "row.cancel": "Отмена",
  "row.deleteFailed": "Не удалось удалить",

  "created.outsideWindow": "Создано, но в этот интервал не попадает: он кончается в {ends}. Нажмите «Расследовать», чтобы переставить рамку.",
});

/**
 * enT is the ENGLISH translator, exported so a pure function can default to it
 * — the pattern dict/topology.ts established and pages/alerting.tsx's
 * parsePromDuration/relativeTime and pages/settings.tsx's parseBundle already
 * use. lib/annotations.ts's outsideWindowNote takes `t` as an optional trailing
 * parameter defaulting to this, so every existing call and every fixture keeps
 * answering the same bytes it always did.
 */
export const enT: Translate<AnnotationsKey> = (key, vars) => translate(annotationsDict, "en", key, vars);

/**
 * countForm picks between `bar.count.one` / `.few` / `.many`.
 *
 * A per-dictionary copy, exactly as dict/mtr.ts, dict/diagnostics.ts,
 * dict/run-detail.ts and dict/investigate.ts each keep one: lib/i18n ships no
 * plural machinery, and a shared helper would be the "common.ts" the README
 * forbids for the sake of six lines.
 */
export function countForm(locale: string, n: number): "one" | "few" | "many" {
  if (locale !== "ru") return n === 1 ? "one" : "many";
  const teen = n % 100;
  if (teen >= 11 && teen <= 14) return "many";
  const last = n % 10;
  if (last === 1) return "one";
  if (last >= 2 && last <= 4) return "few";
  return "many";
}
