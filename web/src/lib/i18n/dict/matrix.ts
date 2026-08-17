import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * matrix — pages/matrix.tsx: the N×N grid's chrome. Its header, the protocol
 * switch, the cell tooltip's field names, the legend and the two empty states.
 *
 * NOT HERE, on purpose:
 *   - node names (the row and column headers, the tooltip's two lines and the
 *     `title` on each) and every measured number.
 *   - the protocol names TCP / UDP / ICMP, and `pod` — the plane is a
 *     Kubernetes network plane, not a word.
 *   - `problem.detail` behind "Matrix is unavailable": the card prints
 *     `error.message`, which is the server's own sentence.
 *   - **lib/matrix-cells.ts's `cellSummary`**, which is the whole text of every
 *     cell's aria-label after the "src → dst: " part ("no data", "fail 50.0%",
 *     "RTT p95 2.0ms", "no failure signal recorded"). It is SHARED with the
 *     Overview, the object cards and the topology edges — one reading of what a
 *     cell means, deliberately in one file — so it is another surface's to
 *     translate, not this page's to fork.
 *
 * The grid's corner header is «откуда \ куда» rather than "src \ dst": those
 * three-letter forms are English abbreviations on screen, not the API's field
 * names (which are `source` and `destination` and never rendered).
 */

const en = {
  "title": "Matrix",
  "description.live": "Live N×N node connectivity, recomputed from Prometheus every 15s.",
  "description.engaged":
    "N×N node connectivity as of {at}, evaluated straight from Prometheus at that instant.",

  "protocol.aria": "Protocol",
  "plane": "plane: pod",

  "error.title": "Matrix is unavailable",
  "loading": "Loading matrix…",

  /* ── the two empty states, live and engaged ─────────────────────────────── */
  "empty.live.title": "No probe data in Prometheus yet",
  "empty.live.body":
    "The {protocol} matrix fills in once the agents complete a probe round and Prometheus scrapes them — usually within a minute of the DaemonSet becoming ready.",
  "empty.engaged.title": "No probe data in Prometheus at this time",
  "empty.engaged.body":
    "Nothing was scraped for {protocol} probes at that instant — it may predate the deployment, or fall outside Prometheus' own retention.",

  /* ── the zoom ───────────────────────────────────────────────────────────────
     The topology map's own three words, deliberately repeated rather than
     shared: two surfaces, two files (lib/i18n/README.md). What must NOT differ
     is the vocabulary, and it does not. */
  "zoom.aria": "Zoom",
  "zoom.in": "Zoom in",
  "zoom.out": "Zoom out",
  "zoom.fit": "Fit to view",
  "zoom.level": "{pct}%",
  /* The shortcut, stated rather than left to be discovered. A plain wheel is
     deliberately NOT zoom: it is how a grid wider than its box is panned. */
  "zoom.hint": "Ctrl and the wheel zoom the grid; the wheel alone scrolls it.",

  /* ── the grid ───────────────────────────────────────────────────────────── */
  "grid.caption": "Node-to-node failure ratio matrix, {protocol}",
  "grid.prefix": "Node names drop the shared prefix {prefix}",
  "grid.corner": "src \\ dst",
  "cell.self": "{node}: self",
  "cell.investigate": "Investigate {src} → {dst}",
  /* The cell's secondary line when the failure series is silent. */
  "cell.noFailData": "no fail data",
  "cell.loss": "loss {ratio}",

  "tooltip.unmeasured": "No probe data in Prometheus for this pair.",
  "tooltip.failRatio": "Failure ratio",
  "tooltip.noSamples": "no samples",
  "tooltip.rtt": "RTT p95",
  "tooltip.loss": "Packet loss",

  /* ── legend ─────────────────────────────────────────────────────────────── */
  "legend.ok": "Healthy · fail < 1%",
  "legend.warn": "Degraded · 1–10%",
  "legend.bad": "Failing · ≥ 10%",
  "legend.unknown": "No data",
  /* The green row reads "fail < 1%", and a cell with NO fail samples is green
     too — so the note has to say on what grounds. It is green by the ABSENCE of
     a bad signal, not by a measured zero, and the console never turns the one
     into the other (QA scope 2, finding #12). */
  "legend.note":
    "colour = worst of fail % and packet loss · a cell with no fail samples shows its p95 and stays green on the absence of a bad signal, not on a measured zero",

  /* ── the row and column headers ─────────────────────────────────────────── */
  "header.node": "Open the card for {node}",
} as const;

