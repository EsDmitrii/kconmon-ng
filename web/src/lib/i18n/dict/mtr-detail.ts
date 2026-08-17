import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * mtr-detail — the THREE shared components that render one stored path and what
 * happened around it: components/mtr-hop-table.tsx (the hop table, the
 * enrichment row, the per-hop RTT trend), components/mtr-path-diff.tsx (the
 * aligned two-column diff) and components/mtr-changes-timeline.tsx (the path
 * markers over the pair's loss series).
 *
 * ONE file for three components because they are one PANE: pages/mtr.tsx mounts
 * all three in its detail column, they share fmtTime and shortHash, and the
 * hop-table's own words ("hop", "path", "trace") have to be the diff's and the
 * timeline's. dict/mtr.ts's header already points here — the page keeps its
 * chrome, the pane keeps its table.
 *
 * The count vocabulary is dict/mtr.ts's, deliberately and to the letter: «путь
 * / пути / путей», «трассировка / трассировки / трассировок», «хоп». The pane
 * and the history list beside it name the same objects.
 *
 * NOT HERE, on purpose:
 *   - hop addresses, hostnames, path hashes, node and destination names, and
 *     every RTT and loss figure. Data.
 *   - "MTR", "RTT", "DNS", "Δ RTT", the `peer`/`external` metric family names
 *     and `console.mtr.enrichment.enabled` / `console.prometheus.address`.
 *     Protocol names, metric names and config keys.
 *   - the "*" a tracer writes for a hop that never answered, and the em-dash
 *     for an absent value. Symbols.
 *   - every stamp: fmtTime is toLocaleString in the VIEWER's locale.
 */

const en = {
  /* ── the hop table ─────────────────────────────────────────────────────── */
  "table.aria": "Hops",
  /* The expander column's header, screen-reader only — the cells hold a chevron
     and would otherwise be an unnamed column. */
  "table.expand": "Expand",
  "table.address": "Address",
  "table.hostname": "Hostname",
  "table.rtt": "RTT",
  "table.loss": "Loss",
  /* Shown ONLY while the table really does run past its card — an affordance
     that is always on the screen is decoration nobody reads. */
  "table.scrollHint": "Scroll sideways for the remaining columns.",

  "snapshot.firstSeen": "First seen",
  "snapshot.lastSeen": "Last seen",
  "snapshot.traces": "Traces",

  /* ── the traces behind the route ───────────────────────────────────────── */
  /* A route row folds every trace that walked it into one count and one hop
     table — the LAST reading. The count used to be a dead end: «а как их
     посмотреть???». These name the list that answers it. */
  "traces.title": "Traces of this route",
  "traces.aria": "Traces of this route",
  "traces.count": "{shown} of {total}",
  "traces.loading": "Loading traces…",
  "traces.error": "Could not load the traces of this route.",
  /* Retention is the honest reason a non-zero count can have an empty list:
     traces age out with the RUN sweep, the route with the path-history one. */
  "traces.empty":
    "No stored traces remain for this route — the route outlived them (traces are kept with the diagnostics runs, not with the path history).",
  "traces.loadOlder": "Load older",
  "traces.loadingOlder": "Loading older…",
  /* A page that failed, said beside the button that asked for it — the pages already on screen stay
     on screen. */
  "traces.olderError": "Could not load more traces. The ones above are still what was recorded.",
  "traces.failed": "failed",
  "traces.hops.aria": "Hops of this trace",

  /* ── the enrichment row ────────────────────────────────────────────────── */
  "hop.enrichment.aria": "Enrichment for hop {number}, {ip}",
  /* A `*` in a route: the hop did not answer. Not "no hop" — the packet still
     went through something, it just did not say what. */
  /* "Did not answer" is what the tracer OBSERVED, and on a cluster where the agent has no raw ICMP
     socket that is all it can observe: TTL-exceeded replies from intermediate routers are not
     delivered to the unprivileged datagram socket, so every hop before the destination reads this
     way whether or not it really answered. The wording says what was seen rather than what the
     network did. */
  "hop.silent": "no reply was seen from this hop",
  "enrichment.rdns": "Reverse DNS",
  "enrichment.network": "Network",
  "enrichment.location": "Location",
  /* Covers BOTH causes rather than asserting one: the API answers an empty map
     whether enrichment is switched off or simply knows nothing, and no config
     endpoint exposes which. Naming the knob is what an operator can act on. */
  "enrichment.empty":
    "No enrichment recorded for this address — enrichment may be disabled on this console " +
    "(console.mtr.enrichment.enabled, off by default), or it is on and no source knows this address.",

  /* ── the per-hop RTT trend ─────────────────────────────────────────────── */
  "trend.aria": "RTT trend for {ip}",
  "trend.title": "RTT trend for {ip}",
  "trend.noRtt": "None of the loaded paths carries an RTT for this address.",
  /* What the chart is NOT showing. `{traces}` is the whole parenthesis or the
     empty string — one key, so the Russian can move the clause rather than
     having a fragment glued on at a fixed position. It names the history
     pane's own button, so its wording must match dict/mtr.ts's
     "history.loadOlder". */
  "trend.partial": "Trend covers the {paths} loaded here{traces} — use Load older in the path history to widen it.",
  "trend.partial.traces": " ({loaded} of the pair's {total})",
  "trend.paths.one": "{count} path",
  "trend.paths.few": "{count} paths",
  "trend.paths.many": "{count} paths",
  "trend.traces.one": "{count} trace",
  "trend.traces.few": "{count} traces",
  "trend.traces.many": "{count} traces",

  /* ── the path diff ─────────────────────────────────────────────────────── */
  "diff.aria": "Path diff",
  /* The marker column's header, screen-reader only. */
  "diff.change": "Change",
  /* The visible key for the marks. They had a title and an aria-label, which
     left them legible to a screen reader and to nobody looking at the screen. */
  "diff.legend": "What the marks mean",
  "diff.older": "Older",
  "diff.newer": "Newer",
  "diff.identical": "Both paths visit the same hops in the same order — only the recorded round-trip times differ.",
  /* The marker's own accessible NAME. DiffKind is a type before it is a word —
     the same call dict/palette.ts made for CommandGroup — so the union stays
     English in the code and these are what it renders as. */
  "diff.kind.same": "same",
  "diff.kind.changed": "changed",
  "diff.kind.added": "added",
  "diff.kind.removed": "removed",
  "diff.kind.same.title": "the same address in both paths",
  "diff.kind.changed.title": "a different address at the same place in the route",
  "diff.kind.added.title": "only the newer path visits this hop",
  "diff.kind.removed.title": "only the older path visited this hop",

  /* ── the path-changes strip ────────────────────────────────────────────── */
  "changes.aria": "Path changes over time",
  "changes.list.aria": "Path changes",
  "changes.marker.aria": "Path {hash} first seen {at}",
  /* Two lines: the FULL hash, then when it was first seen. The newline is part
     of the tooltip's shape, not punctuation to be tidied away. */
  "changes.marker.title": "{hash}\nfirst seen {at}",
  "changes.promUnset": "Path changes only — set console.prometheus.address to overlay this pair's packet loss.",
  "changes.empty":
    "No loss series for this pair over the window — nothing is probing it, or the {family} metric family carries no " +
    "samples for it yet.",
  "changes.loading": "Loading the pair's loss series…",
  /* Prometheus's own error envelope RESOLVES rather than throws (lib/api.ts's
     `handle`), so a query-level failure lands under the strip. This is only the
     stand-in for a failure that carried no sentence of its own — when it did,
     the server's words render verbatim. */
  "changes.queryFailed": "query failed",
  /* The pair's route history can reach back further than the PromQL proxy will
     answer for; the loss series is then the last {hours}h of the same window. */
  "changes.clamped": "Loss shown for the last {hours}h — the route history reaches further back than this console will query.",
} as const;

export type MTRDetailKey = keyof typeof en;

export const mtrDetailDict: Dictionary<MTRDetailKey> = defineDict(en, {
  "table.aria": "Хопы",
  "table.expand": "Развернуть",
  "table.address": "Адрес",
  "table.hostname": "Имя хоста",
  "table.rtt": "RTT",
  "table.loss": "Потери",
  "table.scrollHint": "Прокрутите вбок, там ещё колонки.",

  "snapshot.firstSeen": "Впервые виден",
  "snapshot.lastSeen": "Последний раз виден",
  "snapshot.traces": "Трассировок",

  "traces.title": "Трассировки этого маршрута",
  "traces.aria": "Трассировки этого маршрута",
  "traces.count": "{shown} из {total}",
  "traces.loading": "Загружаем трассировки…",
  "traces.error": "Не удалось загрузить трассировки этого маршрута.",
  "traces.empty":
    "Сохранённых трассировок для этого маршрута не осталось: маршрут пережил их — трассировки хранятся вместе с прогонами диагностики, а не с историей путей.",
  "traces.loadOlder": "Загрузить старые",
  "traces.loadingOlder": "Загружаем старые…",
  "traces.olderError": "Не удалось догрузить трассировки. То, что выше, по-прежнему то, что записано.",
  "traces.failed": "ошибка",
  "traces.hops.aria": "Хопы этой трассировки",

  "hop.enrichment.aria": "Обогащение для хопа {number}, {ip}",
  "hop.silent": "ответа от этого хопа не получено",
  "enrichment.rdns": "Обратный DNS",
  "enrichment.network": "Сеть",
  "enrichment.location": "Расположение",
  "enrichment.empty":
    "Для этого адреса обогащения не записано. Либо оно выключено на этой консоли " +
    "(console.mtr.enrichment.enabled, по умолчанию так и есть), либо включено, но адрес не знает ни один источник.",

  "trend.aria": "Тренд RTT для {ip}",
  "trend.title": "Тренд RTT для {ip}",
  "trend.noRtt": "Ни у одного из загруженных путей нет RTT для этого адреса.",
  "trend.partial":
    "Тренд покрывает загруженные здесь {paths}{traces}. Чтобы расширить, нажмите «Загрузить старые» в истории путей.",
  "trend.partial.traces": " ({loaded} из {total} у этой пары)",
  "trend.paths.one": "{count} путь",
  "trend.paths.few": "{count} пути",
  "trend.paths.many": "{count} путей",
  "trend.traces.one": "{count} трассировка",
  "trend.traces.few": "{count} трассировки",
  "trend.traces.many": "{count} трассировок",

  "diff.aria": "Разница путей",
  "diff.change": "Изменение",
  "diff.legend": "Что значат пометки",
  "diff.older": "Старый",
  "diff.newer": "Новый",
  "diff.identical": "Оба пути проходят одни и те же хопы в том же порядке, различаются только записанные RTT.",
  "diff.kind.same": "совпадает",
  "diff.kind.changed": "изменился",
  "diff.kind.added": "добавлен",
  "diff.kind.removed": "убран",
  "diff.kind.same.title": "тот же адрес в обоих путях",
  "diff.kind.changed.title": "другой адрес на том же месте маршрута",
  "diff.kind.added.title": "этот хоп есть только в новом пути",
  "diff.kind.removed.title": "этот хоп был только в старом пути",

  "changes.aria": "Смены путей во времени",
  "changes.list.aria": "Смены путей",
  "changes.marker.aria": "Путь {hash}, впервые виден {at}",
  "changes.marker.title": "{hash}\nвпервые виден {at}",
  "changes.promUnset":
    "Только смены путей. Задайте console.prometheus.address, и сверху лягут потери пакетов этой пары.",
  "changes.empty":
    "За этот интервал по паре нет серии потерь: либо её никто не зондирует, либо в семействе метрик {family} " +
    "для неё пока нет выборок.",
  "changes.loading": "Загружаем серию потерь для этой пары…",
  "changes.queryFailed": "запрос не выполнен",
  "changes.clamped": "Потери показаны за последние {hours} ч: история путей уходит дальше, чем консоль готова запросить.",
});

/** countForm picks between the `.one` / `.few` / `.many` keys above (paths,
 *  traces). A per-dictionary copy, the same one dict/mtr.ts keeps — lib/i18n
 *  ships no plural machinery and the README forbids a shared file for six
 *  lines. English needs two forms and 21 must read "21 paths"; Russian needs
 *  three and 21 must read «21 путь». */
export function countForm(locale: string, n: number): "one" | "few" | "many" {
  if (locale !== "ru") return n === 1 ? "one" : "many";
  const teen = n % 100;
  if (teen >= 11 && teen <= 14) return "many";
  const last = n % 10;
  if (last === 1) return "one";
  if (last >= 2 && last <= 4) return "few";
  return "many";
}
