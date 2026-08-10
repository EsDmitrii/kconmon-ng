import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * recent-changes — components/recent-changes.tsx, the rail every object card
 * (node, pair, target) mounts on its right.
 *
 * Its own file rather than a corner of dict/cards.ts, for the reason
 * dict/annotations.ts and dict/maintenance.ts are their own files: it is a
 * SHARED COMPONENT, not a page, and the three cards each mount it with a
 * different filter. cards.ts's `target.changesNote` — the sentence the target
 * card writes ABOUT this rail — stays where it is; that one is the card
 * explaining its own filter, and it already calls the rail «лента».
 *
 * ── every row in it is data ───────────────────────────────────────────────
 * A LiveEvent's `summary`, its `severity` badge text and its stamp all render
 * verbatim: the first is a sentence the controller wrote, the second is the
 * stored enum value, and the third is the VIEWER's locale, which is the right
 * answer whichever language the chrome is in. What translates is the frame
 * around them — the heading, the landmark's name, the bound, and the three
 * lines that say why the list is short.
 *
 * The stamp is lib/utils' fmtEventStamp, not a bare toLocaleTimeString: this
 * rail read 3:12 PM for the event the Live feed called 15:12, and a row from
 * yesterday read exactly like one from this afternoon (QA scope 2, #9 and #10).
 *
 * ── the honest lines ──────────────────────────────────────────────────────
 * `db.note` is a DEGRADED state, not an error: with no database there is no
 * scrollback, but the socket half of this rail keeps working, and the Russian
 * must not read as a failure. «Онлайн» is the word for the pushed feed
 * throughout this console (dict/chrome.ts's nav item, dict/realtime.ts's
 * badge) — never «реальное время», which belongs to the Time Machine's present.
 *
 * `error.fallback` is the client's own last resort for a failed history fetch
 * that carried no problem+json; whenever the server sent a `detail` or `title`,
 * that wins, verbatim, in both languages.
 */

const en = {
  "title": "Recent changes",
  /* The <aside>'s accessible NAME, separate from the visible heading even
     though they read the same today: a screen reader announces the landmark
     before the heading, and the two are free to diverge. */
  "aria": "Recent changes",
  /* {at} is the Time Machine's instant, already toLocaleString()'d by the
     component — interpolated, never translated. */
  "upTo": "up to {at}",
  "db.note": "History requires a database — showing live events only.",
  "error.fallback": "Event history is unavailable",
  "loading": "Loading recent changes…",
  "empty": "No recent changes.",
} as const;

export type RecentChangesKey = keyof typeof en;

export const recentChangesDict: Dictionary<RecentChangesKey> = defineDict(en, {
  "title": "Недавние изменения",
  "aria": "Недавние изменения",
  "upTo": "до {at}",
  "db.note": "Истории нужна база, поэтому показываем только онлайн-события.",
  "error.fallback": "История событий недоступна",
  "loading": "Загружаем недавние изменения…",
  "empty": "Недавних изменений нет.",
});
