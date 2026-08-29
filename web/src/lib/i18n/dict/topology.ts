import { defineDict, type Dictionary, type Translate, translate } from "@/lib/i18n";

/**
 * topology — pages/topology.tsx: the zone/node map's chrome. Its header, the
 * two empty states, the reconstruction notices, the legend, the edge caption,
 * the map's own control cluster and the words each node box announces to a
 * screen reader.
 *
 * There used to be a THIRD empty state, `empty.orphan.*`, for a response whose
 * `nodes` was empty while `agents` was not. It said zone lanes could not be
 * drawn — and the agents it was counting each carry a nodeName AND a zone,
 * which is every field a lane needs. The map is now built from them
 * (pages/topology.tsx's `mapNodes`), so the sentence went with the state it
 * described and `fromAgents.*` says what was actually true about it: the map
 * is real, and READINESS is the one field agents cannot supply.
 *
 * NOT HERE, on purpose:
 *   - node names and zone names. Both are printed verbatim, inside the boxes,
 *     in their `title` and in the aria-label built from `node.aria` below.
 *   - `problem.detail` behind "Topology is unavailable" — the card prints
 *     `error.message`, i.e. the 422's own retention sentence.
 *   - console.database.retentionDays, Kubernetes, DaemonSet, TCP.
 *
 * buildFlow is a PURE, exported, directly-tested function, so it takes its
 * translator as an argument and defaults to `enT` below rather than calling a
 * hook: a two-argument call from a unit test keeps producing exactly the
 * English it always did, out of this same `en` table.
 */

