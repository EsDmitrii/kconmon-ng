import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * investigate-entry — components/investigate-entry.tsx: the way IN to
 * Investigation Mode from an object card (the header link) and the rail of
 * incidents already open about that object.
 *
 * A shared component mounted by all three cards, so its own file — the same
 * call dict/recent-changes.ts makes, one rail over.
 *
 * ── it must not invent a second vocabulary ────────────────────────────────
 * dict/overview.ts and dict/investigate.ts already say nearly all of this, and
 * the words are taken from them rather than re-derived:
 *
 *     Open incidents          → «Открытые инциденты»  (overview `incidents.title`)
 *     Investigate, the verb   → «Расследовать»        (investigate `form.submit`)
 *     the Open badge          → «Открыт»              (investigate `incident.open`)
 *     …needs incidents:read   → «…нужно право incidents:read — запрос не
 *                                отправлялся.»        (overview `incidents.denied`)
 *     …set database.dsnFile   → «…задайте database.dsnFile (Helm: …). Запрос не
 *                                отправлялся.»        (overview `db.note`)
 *
 * The English halves are NOT the same bytes as overview's — this rail says
 * "Incidents need…", the panel says "Open incidents need…" — and they stay
 * apart, because `en` is what renders today and moving a string is not a
 * translation. It is the RUSSIAN that has to agree, and it does.
 *
 * NOT HERE: an incident's own `title` (the human who filed it wrote it), its
 * id, the `incidents:read` permission string and the `database.dsnFile` /
 * `database.existingSecret` config keys.
 */

const en = {
  /* The header action — a real <a href> an operator copies and pastes, whose
     LABEL is the imperative the matrix cell and the investigate form both use. */
  "investigate": "Investigate",

  "title": "Open incidents",
  /* The <aside>'s accessible name; three card tests find this rail by it. */
  "aria": "Open incidents",
  /* Under the title while the Time Machine is engaged; {at} is lib/i18n's stampFull. */
  "at": "at {at}",

  /* The two honesty lines. Each names the ONE thing that was missing and says
     plainly that no request was made — never "unavailable", which would let an
     operator hunt for an outage that is really a permission or a config key. */
  "denied": "Incidents need incidents:read — none was requested.",
  "noDatabase": "Incidents are stored — set database.dsnFile (Helm: database.existingSecret). Nothing was requested.",

  "empty": "No open incident names this object.",
  /* The line under that title in the empty slate: what would fill the rail. */
  "empty.body": "An incident filed against this object will show up here while it stays open.",
  "empty.at": "No incident open at that instant names this object.",
  "empty.at.body": "An incident filed against this object shows up here if it was open at the instant on screen.",
  /* The row badge. Every incident in this list is open by construction — live
     the query asks for status=open, engaged the rail keeps only the ones open
     at the instant on screen — so it is a label, not a status readout. */
  "open": "Open",
  "failed": "Open incidents could not be loaded: {error}",
  "failed.generic": "the request failed",
  "configFailed": "Could not read the console configuration, so incidents were not requested: {error}",
  /* Engaged, scanIncidents hit its page cap before the list ended (`truncated`). */
  "scanCapped": "The scan stopped at its page limit, so an older incident open at this instant may be missing.",
  /* AuthGate renders the page when "who am I" failed; that answer is asked again only on reload. */
  "meFailed": "Could not check access to incidents, so none were requested: {error}. Reload the page to try again.",
} as const;

export type InvestigateEntryKey = keyof typeof en;

export const investigateEntryDict: Dictionary<InvestigateEntryKey> = defineDict(en, {
  "investigate": "Расследовать",

  "title": "Открытые инциденты",
  "aria": "Открытые инциденты",
  "at": "на {at}",

  "denied": "Инцидентам нужно право incidents:read, которого у роли нет, так что запрос не отправлялся.",
  "noDatabase": "Инциденты хранятся в базе, задайте database.dsnFile (Helm: database.existingSecret). Запрос не отправлялся.",

  "empty": "Ни один открытый инцидент не называет этот объект.",
  "empty.body": "Инцидент, заведённый на этот объект, будет виден здесь, пока открыт.",
  "empty.at": "Ни один инцидент, открытый в этот момент, не называет этот объект.",
  "empty.at.body": "Инцидент, заведённый на этот объект, виден здесь, если он был открыт в показанный момент.",
  "open": "Открыт",
  "failed": "Не удалось загрузить открытые инциденты: {error}",
  "failed.generic": "запрос не выполнен",
  "configFailed": "Не удалось прочитать конфигурацию консоли, поэтому инциденты не запрашивались: {error}",
  "scanCapped": "Просмотр остановился на пределе страниц, поэтому более старый инцидент, открытый в этот момент, может отсутствовать в списке.",
  "meFailed": "Не удалось проверить доступ к инцидентам, поэтому запрос не отправлялся: {error}. Перезагрузите страницу, чтобы повторить.",
});
