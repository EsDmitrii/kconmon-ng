import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { AnnotationBar } from "@/components/annotations";
import { InvestigationTimeline, type PinControl, type SourceNote } from "@/components/investigation-timeline";
import { MaintenanceBar } from "@/components/maintenance";
import { SignalPanels, deltaFromVectors } from "@/components/investigation-signals";
import { stepSecondsFor } from "@/components/mtr-changes-timeline";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { Segmented } from "@/components/ui/segmented";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { useConfirmStep, useKeyedConfirmStep } from "@/hooks/use-confirm-step";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { useTopology } from "@/hooks/use-topology";
import { mergeAnnotations } from "@/lib/annotations";
import {
  ApiError,
  createIncident,
  createRun,
  deleteIncident,
  getAuditEntries,
  getConfig,
  getEvents,
  getIncident,
  getK8sEvents,
  getMTRDestinations,
  getMTRSnapshots,
  getMaintenance,
  getRun,
  getRuns,
  listAlerts,
  listAnnotations,
  listTargets,
  patchIncident,
  promqlQuery,
  promqlQueryRange,
} from "@/lib/api";
import { stampClock, stampFull, useLocale, useT, type Locale, type Translate } from "@/lib/i18n";
/* The centre pane picks the plural forms of the counts it renders; countForm is
   here for the ONE count this page owns — how many of our own alerts the scope
   kept off the timeline, which is a source note and therefore the page's. */
import { countForm, investigateDict, type InvestigateKey } from "@/lib/i18n/dict/investigate";
import { investigationSourcesDict, type InvestigationSourcesKey } from "@/lib/i18n/dict/investigation-sources";
import { subscribeToLocation } from "@/lib/location";
import { cn, endSentence } from "@/lib/utils";
import {
  CAUSE_WEIGHTS,
  DEFAULT_CAUSE_WINDOW_SECONDS,
  DEFAULT_THRESHOLDS,
  anomalyOnset,
  mergeTimeline,
  rankCauses,
  thresholdCrossings,
} from "@/lib/investigation";
import {
  DEFAULT_RANGE_SECONDS,
  INCIDENT_NOTES_MAX,
  INCIDENT_TITLE_MAX,
  INVESTIGATE_PATH,
  PAIR_SEPARATOR,
  PIN_NOTE_MAX,
  RANGE_PRESETS,
  annotationEntries,
  auditEntries,
  buildExportPayload,
  commitWindow,
  eventEntries,
  exportFileName,
  ignoredInvestigationParams,
  inRange,
  incidentParams,
  investigationFailRatioQuery,
  investigationLossQuery,
  investigationParamsToSearch,
  investigationRttQuery,
  k8sEntries,
  maintenanceEntries,
  parseInvestigationParams,
  pathChangeEntries,
  pinKey,
  pinnedRefFor,
  rangeExceedsPromBound,
  runEntries,
  runTouchesScope,
  samplesFromMatrix,
  scopeCaptionValue,
  scopeFilterValue,
  scopeIncompleteReason,
  scopeNodeOptions,
  scopeZoneOptions,
  scopedAlertEntries,
  scopesToQuery,
  validAt,
  type InvestigationParams,
  type InvestigationScope,
  type RangePreset,
  type ScopeKind,
} from "@/lib/investigation-sources";
import { withAtParam, useTimeContext, useWriteGuard, useWritesDisabled } from "@/lib/timemachine";
import type { Incident, IncidentStatus, K8sEvent, MaintenanceWindow, PathSnapshot, PinnedRef } from "@/lib/types";
import { buildRunRequest, CONTROL_CLASS } from "@/pages/diagnostics";

/**
 * `?kind=&scope=&from= &to=` is what a card's "Investigate" action builds, what the browser's Back
 * button restores.
 */


/** downloadJson is the Blob-link download, isolated so the page body stays
 *  testable: jsdom implements neither createObjectURL nor a real click-to-save,
 *  and the payload itself is pinned through buildExportPayload instead. */
function downloadJson(name: string, payload: unknown): void {
  if (typeof URL.createObjectURL !== "function") return;
  const blob = new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}

/* ── the page ───────────────────────────────────────────────────────────── */

const AUDIT_SCAN_LIMIT = 200;
const EVENT_LIMIT = 200;
const RUN_SCAN_LIMIT = 20;
/** MTR_FANOUT bounds the node/target case: one snapshot request per peer, and a
 *  busy node can face the whole fleet. Four is enough to show a route moving
 *  and small enough that the page does not become a fan-out. */
const MTR_FANOUT = 4;
const CAUSE_TOP_N = 5;

const DOC_LINK =
  "https://github.com/EsDmitrii/kconmon-ng/blob/main/web/src/lib/investigation.ts";

/* The scope kinds, each carrying the KEY of its label: the values are the URL
   vocabulary (lib/investigation-sources.ts parses them back), the words belong
   to the dictionary. */
const SCOPE_OPTIONS: { value: ScopeKind; key: InvestigateKey }[] = [
  { value: "pair", key: "scope.pair" },
  { value: "node", key: "scope.node" },
  { value: "target", key: "scope.target" },
  { value: "zone-pair", key: "scope.zonePair" },
  { value: "cluster", key: "scope.cluster" },
];

function presetForSpan(from: Date, to: Date): RangePreset {
  const seconds = Math.round((to.getTime() - from.getTime()) / 1000);
  const match = RANGE_PRESETS.find((p) => p.seconds === seconds);
  return match ? match.value : "custom";
}

/** The headline beside the page title. A pair is two node names and an arrow —
 *  pure data, no key; everything else has a word in it. */
function scopeHeadline(t: Translate<InvestigateKey>, scope: InvestigationScope): string {
  switch (scope.kind) {
    case "pair":
      return `${scope.a} ${PAIR_SEPARATOR} ${scope.b}`;
    case "zone-pair":
      return t("scope.headline.zonePair", { a: scope.a, sep: PAIR_SEPARATOR, b: scope.b });
    case "cluster":
      return t("scope.headline.cluster");
    default:
      return scope.a || t("scope.headline.empty");
  }
}

function queryErrorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiError ? (error.problem.detail ?? error.problem.title) : fallback;
}

/**
 * writeParams rewrites ONLY the parameters this page owns, preserving pathname,
 * hash and every other query key.
 *
 * `replace` is for a URL the page is CORRECTING rather than navigating: a link
 * carrying a parameter this page could not honour was never a place to go back
 * to, which is the same call lib/timemachine.tsx's syncAtParam makes about a
 * `?at=` it had to ignore (QA scope 3, finding #14).
 */
function paramsHref(p: InvestigationParams): { href: string; search: string } {
  const url = new URL(window.location.href);
  for (const key of ["kind", "scope", "from", "to", "incident"]) url.searchParams.delete(key);
  for (const [k, v] of new URLSearchParams(investigationParamsToSearch(p).slice(1))) url.searchParams.set(k, v);
  return { href: `${url.pathname}${url.search}${url.hash}`, search: url.search };
}

function writeParams(p: InvestigationParams, replace = false): string {
  const { href, search } = paramsHref(p);
  if (replace) window.history.replaceState({}, "", href);
  else window.history.pushState({}, "", href);
  return search;
}

/**
 * writeParamsApplied records the search as APPLIED before writing it.
 *
 * The order is load-bearing, not tidiness: lib/location.ts patches
 * pushState/replaceState and notifies its subscribers SYNCHRONOUSLY, so this
 * page's own correction re-enters its location listener mid-call. With the ref
 * updated afterwards the listener sees a URL it does not recognise, re-hydrates
 * from it, and clears the very banner the correction was raised to explain.
 */
function writeParamsApplied(p: InvestigationParams, applied: { current: string }, replace = false): void {
  applied.current = paramsHref(p).search;
  writeParams(p, replace);
}

/** warnIgnored is the console half of finding #14 — the notice on the page is
 *  the operator's, this line is for whoever is reading the devtools while a
 *  generated link comes out wrong. */
function warnIgnored(ignored: string[]): void {
  console.warn(`[investigate] ignoring ${ignored.map((k) => `?${k}`).join(" ")}: not a value this page can honour`);
}

/**
 * correctURL makes the address bar state what the page is ACTUALLY framing, for
 * every case where hydration could not honour the link verbatim: a parameter it
 * had to drop (finding #14), a window the Time Machine clamped, or one it
 * refused (finding #2). replaceState, never push — a URL the page could not
 * honour was never a place to go back to, which is the call lib/timemachine.tsx
 * makes about a `?at=` it had to ignore.
 *
 * An `?incident=` link is left ALONE: the id is the authority there, the scope
 * parameters have no say, and rewriting would delete the one parameter the
 * permalink exists to carry.
 */
function correctURL(h: Hydrated, incidentParam: string | null, applied: { current: string }): void {
  if (h.ignored.length > 0) warnIgnored(h.ignored);
  if (incidentParam !== null) return;
  if (h.ignored.length === 0 && !h.clamped && h.error === undefined) return;
  writeParamsApplied(h.params, applied, true);
}

/** Hydration is what the page does with a URL it did not write: at mount, and
 *  again every time the address changes underneath it. */
interface Hydrated {
  params: InvestigationParams;
  /** commitWindow moved an edge — the same banner apply() raises. */
  clamped: boolean;
  /** commitWindow refused outright — the same sentence apply() raises. */
  error?: string;
  /** Parameters the URL carried and parseInvestigationParams threw away. */
  ignored: string[];
}

/**
 * hydrateInvestigation is the ONE reader of a URL this page did not write, and
 * it runs the Time Machine's gate (QA scope 3, finding #2).
 *
 * commitWindow used to live only in apply(), so a deep link
 * `?at=X&from=X+1h&to=X+2h` rendered rows dated AFTER the instant the whole
 * console claimed to be showing — the one thing the Time Machine exists to
 * prevent, walked straight past by the path an operator actually arrives on.
 * Clamped and refused windows now produce the same banner and the same refusal
 * they produce when the form commits them.
 *
 * A REFUSED window has nothing to clamp to (it lies entirely after the instant),
 * and there is no previous window to fall back on the way apply() has one, so the
 * page frames the default hour ending at the viewed instant and says why. The
 * refusal is the sentence, not the frame: what is on screen is a window this page
 * chose, and it is only ever reachable with the banner above it.
 */
function hydrateInvestigation(search: string, now: Date, at: Date | null, t: Translate<InvestigationSourcesKey>): Hydrated {
  const parsed = parseInvestigationParams(search, now);
  const ignored = ignoredInvestigationParams(search);
  const commit = commitWindow(parsed.from, parsed.to, at, t);
  if (commit.ok) {
    return { params: { ...parsed, from: commit.from, to: commit.to }, clamped: commit.clamped, ignored };
  }
  const anchor = at ?? now;
  return {
    params: {
      kind: parsed.kind,
      a: parsed.a,
      b: parsed.b,
      from: new Date(anchor.getTime() - DEFAULT_RANGE_SECONDS * 1000),
      to: anchor,
    },
    clamped: false,
    error: commit.reason,
    ignored,
  };
}

