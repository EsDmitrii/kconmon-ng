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
 *     …set console.database.mode → «…задайте console.database.mode. Запрос не
 *                                отправлялся.»        (overview `db.note`)
 *
 * The English halves are NOT the same bytes as overview's — this rail says
 * "Incidents need…", the panel says "Open incidents need…" — and they stay
 * apart, because `en` is what renders today and moving a string is not a
 * translation. It is the RUSSIAN that has to agree, and it does.
 *
 * NOT HERE: an incident's own `title` (the human who filed it wrote it), its
 * id, the `incidents:read` permission string and the `console.database.mode`
 * config key.
 */

const en = {
  /* The header action — a real <a href> an operator copies and pastes, whose
     LABEL is the imperative the matrix cell and the investigate form both use. */
  "investigate": "Investigate",

  "title": "Open incidents",
  /* The <aside>'s accessible name; three card tests find this rail by it. */
  "aria": "Open incidents",

  /* The two honesty lines. Each names the ONE thing that was missing and says
     plainly that no request was made — never "unavailable", which would let an
     operator hunt for an outage that is really a permission or a config key. */
  "denied": "Incidents need incidents:read — none was requested.",
  "noDatabase": "Incidents are stored — set console.database.mode. Nothing was requested.",

  "empty": "No open incident names this object.",
  /* The row badge. Every incident in this list is open by construction — the
     query asks for status=open — so it is a label, not a status readout. */
  "open": "Open",
} as const;

export type InvestigateEntryKey = keyof typeof en;

export const investigateEntryDict: Dictionary<InvestigateEntryKey> = defineDict(en, {
  "investigate": "Расследовать",

  "title": "Открытые инциденты",
  "aria": "Открытые инциденты",

  "denied": "Инцидентам нужно право incidents:read, которого у роли нет, так что запрос не отправлялся.",
  "noDatabase": "Инциденты хранятся в базе, задайте console.database.mode. Запрос не отправлялся.",

  "empty": "Ни один открытый инцидент не называет этот объект.",
  "open": "Открыт",
});
