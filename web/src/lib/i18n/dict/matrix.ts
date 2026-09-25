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
  /* The "?" by the title (M7-5); the docs page is docs/console/matrix. */
  "help.body":
    "One cell per directed pair: source rows × destination columns, recomputed from Prometheus every 15s. " +
    "A cell shows the pair's failure percentage and its p95 RTT; UDP and ICMP cells also carry packet loss. " +
    "The protocol choice travels in the URL, so a matrix view is shareable as it stands. " +
    "Ctrl and the wheel zoom the grid, the wheel alone scrolls it, and a cell opens that pair's page. " +
    "Under a sparse topology plan, pairs no agent is assigned to probe render as dashed 'not probed' cells — expected silence, not an outage.",

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
  /* The same sentence for a hand with no wheel: what a phone can do with the
     grid is the browser's own pinch, and a drag inside the box. */
  "zoom.hint.touch": "Pinch to zoom, drag to scroll.",

  /* ── the grid ───────────────────────────────────────────────────────────── */
  "grid.caption": "Node-to-node failure ratio matrix, {protocol}",
  "grid.prefix": "Node names drop the shared prefix {prefix}",
  "grid.corner": "src \\ dst",
  "cell.self": "{node}: self",
  "cell.investigate": "Investigate {src} → {dst}",
  /* The cell's secondary line when the failure series is silent. */
  "cell.noFailData": "no fail data",
  "cell.loss": "loss {ratio}",
  "cell.mtuBlackhole": "black hole",
  "cell.mtuReduced": "of {probe}",
  "cell.mtuFull": "full size",

  "tooltip.unmeasured": "No probe data in Prometheus for this pair.",
  /* The sparse-plan cell (M10). Says what CAN still be done — Investigate probes on demand
     regardless of the plan — because the cell deliberately opens no pair page: a pair the plan
     excludes will never grow the continuous history that page promises. */
  "tooltip.notProbed":
    "The sparse topology plan assigns no agent to probe this pair, so no data is expected here. Investigate probes it on demand.",
  "tooltip.failRatio": "Failure ratio",
  "tooltip.noSamples": "no samples",
  "tooltip.rtt": "RTT p95",
  "tooltip.loss": "Packet loss",
  "tooltip.pathMtu": "Path MTU / probe size",

  /* The aria-label's reading of a sparse-plan cell — the one phrase that may claim the silence is
     INTENDED. cellSummary's "no data" stays reserved for a pair something should have measured. */
  "cell.notProbed": "not probed by the topology plan",

  /* ── legend ─────────────────────────────────────────────────────────────── */
  "legend.ok": "Healthy · fail < 1%",
  "legend.warn": "Degraded · 1–10%",
  "legend.bad": "Failing · ≥ 10%",
  "legend.unknown": "No data",
  /* Rendered ONLY while a sparse plan is in force: in full mode the state cannot occur, and a
     legend row for an impossible state would send readers hunting for it. */
  "legend.notProbed": "Not probed · excluded by the topology plan",
  /* The green row reads "fail < 1%", and a cell with NO fail samples is green
     too — so the note has to say on what grounds. It is green by the ABSENCE of
     a bad signal, not by a measured zero, and the console never turns the one
     into the other (QA scope 2, finding #12). */
  "legend.note":
    "colour = worst of fail % and packet loss · a cell with no fail samples shows its p95 and stays green on the absence of a bad signal, not on a measured zero",
  /* The PMTU grid draws sizes, not latencies: the note says what its figure and its amber mean. */
  "legend.note.pmtu":
    "figure = the largest datagram that crossed, in bytes · amber = reduced, the path is smaller than the probe and says so · red = black hole, full-size datagrams vanish without an ICMP error",

  /* ── the row and column headers ─────────────────────────────────────────── */
  "header.node": "Open the card for {node}",

  /* ── external agents: bare hosts in the grid ────────────────────────────── */
  /* The header of an agent that runs outside any Pod (kconmon-ng.io/external:
     "true"): the tooltip's second line under the name, and the header's aria.
     «внешний» is the one word for it across topology, cards and overview;
     lib/i18n/cards.test.tsx pins the equality. */
  "header.external": "external agent",
  "header.node.external": "Open the card for {node}, external agent",
  /* An external agent that is never a cell's SOURCE while the grid has cells
     at all: Prometheus is not scraping it, so its row is silence with a KNOWN
     cause. The cell keeps the 'no data' fill, the em-dash and the aria — it IS
     no data — and only the tooltip says why, and what to do about it. */
  "tooltip.unscraped":
    "No series from {src}: Prometheus is not scraping this external agent's metrics port. Add a scrape job — see External agents docs.",
  /* Where every "see External agents docs" points. A URL is data, identical in
     both halves; ONE key so the target moves in one place (lib/agents.ts reads
     it for the surfaces outside this file). */
  "docs.scrapeExternal": "https://esdmitrii.github.io/kconmon-ng/external-agents/#scraping-external-agents",
  /* The note above the grid, beside the prefix note, listing the unscraped
     external agents by name; gone the moment a source has a single cell. */
  "note.unscraped.one":
    "{nodes} is an external agent Prometheus is not scraping, so its row has no data. Add a scrape job for its metrics port — see External agents docs.",
  "note.unscraped.many":
    "{nodes} are external agents Prometheus is not scraping, so their rows have no data. Add a scrape job for their metrics ports — see External agents docs.",
  /* A source that advertised its planes (plane:* capabilities) and left this
     protocol out. Dashed like 'not probed': expected silence. An agent that
     advertised NO plane is read as running every plane (lib/agents.ts's
     fail-open rule), so a pre-2.4.0 agent never lands here. */
  "tooltip.unsupported": "{node} does not run {protocol} probes",
  /* The aria-label's reading of that cell, after the "src → dst: " part. */
  "cell.unsupported": "the source does not run {protocol} probes",
  /* Rendered only while at least one such cell is on the grid, like legend.notProbed. */
  "legend.unsupported": "Not run · the source does not run this protocol's probes",
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
  "help.body":
    "Одна ячейка на направленную пару: источники по строкам, назначения по столбцам, пересчёт из Prometheus каждые 15 с. " +
    "В ячейке — доля сбоев пары и её p95 RTT; у ячеек UDP и ICMP ещё и потери пакетов. " +
    "Выбор протокола лежит в URL, так что вид матрицы можно передать ссылкой как есть. " +
    "Ctrl с колесом масштабируют сетку, колесо само по себе её прокручивает, а ячейка открывает страницу своей пары. " +
    "При разреженном плане топологии пары, которые никто не зондирует по плану, рисуются пунктирными ячейками «не зондируется» — это ожидаемая тишина, а не сбой.",

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
  "zoom.hint.touch": "Щипок меняет масштаб, перетаскивание прокручивает сетку.",

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
  "cell.mtuBlackhole": "чёрная дыра",
  "cell.mtuReduced": "из {probe}",
  "cell.mtuFull": "полный размер",

  "tooltip.unmeasured": "Для этой пары в Prometheus нет данных зондов.",
  "tooltip.notProbed":
    "План разреженной топологии не назначает агента зондировать эту пару, данных здесь не ожидается. «Расследовать» зондирует её по требованию.",
  "tooltip.failRatio": "Доля сбоев",
  "tooltip.noSamples": "нет выборок",
  "tooltip.rtt": "RTT p95",
  "tooltip.loss": "Потери пакетов",
  "tooltip.pathMtu": "MTU пути / размер пробы",

  /* «не зондируется», not «нет данных»: вторая формулировка зарезервирована за парой, которую
     ДОЛЖНЫ были измерить. Здесь тишина запланирована. */
  "cell.notProbed": "не зондируется по плану топологии",

  "legend.ok": "Норма · сбой < 1%",
  "legend.warn": "Деградация · 1–10%",
  "legend.bad": "Сбой · ≥ 10%",
  "legend.unknown": "Нет данных",
  "legend.notProbed": "Не зондируется · исключено планом топологии",
  "legend.note":
    "цвет = худшее из доли сбоев и потерь пакетов · ячейка без выборок сбоев показывает свой p95 и остаётся зелёной потому, что плохого сигнала нет, а не потому, что измерен ноль",
  "legend.note.pmtu":
    "число = наибольшая прошедшая датаграмма в байтах · янтарный = путь меньше пробы и сообщает об этом · красный = чёрная дыра, полноразмерные датаграммы пропадают без ICMP-ошибки",

  "header.node": "Открыть карточку узла {node}",

  "header.external": "внешний агент",
  "header.node.external": "Открыть карточку узла {node}, внешний агент",
  "tooltip.unscraped":
    "Серий от {src} нет: Prometheus не собирает метрики с порта этого внешнего агента. Добавьте scrape job, см. документацию по внешним агентам.",
  "docs.scrapeExternal": "https://esdmitrii.github.io/kconmon-ng/external-agents/#scraping-external-agents",
  "note.unscraped.one":
    "{nodes}: внешний агент, метрики которого Prometheus не собирает, поэтому в его строке нет данных. Добавьте scrape job на его порт метрик, см. документацию по внешним агентам.",
  "note.unscraped.many":
    "{nodes}: внешние агенты, метрики которых Prometheus не собирает, поэтому в их строках нет данных. Добавьте scrape job на их порты метрик, см. документацию по внешним агентам.",
  "tooltip.unsupported": "{node} не запускает зонды {protocol}",
  /* «не запускает», not «нет данных»: the second phrase is reserved for a pair
     something should have measured. Here the source said it would not. */
  "cell.unsupported": "источник не запускает зонды {protocol}",
  "legend.unsupported": "Не запускается · источник не запускает зонды этого протокола",
});
