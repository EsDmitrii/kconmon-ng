import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * user-menu — components/user-menu.tsx, the sidebar footer's identity menu.
 *
 * Chrome by position, its own surface by file: dict/chrome.ts is the SIDEBAR's
 * table (its links, its group headers, its footer line) and this menu is a
 * separate component with its own test file, so it gets its own four strings
 * rather than four more keys in a file the sidebar owns.
 *
 * NOT HERE, and this is most of the menu:
 *   - `me.subject.displayName` and `me.subject.roles` — the identity the server
 *     resolved. Role names are permission-vocabulary (viewer, operator, admin),
 *     the same way dict/cards.ts leaves them alone; the console does not
 *     rename an operator's role for them.
 *   - `tokens:manage`, the permission gating the link. Never translated.
 *
 * «Управление токенами» is the wording dict/settings.ts's `about.maintenance`
 * already uses («Роли и токены API из этой консоли не администрируются
 * вовсе.») — the link points at /settings, and the two must not call the same
 * thing by two names.
 */

const en = {
  /* Shown INSTEAD of the joined role list, so it reads as a continuation of
     "…, and:" — lower case, no full stop, exactly as it renders today. */
  "roles.none": "no roles bound",
  "tokens": "Token management",
  "signOut": "Sign out",
  "signOut.pending": "Signing out…",
} as const;

export type UserMenuKey = keyof typeof en;

export const userMenuDict: Dictionary<UserMenuKey> = defineDict(en, {
  "roles.none": "ролей не назначено",
  "tokens": "Управление токенами",
  "signOut": "Выйти",
  "signOut.pending": "Выходим…",
});
