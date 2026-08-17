import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * error-boundary — the fallback shown when a page throws while rendering.
 *
 * Without it a render throw (an alert whose labels arrived null, a matrix cell
 * of the wrong shape) blanks the whole app, sidebar and all. The boundary keeps
 * the chrome and turns the crash into an honest, recoverable panel.
 *
 * NOT HERE: the error message itself. It is whatever the thrown Error carried
 * and prints verbatim, the same rule the 404 address and the webhook status
 * follow.
 */
const en = {
  "title": "This page hit an error",
  "body": "Something on this page failed to render. The rest of the console is unaffected, reload to try again.",
  "reload": "Reload this page",
} as const;

export type ErrorBoundaryKey = keyof typeof en;

export const errorBoundaryDict: Dictionary<ErrorBoundaryKey> = defineDict(en, {
  "title": "На этой странице ошибка",
  "body": "Что-то на странице не отрисовалось. Остальная консоль работает, перезагрузите страницу и попробуйте снова.",
  "reload": "Перезагрузить страницу",
});
