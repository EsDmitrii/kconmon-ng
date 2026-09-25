/**
 * The server's own floors for local accounts (internal/console/httpapi/users.go). The console checks
 * them before sending, which saves a round trip and an argon2id hash per rejected attempt; the
 * server stays the authority.
 */

export const MIN_PASSWORD_CHARS = 12;

/** A username the server accepts: printable in the audit log, the user menu and a session key. */
export const USERNAME_PATTERN = /^[A-Za-z0-9._@-]{1,64}$/;

/** Characters, not UTF-16 units: the server counts runes, and an emoji is one character to a user. */
export function passwordTooShort(password: string): boolean {
  return [...password].length < MIN_PASSWORD_CHARS;
}
