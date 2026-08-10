import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * login — pages/login.tsx, the one page an operator can reach BEFORE the
 * console knows who they are.
 *
 * That is the whole reason this file is worth writing down: the language choice
 * lives in localStorage (LOCALE_STORAGE_KEY), not in a profile the server has
 * to hand back, so it is already there when this page renders — an operator who
 * switched to Russian yesterday and got logged out overnight is not shown an
 * English form as a punishment for the session expiring.
 *
 * ── two "Sign in"s, two Russian words ─────────────────────────────────────
 * English says "Sign in" twice on the same card: once as the heading, once on
 * the button. Russian does not — a heading is a noun («Вход») and a button is
 * an imperative («Войти»), which is the README's rule for buttons and the same
 * split settings.ts's `about.anonymous` already made when it says «Входа нет.»
 * So they are two keys even though the English table repeats itself.
 *
 * NOT HERE:
 *   - SSO and the IdP's own name. Protocol/product names.
 *   - Every problem+json the login endpoint answers with. `error.fallback` is
 *     the client's own last resort for a failure that carried no problem
 *     document at all (a dropped connection); the server's `detail`/`title`
 *     wins whenever there is one, verbatim, in both languages.
 *   - The `autoComplete` tokens ("username", "current-password"). They are a
 *     contract with the browser's password manager, not words on screen.
 */

const en = {
  "title": "Sign in",
  /* The OIDC card: the console hands the whole exchange to the IdP, and the
     link is a real browser navigation. The sentence says who is about to ask
     for the password, because it is not this page. */
  "oidc.lead": "Authenticate through your identity provider.",
  "oidc.action": "Sign in with SSO",

  "field.username": "Username",
  "field.password": "Password",
  "submit": "Sign in",
  "error.fallback": "Sign in failed",
} as const;

export type LoginKey = keyof typeof en;

export const loginDict: Dictionary<LoginKey> = defineDict(en, {
  "title": "Вход",
  "oidc.lead": "Аутентификацию проводит ваш провайдер входа.",
  "oidc.action": "Войти через SSO",

  "field.username": "Имя пользователя",
  "field.password": "Пароль",
  "submit": "Войти",
  "error.fallback": "Войти не удалось",
});
