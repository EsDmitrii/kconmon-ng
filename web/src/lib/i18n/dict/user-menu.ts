import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * user-menu — components/user-menu.tsx, the sidebar footer's identity menu.
 *
 * Chrome by position, its own surface by file: dict/chrome.ts is the SIDEBAR's
 * table (its links, its group headers, its footer line) and this menu is a
 * separate component with its own test file, so it gets its own strings
 * rather than more keys in a file the sidebar owns; the password dialog it
 * opens (components/change-password.tsx) reads them too.
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

  /* The self-service password change (auth.mode=local only). */
  "password": "Change password",
  "password.description": "Your other sessions are signed out; this one keeps working.",
  "password.current": "Current password",
  "password.next": "New password",
  "password.nextHelp": "At least 12 characters.",
  "password.repeat": "Repeat the new password",
  "password.submit": "Change password",
  "password.cancel": "Cancel",
  "password.close": "Done",
  "password.tooShort": "The new password needs at least 12 characters.",
  "password.mismatch": "The two new passwords differ.",
  "password.same": "The new password is the current one.",
  "password.wrong": "The current password is wrong.",
  "password.failed": "Could not change the password.",
  "password.done": "Password changed. Your other sessions are signed out.",
} as const;

export type UserMenuKey = keyof typeof en;

export const userMenuDict: Dictionary<UserMenuKey> = defineDict(en, {
  "roles.none": "ролей не назначено",
  "tokens": "Управление токенами",
  "signOut": "Выйти",
  "signOut.pending": "Выходим…",

  "password": "Сменить пароль",
  "password.description": "Остальные ваши сеансы завершатся, этот продолжит работать.",
  "password.current": "Текущий пароль",
  "password.next": "Новый пароль",
  "password.nextHelp": "Не короче 12 символов.",
  "password.repeat": "Повторите новый пароль",
  "password.submit": "Сменить пароль",
  "password.cancel": "Отмена",
  "password.close": "Готово",
  "password.tooShort": "Новый пароль должен быть не короче 12 символов.",
  "password.mismatch": "Новые пароли не совпадают.",
  "password.same": "Новый пароль совпадает с текущим.",
  "password.wrong": "Текущий пароль неверный.",
  "password.failed": "Не удалось сменить пароль.",
  "password.done": "Пароль изменён. Остальные ваши сеансы завершены.",
});
