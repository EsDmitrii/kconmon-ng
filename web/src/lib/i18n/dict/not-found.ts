import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * not-found — the page a reader gets for an address this console has no route
 * for (routes.tsx's NotFoundPage).
 *
 * It exists because the router's own default is the two English words "Not
 * Found" on an otherwise empty panel: no title, no explanation, no way back,
 * and the same two words whatever the language switch says. A 404 is the one
 * page a reader arrives at by accident — from a stale bookmark, a link in a
 * ticket written against an older build, a typed path — so it is the page that
 * can least afford to be a dead end.
 *
 * NOT HERE: the address itself. It is the reader's own bytes and prints as
 * they came, the same rule the settings page's file name and the webhook row's
 * lastStatus follow.
 */
const en = {
  "title": "Page not found",
  /* {path} is the address that was asked for. Naming it is the difference
     between "something went wrong" and "this link is stale" — the reader is
     usually the one who can tell which. */
  "body": "This console has no page at {path}.",
  "hint": "The address may have changed between builds, or the link may have been mistyped.",
  "home": "Back to Overview",
} as const;

export type NotFoundKey = keyof typeof en;

export const notFoundDict: Dictionary<NotFoundKey> = defineDict(en, {
  "title": "Страница не найдена",
  "body": "В этой консоли нет страницы по адресу {path}.",
  "hint": "Возможно, адрес изменился между сборками или в ссылке опечатка.",
  "home": "Вернуться к обзору",
});
