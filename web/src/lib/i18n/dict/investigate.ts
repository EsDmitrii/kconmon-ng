import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * investigate — Investigation Mode: pages/investigate.tsx (the entry form, the
 * actions rail, incident mode, the ranked causes) AND its centre pane,
 * components/investigation-timeline.tsx. One surface, one file: the pane is not
 * shared with any other route, and its per-source notes are written by the page
 * that owns them.
 *
 * This is the console's most text-heavy surface, and almost all of that text is
 * HONESTY COPY — one line per source saying what the timeline is missing and
 * why. Every one of those sentences must survive translation with the SAME
 * caveat at the SAME strength: "was not requested" is not "failed", "the newest
 * 200 rows" is not "all rows", and "nothing was requested for this instant" is
 * not "there is nothing". A softened Russian caveat is a lie the English does
 * not tell.
 *
 * NOT HERE, on purpose:
 *   - permission strings (`events:read`, `incidents:write`, `promql:query`),
 *     config keys (`console.database.mode`, `console.prometheus.address`) and
 *     endpoints (`GET /api/v1/audit`, `/api/v1/mtr/snapshots`). Identifiers.
 *   - node, zone, target and incident names, incident ids, pinned-ref kinds and
 *     ids, and `params.kind` in the header badge: wire values.
 *   - every timeline ENTRY's title and detail, `scopeIncompleteReason`,
 *     `commitWindow`'s refusals and CLAMPED_BANNER. All of them belong to
 *     lib/investigation-sources.ts, a module outside this surface — and they
 *     now live in dict/investigation-sources.ts, with that module's own tests.
 *     The page hands those pure functions a translator (`ts`); the SERVER's
 *     half of each row — an audit action, a k8s reason, a node name — stays
 *     data in both languages.
 *   - the range preset tokens 15m / 1h / 6h; only "Custom" is a word.
 *   - problem+json detail from any of the nine sources: the server's sentence,
 *     rendered verbatim after the source's own name.
 */