/**
 * readIncidentParam is the ONE reader of `?incident=`, and it is the reader
 * because an EMPTY value is not an id (QA scope 4).
 *
 * `?incident=` — the shape a link builder produces from an undefined id, and the
 * shape left behind by hand-deleting one — used to put the page into incident
 * mode for the id "": it fired `GET /api/v1/incidents/`, took the 404 as "that
 * incident was deleted", and rendered a not-found card whose sentence named
 * nothing at all («No incident with the id  — it was most likely deleted»).
 * Worse, the parameter's mere presence made correctURL bow out, so the four
 * scope parameters beside it went uncorrected and unreported too.
 */
function readIncidentParam(search: string): string | null {
  const raw = new URLSearchParams(search).get("incident");
  return raw === null || raw.trim() === "" ? null : raw;
}

/** writeIncidentParam is the other direction: the id REPLACES the four scope parameters. */
function writeIncidentParam(id: string): string {
  const url = new URL(window.location.href);
  for (const key of ["kind", "scope", "from", "to"]) url.searchParams.delete(key);
  url.searchParams.set("incident", id);
  window.history.pushState({}, "", `${url.pathname}${url.search}${url.hash}`);
  return url.search;
}

const INPUT_CLASS =
  "h-8 w-full rounded-md bg-surface-2 px-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

const TEXTAREA_CLASS =
  "w-full rounded-md bg-surface-2 px-2 py-1.5 text-sm text-foreground placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

/** fmtStamp is the incident strip's stamp, through lib/i18n's shared helper so
 *  the opened/resolved line, the save form's window and the bars below all draw
 *  an instant the same way (QA scope 3, findings #7 and #18). */
function fmtStamp(iso: string, locale: Locale): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : stampFull(d, locale);
}

/** See IncidentStrip's `say`. Four seconds: long enough to read six words,
 *  short enough that the line is gone before the next action. */
export const COPY_NOTE_TTL_MS = 4000;

