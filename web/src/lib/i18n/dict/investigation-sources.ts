import { defineDict, translate, type Dictionary, type Translate } from "@/lib/i18n";

/**
 * investigation-sources — lib/investigation-sources.ts's OPERATOR-FACING
 * sentences: the scope form's refusals, commitWindow's two, the clamp banner,
 * and the words the timeline mappers put AROUND the rows they fold. Its sibling
 * lib/investigation.ts reads the same table for the four threshold headlines it
 * derives; the two files are one lib and splitting their words across two
 * dictionaries would only make «порог» easier to say twice.
 *
 * A module dictionary rather than a corner of dict/investigate.ts, for the
 * reason that file's own header gives: this is "a module outside that surface".
 * It is imported by pages/investigate.tsx AND by pages/diagnostics.tsx
 * (scopeNodeOptions), it is unit-tested on its own, and its functions are pure
 * — so it takes the wave's optional trailing translator, defaulting to English,
 * and every existing call and fixture answers the same bytes it always did.
 *
 * THE LINE THIS FILE DRAWS, and it is the whole of it: a timeline row is a
 * SERVER ROW wearing this console's connective words. The connective words are
 * ours — "Route changed", "Alert firing", "already firing when this window
 * opens", "until", "started by". The row is not:
 *
 *   - node, zone, target, alert and incident names; path hashes; label sets.
 *   - `r.type` and `r.status` (the runs enum), a K8s `kind`/`reason`/`message`,
 *     an audit `action`, an annotation's text, a window's reason, an alert's
 *     `severity` LABEL. Wire values a foreign rule may have written.
 *   - every stamp. The mappers hand the formatted instant in as a variable.
 *
 * IDENTITY IS NEVER TRANSLATED. `ref.kind`/`ref.id` are built from ids and
 * label sets, never from a title, so mergeTimeline's dedupe, pinKey and every
 * permalink read exactly the same bytes in both languages. Only `title` and
 * `detail` — display — move.
 */

const en = {
  /* ── scopeIncompleteReason ─────────────────────────────────────────────── */
  "scope.incomplete.sourceNode": "Choose a source node.",
  "scope.incomplete.destinationNode": "Choose a destination node.",
  "scope.incomplete.samePair": "A pair needs two different nodes.",
  "scope.incomplete.sourceZone": "Choose a source zone.",
  "scope.incomplete.destinationZone": "Choose a destination zone.",
  "scope.incomplete.node": "Choose a node.",
  "scope.incomplete.target": "Choose a target.",

  /* ── commitWindow ──────────────────────────────────────────────────────── */
  "window.inverted": "The range end must be after its start.",
  /* «реальное время», the Time Machine's present — dict/chrome.ts's
     "timemachine.returnToLive" and dict/shared.ts's write-guard say it the same
     way, and «Онлайн» is the events PAGE, not this. */
  "window.afterInstant": "The window is after the viewed instant — move the range back, or return to Live.",
  /* Stated as a fact about the WINDOW rather than as a warning about the mode:
     the Time Machine bar above it already explains the mode. */
  "banner.clamped": "Window clamped to the viewed instant.",

  /* ── scopeCaptionValue ─────────────────────────────────────────────────── */
  /* What the annotation and maintenance bars call a scope they did NOT filter
     by: the wide scopes ask every scope unfiltered, and saying "scope global"
     would name the one value they were not using. */
  "scope.allScopes": "all scopes",
  /* The global scope's name inside a timeline row's detail line. The bars keep
     their own copy in their own dictionaries — same word, different surface. */
  "scope.global": "global",

  /* ── the mappers' own words ────────────────────────────────────────────── */
  /* {sep} is PAIR_SEPARATOR, passed in: it is the arrow the whole console draws
     a pair with, not a word this table gets to choose. */
  "entry.pathChange.title": "Route changed: {src} {sep} {dst}",
  /* No plural forms and no `countForm` here: the counts are sidestepped the way
     lib/i18n/README.md recommends («хопов: 12»), which is also the only shape a
     PURE function can produce without being handed a locale as well as a
     translator. English now takes the same shape (QA scope 3, finding #22): the
     unconditional plural printed "1 hops" on any single-hop route, and a label
     before the colon is right for every count there is. */
  "entry.pathChange.detail": "path {hash} · hops: {hops} · traces: {traces}",

  "entry.run.title": "{type} run {status}",
  "entry.run.detail": "{ok}/{total} ok · started by {by}",

  "entry.maintenance.title": "Maintenance: {reason}",
  "entry.maintenance.detail": "{scope} · until {until} · {by}",
  /* The window's END is missing or unreadable. A stamp is not optional chrome
     here — "until" with nothing after it, or with the literal "Invalid Date"
     after it, reads as a broken console rather than as a row the server sent
     incomplete. */
  "entry.maintenance.detail.noEnd": "{scope} · end not stated · {by}",

  "entry.alert.title": "Alert firing: {name}",
  /* The DISPLACED row: an alert that started before the window is drawn at the
     window's own edge, so its title has to say that out loud and its detail
     spells the true start in ISO. Softening either half here would let a reader
     mistake the displaced row for a start inside the window. */
  "entry.alert.title.before": "Alert {name} — already firing when this window opens",
  "entry.alert.detail.before": "{severity} · firing since {since} · {labels}",
  "entry.alert.detail": "{severity} · {labels}",
  "entry.alert.noSeverity": "no severity",

  /* ── lib/investigation.ts's derived threshold rows ─────────────────────── */
  /* The one sibling module that had no table at all: crossings() built these
     four headlines as bare English literals, so a Russian console rendered a
     «порог» badge next to "RTT crossed the threshold" (QA scope 3, finding #6).
     The numbers are handed in already formatted — a percentage and a duration
     are the same digits in both languages. */
  "entry.threshold.loss.above": "Packet loss crossed the threshold",
  "entry.threshold.loss.recovered": "Packet loss recovered",
  "entry.threshold.rtt.above": "RTT crossed the threshold",
  "entry.threshold.rtt.recovered": "RTT recovered",
  "entry.threshold.loss.detail": "{value} (threshold {threshold})",
  "entry.threshold.rtt.detail": "{value} (threshold {threshold} = {factor}× median {median})",
} as const;