const en = {
  "title": "Topology",
  "description.live":
    "Live zone/node map. Problem paths (TCP fail ≥ 1%) are drawn worst-first; hover an edge for its failure ratio.",
  "description.engaged":
    "Zone/node map as of {at}, reconstructed from topology events. Problem paths (TCP fail ≥ 1%) are drawn worst-first; hover an edge for its failure ratio.",
  /* The "?" by the title (M7-5); the docs page is docs/console/topology. */
  "help.body":
    "An interactive zone/node map: nodes are grouped into zone lanes, and problem paths (TCP fail ≥ 1%) are drawn as edges, worst first. " +
    "Node colour uses the same tiers as the Matrix; hovering an edge shows its failure ratio or packet loss. " +
    "Selecting a node opens its node page. " +
    "With the Time Machine engaged, the map is reconstructed from topology events at the viewed instant — a different mechanism from the live view, with its own stated edge cases.",

  "error.title": "Topology is unavailable",
  "loading": "Loading topology…",

  /* ── a refresh failed ON TOP of a node set that did load ────────────────── */
  /* Separate from `error.*` because the two are different answers: "unavailable" over a map that is
     on screen claims the page has nothing, when what it actually has is something older. */
  "stale.title": "This map is no longer refreshing",
  "stale.body":
    "The last refresh did not come back, so what is below is the node set that loaded before it; the console keeps retrying on its own.",

  /* ── the request never left the browser ─────────────────────────────────── */
  /* A paused query is pending WITHOUT fetching, which is neither loading nor an error, and used to
     leave this page with a heading and nothing under it. */
  "offline.title": "Your browser reports no connection",
  "offline.body":
    "The request for the topology was never sent, so there is nothing to show yet; it goes out by itself as soon as the connection is back.",

  /* ── the fold hit its own bound ─────────────────────────────────────────── */
  "truncated.title": "This reconstruction is incomplete",
  "truncated.body":
    "More topology events precede this instant than the fold will read in one pass, so nodes whose only event is older than the window may be missing from the map.",

  /* ── events counted, none of them foldable ──────────────────────────────── */
  "unfoldable.title": "Nothing to reconstruct at this time",
  "unfoldable.body.one":
    "This console found {count} topology event at or before this instant and could fold {folded} of them into a node set: the rest name no node, so there is nothing to rebuild the map from. Those were recorded before the controller started attributing topology changes, so that stretch of history cannot be reconstructed — pick a more recent instant, where events name the node they are about.",
  "unfoldable.body.many":
    "This console found {count} topology events at or before this instant and could fold {folded} of them into a node set: the rest name no node, so there is nothing to rebuild the map from. Those were recorded before the controller started attributing topology changes, so that stretch of history cannot be reconstructed — pick a more recent instant, where events name the node they are about.",

  /* ── the map was drawn from AGENTS ──────────────────────────────────────── */
  /* Not an empty state: the agents carry a node name and a zone, which is every
     field a lane needs. What they cannot carry is READINESS, and this notice is
     where that gap is stated once instead of being implied by a missing badge. */
  "fromAgents.title": "Map built from registered agents",
  "fromAgents.body.one":
    "The controller has no Kubernetes node view, so this map is drawn from the {count} registered agent and the zone it registered with. Node colour still comes from the probe matrix; readiness is a Kubernetes node condition, so it is unknown here.",
  "fromAgents.body.many":
    "The controller has no Kubernetes node view, so this map is drawn from the {count} registered agents and the zones they registered with. Node colour still comes from the probe matrix; readiness is a Kubernetes node condition, so it is unknown here.",

  /* ── an empty node set: nothing anywhere, in two flavours ───────────────── */
  "empty.historical.title": "No nodes existed at this time",
  "empty.historical.body":
    "No topology event at or before this instant put a node into the cluster — try a later time, or check that it is inside console.database.retentionDays.",
  "empty.live.title": "No nodes reported by the controller yet",
  "empty.live.body":
    "Nodes appear as soon as agents register with the controller — check that the DaemonSet is running and the controller is reachable.",

  /* ── legend and the edge caption ────────────────────────────────────────── */
  "legend.ok": "Healthy",
  "legend.warn": "Degraded · worst path 1–10%",
  "legend.bad": "Failing · ≥ 10% or not ready",
  "edges.none": "no problem paths right now",
  "edges.capped": "showing {shown} worst of {total} problem paths",
  "edges.one": "{count} problem path",
  "edges.many": "{count} problem paths",

  /* ── what buildFlow writes into the picture ─────────────────────────────── */
  "zone.one": "{zone} · {count} node",
  "zone.many": "{zone} · {count} nodes",
  /* The lane for nodes the informer gave no zone label. A NAME is needed
     because the lane header is «{zone} · N nodes» and the empty string made it
     read « · 3 nodes» — a heading that names nothing, next to headings that do.
     It says what is true (no zone was reported), not that the nodes are
     zoneless in Kubernetes' eyes. */
  "zone.none": "no zone reported",
  "node.notReady": "not ready",
  /* React Flow renders `ariaLabel` onto the node element, so this IS what a
     screen reader says about each box: who, where, how. */
  /* {zone} arrives ALREADY worded — a real zone name, or the "no zone reported"
     sentence — so this template must not prefix it with the word again. */
  "node.aria.zone": "zone {zone}",
  /* An edge is a tab stop of its own; {detail} is edge.label / edge.label.loss. */
  "edge.aria": "{source} to {destination}, {detail}",
  /* React Flow's own per-element instructions, which are English by default and
     describe dragging that this read-only map does not allow. */
  "flow.node.a11y": "Press Enter to open this node's card.",
  "flow.edge.a11y": "A path between two nodes.",
  "node.aria": "{node}, {zone}, {health}",
  "node.aria.notReady": "{node}, {zone}, {health}, not ready",
  /* The agents-built map's third form. A box with no "not ready" badge would
     otherwise be announced exactly like a node the informer confirmed ready. */
  "node.aria.readyUnknown": "{node}, {zone}, {health}, readiness unknown",
  "health.ok": "healthy",
  "health.degraded": "degraded",
  "health.failing": "failing",
  /* The hover label on a problem edge. It names the vector the percentage came
     from: a path drawn because of packet loss must not read as a failure %. */
  "edge.label": "{pct}",
  "edge.label.loss": "{pct} loss",

  /* ── the React Flow control cluster ─────────────────────────────────────── */
  /* Rendered as our OWN ControlButtons: the library's default trio ships with
     English aria/title baked in and takes no per-button override. */
  "controls.aria": "Map controls",
  "controls.zoomIn": "Zoom in",
  "controls.zoomOut": "Zoom out",
  "controls.fitView": "Fit the whole map",
} as const;

export type TopologyKey = keyof typeof en;

/**
 * «Норма» / «деградация» / «сбой» — the matrix legend's and the Overview's
 * words, because the three surfaces read the same thresholds out of
 * lib/matrix-cells.ts and an operator moving between them must not have to
 * re-learn which word means which tier.
 */