/** SaveIncidentForm is the popover behind "Save as incident". */
function SaveIncidentForm({
  scopeText,
  from,
  to,
  wide,
  onCreate,
  onCancel,
}: {
  scopeText: string;
  from: Date;
  to: Date;
  wide: boolean;
  onCreate: (title: string, notes: string) => Promise<void>;
  onCancel: () => void;
}) {
  const t = useT(investigateDict);
  const { locale } = useLocale();
  const [title, setTitle] = useState("");
  const [notes, setNotes] = useState("");
  /* The in-flight guard, not just a disabled look: begin is a REF write. */
  const { submitting: busy, begin, end } = useSubmitGuard();
  const [error, setError] = useState<string>();
  /* Focus goes to the field that is wrong (QA round 3, finding #22, and the
     contract components/annotations.tsx's focusField already keeps): a message
     under a form the reader may have scrolled past is a message nobody sees. */
  const titleRef = useRef<HTMLInputElement>(null);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const trimmed = title.trim();
    if (trimmed === "") {
      setError(t("save.titleRequired"));
      titleRef.current?.focus();
      return;
    }
    setError(undefined);
    if (!begin()) return;
    try {
      await onCreate(trimmed, notes.trim());
    } catch (err) {
      setError(queryErrorMessage(err, t("save.failed")));
      end();
    }
  }

  return (
    <Card asChild className="mt-3 p-4">
      {/* role="form", not role="dialog" (QA round 3, finding #15). The rail
          stays live behind it, focus is not trapped and Escape dismisses
          nothing — three promises the dialog role makes and this disclosure
          does not keep. Escape-to-discard is deliberately absent here too: the
          notes box holds typed text with no undo behind it. */}
      <form role="form" aria-label={t("save.aria")} onSubmit={handleSubmit} className="flex flex-col gap-3">
        <p className="text-xs text-muted-foreground">
          {/* scopeText is the scope's own wire value; both stamps go through
              lib/i18n's stampFull — the SAME helper the incident strip and the
              maintenance bar use, so one window is not rendered three ways on
              one page (QA scope 3, finding #18). */}
          {t("save.scopeLabel")}{" "}
          <span className={cn("font-medium text-foreground", scopeText !== "" && "mono-data")}>{scopeText === "" ? t("save.global") : scopeText}</span> ·{" "}
          {t("save.window", { from: stampFull(from, locale), to: stampFull(to, locale) })}
        </p>
        {/* The one lossy case, said out loud rather than discovered on reopen:
            the incident scope vocabulary has no zone-pair member, so both wide
            scopes store "" and reopen framed on the whole cluster. */}
        {wide ? (
          <p className="type-meta">{t("save.wideNote")}</p>
        ) : null}
        <label className="flex flex-col gap-1 text-[13px]">
          <span className="text-muted-foreground">{t("save.title")}</span>
          <input
            ref={titleRef}
            aria-label={t("save.title.aria")}
            value={title}
            maxLength={INCIDENT_TITLE_MAX}
            onChange={(e) => setTitle(e.target.value)}
            placeholder={t("save.title.placeholder")}
            className={INPUT_CLASS}
          />
        </label>
        <label className="flex flex-col gap-1 text-[13px]">
          <span className="text-muted-foreground">{t("save.notes")}</span>
          <textarea
            aria-label={t("save.notes.aria")}
            value={notes}
            rows={3}
            maxLength={INCIDENT_NOTES_MAX}
            onChange={(e) => setNotes(e.target.value)}
            className={TEXTAREA_CLASS}
          />
        </label>
        {error ? (
          <p role="alert" className="text-xs text-health-bad">
            {error}
          </p>
        ) : null}
        <div className="flex gap-2">
          <Button type="submit" size="sm" loading={busy}>
            {t("save.submit")}
          </Button>
          <Button type="button" size="sm" variant="outline" onClick={onCancel}>
            {t("save.cancel")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

/**
 * IncidentStrip is the header of an investigation that has been SAVED: what it is called; that is
 * not a micro-optimisation: an incident is worked on by several people at once.
 */
function IncidentStrip({
  incident,
  canWrite,
  writesDisabled,
  onPatched,
  onDeleted,
  targetsGated,
}: {
  incident: Incident;
  canWrite: boolean;
  writesDisabled: boolean;
  onPatched: (updated: Incident) => void;
  /** Where the page goes after the row stops existing (QA round 3, #21). */
  onDeleted: () => void;
  targetsGated: boolean;
}) {
  const t = useT(investigateDict);
  const { locale } = useLocale();
  const guard = useWriteGuard();
  const [notes, setNotes] = useState(incident.notes);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [copyNote, setCopyNote] = useState<string>();
  /* An incident is somebody's written record of an outage — the notes, the pinned findings.
     Two presses, and the keyboard kept whole across the swap — hooks/use-confirm-step. */
  const {
    confirming: confirmingDelete,
    confirmRef,
    triggerRef,
    ask: askDelete,
    reset: resetDelete,
  } = useConfirmStep();
  const copyTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const resolved = incident.status === "resolved";

  // The server is the authority after every write, so a fresh row resets the
  // editor rather than the editor holding a value the row disagrees with.
  useEffect(() => {
    setNotes(incident.notes);
  }, [incident]);

  const patch = useCallback(
    async (body: { status?: IncidentStatus; notes?: string }) => {
      setBusy(true);
      setError(undefined);
      try {
        onPatched(await patchIncident(incident.id, body));
      } catch (err) {
        setError(queryErrorMessage(err, t("incident.patchFailed")));
      } finally {
        setBusy(false);
      }
    },
    [incident.id, onPatched, t],
  );

  /**
   * COPY_NOTE_TTL_MS — how long "Permalink copied." stays on screen; a confirmation of a completed
   * one-shot act is only true for the moment after it: left up.
   */
  const say = useCallback((message: string, transient: boolean) => {
    clearTimeout(copyTimer.current);
    setCopyNote(message);
    if (transient) copyTimer.current = setTimeout(() => setCopyNote(undefined), COPY_NOTE_TTL_MS);
  }, []);

  useEffect(() => () => clearTimeout(copyTimer.current), []);

  const copyPermalink = useCallback(async () => {
    const href = window.location.href;
    const clipboard = navigator.clipboard;
    if (!clipboard || typeof clipboard.writeText !== "function") {
      say(t("incident.copy.noClipboard"), false);
      return;
    }
    try {
      await clipboard.writeText(href);
      say(t("incident.copied"), true);
    } catch {
      say(t("incident.copy.refused"), false);
    }
  }, [say, t]);

  const handleDelete = useCallback(async () => {
    setBusy(true);
    setError(undefined);
    try {
      await deleteIncident(incident.id);
      onDeleted();
    } catch (err) {
      setError(queryErrorMessage(err, t("incident.deleteFailed")));
      setBusy(false);
      resetDelete();
    }
  }, [incident.id, onDeleted, resetDelete, t]);

  return (
    <Card asChild className="border-l-4 border-l-primary p-5">
      <section aria-label={t("incident.aria")}>
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          <h2 className="type-section">{incident.title}</h2>
          <Badge variant={resolved ? "neutral" : "warn"} dot>
            {resolved ? t("incident.resolved") : t("incident.open")}
          </Badge>
          <span className="text-xs text-muted-foreground">
            {/* createdBy is a subject id; both stamps are data interpolated into
                a translated sentence, formatted by the shared helper. */}
            {t("incident.openedBy", { who: incident.createdBy, at: fmtStamp(incident.createdAt, locale) })}
            {incident.resolvedAt ? t("incident.resolvedAt", { at: fmtStamp(incident.resolvedAt, locale) }) : ""}
          </span>
          <div className="ml-auto flex flex-wrap items-center gap-2">
            <Button type="button" size="sm" variant="outline" onClick={() => void copyPermalink()}>
              {t("incident.copyPermalink")}
            </Button>
            {/* Permission HIDES, time DISABLES — the same split every other
                write on this page makes. */}
            {canWrite ? (
              <Button
                type="button"
                size="sm"
                variant="outline"
                loading={busy}
                {...guard}
                disabled={writesDisabled}
                onClick={() => void patch({ status: resolved ? "open" : "resolved" })}
              >
                {resolved ? t("incident.reopen") : t("incident.resolve")}
              </Button>
            ) : null}
            {/* Delete, behind the same permission and the same confirm every
                other destructive control in this console wears (finding #21).
                Before this there was NO way to remove an incident from any
                surface — a mistyped one stayed in the list forever, and the
                only record of it was a permalink that kept resolving. */}
            {canWrite ? (
              confirmingDelete ? (
                <>
                  {/* Spoken as well as drawn — the controls swap under the reader. */}
                  <span role="status" className="sr-only">
                    {t("incident.confirmDelete.aria", { title: incident.title })}
                  </span>
                  <Button
                    ref={confirmRef}
                    type="button"
                    size="sm"
                    variant="outline"
                    loading={busy}
                    {...guard}
                    disabled={writesDisabled}
                    aria-label={t("incident.confirmDelete.aria", { title: incident.title })}
                    onClick={() => void handleDelete()}
                  >
                    {t("incident.confirmDelete")}
                  </Button>
                  <Button type="button" size="sm" variant="ghost" onClick={resetDelete}>
                    {t("incident.cancel")}
                  </Button>
                </>
              ) : (
                <Button
                  ref={triggerRef}
                  type="button"
                  size="sm"
                  variant="ghost"
                  {...guard}
                  disabled={writesDisabled}
                  aria-label={t("incident.delete.aria", { title: incident.title })}
                  onClick={askDelete}
                >
                  {t("incident.delete")}
                </Button>
              )
            ) : null}
          </div>
        </div>

        <p className="mt-2 type-meta">
          {t("incident.scope.before")}{" "}
          <span className={cn("font-medium text-foreground", incident.scope !== "" && "mono-data")}>
            {incident.scope === "" ? t("incident.scope.global") : incident.scope}
          </span>{" "}
          {t("incident.scope.after")}
          {targetsGated ? t("incident.scope.targetsGated") : ""}
        </p>

        {copyNote ? (
          <p role="status" className="mt-2 text-xs text-muted-foreground">
            {copyNote}
          </p>
        ) : null}

        <div className="mt-4">
          <h3 className="text-xs font-semibold uppercase tracking-[0.07em] text-muted-foreground">
            {t("incident.notes")}
          </h3>
          {canWrite ? (
            <>
              <textarea
                aria-label={t("incident.notes.aria")}
                value={notes}
                rows={3}
                maxLength={INCIDENT_NOTES_MAX}
                onChange={(e) => setNotes(e.target.value)}
                className={`${TEXTAREA_CLASS} mt-2`}
              />
              <div className="mt-2 flex items-center gap-2">
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  loading={busy}
                  disabled={writesDisabled || notes === incident.notes}
                  onClick={() => void patch({ notes })}
                >
                  {t("incident.notes.save")}
                </Button>
                <span className="nums type-meta">
                  {notes.length}/{INCIDENT_NOTES_MAX}
                </span>
              </div>
            </>
          ) : (
            <p className="mt-2 whitespace-pre-wrap text-xs leading-relaxed text-muted-foreground">
              {incident.notes === "" ? t("incident.notes.gated") : incident.notes}
            </p>
          )}
        </div>

        {error ? (
          <p role="alert" className="mt-2 text-xs text-health-bad">
            {error}
          </p>
        ) : null}
      </section>
    </Card>
  );
}

/**
 * PinnedFindings is the shortlist an operator builds out of the timeline; the note is edited
 * LOCALLY and saved with one button.
 */
function PinnedFindings({
  pinned,
  presentKeys,
  canWrite,
  writesDisabled,
  busy,
  dirty,
  onNote,
  onRemove,
  onSave,
}: {
  pinned: PinnedRef[];
  /**
   * pinKey() of every timeline row currently in the window. A pin whose row is
   * NOT in here was pinned from a window this page is no longer framing, and the
   * page has nothing but the stored (kind, id) to show for it (QA scope 3,
   * finding #10) — so it says so rather than letting "audit / 1757" stand as if
   * it were a finding's name.
   */
  presentKeys: ReadonlySet<string>;
  canWrite: boolean;
  writesDisabled: boolean;
  busy: boolean;
  dirty: boolean;
  onNote: (index: number, note: string) => void;
  onRemove: (index: number) => void;
  onSave: () => void;
}) {
  const t = useT(investigateDict);
  /* Which row is asking "are you sure?", by pinKey rather than by index — the
     list is re-keyed by every save, and an index would move the confirm onto a
     different finding under the operator's cursor. The hook is what carries
     focus across the swap, so the keyboard does not land back on <body>. */
  const { confirming, confirmRef, triggerRef, ask, reset } = useKeyedConfirmStep();

  return (
    <Card asChild className="p-5">
      <section aria-label={t("pinned.aria")}>
        <h3 className="type-section">{t("pinned.title")}</h3>
        {pinned.length === 0 ? (
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
            {t("pinned.empty.lead")} {canWrite ? t("pinned.empty.canWrite") : t("pinned.empty.gated")}{" "}
            {/* All THREE unpinnable classes, named (QA round 3, finding #19).
                The sentence used to list two and stop, so an operator hunting
                for the missing pin control on a firing-alert row was left to
                conclude the console was broken. An alert lives in Prometheus,
                not in any table this console owns, and its id is a fingerprint
                of a label set that stops existing the moment it resolves —
                PIN_KIND_BY_TIMELINE_KIND is the authority and says so. */}
            {t("pinned.empty.unpinnable")}
          </p>
        ) : (
          <ul className="mt-3 flex flex-col gap-2">
            {pinned.map((p, i) => (
              <li key={pinKey(p)} data-testid="pinned-finding" className="flex flex-wrap items-center gap-2 text-xs">
                <Badge variant="neutral">{p.kind}</Badge>
                <span className="mono-data max-w-[12rem] truncate text-muted-foreground" title={p.id}>
                  {p.id}
                </span>
                {canWrite ? (
                  <>
                    <input
                      /* p.kind and p.id are the stored ref — data. */
                      aria-label={t("pinned.note.aria", { kind: p.kind, id: p.id })}
                      value={p.note ?? ""}
                      maxLength={PIN_NOTE_MAX}
                      placeholder={t("pinned.note.placeholder")}
                      onChange={(e) => onNote(i, e.target.value)}
                      className={`${INPUT_CLASS} min-w-0 flex-1`}
                    />
                    {/* Unpin DISCARDS the note (QA round 3, finding #22). The
                        API replaces `pinned` wholesale — there is no per-ref
                        delete and nothing keeps an orphaned note server-side —
                        so removing a finding somebody wrote a reason against
                        destroys the reason with it, with no undo. A note-less
                        unpin stays one click: there is nothing to lose, and
                        pinning is meant to be cheap to change your mind about. */}
                    {(p.note ?? "") !== "" && confirming === pinKey(p) ? (
                      <>
                        {/* Spoken as well as drawn — the row swaps its controls under the reader. */}
                        <span role="status" className="sr-only">
                          {t("pinned.discardAndUnpin.aria", { kind: p.kind, id: p.id })}
                        </span>
                        <Button
                          ref={confirmRef}
                          type="button"
                          size="sm"
                          variant="outline"
                          disabled={writesDisabled || busy}
                          aria-label={t("pinned.discardAndUnpin.aria", { kind: p.kind, id: p.id })}
                          onClick={() => {
                            reset();
                            onRemove(i);
                          }}
                        >
                          {t("pinned.discardAndUnpin")}
                        </Button>
                        <Button type="button" size="sm" variant="ghost" onClick={reset}>
                          {t("pinned.cancel")}
                        </Button>
                      </>
                    ) : (
                      <Button
                        ref={triggerRef}
                        type="button"
                        size="sm"
                        variant="ghost"
                        disabled={writesDisabled || busy}
                        aria-label={t("pinned.unpin.aria", { kind: p.kind, id: p.id })}
                        onClick={() => ((p.note ?? "") === "" ? onRemove(i) : ask(pinKey(p)))}
                      >
                        {t("pinned.unpin")}
                      </Button>
                    )}
                  </>
                ) : (
                  <span className="min-w-0 flex-1 break-words text-muted-foreground">{p.note ?? ""}</span>
                )}
                {/* The stored kind and id are all this row has when its source
                    row is outside the window — and they are NOT a title. Naming
                    the gap costs one line and stops the pane from reading as if
                    "audit / 1757" were what somebody pinned it for. The
                    operator's own note, when there is one, is the actual answer
                    and is already on the row above this line. */}
                {presentKeys.has(pinKey(p)) ? null : (
                  <span data-testid="pin-out-of-window" className="basis-full type-meta">
                    {t("pinned.outOfWindow")}
                  </span>
                )}
              </li>
            ))}
          </ul>
        )}
        {canWrite && pinned.length > 0 ? (
          <Button
            type="button"
            size="sm"
            variant="outline"
            className="mt-3"
            loading={busy}
            disabled={writesDisabled || !dirty}
            onClick={onSave}
          >
            {t("pinned.saveNotes")}
          </Button>
        ) : null}
      </section>
    </Card>
  );
}

/**
 * Select is the scope pickers' one control, and it CARRIES A VALUE THE OPTIONS DO
 * NOT HAVE rather than dropping it (QA scope 4).
 *
 * A select whose `value` matches no option renders blank — and every reason to
 * arrive here with one is ordinary: a permalink written last month, an incident
 * saved before a node was drained, a `?scope=` typed by hand, a `targets:read`
 * this subject does not have (which empties the list entirely). The page went on
 * investigating that scope, and said so in the headline beside the title, while
 * the picker under it showed nothing — so the two halves of the form disagreed,
 * and pressing Investigate committed the name the reader could not see.
 *
 * The option is drawn instead, MARKED: the picker names what is committed, and
 * the mark says the fleet has no such object today, which is itself a finding on
 * an investigation page. Choosing anything else drops it, exactly as it should.
 */
function Select({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: string;
  options: string[];
  onChange: (v: string) => void;
}) {
  const t = useT(investigateDict);
  const orphan = value !== "" && !options.includes(value);
  /* The MARK is a claim about the fleet, so it waits for a list to have been
     loaded. An empty list is "not asked yet" (topology in flight) or "not
     allowed to ask" (no targets:read, which has its own note under the form) —
     neither of those is evidence that the object is gone, and marking on them
     would print «node-a — not in the current topology» about a node that is
     right there, for as long as the first fetch takes. */
  const missing = orphan && options.length > 0;
  return (
    <label className="flex flex-col gap-1 text-[13px]">
      <span className="text-muted-foreground">{label}</span>
      <select
        aria-label={label}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        /* max-w as well as min-w: a <select>'s intrinsic width is its WIDEST option, and a scope
           comes off the wire — the stand carries one at 250 characters. Without a cap the control
           measured ~1950px and `main` (overflow-auto) scrolled the whole page sideways. */
        className={`${CONTROL_CLASS} min-w-[10rem] max-w-[20rem]`}
      >
        <option value="">—</option>
        {orphan ? (
          /* The name is the URL's or the incident's own bytes — data, with or
             without the translated mark after it. */
          <option value={value}>{missing ? t("form.option.missing", { value }) : value}</option>
        ) : null}
        {options.map((o) => (
          <option key={o} value={o}>
            {o}
          </option>
        ))}
      </select>
    </label>
  );
}

export function InvestigatePage() {
  const t = useT(investigateDict);
  /* The SECOND translator, and deliberately not a second entry in this page's own table. */
  const ts = useT(investigationSourcesDict);
  /* The locale itself, for the stamp helpers the pure mappers now take. */
  const { locale } = useLocale();
  const { me, can } = useAuth();
  const qc = useQueryClient();
  const writesDisabled = useWritesDisabled();
  const guard = useWriteGuard();
  /* The viewed instant itself, not just the boolean. */
  const { at } = useTimeContext();
  const { available: dbAvailable, resolved: dbResolved } = useDatabaseAvailable();
  const { data: config } = useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });
  const promConfigured = config?.prometheus.configured ?? false;

  /* The URL is read at mount AND re-read whenever it changes under the page —
     through the same gate the form commits through (QA scope 3, finding #2). */
  const [hydrated] = useState<Hydrated>(() => hydrateInvestigation(window.location.search, new Date(), at, ts));
  const [params, setParams] = useState<InvestigationParams>(hydrated.params);
  const [runError, setRunError] = useState<string>();
  const [runStarted, setRunStarted] = useState<string>();
  /* Why the last commit was refused, and whether it moved an edge — both are
     statements about the CURRENT params, so both are cleared by the next
     successful commit (findings #2, #3 and #6). */
  const [commitError, setCommitError] = useState<string | undefined>(hydrated.error);
  const [clamped, setClamped] = useState(hydrated.clamped);
  /* Parameters the URL carried and the parser threw away, until dismissed. */
  const [ignoredParams, setIgnoredParams] = useState<string[]>(hydrated.ignored);

  /* Incident mode. The id is read from the URL once, exactly like the four
     scope parameters, and is thereafter owned by this state: saving sets it,
     applying the form clears it. */
  const [incidentId, setIncidentId] = useState<string | null>(
    () => readIncidentParam(window.location.search),
  );
  /* The id of a permalink that names an incident the server does not have —
     usually one somebody deleted (QA scope 3, finding #3). Held separately from
     `incidentId` because the page STOPS being in incident mode the moment it
     learns that: the query is retired, the ghost `?incident=` is dropped from
     the address, and this id survives only to be named in the not-found state. */
  const [missingIncidentId, setMissingIncidentId] = useState<string | null>(null);
  const [saveOpen, setSaveOpen] = useState(false);
  const [pinned, setPinned] = useState<PinnedRef[]>([]);
  const [pinBusy, setPinBusy] = useState(false);
  const [pinDirty, setPinDirty] = useState(false);
  const [pinError, setPinError] = useState<string>();

  // Draft state: what the form shows before "Investigate" commits it.
  const [draftKind, setDraftKind] = useState<ScopeKind>(params.kind);
  const [draftA, setDraftA] = useState(params.a);
  const [draftB, setDraftB] = useState(params.b);
  const [preset, setPreset] = useState<RangePreset>(() => presetForSpan(params.from, params.to));
  const [customFrom, setCustomFrom] = useState<Date>(params.from);
  const [customTo, setCustomTo] = useState<Date>(params.to);

  const topology = useTopology();
  /* Both lists come from the NODES AND THE AGENTS. */
  const nodeNames = useMemo(() => scopeNodeOptions(topology.data), [topology.data]);
  const zoneNames = useMemo(() => scopeZoneOptions(topology.data), [topology.data]);

  const canTargets = can("targets:read");
  const targetsQuery = useQuery({
    queryKey: ["investigate", "targets"],
    queryFn: () => listTargets({ limit: 200 }),
    enabled: me !== undefined && canTargets,
  });
  const targets = useMemo(() => targetsQuery.data?.targets ?? [], [targetsQuery.data]);
  const targetNames = useMemo(() => targets.map((t) => t.name), [targets]);

  /* the URL is re-read when it changes The search string this page last APPLIED, whether it read it or wrote it. */
  const appliedSearchRef = useRef<string>(window.location.search);
  /* The id currently in state and the id already hydrated, as refs, so the
     subscription can compare against them without re-subscribing on every
     change. hydratedRef is also what the `?incident=` effect below reads. */
  const incidentIdRef = useRef<string | null>(incidentId);
  incidentIdRef.current = incidentId;
  const hydratedRef = useRef<string | null>(null);
  /* The subscription is installed ONCE and must not be torn down on every
     locale or Time Machine change, so both reach it through refs. */
  const atRef = useRef(at);
  atRef.current = at;
  const tsRef = useRef(ts);
  tsRef.current = ts;

  useEffect(
    () =>
      subscribeToLocation(() => {
        const search = window.location.search;
        if (search === appliedSearchRef.current) return;
        appliedSearchRef.current = search;
        /* An incident id in the new URL takes over exactly as it does at mount; its absence LEAVES incident mode. */
        const nextIncident = readIncidentParam(search);
        if (nextIncident !== incidentIdRef.current) hydratedRef.current = null;
        setIncidentId(nextIncident);
        setMissingIncidentId(null);
        /*
         * Bare /investigate (no parameters at all) resolves, through
         * parseInvestigationParams' own total degradation — and then through the
         * SAME Time Machine gate the form commits through (finding #2). A back
         * button is a deep link like any other.
         */
        const next = hydrateInvestigation(search, new Date(), atRef.current, tsRef.current);
        setParams(next.params);
        setDraftKind(next.params.kind);
        setDraftA(next.params.a);
        setDraftB(next.params.b);
        setPreset(presetForSpan(next.params.from, next.params.to));
        setCustomFrom(next.params.from);
        setCustomTo(next.params.to);
        setSaveOpen(false);
        setCommitError(next.error);
        setClamped(next.clamped);
        setIgnoredParams(next.ignored);
        correctURL(next, nextIncident, appliedSearchRef);
      }),
    [],
  );

  /* The same correction for the URL the page OPENED on. In an effect rather than
     in the state initializer above: warning and rewriting history are side
     effects, and a render is not where those belong. */
  useEffect(() => {
    correctURL(hydrated, readIncidentParam(window.location.search), appliedSearchRef);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hydrated]);

  /**
   * The gate again, on every change of the viewed instant (finding #2).
   *
   * Two reasons it cannot live in the hydration above alone. lib/timemachine
   * resolves `?at=` AFTER the first render, so a deep link's gate would run while
   * the console still believed it was Live — which is exactly how
   * `?at=X&from=X+1h&to=X+2h` came to render rows dated after the instant the
   * whole console claimed to be showing. And engaging the Time Machine on a
   * window that already reaches past the new instant is the same fact arriving by
   * a different door: it must read the same, with the same banner or the same
   * refusal.
   *
   * The ref makes this an INSTANT-change effect rather than a params-change one:
   * re-clamping an already-clamped window would fight every commit the form makes.
   */
  const atMsRef = useRef<number | null>(at?.getTime() ?? null);
  const firstGateRef = useRef(true);
  useEffect(() => {
    const atMs = at?.getTime() ?? null;
    const first = firstGateRef.current;
    firstGateRef.current = false;
    if (!first && atMs === atMsRef.current) return;
    atMsRef.current = atMs;
    if (at === null) return;
    const commit = commitWindow(params.from, params.to, at, ts);
    if (commit.ok && !commit.clamped) return;
    const next: InvestigationParams = commit.ok
      ? { ...params, from: commit.from, to: commit.to }
      : {
          kind: params.kind,
          a: params.a,
          b: params.b,
          from: new Date(at.getTime() - DEFAULT_RANGE_SECONDS * 1000),
          to: at,
        };
    setParams(next);
    setPreset(presetForSpan(next.from, next.to));
    setCustomFrom(next.from);
    setCustomTo(next.to);
    setClamped(commit.ok);
    setCommitError(commit.ok ? undefined : commit.reason);
    /* An `?incident=` link is the row's, not the URL's — see correctURL. */
    if (readIncidentParam(window.location.search) === null) {
      writeParamsApplied(next, appliedSearchRef, true);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [at, ts]);

  const scope: InvestigationScope = useMemo(
    () => ({ kind: params.kind, a: params.a, b: params.b }),
    [params.kind, params.a, params.b],
  );
  const key = useMemo(
    () => `${params.kind}|${params.a}|${params.b}|${params.from.toISOString()}|${params.to.toISOString()}`,
    [params],
  );

  const authResolved = me !== undefined;
  const ready = authResolved && dbResolved;
  /* Every store-backed source answers 503 without console.database.mode, so a
     console with no database issues NO request for them — the same call
     pages/target-card.tsx makes, one line instead of six failed fetches. */
  const dbReady = ready && dbAvailable;

  /* incident mode: the saved row hydrates the page One read, behind incidents:read like every other source. */
  const canIncidentsRead = can("incidents:read");
  const canIncidentsWrite = can("incidents:write");
  const incidentQuery = useQuery({
    queryKey: ["incident", incidentId],
    queryFn: () => getIncident(incidentId as string),
    enabled: incidentId !== null && dbReady && canIncidentsRead,
    retry: false,
  });
  const incident = incidentQuery.data;

  /**
   * A permalink to an incident that is gone (QA scope 3, finding #3).
   *
   * Before this the page kept the id in state, kept `?incident=` in the address
   * bar, and rendered a plausible cluster/1h investigation underneath a small
   * warning — so the URL an operator then copied still claimed an incident, and
   * the rows on screen belonged to a scope nobody had chosen. Now the id is
   * retired the moment the server says 404: the parameter is REPLACED out of the
   * address (it was never a place to go back to), and the page renders a
   * not-found state that names the id instead of a page pretending to be the
   * incident's.
   */
  useEffect(() => {
    if (incidentId === null || !incidentQuery.isError) return;
    const error = incidentQuery.error;
    if (!(error instanceof ApiError) || error.problem.status !== 404) return;
    setMissingIncidentId(incidentId);
    setIncidentId(null);
    hydratedRef.current = null;
    qc.removeQueries({ queryKey: ["incident", incidentId] });
    const url = new URL(window.location.href);
    url.searchParams.delete("incident");
    appliedSearchRef.current = url.search;
    window.history.replaceState({}, "", `${url.pathname}${url.search}${url.hash}`);
  }, [incidentId, incidentQuery.isError, incidentQuery.error, qc]);

  /* The target list decides whether a bare saved scope is a node or a target (scopeFromIncidentScope). */
  const targetsSettled = !canTargets || targetsQuery.isFetched;
  useEffect(() => {
    if (incident === undefined || !targetsSettled || hydratedRef.current === incident.id) return;
    hydratedRef.current = incident.id;
    const next = incidentParams(incident, targetNames, new Date());
    setParams(next);
    setDraftKind(next.kind);
    setDraftA(next.a);
    setDraftB(next.b);
    setPreset(presetForSpan(next.from, next.to));
    setCustomFrom(next.from);
    setCustomTo(next.to);
  }, [incident, targetsSettled, targetNames]);

  /* The pinned list is server-owned: every write replaces it wholesale and the
     response resets this state, so a rejected PATCH leaves the UI showing what
     is actually stored rather than what was attempted. */
  useEffect(() => {
    if (incident === undefined) return;
    setPinned(incident.pinned);
    setPinDirty(false);
  }, [incident]);

  const onIncidentPatched = useCallback(
    (updated: Incident) => {
      qc.setQueryData(["incident", updated.id], updated);
    },
    [qc],
  );

  const savePinned = useCallback(
    async (next: PinnedRef[]) => {
      if (incident === undefined) return;
      setPinBusy(true);
      setPinError(undefined);
      try {
        onIncidentPatched(await patchIncident(incident.id, { pinned: next }));
      } catch (err) {
        setPinError(queryErrorMessage(err, t("pinned.saveFailed")));
        setPinned(incident.pinned);
        setPinDirty(false);
      } finally {
        setPinBusy(false);
      }
    },
    [incident, onIncidentPatched, t],
  );

  const pinnedKeys = useMemo(() => new Set(pinned.map((p) => pinKey(p))), [pinned]);

  /* Toggling writes IMMEDIATELY (a pin is a decision, not a draft) and sends
     the whole array including any notes typed but not yet saved — the field is
     replaced wholesale either way, so leaving them behind would silently
     discard them. */
  const togglePin = useCallback(
    (ref: { kind: string; id: string }) => {
      const key = pinKey(ref);
      const next = pinnedKeys.has(key)
        ? pinned.filter((p) => pinKey(p) !== key)
        : [...pinned, ref as PinnedRef];
      setPinned(next);
      setPinDirty(false);
      void savePinned(next);
    },
    [pinned, pinnedKeys, savePinned],
  );

  const pinning: PinControl | undefined =
    incident !== undefined && canIncidentsWrite && !writesDisabled
      ? { pinnedKeys, onToggle: togglePin, busy: pinBusy }
      : undefined;

  /* ── source 1: fleet events (events:read) ── */
  const canEvents = can("events:read");
  const eventScope = scopeFilterValue(scope);
  const eventsQuery = useQuery({
    queryKey: ["investigate", "events", key],
    queryFn: () =>
      getEvents({
        ...(eventScope === "" ? {} : { scope: eventScope }),
        from: params.from,
        to: params.to,
        limit: EVENT_LIMIT,
      }),
    enabled: dbReady && canEvents,
  });

  /* ── source 2: config changes (audit:read) ── */
  const canAudit = can("audit:read");
  const auditQuery = useQuery({
    queryKey: ["investigate", "audit", key],
    queryFn: () => getAuditEntries({ limit: AUDIT_SCAN_LIMIT }),
    enabled: dbReady && canAudit,
  });

  /* ── source 3: annotations (annotations:read) ── */
  const canAnnotations = can("annotations:read");
  const annotationScopes = useMemo(() => scopesToQuery(scope), [scope]);
  const annotationsQuery = useQuery({
    queryKey: ["investigate", "annotations", key],
    queryFn: async () => {
      const pages = await Promise.all(
        annotationScopes.map((s) =>
          listAnnotations({ from: params.from, to: params.to, ...(s === undefined ? {} : { scope: s }) }),
        ),
      );
      return mergeAnnotations(...pages.map((p) => p.annotations));
    },
    enabled: dbReady && canAnnotations,
  });
  const annotations = useMemo(() => annotationsQuery.data ?? [], [annotationsQuery.data]);

  /* source 4: MTR path changes (mtr:read) The endpoint REQUIRES both a source and a destination (422 otherwise). */
  const canMTR = can("mtr:read");
  const mtrMode: "pair" | "by-source" | "by-destination" | "none" =
    scope.kind === "pair" ? "pair" : scope.kind === "node" ? "by-source" : scope.kind === "target" ? "by-destination" : "none";
  const snapshotsQuery = useQuery({
    queryKey: ["investigate", "snapshots", key],
    queryFn: async (): Promise<PathSnapshot[]> => {
      if (mtrMode === "pair") {
        return (await getMTRSnapshots({ source: scope.a, destination: scope.b, limit: 50 })).snapshots;
      }
      const all = await getMTRDestinations();
      const pairs = all.items
        .filter((d) => (mtrMode === "by-source" ? d.sourceNode === scope.a : d.destination === scope.a))
        .slice(0, MTR_FANOUT);
      const pages = await Promise.all(
        pairs.map((p) => getMTRSnapshots({ source: p.sourceNode, destination: p.destination, limit: 20 })),
      );
      return pages.flatMap((p) => p.snapshots);
    },
    enabled: dbReady && canMTR && mtrMode !== "none",
  });

  /* source 5: diagnostic runs (runs:read) Two steps, the pages/target-card.tsx precedent: one list request. */
  const canRuns = can("runs:read");
  const runsQuery = useQuery({
    queryKey: ["investigate", "runs", key],
    queryFn: () => getRuns({ limit: RUN_SCAN_LIMIT }),
    enabled: dbReady && canRuns,
  });
  const runIdsInWindow = useMemo(
    () =>
      (runsQuery.data?.runs ?? [])
        .filter((r) => {
          const at = validAt(r.createdAt);
          return at !== null && inRange(at, params.from, params.to);
        })
        .map((r) => r.id),
    [runsQuery.data, params.from, params.to],
  );
  const runDetailsQuery = useQuery({
    queryKey: ["investigate", "run-details", key, runIdsInWindow.join(",")],
    queryFn: () => Promise.all(runIdsInWindow.map((id) => getRun(id))),
    enabled: dbReady && canRuns && runIdsInWindow.length > 0,
  });
  const scopedRuns = useMemo(
    () => (runDetailsQuery.data ?? []).filter((r) => runTouchesScope(r.spec, scope)),
    [runDetailsQuery.data, scope],
  );

  /* source 6: K8s events (events:read) A PAIR asks for BOTH nodes, one name-filtered request each. */
  const k8sNames = useMemo(
    () => (scope.kind === "pair" ? [scope.a, scope.b] : scope.kind === "node" ? [scope.a] : []),
    [scope],
  );
  const k8sQuery = useQuery({
    queryKey: ["investigate", "k8s", key],
    queryFn: async (): Promise<K8sEvent[]> => {
      const requests =
        k8sNames.length === 0
          ? [getK8sEvents({ from: params.from, to: params.to, limit: EVENT_LIMIT })]
          : k8sNames.map((name) => getK8sEvents({ name, from: params.from, to: params.to, limit: EVENT_LIMIT }));
      const pages = await Promise.all(requests);
      return pages.flatMap((p) => p.events);
    },
    enabled: dbReady && canEvents,
  });

  /* ── source 7: maintenance windows (maintenance:read) ── */
  const canMaintenance = can("maintenance:read");
  const maintenanceQuery = useQuery({
    queryKey: ["investigate", "maintenance", key],
    queryFn: async (): Promise<MaintenanceWindow[]> => {
      const pages = await Promise.all(
        scopesToQuery(scope).map((s) =>
          getMaintenance({ from: params.from, to: params.to, ...(s === undefined ? {} : { scope: s }) }),
        ),
      );
      const byId = new Map<string, MaintenanceWindow>();
      for (const page of pages) for (const w of page.windows) byId.set(w.id, w);
      return [...byId.values()];
    },
    enabled: dbReady && canMaintenance,
  });
  const windows = useMemo(() => maintenanceQuery.data ?? [], [maintenanceQuery.data]);

  /* ── source 8: threshold crossings, derived (promql:query) ──
     ONE pair of range queries feeds both the signal charts and the derived
     timeline rows: two fetches would let the picture and the rows disagree
     about the very series they both describe. */
  const canPromQL = can("promql:query");
  const signalsEnabled = ready && canPromQL && promConfigured;
  const rangeSeconds = Math.max(1, (params.to.getTime() - params.from.getTime()) / 1000);
  const stepNs = stepSecondsFor(rangeSeconds) * 1e9;
  /* A window wider than console.prometheus.maxRange is a 422 the proxy answers
     with its own untranslated sentence, so it is not SENT: the panes state the
     bound instead, and the store-backed sources above keep their wide window. */
  const rangeTooWide = rangeExceedsPromBound(params.from, params.to);
  const lossQuery = useQuery({
    queryKey: ["investigate", "loss", key],
    queryFn: () => promqlQueryRange(investigationLossQuery(scope), params.from, params.to, stepNs),
    enabled: signalsEnabled && !rangeTooWide,
  });
  const rttQuery = useQuery({
    queryKey: ["investigate", "rtt", key],
    queryFn: () => promqlQueryRange(investigationRttQuery(scope), params.from, params.to, stepNs),
    enabled: signalsEnabled && !rangeTooWide,
  });
  const deltaQuery = useQuery({
    queryKey: ["investigate", "delta", key],
    queryFn: async () => {
      const q = investigationFailRatioQuery(scope);
      const [before, after] = await Promise.all([promqlQuery(q, params.from), promqlQuery(q, params.to)]);
      return { before, after };
    },
    enabled: signalsEnabled,
  });
  const samples = useMemo(() => samplesFromMatrix(lossQuery.data, rttQuery.data), [lossQuery.data, rttQuery.data]);

  /* source 9: firing alerts (alerts:read + Prometheus) NOT store-backed and therefore not behind dbReady. */
  const canAlerts = can("alerts:read");
  const engaged = at !== null;
  const alertsQuery = useQuery({
    queryKey: ["investigate", "alerts"],
    queryFn: listAlerts,
    enabled: ready && canAlerts && promConfigured && !engaged,
  });
  const firingAlerts = useMemo(() => alertsQuery.data?.alerts ?? [], [alertsQuery.data]);
  /* OURS, narrowed to the scope the way runs are. A foreign alert produces
     nothing here and lib/api.ts does not even ask for one — see
     lib/investigation-sources.ts's alertIsOurs for why the rule id is the
     ownership discriminator and the node labels are not. */
  const scopedAlerts = useMemo(
    () => scopedAlertEntries(firingAlerts, params.from, params.to, scope, ts),
    [firingAlerts, params.from, params.to, scope, ts],
  );

  /* ── assembly ── */
  const entries = useMemo(
    () =>
      mergeTimeline(
        /* eventEntries, auditEntries and k8sEntries take no translator: every
           byte they emit is a server row (a summary, an action, a K8s reason
           and message), and there is nothing of ours around it to translate. */
        eventEntries(eventsQuery.data?.events ?? []),
        auditEntries(auditQuery.data?.entries ?? [], params.from, params.to),
        annotationEntries(annotations, ts),
        pathChangeEntries(snapshotsQuery.data ?? [], params.from, params.to, ts),
        runEntries(scopedRuns, ts),
        k8sEntries(k8sQuery.data ?? []),
        maintenanceEntries(windows, ts, locale),
        /* The four threshold headlines are the sibling lib's own strings now
           (finding #6) — they used to be bare English literals under a «порог»
           badge. */
        thresholdCrossings(samples, DEFAULT_THRESHOLDS, ts),
        scopedAlerts.entries,
      ),
    [
      eventsQuery.data,
      auditQuery.data,
      annotations,
      snapshotsQuery.data,
      scopedRuns,
      k8sQuery.data,
      windows,
      samples,
      scopedAlerts,
      params.from,
      params.to,
      ts,
      locale,
    ],
  );

  const onset = useMemo(() => anomalyOnset(entries), [entries]);
  const causes = useMemo(
    () => (onset === null ? [] : rankCauses(entries, onset).slice(0, CAUSE_TOP_N)),
    [entries, onset],
  );

  /* Which pinned findings still have a ROW on screen. A pin outlives the window
     it was made in — that is the point of pinning — and the pinned pane has to
     say when the row behind one is not here to be read (finding #10). */
  const presentPinKeys = useMemo(() => {
    const out = new Set<string>();
    for (const entry of entries) {
      const ref = pinnedRefFor(entry);
      if (ref !== null) out.add(pinKey(ref));
    }
    return out;
  }, [entries]);

  /* Every source that was ASKED and did not answer, named the way the source
     list names it (QA round 3, finding #1). A source that was never requested
     is absent from here by construction: react-query holds no error for a
     disabled query, and "you may not read this" already has its own line. */
  /**
   * Every source, whether it was ASKED, and what it answered.
   *
   * `asked` mirrors each query's own `enabled` — the one thing react-query will
   * not tell us after the fact — and it is what makes "everything failed"
   * expressible at all (QA scope 3, finding #1). A source nobody asked for is
   * not evidence of anything, and counting it either way would make the
   * all-failed claim a lie in one direction or the other: with only the alerts
   * query enabled, one refusal IS everything.
   */
  const sourceStates = useMemo<{ id: string; label: InvestigateKey; asked: boolean; error: unknown }[]>(
    () => [
      { id: "events", label: "source.name.events" as const, asked: dbReady && canEvents, error: eventsQuery.error },
      { id: "audit", label: "source.name.audit" as const, asked: dbReady && canAudit, error: auditQuery.error },
      {
        id: "annotations",
        label: "source.name.annotations" as const,
        asked: dbReady && canAnnotations,
        error: annotationsQuery.error,
      },
      {
        id: "snapshots",
        label: "source.name.snapshots" as const,
        asked: dbReady && canMTR && mtrMode !== "none",
        error: snapshotsQuery.error,
      },
      {
        id: "runs",
        label: "source.name.runs" as const,
        asked: dbReady && canRuns,
        error: runsQuery.error ?? runDetailsQuery.error,
      },
      { id: "k8s", label: "source.name.k8s" as const, asked: dbReady && canEvents, error: k8sQuery.error },
      {
        id: "maintenance",
        label: "source.name.maintenance" as const,
        asked: dbReady && canMaintenance,
        error: maintenanceQuery.error,
      },
      { id: "loss", label: "source.name.loss" as const, asked: signalsEnabled, error: lossQuery.error },
      { id: "rtt", label: "source.name.rtt" as const, asked: signalsEnabled, error: rttQuery.error },
      /* The delta pair was missing from this table entirely, which is why the
         chip printed "0.0% → 0.0%" over two refused evaluations with no line
         anywhere saying they had been refused. */
      { id: "delta", label: "source.name.delta" as const, asked: signalsEnabled, error: deltaQuery.error },
      {
        id: "alerts",
        label: "source.name.alerts" as const,
        asked: ready && canAlerts && promConfigured && !engaged,
        error: alertsQuery.error,
      },
    ],
    [
      dbReady,
      ready,
      canEvents,
      canAudit,
      canAnnotations,
      canMTR,
      canRuns,
      canMaintenance,
      canAlerts,
      promConfigured,
      engaged,
      mtrMode,
      signalsEnabled,
      eventsQuery.error,
      auditQuery.error,
      annotationsQuery.error,
      snapshotsQuery.error,
      runsQuery.error,
      runDetailsQuery.error,
      k8sQuery.error,
      maintenanceQuery.error,
      lossQuery.error,
      rttQuery.error,
      deltaQuery.error,
      alertsQuery.error,
    ],
  );

  const failedSources = useMemo(
    () => sourceStates.filter((s) => s.error !== null && s.error !== undefined),
    [sourceStates],
  );

  /* ── the source list: one honest line per absent or bounded source ── */
  const notes = useMemo<SourceNote[]>(() => {
    const out: SourceNote[] = [];
    if (dbResolved && !dbAvailable) {
      out.push({ id: "database", text: t("source.database") });
    }
    /* Every permission line waits for GET /auth/me, the same way the database line waits for the
       capability probe. `can()` is false for a permission the caller HAS until that request lands,
       so an admin opening this page was shown eight definitive sentences saying they were missing
       everything — and then, a moment later, the timeline they said could not be built. A claim
       about what somebody may not do is not a thing to say before knowing. */
    if (!authResolved) {
      return out;
    }
    if (!canEvents) {
      out.push({ id: "events", text: t("source.events") });
    }
    if (!canAudit) {
      /* "Audit rows", not "config changes" (QA round 5, finding #19): the
         source is the audit log, and most of what it records is a READ
         decision, not a change. The timeline badge above was corrected the
         same way — this note names the same rows, so it has to agree. */
      out.push({ id: "audit", text: t("source.audit") });
    } else {
      out.push({ id: "audit-window", text: t("source.auditWindow", { limit: AUDIT_SCAN_LIMIT }) });
    }
    if (!canAnnotations) {
      out.push({ id: "annotations", text: t("source.annotations") });
    }
    if (!canMTR) {
      out.push({ id: "mtr", text: t("source.mtr") });
    } else if (mtrMode === "none") {
      out.push({ id: "mtr-scope", text: t("source.mtrScope") });
    } else if (mtrMode !== "pair") {
      out.push({ id: "mtr-fanout", text: t("source.mtrFanout", { limit: MTR_FANOUT }) });
    }
    if (!canRuns) {
      out.push({ id: "runs", text: t("source.runs") });
    } else {
      out.push({ id: "runs-scan", text: t("source.runsScan", { limit: RUN_SCAN_LIMIT }) });
    }
    if (!canMaintenance) {
      out.push({ id: "maintenance", text: t("source.maintenance") });
    }
    if (!canPromQL) {
      out.push({ id: "promql", text: t("source.promql") });
    } else if (!promConfigured) {
      out.push({ id: "promql-config", text: t("source.promqlConfig") });
    }
    if (!canAlerts) {
      out.push({ id: "alerts", text: t("source.alerts") });
    } else if (engaged) {
      /* The honest live-only caption, word for word the one pages/overview.tsx gives. */
      out.push({ id: "alerts-live-only", text: t("source.alertsLiveOnly") });
    } else if (!promConfigured) {
      out.push({ id: "alerts-config", text: t("source.alertsConfig") });
    } else {
      out.push({ id: "alerts-now", text: t("source.alertsNow") });
      /* Scope narrowing is the one way an alert this console owns can be firing
         and have no row, so it is counted and named rather than left to be
         noticed by its absence. */
      if (scopedAlerts.hiddenByScope > 0) {
        out.push({
          id: "alerts-scope-hidden",
          text: t(`source.alertsScopeHidden.${countForm(locale, scopedAlerts.hiddenByScope)}` as InvestigateKey, {
            count: scopedAlerts.hiddenByScope,
          }),
        });
      }
    }

    /*
     * one line PER FAILED SOURCE These used to collapse into a single "One of the timeline's
     * sources is unavailable" card carrying whichever error `??` reached first.
     */
    for (const source of failedSources) {
      out.push({
        id: `failed-${source.id}`,
        /* The source's own NAME is ours; the detail after the colon is the
           server's sentence, verbatim. */
        text: t("source.failed", {
          label: t(source.label),
          error: queryErrorMessage(source.error, t("source.failed.fallback")),
        }),
        failed: true,
      });
    }
    return out;
  }, [
    t,
    authResolved,
    dbResolved,
    dbAvailable,
    canEvents,
    canAudit,
    canAnnotations,
    canMTR,
    canRuns,
    canMaintenance,
    canPromQL,
    canAlerts,
    promConfigured,
    mtrMode,
    engaged,
    failedSources,
    scopedAlerts,
    locale,
  ]);

  /**
   * `!ready` FIRST (QA scope 4).
   *
   * Every source below is gated on `ready` — auth and the database capability
   * both resolved — so until that lands react-query holds eleven DISABLED
   * queries, none of which is "loading". The pane read that as a settled fetch
   * over an empty result and rendered its verdict on it: «0 записей в этом
   * интервале» over "nothing happened in this window", for a page that had not
   * yet asked a single question. It is the exact failure the whole partial /
   * all-failed apparatus below exists to prevent, arriving one round trip before
   * any of that apparatus can see it.
   */
  const loading =
    !ready ||
    eventsQuery.isLoading ||
    auditQuery.isLoading ||
    annotationsQuery.isLoading ||
    snapshotsQuery.isLoading ||
    k8sQuery.isLoading ||
    maintenanceQuery.isLoading ||
    lossQuery.isLoading ||
    alertsQuery.isLoading;

  /* Not "some source failed" but "there is no timeline here" (finding #1). Read
     while anything is still in flight it would be a verdict on a race, so it
     waits for the last fetch to settle. */
  const askedSources = useMemo(() => sourceStates.filter((s) => s.asked), [sourceStates]);
  const allFailed =
    !loading &&
    askedSources.length > 0 &&
    askedSources.every((s) => s.error !== null && s.error !== undefined);

  /* ── the entry form ── */

  /* The draft's own completeness, recomputed on every keystroke of the selects
     so the button and the reason under it never disagree (finding #6). */
  const draftScope: InvestigationScope = useMemo(
    () => ({ kind: draftKind, a: draftA, b: draftB }),
    [draftKind, draftA, draftB],
  );
  const incompleteReason = useMemo(() => scopeIncompleteReason(draftScope, ts), [draftScope, ts]);

  /**
   * The CUSTOM range's own refusal, computed on every keystroke of the two
   * pickers rather than on the click (QA scope 3, finding #13).
   *
   * An incomplete scope disabled the button and said why; an inverted or
   * after-the-instant custom range left it enabled and did nothing at all when
   * pressed — two different answers to the same question, one of which is a
   * no-op click, which is the one thing a control must never be. Both now
   * disable, and both say why in the same place. The presets keep the
   * post-click path: their window is computed from `now` at the moment of the
   * click, so there is nothing to pre-judge.
   */
  const rangeReason = useMemo(() => {
    if (preset !== "custom") return null;
    const commit = commitWindow(customFrom, customTo, at, ts);
    return commit.ok ? null : commit.reason;
  }, [preset, customFrom, customTo, at, ts]);

  const apply = useCallback(() => {
    if (incompleteReason !== null || rangeReason !== null) return;
    const now = new Date();
    const chosen = RANGE_PRESETS.find((p) => p.value === preset);
    const rawFrom =
      preset === "custom" ? customFrom : new Date(now.getTime() - (chosen?.seconds ?? DEFAULT_RANGE_SECONDS) * 1000);
    const rawTo = preset === "custom" ? customTo : now;

    /*
     * Time Machine contract for this page: engaging at `t` clamps a committed window's `to` down to
     * `t` (a window entirely after `t` is refused outright).
     */
    const commit = commitWindow(rawFrom, rawTo, at, ts);
    if (!commit.ok) {
      setCommitError(commit.reason);
      return;
    }

    const next: InvestigationParams = { kind: draftKind, a: draftA, b: draftB, from: commit.from, to: commit.to };
    appliedSearchRef.current = writeParams(next);
    setParams(next);
    setCommitError(undefined);
    setClamped(commit.clamped);
    /* The clamp is a fact about the COMMITTED window, so the fields have to
       show it too — otherwise re-pressing Investigate would re-clamp the same
       edge and the form would keep disagreeing with the page. */
    if (commit.clamped) {
      setPreset("custom");
      setCustomFrom(commit.from);
      setCustomTo(commit.to);
    }
    /* Leaving incident mode: see writeParams' own comment. hydratedRef is left
       alone deliberately — re-entering the SAME incident later must hydrate
       again, and clearing the id is what makes the next mount do that. */
    setIncidentId(null);
    /* The not-found state was about a link, and this is a different question
       now — the URL it named is already gone from the address bar. */
    setMissingIncidentId(null);
    /* Whatever the arriving URL could not honour, this one can: the address the
       page just wrote is its own. */
    setIgnoredParams([]);
    setSaveOpen(false);
  }, [draftKind, draftA, draftB, preset, customFrom, customTo, at, incompleteReason, rangeReason, ts]);

  /* Save-as-incident sends the CURRENT scope and range verbatim: the incident is the investigation on screen. */
  const saveIncident = useCallback(
    async (title: string, notes: string) => {
      const created = await createIncident({
        title,
        scope: scopeFilterValue(scope),
        fromAt: params.from.toISOString(),
        toAt: params.to.toISOString(),
        ...(notes === "" ? {} : { notes }),
      });
      qc.setQueryData(["incident", created.id], created);
      hydratedRef.current = created.id;
      setIncidentId(created.id);
      setSaveOpen(false);
      appliedSearchRef.current = writeIncidentParam(created.id);
    },
    [qc, scope, params.from, params.to],
  );

  const changeKind = useCallback((kind: ScopeKind) => {
    setDraftKind(kind);
    setDraftA("");
    setDraftB("");
  }, []);

  /** onIncidentDeleted lands the page back on the bare entry form; staying put was not an option: the row is gone. */
  const onIncidentDeleted = useCallback(() => {
    if (incidentId !== null) qc.removeQueries({ queryKey: ["incident", incidentId] });
    hydratedRef.current = null;
    const url = new URL(window.location.href);
    const kept = new URLSearchParams();
    const atParam = url.searchParams.get("at");
    if (atParam !== null) kept.set("at", atParam);
    const search = kept.toString();
    window.history.pushState({}, "", `${INVESTIGATE_PATH}${search === "" ? "" : `?${search}`}`);
  }, [incidentId, qc]);

  /* ── the actions rail ── */
  const canRun = can("runs:create");
  const targetIdForScope = targets.find((t) => t.name === scope.a)?.id ?? "";
  const runSources = useMemo(() => {
    if (scope.kind === "pair" || scope.kind === "node") return [scope.a];
    if (scope.kind === "zone-pair") {
      return (topology.data?.nodes ?? []).filter((n) => n.zone === scope.a).map((n) => n.name);
    }
    return [];
  }, [scope, topology.data]);
  const runDestinations = useMemo(() => {
    if (scope.kind === "pair") return [scope.b];
    if (scope.kind === "zone-pair") {
      return (topology.data?.nodes ?? []).filter((n) => n.zone === scope.b).map((n) => n.name);
    }
    return [];
  }, [scope, topology.data]);

  const startRun = useCallback(
    async (type: "mtr" | "tcp") => {
      setRunError(undefined);
      setRunStarted(undefined);
      try {
        const created = await createRun(
          buildRunRequest({
            type,
            sources: runSources,
            destinations: runDestinations,
            destinationKind: scope.kind === "target" ? "target" : "node",
            destinationTargetId: targetIdForScope,
            destinationAddress: "",
          }),
        );
        setRunStarted(created.id);
      } catch (err) {
        setRunError(queryErrorMessage(err, t("actions.runFailed")));
      }
    },
    [runSources, runDestinations, scope.kind, targetIdForScope, t],
  );

  const refreshAnnotations = useCallback(() => {
    void qc.invalidateQueries({ queryKey: ["investigate", "annotations"] });
  }, [qc]);

  /*
   * The rail's own bar writes, so it has to re-read what THIS page fetched — the
   * ["investigate","maintenance"] leg.
   */
  const refreshMaintenance = useCallback(() => {
    void qc.invalidateQueries({ queryKey: ["investigate", "maintenance"] });
    void qc.invalidateQueries({ queryKey: ["maintenance"] });
  }, [qc]);

  return (
    <PageShell
      timeMachine
      title={t("title")}
      help={{ body: t("help.body"), slug: "incidents" }}
      description={t("description")}
      actions={
        <>
          {/* params.kind is the URL's own vocabulary — a wire value. */}
          <Badge variant="neutral">{params.kind}</Badge>
          <span className={cn("text-sm text-muted-foreground", scope.kind !== "cluster" && scope.a !== "" && "mono-data")}>{scopeHeadline(t, scope)}</span>
        </>
      }
    >
      {/* ── entry form ── */}
      <Card asChild className="p-5">
        <section aria-label={t("form.aria")}>
          <div className="flex flex-wrap items-end gap-4">
            <div className="flex flex-col gap-1 text-[13px]">
              <span className="text-muted-foreground">{t("scope.label")}</span>
              <Segmented
                aria-label={t("scope.aria")}
                options={SCOPE_OPTIONS.map((o) => ({ value: o.value, label: t(o.key) }))}
                value={draftKind}
                onChange={changeKind}
              />
            </div>

            {draftKind === "pair" ? (
              <>
                <Select label={t("form.sourceNode")} value={draftA} options={nodeNames} onChange={setDraftA} />
                <Select label={t("form.destinationNode")} value={draftB} options={nodeNames} onChange={setDraftB} />
              </>
            ) : null}
            {draftKind === "node" ? (
              <Select label={t("form.node")} value={draftA} options={nodeNames} onChange={setDraftA} />
            ) : null}
            {draftKind === "zone-pair" ? (
              <>
                <Select label={t("form.sourceZone")} value={draftA} options={zoneNames} onChange={setDraftA} />
                <Select label={t("form.destinationZone")} value={draftB} options={zoneNames} onChange={setDraftB} />
              </>
            ) : null}
            {draftKind === "target" ? (
              <Select label={t("form.target")} value={draftA} options={targetNames} onChange={setDraftA} />
            ) : null}

            <div className="flex flex-col gap-1 text-[13px]">
              <span className="text-muted-foreground">{t("form.range")}</span>
              <Segmented
                aria-label={t("form.range.aria")}
                /* 15m / 1h / 6h are durations; only "Custom" is a word. */
                options={RANGE_PRESETS.map((p) => ({
                  value: p.value,
                  label: p.value === "custom" ? t("form.range.custom") : p.label,
                }))}
                value={preset}
                onChange={(v) => setPreset(v)}
              />
            </div>

            {preset === "custom" ? (
              <div className="flex items-end gap-2">
                <DateTimePicker aria-label={t("form.rangeStart")} value={customFrom} onApply={setCustomFrom} />
                <DateTimePicker aria-label={t("form.rangeEnd")} value={customTo} onApply={setCustomTo} />
              </div>
            ) : null}

            {/* Disabled until the scope NAMES something (finding #6). Not a
                silent no-op click and not a 422 either: an incomplete scope
                commits perfectly well and produces an empty timeline, which is
                indistinguishable from a healthy fleet. */}
            <Button
              type="button"
              size="sm"
              disabled={incompleteReason !== null || rangeReason !== null}
              onClick={apply}
            >
              {t("form.submit")}
            </Button>
          </div>

          {/* incompleteReason, commitError and CLAMPED_BANNER are all written
              by lib/investigation-sources.ts — a module outside this surface —
              so they render as they come. They are listed in dict/
              investigate.ts's header as the surface's untranslated strings. */}
          {incompleteReason !== null ? (
            <p data-testid="scope-incomplete" className="mt-3 text-xs text-muted-foreground">
              {incompleteReason}
            </p>
          ) : null}

          {/* The range's refusal wears the SAME clothes as the scope's, next to
              the same disabled button (finding #13). Only one is shown: a
              disabled button has one first reason, and listing two makes the
              reader guess which to fix. */}
          {incompleteReason === null && rangeReason !== null ? (
            <p data-testid="range-invalid" className="mt-3 text-xs text-muted-foreground">
              {rangeReason}
            </p>
          ) : null}

          {/* The refusal, and the clamp, each stated where the decision was
              made (findings #2 and #3). */}
          {commitError ? (
            <p role="alert" className="mt-3 text-xs text-health-bad">
              {commitError}
            </p>
          ) : null}
          {clamped ? (
            <p data-testid="clamp-banner" role="status" className="mt-3 text-xs text-muted-foreground">
              {ts("banner.clamped")}
            </p>
          ) : null}

          {draftKind === "target" && !canTargets ? (
            <p className="mt-3 text-xs text-muted-foreground">{t("form.targetsGated")}</p>
          ) : null}

          {/* What the URL asked for and this page could not honour (finding
              #14). Dismissible because it describes an arrival, not a state:
              once read it has done its whole job, and the address bar has
              already been corrected. */}
          {ignoredParams.length > 0 ? (
            <div
              data-testid="ignored-params"
              role="status"
              className="mt-3 flex flex-wrap items-baseline gap-x-2 rounded-md bg-surface-2 px-3 py-2"
            >
              <p className="min-w-0 flex-1 type-meta">
                {/* The parameter NAMES are the URL's own vocabulary — data. */}
                {t("ignored.body", { params: ignoredParams.map((k) => `?${k}`).join(", ") })}
              </p>
              <Button type="button" size="sm" variant="ghost" onClick={() => setIgnoredParams([])}>
                {t("ignored.dismiss")}
              </Button>
            </div>
          ) : null}

          <p className="mt-3 type-meta">{t("form.urlNote")}</p>
        </section>
      </Card>

      {/* ── actions rail ── */}
      <Card asChild className="p-4">
        <section aria-label={t("actions.aria")}>
          <div className="flex flex-wrap items-center gap-2">
            {/* Permission HIDES, time DISABLES — lib/timemachine.tsx's
                useWritesDisabled documents the split and this is the
                composition it prescribes. */}
            {canRun ? (
              <>
                <Button type="button" size="sm" variant="outline" {...guard} onClick={() => void startRun("mtr")}>
                  {t("actions.runMTR")}
                </Button>
                <Button type="button" size="sm" variant="outline" {...guard} onClick={() => void startRun("tcp")}>
                  {t("actions.runTCP")}
                </Button>
              </>
            ) : null}

            <a
              href={withAtParam("/explore")}
              className="inline-flex h-8 items-center rounded-md border border-border-strong px-3 text-[13px] hover:bg-accent hover:text-accent-foreground"
            >
              {t("actions.compare")}
            </a>

            <Button
              type="button"
              size="sm"
              variant="outline"
              /* exportFileName, not a template literal: a raw ISO instant puts
                 colons in a filename, which Windows refuses outright and
                 browsers mangle silently (finding #20). */
              onClick={() => downloadJson(exportFileName(params.from), buildExportPayload(params, entries, causes))}
            >
              {t("actions.export")}
            </Button>

            {/* Permission HIDES, time DISABLES, again. An incident cannot be
                opened for a window while the console is pinned to an instant:
                the write would be filed against the present regardless. */}
            {canIncidentsWrite && incident === undefined ? (
              <Button type="button" size="sm" variant="outline" {...guard} onClick={() => setSaveOpen((v) => !v)}>
                {t("actions.saveIncident")}
              </Button>
            ) : null}

          </div>

          {saveOpen ? (
            <SaveIncidentForm
              scopeText={scopeFilterValue(scope)}
              from={params.from}
              to={params.to}
              wide={scope.kind === "zone-pair" || scope.kind === "cluster"}
              onCreate={saveIncident}
              onCancel={() => setSaveOpen(false)}
            />
          ) : null}

          {/* Task 7's second disabled seam, made real (M6 Task 9). The bar is
              the shared one every other surface mounts, given the windows THIS
              page already fetched as timeline source 7 — a second useMaintenance
              here would ask the same two questions twice and let the rows and
              the rail disagree about what is declared. The button is named for
              the rail's own vocabulary rather than the compact "＋ maintenance"
              a chart carries.

              The scope is the investigation's, fixed: scopeFilterValue is the
              same string the events and annotations legs ask for, so a window
              declared here is one this page will read back. */}
          <MaintenanceBar
            scope={scopeFilterValue(scope)}
            /* What the count sentence CALLS the scope: the wide scopes were queried unfiltered. */
            scopeCaption={scopeCaptionValue(scope, ts)}
            windows={windows}
            error={maintenanceQuery.error as Error | null}
            onChanged={refreshMaintenance}
            /* The committed window is FROZEN here, so a window declared outside
               it will not appear in the list below and the bar has to say so
               (finding #8). */
            frozenWindow={{ from: params.from, to: params.to }}
            /* The bar is a shared component; this label is the RAIL's own
               vocabulary and therefore this surface's string. */
            createLabel={t("actions.createMaintenance")}
          />

          <p className="mt-2 type-meta">{t("actions.compareNote")}</p>
          {runStarted ? (
            <p role="status" className="mt-2 text-xs text-muted-foreground">
              {/* Two keys around the run-id link: the id is data. */}
              {t("actions.runStarted.before")}{" "}
              <a href={withAtParam(`/diagnostics/runs/${runStarted}`)} className="mono-data text-primary hover:underline">
                {runStarted}
              </a>{" "}
              {t("actions.runStarted.after")}
            </p>
          ) : null}
          {runError ? (
            <p role="alert" className="mt-2 text-xs text-health-bad">
              {runError}
            </p>
          ) : null}
        </section>
      </Card>

      {/* ── incident mode ── */}
      {incidentId !== null && !canIncidentsRead ? (
        <Card role="status" className="p-4">
          <p className="text-xs leading-relaxed text-muted-foreground">{t("incident.readGated")}</p>
        </Card>
      ) : null}

      {/* The permalink named an incident that is GONE (QA scope 3, finding #3).
          A 404 is not "something went wrong reading it" — it is an answer, and
          the honest surface for it names the id that is missing rather than
          quietly framing a cluster/1h investigation nobody asked for under a
          small warning. The `?incident=` parameter has already been replaced out
          of the address by the effect above, so nothing an operator copies from
          here still claims an incident. */}
      {missingIncidentId !== null ? (
        <Card
          data-testid="incident-not-found"
          role="alert"
          className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-5"
        >
          <p className="text-sm font-medium">{t("incident.missing.title")}</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
            {/* The id is the URL's own bytes — data, interpolated. */}
            {t("incident.missing.body", { id: missingIncidentId })}
          </p>
        </Card>
      ) : null}

      {/* Every OTHER way reading an incident can fail — a 403 the permission
          check did not predict, a 500, a transport error. The row may well still
          exist, so the link is left alone and the page says what the server
          said. */}
      {incidentQuery.isError && missingIncidentId === null ? (
        <Card role="alert" className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-4">
          <p className="text-sm font-medium">{t("incident.error.title")}</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
            {/* endSentence, not the bare detail (QA round 5, finding #10). The
                server's problem details are phrases, not sentences — "no
                incident with that id" carries no full stop — so the two ran
                together into "...with that id The page is showing...", which
                reads as one broken sentence rather than two correct ones. The
                detail itself is the server's own words and is not translated. */}
            {endSentence(queryErrorMessage(incidentQuery.error, t("incident.error.fallback")))}{" "}
            {t("incident.error.body")}
          </p>
        </Card>
      ) : null}

      {incident ? (
        <IncidentStrip
          incident={incident}
          canWrite={canIncidentsWrite}
          writesDisabled={writesDisabled}
          onPatched={onIncidentPatched}
          onDeleted={onIncidentDeleted}
          targetsGated={!canTargets}
        />
      ) : null}

      {incident ? (
        <>
          <PinnedFindings
            pinned={pinned}
            presentKeys={presentPinKeys}
            canWrite={canIncidentsWrite}
            writesDisabled={writesDisabled}
            busy={pinBusy}
            dirty={pinDirty}
            onNote={(i, note) => {
              setPinned((prev) => prev.map((p, j) => (j === i ? { ...p, note } : p)));
              setPinDirty(true);
            }}
            onRemove={(i) => {
              const next = pinned.filter((_, j) => j !== i);
              setPinned(next);
              setPinDirty(false);
              void savePinned(next);
            }}
            onSave={() => void savePinned(pinned)}
          />
          {pinError ? (
            <p role="alert" className="text-xs text-health-bad">
              {pinError}
            </p>
          ) : null}
        </>
      ) : null}

      {/* The single "One of the timeline's sources is unavailable" card is GONE
          (QA round 3, finding #1). It carried whichever error `??` reached
          first, so a second failing source was swallowed entirely and the list
          below still claimed to be complete. Each failure is now its own line
          in the timeline's source list, next to the lines explaining the
          sources that were never asked — one place to read "what is this
          timeline missing, and why" — and the count drives the partial banner
          that suppresses the nothing-happened claim. */}

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-[minmax(0,1fr)_24rem]">
        <InvestigationTimeline
          entries={entries}
          notes={notes}
          loading={loading}
          allFailed={allFailed}
          /* The claim below the rows needs a question to have been put — see the
             pane's own `asked`. */
          asked={askedSources.length > 0}
          pinning={pinning}
          /* The same identity the source queries are keyed on, so the pane's
             page resets exactly when the rows it is paging over are refetched
             for a different scope or window — never a render later. */
          windowKey={key}
        />

        <div className="flex flex-col gap-5">
          <SignalPanels
            scopeLabel={scopeHeadline(t, scope)}
            loss={lossQuery.data}
            /* The REJECTION, not just the envelope (finding #2): a refused
               range query left the pane blank, which reads as "still
               loading" forever. */
            lossError={lossQuery.error as Error | null}
            rtt={rttQuery.data}
            rttError={rttQuery.error as Error | null}
            delta={deltaFromVectors(deltaQuery.data?.before, deltaQuery.data?.after)}
            /* Without this the chip printed a figure for two evaluations that
               never came back (finding #1). */
            deltaError={deltaQuery.error as Error | null}
            windows={windows}
            annotations={annotations}
            promConfigured={promConfigured}
            gated={!canPromQL}
            rangeTooWide={rangeTooWide}
          />

          <Card asChild className="p-5">
            <section aria-label={t("causes.aria")}>
              <h3 className="type-section">{t("causes.title")}</h3>
              {onset === null ? (
                <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t("causes.noOnset")}</p>
              ) : (
                <>
                  <p className="mt-1 type-meta">
                    {/* The SAME clock the timeline rows and the cursor readout
                        draw — the onset is one of those rows (finding #18). */}
                    {t("causes.onset", {
                      at: stampClock(onset, locale),
                      window: DEFAULT_CAUSE_WINDOW_SECONDS,
                    })}
                  </p>
                  {causes.length === 0 ? (
                    <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
                      {t("causes.none", { window: DEFAULT_CAUSE_WINDOW_SECONDS })}
                    </p>
                  ) : (
                    <ol aria-label={t("causes.list.aria")} className="mt-2 flex flex-col gap-2">
                      {causes.map((c) => {
                        const deltaSeconds = Math.round((onset.getTime() - c.entry.at.getTime()) / 1000);
                        const width = Math.max(2, (c.score / Math.max(...Object.values(CAUSE_WEIGHTS))) * 100);
                        return (
                          <li key={`${c.entry.kind}:${c.entry.ref?.id ?? c.entry.at.getTime()}`} className="text-xs">
                            <div className="flex items-baseline gap-2">
                              <span className="mono-data w-10 shrink-0 text-muted-foreground">{c.score.toFixed(2)}</span>
                              <span className="min-w-0 flex-1 break-words">{c.entry.title}</span>
                            </div>
                            <div className="mt-1 flex items-center gap-2">
                              <span aria-hidden="true" className="h-1 rounded-full bg-primary" style={{ width: `${width}%` }} />
                              <span className="type-meta">
                                {t("causes.row", { delta: deltaSeconds, weight: CAUSE_WEIGHTS[c.entry.kind] })}
                              </span>
                            </div>
                          </li>
                        );
                      })}
                    </ol>
                  )}
                </>
              )}
              <p className="mt-3 type-meta">
                {/* Three keys: the link sits inside the sentence. */}
                {t("causes.method.before")}{" "}
                {/* The link stays — the weights being readable is the whole
                    claim this paragraph makes — but it says out loud where it
                    goes (QA scope 3, finding #21). It points at GitHub's `main`,
                    so it is both unreachable from an air-gapped console and, on
                    any console, a description of whatever main holds today
                    rather than of the build in front of the reader. */}
                <a
                  href={DOC_LINK}
                  target="_blank"
                  rel="noreferrer"
                  title={t("causes.method.link.title")}
                  className="text-primary hover:underline"
                >
                  {t("causes.method.link")}
                </a>
                {t("causes.method.after")}
              </p>
            </section>
          </Card>

          <Card asChild className="p-5">
            <section aria-label={t("notes.aria")}>
              <h3 className="type-section">{t("notes.title")}</h3>
              <AnnotationBar
                scope={eventScope}
                /* finding #7 — see the MaintenanceBar above. */
                scopeCaption={scopeCaptionValue(scope, ts)}
                annotations={annotations}
                error={annotationsQuery.error as Error | null}
                onChanged={refreshAnnotations}
                /* finding #8 — the window this list is frozen to. */
                frozenWindow={{ from: params.from, to: params.to }}
              />
            </section>
          </Card>
        </div>
      </div>
    </PageShell>
  );
}