const en = {
  "title": "Incidents",
  "description":
    "One window over one scope: every source the console can read, merged into a timeline, with the correlation rules written down rather than guessed at.",
  /* The "?" by the title (M7-5); the docs page is docs/console/incidents. */
  "help.body":
    "One window over one scope: commit a pair, node, target, zone pair or the whole cluster, and a time range. " +
    "The timeline merges every source the console can read — fleet and k8s events, audit rows, annotations, maintenance windows, diagnostic runs, MTR path changes, threshold crossings and firing alerts. " +
    "The correlation rules are written on the page rather than guessed at. " +
    "Everything lives in the URL, so the view doubles as an incident permalink.",

  /* ── the scope vocabulary ──────────────────────────────────────────────── */
  "scope.aria": "Scope kind",
  "scope.label": "Scope",
  "scope.pair": "Pair",
  "scope.node": "Node",
  "scope.target": "Target",
  "scope.zonePair": "Zone pair",
  "scope.cluster": "Cluster",
  /* The headline beside the page title. The pair form is two node names and an
     arrow, i.e. pure data, and has no key. */
  "scope.headline.zonePair": "zone {a} {sep} zone {b}",
  "scope.headline.cluster": "the whole cluster",
  "scope.headline.empty": "(nothing selected)",

  /* ── the entry form ────────────────────────────────────────────────────── */
  "form.aria": "Investigation scope",
  "form.sourceNode": "Source node",
  "form.destinationNode": "Destination node",
  "form.node": "Node",
  "form.sourceZone": "Source zone",
  "form.destinationZone": "Destination zone",
  "form.target": "Target",
  "form.range": "Range",
  "form.range.aria": "Range preset",
  "form.range.custom": "Custom",
  "form.rangeStart": "Range start",
  "form.rangeEnd": "Range end",
  "form.submit": "Investigate",
  /* The committed scope names something the option list does not have — a
     drained node, a deleted target, a hand-typed name, or simply a list this
     subject may not read. The picker draws it anyway (see the Select in
     pages/investigate.tsx) so that the control and the headline beside the page
     title agree; the mark is what stops the reader reading it as a live object. */
  "form.option.missing": "{value} — not in the current topology",
  "form.targetsGated":
    "The target list needs targets:read. The scope still works from a permalink — the URL carries the target's " +
    "name, not its id.",
  "form.urlNote":
    "Everything above is in the URL (?kind=&scope=&from=&to=) — this page is shareable as it stands, " +
    "which is also what makes an incident permalink free.",
  /* {params} is the list of URL keys, the URL's own vocabulary — data. The
     parser is total and degrades every malformed value to something fetchable,
     which is right; doing it in SILENCE was not. */
  "ignored.body":
    "This link carried {params}, which this page could not read — the defaults below are what it is showing " +
    "instead, and the address bar has been corrected to match.",
  "ignored.dismiss": "Dismiss",

  /* ── the actions rail ──────────────────────────────────────────────────── */
  "actions.aria": "Actions",
  "actions.runMTR": "Run MTR now",
  "actions.runTCP": "Run TCP now",
  "actions.compare": "Compare in Metrics",
  "actions.export": "Export JSON",
  "actions.saveIncident": "Save as incident",
  "actions.createMaintenance": "Create maintenance",
  /* The Metrics page's A/B slots are bound to curated metrics and it reads no
     range from the URL — saying so beats a link that quietly drops half of
     what it promised. */
  "actions.compareNote":
    "The Metrics page binds its A/B slots to curated metrics and reads no range from the URL, so this link " +
    "opens the page and the window has to be chosen there.",
  /* Two keys around the run-id link. */
  "actions.runStarted.before": "Run",
  "actions.runStarted.after": "started.",
  "actions.runFailed": "Failed to start the run",

  /* ── save as incident ──────────────────────────────────────────────────── */
  "save.aria": "Save as incident",
  "save.scopeLabel": "Scope",
  "save.global": "global",
  "save.window": "{from} → {to} — taken from this investigation, not editable here.",
  /* The one lossy case, said out loud rather than discovered on reopen. */
  "save.wideNote":
    "A zone pair and the whole cluster both save as the GLOBAL scope — that vocabulary has no zone member — so " +
    "reopening this incident frames the cluster. The range is kept exactly.",
  "save.title": "Title",
  "save.title.aria": "Incident title",
  "save.title.placeholder": "Packet loss between node-a and node-b",
  "save.notes": "Notes (optional)",
  "save.notes.aria": "Incident notes",
  "save.submit": "Create incident",
  "save.cancel": "Cancel",
  "save.titleRequired": "A title is required.",
  "save.failed": "Failed to save the incident",

  /* ── the incident strip ────────────────────────────────────────────────── */
  "incident.aria": "Incident",
  "incident.resolved": "Resolved",
  "incident.open": "Open",
  "incident.openedBy": "opened by {who} · {at}",
  "incident.resolvedAt": " · resolved {at}",
  "incident.copyPermalink": "Copy permalink",
  "incident.reopen": "Reopen",
  "incident.resolve": "Resolve",
  "incident.delete": "Delete",
  "incident.delete.aria": "Delete incident: {title}",
  "incident.confirmDelete": "Confirm delete",
  "incident.confirmDelete.aria": "Confirm delete incident: {title}",
  "incident.cancel": "Cancel",
  "incident.scope.before": "This incident's own scope is",
  "incident.scope.global": "global",
  "incident.scope.after": "— the row, not the URL, decides what this page frames.",
  "incident.scope.targetsGated":
    " Without targets:read a saved target name cannot be told apart from a node name, so it reopens as a node scope.",
  "incident.notes": "Notes",
  "incident.notes.aria": "Incident notes",
  "incident.notes.save": "Save notes",
  "incident.notes.gated": "No notes. Writing them needs incidents:write.",
  "incident.patchFailed": "Failed to update the incident",
  "incident.deleteFailed": "Failed to delete the incident",
  /* Three clipboard outcomes. Only the first is timed out (COPY_NOTE_TTL_MS):
     the other two describe a state of the browser that has not gone away, and
     carry the fallback an operator may still be acting on. */
  "incident.copied": "Permalink copied.",
  "incident.copy.noClipboard": "This browser gave the page no clipboard — the permalink is in the address bar.",
  "incident.copy.refused": "The browser refused the copy — the permalink is in the address bar.",
  /* The link names an incident this session may not read. */
  "incident.readGated":
    "This link names an incident, and reading one needs incidents:read — it was not requested. The investigation " +
    "below is the URL's own scope and range, not the incident's.",
  /* The DELETED incident. Separate from the error card below, and deliberately:
     a 404 is an answer, not a failure, and the page owes the reader the id it
     could not find rather than a plausible investigation nobody chose. */
  "incident.missing.title": "That incident no longer exists",
  "incident.missing.body":
    "No incident with the id {id} — it was most likely deleted. The link has been dropped from the address bar so " +
    "nothing copied from here still claims one; the form above is the way to start a new investigation.",
  "incident.error.title": "No incident matches this link",
  "incident.error.fallback": "The incident could not be read",
  "incident.error.body":
    "The page is showing the default investigation instead — an incident can be deleted, and a stale permalink is " +
    "not an error state worth blanking the page for.",

  /* ── pinned findings ───────────────────────────────────────────────────── */
  "pinned.aria": "Pinned findings",
  "pinned.title": "Pinned findings",
  "pinned.empty.lead": "Nothing pinned yet.",
  "pinned.empty.canWrite": "The pin on a timeline row adds it here.",
  "pinned.empty.gated": "Pinning needs incidents:write.",
  /* All THREE unpinnable classes, named (QA round 3, #19): an operator hunting
     for the missing pin control on a firing-alert row was left to conclude the
     console was broken. */
  "pinned.empty.unpinnable":
    "Maintenance windows, threshold crossings and firing alerts cannot be pinned at all — the stored vocabulary " +
    "has no kind for a declared window, a threshold row is derived from a query rather than being a row anywhere, " +
    "and an alert lives in Prometheus rather than in this console, keyed by a label set that disappears when it " +
    "resolves.",
  /* A pin OUTLIVES the window it was made in — that is what pinning is for —
     so the pane has to say when the row behind one is not on screen to be read.
     Without this the finding rendered as a bare "audit / 1757", which reads like
     a title and is not one. */
  "pinned.outOfWindow": "This row is outside the current window; its title is not available here.",
  "pinned.note.aria": "Note for {kind} {id}",
  "pinned.note.placeholder": "why this matters",
  "pinned.unpin": "Unpin",
  "pinned.unpin.aria": "Unpin {kind} {id}",
  /* Unpinning DISCARDS the note, with no undo — hence the second click. */
  "pinned.discardAndUnpin": "Discard note & unpin",
  "pinned.discardAndUnpin.aria": "Confirm unpin {kind} {id} and discard its note",
  "pinned.cancel": "Cancel",
  "pinned.saveNotes": "Save pin notes",
  "pinned.saveFailed": "Failed to save the pinned findings",

  /* ── the ranked causes ─────────────────────────────────────────────────── */
  "causes.aria": "Correlation",
  "causes.title": "Likely causes",
  "causes.noOnset":
    "No threshold crossing in range — nothing to rank. The onset is the first crossing of loss above 1% or " +
    "RTT above twice the range median; without one there is no anchor, and inventing an anchor is how these " +
    "panels start lying.",
  "causes.onset": "Onset {at} · candidates within {window}s before it",
  "causes.none": "Nothing scoreable happened in the {window} seconds before the onset. An empty ranking is an answer.",
  "causes.list.aria": "Ranked causes",
  "causes.row": "{delta}s before the onset · weight {weight}",
  "causes.method.before":
    "Ranked by temporal proximity; the weights live in the open — no model, four arithmetic steps, " +
    "reproducible by hand from",
  "causes.method.link": "the scoring source",
  /* Where the link actually goes, on the link itself. It is GitHub's main
     branch: unreachable from an air-gapped console, and on any console a
     description of whatever main holds now rather than of this build. */
  "causes.method.link.title":
    "Opens the scoring source on github.com, on the main branch — not this build, and not reachable from an " +
    "air-gapped console.",
  "causes.method.after": ".",

  /* ── the notes card ────────────────────────────────────────────────────── */
  "notes.aria": "Notes",
  "notes.title": "Notes on this scope",

  /* ── the source list: one honest line per absent, bounded or failed source ─
     Nothing here may be softened. Each says which source, why it contributed
     nothing, and — where the answer is bounded rather than absent — exactly
     what the bound is. */
  "source.database":
    "Events, audit, annotations, path history, runs, cluster events and maintenance are all stored — set " +
    "console.database.mode. None of them was requested.",
  "source.events": "Fleet events and Kubernetes events need events:read — neither was requested.",
  /* "Audit rows", not "config changes" (QA round 5, #19): most of what the
     audit log records is a READ decision, not a change. */
  "source.audit": "Audit rows need audit:read — the audit log was not requested.",
  /* Two bounds, not one (QA scope 3, finding #9). The window bound was already
     stated; the SCOPE bound was not, and the audit leg is the only source on
     this page that ignores the scope entirely — GET /api/v1/audit has no scope
     filter, so a pair investigation is reading the whole console's audit log and
     the caption owed the reader that. The runs caption's shape is the template. */
  "source.auditWindow":
    "Config changes come from the newest {limit} audit rows filtered to this window here: GET /api/v1/audit has " +
    "no time filter, so a very busy console can push older in-range rows off that page. They are NOT narrowed to " +
    "this scope either — the audit log is cluster-wide and has no scope filter, so these rows are every subject's " +
    "requests, not this pair's.",
  "source.annotations": "Annotations need annotations:read — no note was requested.",
  "source.mtr": "Path changes need mtr:read — no MTR snapshot was requested.",
  "source.mtrScope":
    "Path history needs a pair, a node or a target: GET /api/v1/mtr/snapshots requires both a source and a " +
    "destination, and a zone pair or the whole cluster names neither. Nothing was requested.",
  "source.mtrFanout":
    "Path changes cover the {limit} most recently traced pairs touching this scope — the snapshots endpoint is " +
    "per pair, and a whole node's fan-out is not a page's worth of requests.",
  "source.runs": "Diagnostic runs need runs:read — no run history was requested.",
  "source.runsScan":
    "Runs are the newest {limit}, narrowed to this window and then to this scope by their spec — GET /api/v1/runs " +
    "has no scope filter.",
  "source.maintenance": "Maintenance windows need maintenance:read — none was requested.",
  "source.promql":
    "Threshold crossings need promql:query — the scope's loss and RTT series were not requested, so the timeline " +
    "carries no derived rows.",
  "source.promqlConfig": "Threshold crossings read Prometheus — set console.prometheus.address. Nothing was requested.",
  "source.alerts": "Firing alerts need alerts:read — no alert state was requested.",
  /* Engaged, this source asks for NOTHING: the firing set is Prometheus's NOW
     and there is no firing history behind it. Word for word what /overview
     says in the same situation. */
  "source.alertsLiveOnly":
    "Alert state is a live-only signal — Prometheus keeps no firing history here. Nothing was requested for this " +
    "instant.",
  "source.alertsConfig": "Firing alerts read Prometheus — set console.prometheus.address. Nothing was requested.",
  /* WHOSE alerts these are, said before anything else about them. The timeline
     used to carry a cluster's whole kube-prometheus-stack backdrop — TargetDown,
     etcdMembersDown, Watchdog — as unscoped rows indistinguishable from this
     product's own. They are not here now, and a caption that did not say so
     would leave the reader believing this list covers the cluster. */
  "source.alertsNow":
    "Only alerts from rules this console manages are here, narrowed to this scope by their labels — a rule this " +
    "console does not manage belongs to whoever wrote it, and its firing state lives in Alertmanager and Grafana, " +
    "not on this page. They are the set firing NOW: a row at activeAt for each one that started inside this " +
    "window, and a row at the window's start for each one that was already firing. Resolutions are not recorded; " +
    "only what is firing now is visible.",
  /* Scope filtering is the one way an alert this console DOES own can be absent
     from the rows, so it is counted and named rather than left to be noticed. */
  "source.alertsScopeHidden.one":
    "{count} alert from this console's own rules is firing in this window but outside this scope, so it has no " +
    "row below.",
  "source.alertsScopeHidden.few":
    "{count} alerts from this console's own rules are firing in this window but outside this scope, so they have " +
    "no rows below.",
  "source.alertsScopeHidden.many":
    "{count} alerts from this console's own rules are firing in this window but outside this scope, so they have " +
    "no rows below.",

  /* One line PER FAILED SOURCE. {label} is one of the ten names below and
     {error} is the SERVER's own detail, verbatim. */
  "source.failed": "{label}: {error}",
  "source.failed.fallback": "the request failed.",
  "source.name.events": "Events",
  "source.name.audit": "Config changes",
  "source.name.annotations": "Annotations",
  "source.name.snapshots": "Path changes",
  "source.name.runs": "Diagnostic runs",
  "source.name.k8s": "Cluster events",
  "source.name.maintenance": "Maintenance windows",
  "source.name.loss": "Packet loss series",
  "source.name.rtt": "RTT series",
  "source.name.delta": "Failure-ratio delta",
  "source.name.alerts": "Firing alerts",

  /* ── the timeline pane (components/investigation-timeline.tsx) ─────────── */
  "timeline.aria": "Timeline",
  "timeline.title": "Timeline",
  /* The WINDOW's count, never the page's — pagination is a reading aid and may
     not change what this pane CLAIMS. */
  "timeline.entries.one": "{count} entry in this window",
  "timeline.entries.few": "{count} entries in this window",
  "timeline.entries.many": "{count} entries in this window",
  "timeline.sources.aria": "Timeline sources",
  "timeline.sources.summary.one": "{count} note about these sources",
  "timeline.sources.summary.few": "{count} notes about these sources",
  "timeline.sources.summary.many": "{count} notes about these sources",
  "timeline.partial.one": "{count} source failed; the timeline below is partial.",
  "timeline.partial.few": "{count} sources failed; the timeline below is partial.",
  "timeline.partial.many": "{count} sources failed; the timeline below is partial.",
  "timeline.loading": "Assembling the timeline…",
  /* The nothing-happened claim needs EVERY enabled source to have settled
     successfully; with one failed, the partial line above says so instead. */
  "timeline.empty":
    "Nothing happened in this window — no event, no configuration change and no threshold crossing from any " +
    "source this session can read.",
  /* EVERY source failed. The partial line above counts them; this says the thing
     counting cannot say — that there is no timeline here, and that the emptiness
     is a fact about the console rather than about the fleet. The empty copy is
     suppressed underneath it, because with nothing answered «ничего не
     произошло» is not a finding, it is a guess. */
  "timeline.allFailed.title": "No timeline could be assembled",
  "timeline.allFailed.body":
    "Every source this investigation asked for came back an error — the lines above name each one and what the " +
    "server said about it. Nothing below is evidence about the fleet, in either direction: this window has not " +
    "been read, so it is neither quiet nor busy as far as this page knows.",
  "timeline.entries.aria": "Timeline entries",
  "timeline.pin": "Pin: {title}",
  "timeline.unpin": "Unpin: {title}",

  /* The pager's own words moved to dict/shared.ts with the control itself —
     ui/pager.tsx is one component with a mount on every list now. */

  /* The badge on a row, per source. "k8s" is what a cluster event is called
     out loud and stays. `audit` says "audit", not "config change" (QA round 5,
     #19): the log records every authorization DECISION, and labelling a read
     "config change" tells an operator somebody changed something when nobody
     did — the most expensive kind of wrong a timeline can be. */
  "kind.event": "event",
  "kind.audit": "audit",
  "kind.annotation": "annotation",
  "kind.pathChange": "path change",
  "kind.run": "run",
  "kind.k8s": "k8s",
  "kind.maintenance": "maintenance",
  "kind.threshold": "threshold",
  "kind.alert": "alert",
} as const;