export type MatrixKey = keyof typeof en;

/**
 * The tier vocabulary is the Overview's and the Topology's: «норма» /
 * «деградация» / «сбой». One word per concept across the three surfaces that
 * read the same lib/matrix-cells.ts thresholds.
 */
export const matrixDict: Dictionary<MatrixKey> = defineDict(en, {
  "title": "Матрица",
  "description.live": "Живая связность узлов N×N, пересчитывается из Prometheus каждые 15 с.",
  "description.engaged": "Связность узлов N×N на {at}, посчитана прямо из Prometheus на этот момент.",

  "protocol.aria": "Протокол",
  "plane": "плоскость: pod",

  "error.title": "Матрица недоступна",
  "loading": "Загрузка матрицы…",

  "empty.live.title": "В Prometheus ещё нет данных зондов",
  "empty.live.body":
    "Матрица {protocol} заполнится, когда агенты пройдут круг зондирования, а Prometheus их соберёт. Обычно это минута с того момента, как DaemonSet стал готов.",
  "empty.engaged.title": "На этот момент данных зондов в Prometheus нет",
  "empty.engaged.body":
    "Для зондов {protocol} на тот момент ничего не собрано. Возможно, момент раньше развёртывания, а возможно, он выпал из собственного retention Prometheus.",

  "zoom.aria": "Масштаб",
  "zoom.in": "Приблизить",
  "zoom.out": "Отдалить",
  "zoom.fit": "Вписать",
  "zoom.level": "{pct}%",
  "zoom.hint": "Ctrl с колесом меняет масштаб, одно колесо прокручивает сетку.",

  "grid.caption": "Матрица доли сбоев между узлами, {protocol}",
  "grid.prefix": "В именах узлов опущен общий префикс {prefix}",
  "grid.corner": "откуда \\ куда",
  "cell.self": "{node}: сам к себе",
  "cell.investigate": "Расследовать {src} → {dst}",
  /* «сбои: н/д», not «нет данных о сбоях». The cell is 4rem wide at its floor
     and this line sits UNDER the p95 — the long form is 102px against a 76–95px
     cell, so it wrapped into two ragged lines and the tier rail painted over
     the first letter (owner report). Two things had to survive the cut:
       - the SCOPE. «нет данных о сбоях» opens with «нет данных» — the phrase
         reserved for a pair nothing probed (legend.unknown, cellSummary's
         `noData`) — so a wrap left «нет данных о» on its own line, above a p95,
         saying the opposite of the truth. «сбои: …» puts the scope FIRST, the
         way the English "no fail data" scopes "no data" with "fail".
       - the HONESTY. «н/д» is «нет данных» about the failure figure: the lazy
         counter emitted no sample. It does NOT say «сбоев нет» — the console
         never turns silence into a measured zero (see tooltip.noSamples).
     The full sentence still lives in the aria-label and the tooltip
     (dict/matrix-cells.ts «данных о сбоях не записано», tooltip.noSamples
     «нет выборок»); this is the 10.5px line that has to fit a grid cell. */
  "cell.noFailData": "сбои: н/д",
  "cell.loss": "потери {ratio}",

  "tooltip.unmeasured": "Для этой пары в Prometheus нет данных зондов.",
  "tooltip.failRatio": "Доля сбоев",
  "tooltip.noSamples": "нет выборок",
  "tooltip.rtt": "RTT p95",
  "tooltip.loss": "Потери пакетов",

  "legend.ok": "Норма · сбой < 1%",
  "legend.warn": "Деградация · 1–10%",
  "legend.bad": "Сбой · ≥ 10%",
  "legend.unknown": "Нет данных",
  "legend.note":
    "цвет = худшее из доли сбоев и потерь пакетов · ячейка без выборок сбоев показывает свой p95 и остаётся зелёной потому, что плохого сигнала нет, а не потому, что измерен ноль",

  "header.node": "Открыть карточку узла {node}",
});