export type InvestigationSourcesKey = keyof typeof en;

export const investigationSourcesDict: Dictionary<InvestigationSourcesKey> = defineDict(en, {
  "scope.incomplete.sourceNode": "Выберите узел-источник.",
  "scope.incomplete.destinationNode": "Выберите узел назначения.",
  "scope.incomplete.samePair": "Паре нужны два разных узла.",
  "scope.incomplete.sourceZone": "Выберите зону-источник.",
  "scope.incomplete.destinationZone": "Выберите зону назначения.",
  "scope.incomplete.node": "Выберите узел.",
  "scope.incomplete.target": "Выберите цель.",

  "window.inverted": "Конец диапазона должен быть позже начала.",
  "window.afterInstant":
    "Интервал целиком позже просматриваемого момента. Сдвиньте диапазон назад или вернитесь в реальное время.",
  "banner.clamped": "Интервал обрезан по просматриваемому моменту.",

  "scope.allScopes": "все области",
  "scope.global": "глобальная",

  "entry.pathChange.title": "Путь изменился: {src} {sep} {dst}",
  "entry.pathChange.detail": "путь {hash} · хопов: {hops} · трассировок: {traces}",


  "entry.run.title": "запуск {type}: {status}",
  "entry.run.detail": "{ok}/{total} успешно · запустил {by}",

  "entry.maintenance.title": "Работы: {reason}",
  "entry.maintenance.detail": "{scope} · до {until} · {by}",
  "entry.maintenance.detail.noEnd": "{scope} · конец не указан · {by}",

  "entry.alert.title": "Активное оповещение: {name}",
  "entry.alert.title.before": "Оповещение {name}: уже горело, когда интервал открывался",
  "entry.alert.detail.before": "{severity} · активно с {since} · {labels}",
  "entry.alert.detail": "{severity} · {labels}",
  "entry.alert.noSeverity": "важность не указана",

  /* «порог» — то же слово, что на бейдже строки (dict/investigate.ts,
     kind.threshold). Одно понятие, одно слово. */
  "entry.threshold.loss.above": "Потери пакетов перешли порог",
  "entry.threshold.loss.recovered": "Потери пакетов вернулись в норму",
  "entry.threshold.rtt.above": "RTT перешёл порог",
  "entry.threshold.rtt.recovered": "RTT вернулся в норму",
  "entry.threshold.loss.detail": "{value} (порог {threshold})",
  "entry.threshold.rtt.detail": "{value} (порог {threshold} = {factor}× медианы {median})",
});

/** enT is the ENGLISH translator every pure function in this module defaults
 *  to — dict/topology.ts's pattern, and the reason existing calls, fixtures and
 *  the ~1600 English-pinned assertions never had to move. */
export const enT: Translate<InvestigationSourcesKey> = (key, vars) =>
  translate(investigationSourcesDict, "en", key, vars);