export type InvestigateKey = keyof typeof en;

/**
 * The PAGE is «Инциденты» (M3-8, per dict/chrome.ts's nav) while the sidebar
 * GROUP keeps «Расследование» — one word per concept, everywhere.
 */
export const investigateDict: Dictionary<InvestigateKey> = defineDict(en, {
  "title": "Инциденты",
  "description":
    "Один интервал, одна область. Все источники, до которых консоль дотягивается, сведены в общую ленту, а правила корреляции не угадываются, а прописаны.",
  "help.body":
    "Один интервал над одной областью: зафиксируйте пару, узел, цель, пару зон или весь кластер и диапазон времени. " +
    "Лента сводит все источники, до которых консоль дотягивается: события флота и k8s, строки аудита, заметки, окна работ, диагностические запуски, смены маршрутов MTR, пересечения порогов и активные оповещения. " +
    "Правила корреляции прописаны прямо на странице, а не угадываются. " +
    "Всё лежит в URL, так что вид заодно служит пермалинком инцидента.",

  "scope.aria": "Тип области",
  "scope.label": "Область",
  "scope.pair": "Пара",
  "scope.node": "Узел",
  "scope.target": "Цель",
  "scope.zonePair": "Пара зон",
  "scope.cluster": "Кластер",
  "scope.headline.zonePair": "зона {a} {sep} зона {b}",
  "scope.headline.cluster": "весь кластер",
  "scope.headline.empty": "(ничего не выбрано)",

  "form.aria": "Область расследования",
  "form.sourceNode": "Узел-источник",
  "form.destinationNode": "Узел назначения",
  "form.node": "Узел",
  "form.sourceZone": "Зона-источник",
  "form.destinationZone": "Зона назначения",
  "form.target": "Цель",
  "form.range": "Диапазон",
  "form.range.aria": "Пресет диапазона",
  "form.range.custom": "Свой",
  "form.rangeStart": "Начало диапазона",
  "form.rangeEnd": "Конец диапазона",
  "form.submit": "Расследовать",
  "form.option.missing": "{value} — нет в текущей топологии",
  "form.targetsGated":
    "Списку целей нужно право targets:read. По постоянной ссылке область всё равно откроется: в адресе лежит имя " +
    "цели, а не её идентификатор.",
  "form.urlNote":
    "Всё, что выше, лежит в адресе (?kind=&scope=&from=&to=), так что страницу можно кинуть коллеге как есть. " +
    "Постоянная ссылка на инцидент отсюда же и берётся, бесплатно.",
  "ignored.body":
    "В ссылке было {params}, и прочитать это страница не смогла. Ниже показаны значения по умолчанию, а адресная " +
    "строка приведена к тому, что на экране.",
  "ignored.dismiss": "Скрыть",

  "actions.aria": "Действия",
  "actions.runMTR": "Запустить MTR сейчас",
  "actions.runTCP": "Запустить TCP сейчас",
  "actions.compare": "Сравнить в Метриках",
  "actions.export": "Выгрузить JSON",
  "actions.saveIncident": "Сохранить как инцидент",
  "actions.createMaintenance": "Создать окно работ",
  "actions.compareNote":
    "Слоты A/B в Метриках привязаны к готовым графикам, а диапазон из адреса та страница не читает. Ссылка просто " +
    "откроет её, интервал придётся выставить руками.",
  "actions.runStarted.before": "Запуск",
  "actions.runStarted.after": "начат.",
  "actions.runFailed": "Не удалось запустить",

  "save.aria": "Сохранить как инцидент",
  "save.scopeLabel": "Область",
  "save.global": "глобальная",
  "save.window": "{from} → {to}. Взято из этого расследования, здесь не правится.",
  "save.wideNote":
    "И пара зон, и весь кластер сохраняются как ГЛОБАЛЬНАЯ область: в том словаре зон просто нет. Поэтому при " +
    "повторном открытии инцидент обрамит кластер. А диапазон сохраняется точь-в-точь.",
  "save.title": "Заголовок",
  "save.title.aria": "Заголовок инцидента",
  "save.title.placeholder": "Потери пакетов между node-a и node-b",
  "save.notes": "Заметки (необязательно)",
  "save.notes.aria": "Заметки инцидента",
  "save.submit": "Создать инцидент",
  "save.cancel": "Отмена",
  "save.titleRequired": "Нужен заголовок.",
  "save.failed": "Не удалось сохранить инцидент",

  "incident.aria": "Инцидент",
  "incident.resolved": "Закрыт",
  "incident.open": "Открыт",
  "incident.openedBy": "открыл {who} · {at}",
  "incident.resolvedAt": " · закрыт {at}",
  "incident.copyPermalink": "Скопировать ссылку",
  "incident.reopen": "Переоткрыть",
  "incident.resolve": "Закрыть",
  "incident.delete": "Удалить",
  "incident.delete.aria": "Удалить инцидент: {title}",
  "incident.confirmDelete": "Подтвердить удаление",
  "incident.confirmDelete.aria": "Подтвердить удаление инцидента: {title}",
  "incident.cancel": "Отмена",
  "incident.scope.before": "Собственная область этого инцидента",
  "incident.scope.global": "глобальная",
  "incident.scope.after": "(рамку страницы задаёт запись, а не адрес).",
  "incident.scope.targetsGated":
    " Без targets:read сохранённое имя цели не отличишь от имени узла, поэтому область откроется узлом.",
  "incident.notes": "Заметки",
  "incident.notes.aria": "Заметки инцидента",
  "incident.notes.save": "Сохранить заметки",
  "incident.notes.gated": "Заметок нет. Чтобы их писать, нужно право incidents:write.",
  "incident.patchFailed": "Не удалось обновить инцидент",
  "incident.deleteFailed": "Не удалось удалить инцидент",
  "incident.copied": "Ссылка скопирована.",
  "incident.copy.noClipboard": "Браузер не дал странице буфер обмена. Ссылка при этом никуда не делась, она в адресной строке.",
  "incident.copy.refused": "Браузер отказал в копировании. Ссылка при этом никуда не делась, она в адресной строке.",
  "incident.readGated":
    "Ссылка называет инцидент, а на чтение инцидента нужно право incidents:read. Запрос не отправлялся. Ниже " +
    "расследование по области и диапазону из адреса, а не по самому инциденту.",
  "incident.missing.title": "Этого инцидента больше нет",
  "incident.missing.body":
    "Инцидента с идентификатором {id} нет: скорее всего, его удалили. Ссылку убрали из адресной строки, чтобы " +
    "скопированный отсюда адрес больше не обещал инцидент. Новое расследование начинается формой выше.",
  "incident.error.title": "По этой ссылке инцидента нет",
  "incident.error.fallback": "Инцидент не удалось прочитать",
  "incident.error.body":
    "Вместо него страница показывает расследование по умолчанию. Инцидент могли удалить, а устаревшая ссылка не " +
    "тот случай, ради которого стоит гасить всю страницу.",

  "pinned.aria": "Закреплённые находки",
  "pinned.title": "Закреплённые находки",
  "pinned.empty.lead": "Пока ничего не закреплено.",
  "pinned.empty.canWrite": "Булавка на строке ленты добавляет её сюда.",
  "pinned.empty.gated": "Для закрепления нужно право incidents:write.",
  "pinned.empty.unpinnable":
    "Окна работ, пересечения порога и активные оповещения не закрепляются вообще. В хранимом словаре нет вида для " +
    "объявленного окна; строка порога выводится из запроса, а не лежит где-то записью; оповещение живёт в " +
    "Prometheus, а не здесь, и опознаётся набором меток, который исчезает вместе с ним.",
  "pinned.outOfWindow": "Строка вне текущего окна; заголовок недоступен.",
  "pinned.note.aria": "Заметка для {kind} {id}",
  "pinned.note.placeholder": "чем это важно",
  "pinned.unpin": "Открепить",
  "pinned.unpin.aria": "Открепить {kind} {id}",
  "pinned.discardAndUnpin": "Стереть заметку и открепить",
  "pinned.discardAndUnpin.aria": "Подтвердить открепление {kind} {id} со стиранием заметки",
  "pinned.cancel": "Отмена",
  "pinned.saveNotes": "Сохранить заметки к находкам",
  "pinned.saveFailed": "Не удалось сохранить закреплённые находки",

  "causes.aria": "Корреляция",
  "causes.title": "Вероятные причины",
  "causes.noOnset":
    "В диапазоне ни разу не перешли порог, ранжировать нечего. Началом считается первое превышение: потери выше " +
    "1 % либо RTT выше удвоенной медианы по диапазону. Без него якоря нет, а стоит якорь выдумать, и панель " +
    "начнёт врать ровно с этого места.",
  "causes.onset": "Начало {at} · кандидаты в пределах {window} с до него",
  "causes.none":
    "За {window} секунд до начала не случилось ничего, что поддаётся оценке. Пустой рейтинг тоже ответ.",
  "causes.list.aria": "Ранжированные причины",
  "causes.row": "за {delta} с до начала · вес {weight}",
  "causes.method.before":
    "Ранжируем по близости во времени, и веса лежат на виду: никакой модели, четыре арифметических шага, всё " +
    "воспроизводится руками по",
  "causes.method.link": "исходнику оценки",
  "causes.method.link.title":
    "Откроет исходник оценки на github.com, ветка main: это не текущая сборка, и из изолированного контура ссылка " +
    "не откроется.",
  "causes.method.after": ".",

  "notes.aria": "Заметки",
  "notes.title": "Заметки по этой области",

  "source.database":
    "События, аудит, заметки, история путей, запуски, события кластера и окна работ хранятся в базе, а её надо " +
    "задать: console.database.mode. Ни один из этих источников не запрашивался.",
  "source.events": "Событиям флота и Kubernetes нужно право events:read. Ни то, ни другое не запрашивалось.",
  "source.audit": "Строкам аудита нужно право audit:read. Журнал аудита не запрашивался.",
  "source.auditWindow":
    "Изменения конфигурации берутся из свежих {limit} строк аудита, а по интервалу их фильтруют уже здесь: у " +
    "GET /api/v1/audit нет фильтра по времени. На нагруженной консоли строки постарше, которые в интервал " +
    "попадают, могут вытесниться с этой страницы. По области они тоже НЕ сужаются: журнал аудита общий на весь " +
    "кластер и фильтра по области у него нет, так что здесь запросы всех субъектов, а не только этой пары.",
  "source.annotations": "Заметкам нужно право annotations:read. Ни одна не запрашивалась.",
  "source.mtr": "Сменам путей нужно право mtr:read. Ни один снимок MTR не запрашивался.",
  "source.mtrScope":
    "Истории путей нужна пара, узел или цель: GET /api/v1/mtr/snapshots требует и источник, и назначение, а пара " +
    "зон или весь кластер не называют ни того, ни другого. Ничего не запрашивалось.",
  "source.mtrFanout":
    "Смены путей покрывают {limit} последних оттрассированных пар, которые задевают эту область. Эндпоинт снимков " +
    "работает по одной паре, а веер целого узла в страницу запросов не укладывается.",
  "source.runs": "Диагностическим запускам нужно право runs:read. История запусков не запрашивалась.",
  "source.runsScan":
    "Запуски берутся свежими, числом {limit}, потом сужаются до интервала, потом до области по их спецификации: у " +
    "GET /api/v1/runs нет фильтра по области.",
  "source.maintenance": "Окнам работ нужно право maintenance:read. Ни одно не запрашивалось.",
  "source.promql":
    "Пересечениям порога нужно право promql:query. Серии потерь и RTT по этой области не запрашивались, поэтому " +
    "выведенных из них строк в ленте нет.",
  "source.promqlConfig":
    "Пересечения порога читаются из Prometheus, а его адрес не задан: console.prometheus.address. Ничего не " +
    "запрашивалось.",
  "source.alerts": "Активным оповещениям нужно право alerts:read. Состояние оповещений не запрашивалось.",
  "source.alertsLiveOnly":
    "Состояние оповещений говорит только про сейчас: истории срабатываний Prometheus здесь не держит. На " +
    "выбранный момент ничего не запрашивалось.",
  "source.alertsConfig":
    "Активные оповещения читаются из Prometheus, а его адрес не задан: console.prometheus.address. Ничего не " +
    "запрашивалось.",
  "source.alertsNow":
    "Здесь только оповещения по правилам, которыми управляет эта консоль, суженные по меткам до этой области. " +
    "Правило, которым консоль не управляет, принадлежит тому, кто его написал, и его состояние живёт в " +
    "Alertmanager и Grafana, а не на этой странице. Набор берётся активным СЕЙЧАС: строка на activeAt для " +
    "каждого, что началось внутри интервала, и строка на начало интервала для тех, что уже были активны. " +
    "Погашения не записываются, видно только то, что горит прямо сейчас.",
  "source.alertsScopeHidden.one":
    "{count} оповещение по правилам этой консоли горит в интервале, но вне этой области, поэтому строки для него " +
    "ниже нет.",
  "source.alertsScopeHidden.few":
    "{count} оповещения по правилам этой консоли горят в интервале, но вне этой области, поэтому строк для них " +
    "ниже нет.",
  "source.alertsScopeHidden.many":
    "{count} оповещений по правилам этой консоли горят в интервале, но вне этой области, поэтому строк для них " +
    "ниже нет.",

  "source.failed": "{label}: {error}",
  "source.failed.fallback": "запрос не выполнен.",
  "source.name.events": "События",
  "source.name.audit": "Изменения конфигурации",
  "source.name.annotations": "Заметки",
  "source.name.snapshots": "Смены путей",
  "source.name.runs": "Диагностические запуски",
  "source.name.k8s": "События кластера",
  "source.name.maintenance": "Окна работ",
  "source.name.loss": "Серия потерь пакетов",
  "source.name.rtt": "Серия RTT",
  "source.name.delta": "Дельта доли сбоев",
  "source.name.alerts": "Активные оповещения",

  "timeline.aria": "Лента",
  "timeline.title": "Лента",
  "timeline.entries.one": "{count} запись в этом интервале",
  "timeline.entries.few": "{count} записи в этом интервале",
  "timeline.entries.many": "{count} записей в этом интервале",
  "timeline.sources.aria": "Источники ленты",
  "timeline.sources.summary.one": "{count} замечание об источниках",
  "timeline.sources.summary.few": "{count} замечания об источниках",
  "timeline.sources.summary.many": "{count} замечаний об источниках",
  "timeline.partial.one": "{count} источник не ответил, лента ниже неполная.",
  "timeline.partial.few": "{count} источника не ответили, лента ниже неполная.",
  "timeline.partial.many": "{count} источников не ответили, лента ниже неполная.",
  "timeline.loading": "Собираем ленту…",
  "timeline.empty":
    "В этом интервале ничего не произошло: ни одного события, ни одного изменения конфигурации, ни одного " +
    "пересечения порога ни от одного источника, доступного этой сессии.",
  "timeline.allFailed.title": "Ленту собрать не из чего",
  "timeline.allFailed.body":
    "Все источники, к которым это расследование обратилось, ответили ошибкой. Выше перечислены они поимённо и то, " +
    "что сказал сервер по каждому. Ниже нет свидетельств о флоте ни в одну сторону: интервал не прочитан, так что " +
    "для этой страницы он и не тихий, и не шумный.",
  "timeline.entries.aria": "Записи ленты",
  "timeline.pin": "Закрепить: {title}",
  "timeline.unpin": "Открепить: {title}",

  "kind.event": "событие",
  "kind.audit": "аудит",
  "kind.annotation": "заметка",
  "kind.pathChange": "смена пути",
  "kind.run": "запуск",
  "kind.k8s": "k8s",
  "kind.maintenance": "работы",
  "kind.threshold": "порог",
  "kind.alert": "оповещение",
});

/**
 * countForm picks between the `.one` / `.few` / `.many` keys (timeline entries
 * and failed sources).
 *
 * No plural MACHINERY — lib/i18n ships none on purpose, and this is the
 * README's "declare the forms as keys and pick between them" pattern. The RULE
 * is per language because the rules genuinely differ: English needs two forms
 * and 21 must read "21 entries", Russian needs three and 21 must read
 * «21 запись».
 */
export function countForm(locale: string, n: number): "one" | "few" | "many" {
  if (locale !== "ru") return n === 1 ? "one" : "many";
  const teen = n % 100;
  if (teen >= 11 && teen <= 14) return "many";
  const last = n % 10;
  if (last === 1) return "one";
  if (last >= 2 && last <= 4) return "few";
  return "many";
}