export const topologyDict: Dictionary<TopologyKey> = defineDict(en, {
  "title": "Топология",
  "description.live":
    "Живая карта зон и узлов. Проблемные пути (TCP, сбой ≥ 1%) нарисованы худшими вперёд, наведите на ребро и увидите долю сбоев.",
  "description.engaged":
    "Карта зон и узлов на {at}, восстановлена из событий топологии. Проблемные пути (TCP, сбой ≥ 1%) нарисованы худшими вперёд, наведите на ребро и увидите долю сбоев.",
  "help.body":
    "Интерактивная карта зон и узлов: узлы сгруппированы в дорожки по зонам, а проблемные пути (TCP, сбой ≥ 1%) нарисованы рёбрами, худшие вперёд. " +
    "Цвет узла использует те же ступени, что и Матрица; наведение на ребро показывает долю сбоев или потери пакетов. " +
    "Выбор узла открывает его страницу. " +
    "С включённой Машиной времени карта восстанавливается из событий топологии на выбранный момент — это другой механизм, чем живой вид, и свои оговорки он называет сам.",

  "error.title": "Топология недоступна",
  "loading": "Загрузка топологии…",

  "stale.title": "Данные не обновляются",
  "stale.body":
    "Последнее обновление не вернулось, поэтому ниже показан тот набор узлов, который успел загрузиться до него. Консоль продолжает пробовать сама.",

  "offline.title": "Браузер сообщает, что соединения нет",
  "offline.body":
    "Запрос топологии даже не ушёл, показывать пока нечего. Он отправится сам, как только связь вернётся.",

  "truncated.title": "Эта реконструкция неполная",
  "truncated.body":
    "Событий топологии до этого момента больше, чем свёртка успевает прочитать за один проход. Узлы, чьё единственное событие старше интервала, на карту могут не попасть.",

  "unfoldable.title": "На этот момент восстанавливать нечего",
  "unfoldable.body.one":
    "Консоль нашла событий топологии на этот момент и раньше: {count}, а свернуть в набор узлов смогла {folded}: остальные не называют узел, и строить карту не из чего. Записали их до того, как контроллер начал указывать, какого узла касается изменение топологии, поэтому этот отрезок истории восстановить нельзя. Возьмите момент поближе, там события называют свой узел.",
  "unfoldable.body.many":
    "Консоль нашла событий топологии на этот момент и раньше: {count}, а свернуть в набор узлов смогла {folded}: остальные не называют узел, и строить карту не из чего. Записали их до того, как контроллер начал указывать, какого узла касается изменение топологии, поэтому этот отрезок истории восстановить нельзя. Возьмите момент поближе, там события называют свой узел.",

  "fromAgents.title": "Карта построена по зарегистрированным агентам",
  "fromAgents.body.one":
    "Вида на узлы Kubernetes у контроллера нет, поэтому карта нарисована по агентам: их {count}, зону каждый принёс со своей регистрации. Цвет узла по-прежнему считается из матрицы зондирования, а готовность приходит от информера узлов Kubernetes, так что здесь она неизвестна.",
  "fromAgents.body.many":
    "Вида на узлы Kubernetes у контроллера нет, поэтому карта нарисована по агентам: их {count}, зону каждый принёс со своей регистрации. Цвет узла по-прежнему считается из матрицы зондирования, а готовность приходит от информера узлов Kubernetes, так что здесь она неизвестна.",

  "empty.historical.title": "На этот момент узлов не было",
  "empty.historical.body":
    "Ни одно событие топологии на этот момент и раньше не вводило узел в кластер. Возьмите момент позже или проверьте, укладывается ли он в console.database.retentionDays.",
  "empty.live.title": "Контроллер пока не сообщил ни одного узла",
  "empty.live.body":
    "Узлы появятся, как только агенты зарегистрируются у контроллера. Проверьте, что DaemonSet запущен и контроллер доступен.",

  "legend.ok": "Норма",
  "legend.warn": "Деградация · худший путь 1–10%",
  "legend.bad": "Сбой · ≥ 10% или не готов",
  "edges.none": "проблемных путей сейчас нет",
  "edges.capped": "показаны {shown} худших из {total} проблемных путей",
  "edges.one": "проблемных путей: {count}",
  "edges.many": "проблемных путей: {count}",

  "zone.one": "{zone} · узлов: {count}",
  "zone.many": "{zone} · узлов: {count}",
  "zone.none": "зона не указана",
  "node.notReady": "не готов",
  "node.aria.zone": "зона {zone}",
  "edge.aria": "{source} → {destination}, {detail}",
  "flow.node.a11y": "Нажмите Enter, чтобы открыть карточку узла.",
  "flow.edge.a11y": "Путь между двумя узлами.",
  "node.aria": "{node}, {zone}, {health}",
  "node.aria.notReady": "{node}, {zone}, {health}, не готов",
  "node.aria.readyUnknown": "{node}, {zone}, {health}, готовность неизвестна",
  "health.ok": "в норме",
  "health.degraded": "деградация",
  "health.failing": "сбой",
  "edge.label": "{pct}",
  "edge.label.loss": "{pct} потерь",

  "controls.aria": "Управление картой",
  "controls.zoomIn": "Приблизить",
  "controls.zoomOut": "Отдалить",
  "controls.fitView": "Вписать всю карту",
});

/**
 * enT is buildFlow's default translator: this dictionary's ENGLISH half, read
 * through the same resolution rule a component gets from useT. It exists so a
 * pure function can carry page copy without becoming a hook, and so the unit
 * tests that call buildFlow(topo, matrix) keep reading English out of the one
 * table that defines it.
 */
export const enT: Translate<TopologyKey> = (key, vars) => translate(topologyDict, "en", key, vars);
