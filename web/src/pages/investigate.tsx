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
import { subscribeToLocation } from "@/lib/location";
import { endSentence } from "@/lib/utils";
import {
  CAUSE_WEIGHTS,
  DEFAULT_CAUSE_WINDOW_SECONDS,
  anomalyOnset,
  mergeTimeline,
  rankCauses,
  thresholdCrossings,
} from "@/lib/investigation";
import {
  CLAMPED_BANNER,
  DEFAULT_RANGE_SECONDS,
  INCIDENT_NOTES_MAX,
  INCIDENT_TITLE_MAX,
  INVESTIGATE_PATH,
  PAIR_SEPARATOR,
  PIN_NOTE_MAX,
  RANGE_PRESETS,
  alertEntries,
  annotationEntries,
  auditEntries,
  buildExportPayload,
  commitWindow,
  eventEntries,
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
  runEntries,
  runTouchesScope,
  samplesFromMatrix,
  scopeCaptionValue,
  scopeFilterValue,
  scopeIncompleteReason,
  scopeNodeOptions,
  scopeZoneOptions,
  scopesToQuery,
  validAt,
  type InvestigationParams,
  type InvestigationScope,
  type RangePreset,
  type ScopeKind,
} from "@/lib/investigation-sources";
import { useTimeContext, useWriteGuard, useWritesDisabled } from "@/lib/timemachine";
import type { Incident, IncidentStatus, K8sEvent, MaintenanceWindow, PathSnapshot, PinnedRef } from "@/lib/types";
import { buildRunRequest, CONTROL_CLASS } from "@/pages/diagnostics";

/**
 * investigate.tsx — Investigation Mode (DESIGN.md §7.6, the flagship page).
 *
 * THE URL IS THE WHOLE ENTRY CONTRACT (plan Decision 11). `?kind=&scope=&from=
 * &to=` is what a card's "Investigate" action builds, what the browser's Back
 * button restores, and what makes an incident permalink free — so the page
 * reads its parameters off window.location (the convention login.tsx's
 * ?returnTo= and lib/timemachine.tsx's ?at= already established) rather than
 * holding them anywhere a link cannot reach.
 *
 * `?incident={id}` is the FIFTH parameter and it OUTRANKS the other four (M6
 * Task 8, plan Decision 7). In incident mode the saved row is the authority:
 * GET /api/v1/incidents/{id} hydrates scope and range through the very same
 * setParams path the entry form uses, and the URL carries the id ALONE — a
 * permalink that also spelled kind/scope/from/to could disagree with the
 * incident it names after one edit. There is no second page: an incident IS
 * this page, framed by the row.
 *
 * Re-scoping from the form LEAVES incident mode (writeParams drops the id).
 * The row is the authority for what it frames, and a view that has drifted
 * from it must stop claiming to be it.
 *
 * ASSEMBLY IS CLIENT-SIDE (Decision 1) and PER-SOURCE GATED (Decision 12 + M6
 * Global Constraints): nine sources — M7 Task 8 turned the ninth, firing
 * alerts, from an honest-empty note into a real one — each behind its own read
 * permission, each degrading to ONE muted line and ZERO requests rather than to
 * a failed fetch: a viewer without audit:read loses the config-change rows, not
 * the page.
 * lib/investigation.ts merges and ranks; nothing here re-implements it.
 *
 * THREE MODULES, one page. This file is orchestration and chrome only:
 *   - lib/investigation-sources.ts — the scope vocabulary and its URL encoding,
 *     the PromQL each scope produces, and every source's rows→TimelineEntry
 *     mapper. Pure, unit-tested without a mount.
 *   - components/investigation-timeline.tsx — the centre pane and the honest
 *     per-source status list.
 *   - components/investigation-signals.tsx — the right column: loss/RTT charts,
 *     the cursor and maintenance overlays, the matrix delta chip.
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

const SCOPE_OPTIONS: { value: ScopeKind; label: string }[] = [
  { value: "pair", label: "Pair" },
  { value: "node", label: "Node" },
  { value: "target", label: "Target" },
  { value: "zone-pair", label: "Zone pair" },
  { value: "cluster", label: "Cluster" },
];

function presetForSpan(from: Date, to: Date): RangePreset {
  const seconds = Math.round((to.getTime() - from.getTime()) / 1000);
  const match = RANGE_PRESETS.find((p) => p.seconds === seconds);
  return match ? match.value : "custom";
}

function scopeHeadline(scope: InvestigationScope): string {
  switch (scope.kind) {
    case "pair":
      return `${scope.a} ${PAIR_SEPARATOR} ${scope.b}`;
    case "zone-pair":
      return `zone ${scope.a} ${PAIR_SEPARATOR} zone ${scope.b}`;
    case "cluster":
      return "the whole cluster";
    default:
      return scope.a || "(nothing selected)";
  }
}

function queryErrorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiError ? (error.problem.detail ?? error.problem.title) : fallback;
}

/** writeParams rewrites ONLY the parameters this page owns, preserving
 *  pathname, hash and every other query key — `?at=` above all, since a
 *  historical investigation must survive re-scoping. pushState, so Back walks
 *  the investigations the way it walks the Time Machine's instants.
 *
 *  `incident` is DROPPED here and not merely left alone: these four parameters
 *  are what an operator just chose by hand, and keeping the id would leave a
 *  URL claiming to be an incident while showing a different window.
 *
 *  Returns the search string it wrote, so the caller can record it as its own
 *  (QA round 3, finding #10): the page now listens for URL changes, and it must
 *  be able to tell somebody else's navigation from the echo of its own. */
function writeParams(p: InvestigationParams): string {
  const url = new URL(window.location.href);
  for (const key of ["kind", "scope", "from", "to", "incident"]) url.searchParams.delete(key);
  for (const [k, v] of new URLSearchParams(investigationParamsToSearch(p).slice(1))) url.searchParams.set(k, v);
  window.history.pushState({}, "", `${url.pathname}${url.search}${url.hash}`);
  return url.search;
}

/** writeIncidentParam is the other direction: the id REPLACES the four scope
 *  parameters, because the row now answers all of them. Everything else in the
 *  query string (`?at=`) survives, same as writeParams — and it returns the
 *  search it wrote for the same reason. */
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
  "w-full rounded-md bg-surface-2 px-2 py-1.5 text-sm text-foreground placeholder:text-muted-foreground/70 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

function fmtStamp(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

/** See IncidentStrip's `say`. Four seconds: long enough to read six words,
 *  short enough that the line is gone before the next action. */
export const COPY_NOTE_TTL_MS = 4000;

/**
 * SaveIncidentForm is the popover behind "Save as incident".
 *
 * It asks for a TITLE and nothing else that the page already knows: the scope
 * and the range come from the investigation on screen, unedited, because an
 * incident whose window differs from the one its author was reading is an
 * incident that frames the wrong minutes. The two are shown, not offered.
 */
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
  const [title, setTitle] = useState("");
  const [notes, setNotes] = useState("");
  /* The in-flight guard, not just a disabled look (QA round 5, finding #17):
     begin() is a REF write, so three clicks in one task produce one incident
     rather than three. hooks/use-submit-guard.ts says why a useState flag
     cannot do this. */
  const { submitting: busy, begin, end } = useSubmitGuard();
  const [error, setError] = useState<string>();
  /* Focus goes to the field that is wrong (QA round 3, finding #22, and the
     contract components/annotations.tsx's focusField already keeps): a message
     under a form the reader may have scrolled past is a message nobody sees. */
  const titleRef = useRef<HTMLInputElement>(null);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const t = title.trim();
    if (t === "") {
      setError("A title is required.");
      titleRef.current?.focus();
      return;
    }
    setError(undefined);
    if (!begin()) return;
    try {
      await onCreate(t, notes.trim());
    } catch (err) {
      setError(queryErrorMessage(err, "Failed to save the incident"));
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
      <form role="form" aria-label="Save as incident" onSubmit={handleSubmit} className="flex flex-col gap-3">
        <p className="text-xs text-muted-foreground">
          Scope <span className="font-medium text-foreground">{scopeText === "" ? "global" : scopeText}</span> ·{" "}
          {from.toLocaleString()} → {to.toLocaleString()} — taken from this investigation, not editable here.
        </p>
        {/* The one lossy case, said out loud rather than discovered on reopen:
            the incident scope vocabulary has no zone-pair member, so both wide
            scopes store "" and reopen framed on the whole cluster. */}
        {wide ? (
          <p className="text-[11px] leading-relaxed text-muted-foreground">
            A zone pair and the whole cluster both save as the GLOBAL scope — that vocabulary has no zone member — so
            reopening this incident frames the cluster. The range is kept exactly.
          </p>
        ) : null}
        <label className="flex flex-col gap-1 text-[13px]">
          <span className="text-muted-foreground">Title</span>
          <input
            ref={titleRef}
            aria-label="Incident title"
            value={title}
            maxLength={INCIDENT_TITLE_MAX}
            onChange={(e) => setTitle(e.target.value)}
            placeholder="Packet loss between node-a and node-b"
            className={INPUT_CLASS}
          />
        </label>
        <label className="flex flex-col gap-1 text-[13px]">
          <span className="text-muted-foreground">Notes (optional)</span>
          <textarea
            aria-label="Incident notes"
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
            Create incident
          </Button>
          <Button type="button" size="sm" variant="outline" onClick={onCancel}>
            Cancel
          </Button>
        </div>
      </form>
    </Card>
  );
}

/**
 * IncidentStrip is the header of an investigation that has been SAVED: what it
 * is called, whether it is still open, who opened it and when — plus the three
 * writes an incident evolves through (resolve/reopen, notes, and the pinned
 * list the timeline toggles into).
 *
 * Every write is a PATCH of exactly the field it changes. That is not a
 * micro-optimisation: an incident is worked on by several people at once, and
 * a full-replace PUT would let whoever saves last silently discard the notes
 * somebody else typed thirty seconds ago.
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
  const guard = useWriteGuard();
  const [notes, setNotes] = useState(incident.notes);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [copyNote, setCopyNote] = useState<string>();
  /* Second-click confirm, the idiom every destructive control in this console
     uses (QA round 2, finding #14). An incident is somebody's written record of
     an outage — the notes, the pinned findings, the permalink other people have
     in a channel — and DELETE is the only irreversible act on this page. */
  const [confirmingDelete, setConfirmingDelete] = useState(false);
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
        setError(queryErrorMessage(err, "Failed to update the incident"));
      } finally {
        setBusy(false);
      }
    },
    [incident.id, onPatched],
  );

  /**
   * COPY_NOTE_TTL_MS — how long "Permalink copied." stays on screen (QA round 3,
   * finding #22).
   *
   * A confirmation of a completed one-shot act is only true for the moment
   * after it: left up, it starts describing a click made ten minutes and three
   * scope changes ago, and the next reader has to work out whether it means
   * THIS URL. The two FAILURE messages are not timed out — they name a state of
   * the browser that has not gone away, and they carry the fallback ("it is in
   * the address bar") an operator may still be acting on. */
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
      say("This browser gave the page no clipboard — the permalink is in the address bar.", false);
      return;
    }
    try {
      await clipboard.writeText(href);
      say("Permalink copied.", true);
    } catch {
      say("The browser refused the copy — the permalink is in the address bar.", false);
    }
  }, [say]);

  const handleDelete = useCallback(async () => {
    setBusy(true);
    setError(undefined);
    try {
      await deleteIncident(incident.id);
      onDeleted();
    } catch (err) {
      setError(queryErrorMessage(err, "Failed to delete the incident"));
      setBusy(false);
      setConfirmingDelete(false);
    }
  }, [incident.id, onDeleted]);

  return (
    <Card asChild className="border-l-4 border-l-primary p-5">
      <section aria-label="Incident">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          <h2 className="text-sm font-semibold">{incident.title}</h2>
          <Badge variant={resolved ? "neutral" : "warn"} dot>
            {resolved ? "Resolved" : "Open"}
          </Badge>
          <span className="text-xs text-muted-foreground">
            opened by {incident.createdBy} · {fmtStamp(incident.createdAt)}
            {incident.resolvedAt ? ` · resolved ${fmtStamp(incident.resolvedAt)}` : ""}
          </span>
          <div className="ml-auto flex flex-wrap items-center gap-2">
            <Button type="button" size="sm" variant="outline" onClick={() => void copyPermalink()}>
              Copy permalink
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
                {resolved ? "Reopen" : "Resolve"}
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
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    loading={busy}
                    {...guard}
                    disabled={writesDisabled}
                    aria-label={`Confirm delete incident: ${incident.title}`}
                    onClick={() => void handleDelete()}
                  >
                    Confirm delete
                  </Button>
                  <Button type="button" size="sm" variant="ghost" onClick={() => setConfirmingDelete(false)}>
                    Cancel
                  </Button>
                </>
              ) : (
                <Button
                  type="button"
                  size="sm"
                  variant="ghost"
                  {...guard}
                  disabled={writesDisabled}
                  aria-label={`Delete incident: ${incident.title}`}
                  onClick={() => setConfirmingDelete(true)}
                >
                  Delete
                </Button>
              )
            ) : null}
          </div>
        </div>

        <p className="mt-2 text-[11px] leading-relaxed text-muted-foreground">
          This incident&apos;s own scope is{" "}
          <span className="font-medium text-foreground">{incident.scope === "" ? "global" : incident.scope}</span> — the
          row, not the URL, decides what this page frames.
          {targetsGated
            ? " Without targets:read a saved target name cannot be told apart from a node name, so it reopens as a node scope."
            : ""}
        </p>

        {copyNote ? (
          <p role="status" className="mt-2 text-xs text-muted-foreground">
            {copyNote}
          </p>
        ) : null}

        <div className="mt-4">
          <h3 className="text-xs font-semibold uppercase tracking-[0.07em] text-muted-foreground">Notes</h3>
          {canWrite ? (
            <>
              <textarea
                aria-label="Incident notes"
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
                  Save notes
                </Button>
                <span className="nums text-[11px] text-muted-foreground">
                  {notes.length}/{INCIDENT_NOTES_MAX}
                </span>
              </div>
            </>
          ) : (
            <p className="mt-2 whitespace-pre-wrap text-xs leading-relaxed text-muted-foreground">
              {incident.notes === "" ? "No notes. Writing them needs incidents:write." : incident.notes}
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
 * PinnedFindings is the shortlist an operator builds out of the timeline: the
 * rows that actually explain the incident, each with a line saying why.
 *
 * The note is edited LOCALLY and saved with one button, unlike the pin toggle
 * which writes immediately — a PATCH per keystroke would be a write storm, and
 * a note half-typed is not yet a fact about the incident. Both paths send the
 * WHOLE array, because that is what the API replaces (there is no add/remove,
 * and a server-side merge is exactly what two operators pinning at once would
 * race on).
 */
function PinnedFindings({
  pinned,
  canWrite,
  writesDisabled,
  busy,
  dirty,
  onNote,
  onRemove,
  onSave,
}: {
  pinned: PinnedRef[];
  canWrite: boolean;
  writesDisabled: boolean;
  busy: boolean;
  dirty: boolean;
  onNote: (index: number, note: string) => void;
  onRemove: (index: number) => void;
  onSave: () => void;
}) {
  /* Which row is asking "are you sure?", by pinKey rather than by index — the
     list is re-keyed by every save, and an index would move the confirm onto a
     different finding under the operator's cursor. */
  const [confirming, setConfirming] = useState<string | null>(null);

  return (
    <Card asChild className="p-5">
      <section aria-label="Pinned findings">
        <h3 className="text-sm font-semibold">Pinned findings</h3>
        {pinned.length === 0 ? (
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
            Nothing pinned yet. {canWrite ? "The pin on a timeline row adds it here." : "Pinning needs incidents:write."}{" "}
            {/* All THREE unpinnable classes, named (QA round 3, finding #19).
                The sentence used to list two and stop, so an operator hunting
                for the missing pin control on a firing-alert row was left to
                conclude the console was broken. An alert lives in Prometheus,
                not in any table this console owns, and its id is a fingerprint
                of a label set that stops existing the moment it resolves —
                PIN_KIND_BY_TIMELINE_KIND is the authority and says so. */}
            Maintenance windows, threshold crossings and firing alerts cannot be pinned at all — the stored vocabulary
            has no kind for a declared window, a threshold row is derived from a query rather than being a row anywhere,
            and an alert lives in Prometheus rather than in this console, keyed by a label set that disappears when it
            resolves.
          </p>
        ) : (
          <ul className="mt-3 flex flex-col gap-2">
            {pinned.map((p, i) => (
              <li key={pinKey(p)} data-testid="pinned-finding" className="flex flex-wrap items-center gap-2 text-xs">
                <Badge variant="neutral">{p.kind}</Badge>
                <span className="nums max-w-[12rem] truncate text-muted-foreground" title={p.id}>
                  {p.id}
                </span>
                {canWrite ? (
                  <>
                    <input
                      aria-label={`Note for ${p.kind} ${p.id}`}
                      value={p.note ?? ""}
                      maxLength={PIN_NOTE_MAX}
                      placeholder="why this matters"
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
                        <Button
                          type="button"
                          size="sm"
                          variant="outline"
                          disabled={writesDisabled || busy}
                          aria-label={`Confirm unpin ${p.kind} ${p.id} and discard its note`}
                          onClick={() => {
                            setConfirming(null);
                            onRemove(i);
                          }}
                        >
                          Discard note &amp; unpin
                        </Button>
                        <Button type="button" size="sm" variant="ghost" onClick={() => setConfirming(null)}>
                          Cancel
                        </Button>
                      </>
                    ) : (
                      <Button
                        type="button"
                        size="sm"
                        variant="ghost"
                        disabled={writesDisabled || busy}
                        aria-label={`Unpin ${p.kind} ${p.id}`}
                        onClick={() => ((p.note ?? "") === "" ? onRemove(i) : setConfirming(pinKey(p)))}
                      >
                        Unpin
                      </Button>
                    )}
                  </>
                ) : (
                  <span className="min-w-0 flex-1 break-words text-muted-foreground">{p.note ?? ""}</span>
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
            Save pin notes
          </Button>
        ) : null}
      </section>
    </Card>
  );
}

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
  return (
    <label className="flex flex-col gap-1 text-[13px]">
      <span className="text-muted-foreground">{label}</span>
      <select
        aria-label={label}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className={`${CONTROL_CLASS} min-w-[10rem]`}
      >
        <option value="">—</option>
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
  const { me, can } = useAuth();
  const qc = useQueryClient();
  const writesDisabled = useWritesDisabled();
  const guard = useWriteGuard();
  /* The viewed instant itself, not just the boolean: it CLAMPS the committed
     window (QA round 3, finding #3) and it decides whether the alert source is
     asked anything at all. */
  const { at } = useTimeContext();
  const { available: dbAvailable, resolved: dbResolved } = useDatabaseAvailable();
  const { data: config } = useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });
  const promConfigured = config?.prometheus.configured ?? false;

  /* The URL is read at mount AND re-read whenever it changes under the page
     (QA round 3, finding #10 — see lib/location.ts for why popstate alone is
     not enough). It is not re-read on every render: that would make the page
     fight its own pushState. `?incident=` hydration (Task 8) feeds this same
     setter. */
  const [params, setParams] = useState<InvestigationParams>(() =>
    parseInvestigationParams(window.location.search, new Date()),
  );
  const [cursorAt, setCursorAt] = useState<Date | null>(null);
  const [runError, setRunError] = useState<string>();
  const [runStarted, setRunStarted] = useState<string>();
  /* Why the last commit was refused, and whether it moved an edge — both are
     statements about the CURRENT params, so both are cleared by the next
     successful commit (findings #2, #3 and #6). */
  const [commitError, setCommitError] = useState<string>();
  const [clamped, setClamped] = useState(false);

  /* Incident mode. The id is read from the URL once, exactly like the four
     scope parameters, and is thereafter owned by this state: saving sets it,
     applying the form clears it. */
  const [incidentId, setIncidentId] = useState<string | null>(
    () => new URLSearchParams(window.location.search).get("incident"),
  );
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
  /* Both lists come from the NODES AND THE AGENTS (QA round 3, finding #5).
     `topology.nodes` is the controller's view and is empty on a console with
     no controller wired — which is exactly the console that still has agents
     reporting and metrics worth investigating, and where every select was
     empty and every scope but `cluster` unreachable. */
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

  /* ── the URL is re-read when it changes (QA round 3, finding #10) ──
     The search string this page last APPLIED, whether it read it or wrote it.
     Everything below compares against it, which is what makes the page's own
     pushState a no-op here: without that, writeParams would fire the listener,
     the listener would re-parse and setParams, and an incident save would be
     re-read as a plain URL a beat after it wrote the id. */
  const appliedSearchRef = useRef<string>(window.location.search);
  /* The id currently in state and the id already hydrated, as refs, so the
     subscription can compare against them without re-subscribing on every
     change. hydratedRef is also what the `?incident=` effect below reads. */
  const incidentIdRef = useRef<string | null>(incidentId);
  incidentIdRef.current = incidentId;
  const hydratedRef = useRef<string | null>(null);

  useEffect(
    () =>
      subscribeToLocation(() => {
        const search = window.location.search;
        if (search === appliedSearchRef.current) return;
        appliedSearchRef.current = search;
        const qs = new URLSearchParams(search);
        /* An incident id in the new URL takes over exactly as it does at
           mount; its absence LEAVES incident mode, because the URL no longer
           claims to be one. hydratedRef is cleared so re-entering the same
           incident later hydrates again. */
        const nextIncident = qs.get("incident");
        if (nextIncident !== incidentIdRef.current) hydratedRef.current = null;
        setIncidentId(nextIncident);
        /* Bare /investigate (no parameters at all) resolves, through
           parseInvestigationParams' own total degradation, to the cluster over
           the last hour — which IS the entry form's default state. So "reset to
           the entry form" needs no special case here: it is what the parser
           already answers for an empty query string. */
        const next = parseInvestigationParams(search, new Date());
        setParams(next);
        setDraftKind(next.kind);
        setDraftA(next.a);
        setDraftB(next.b);
        setPreset(presetForSpan(next.from, next.to));
        setCustomFrom(next.from);
        setCustomTo(next.to);
        setCursorAt(null);
        setSaveOpen(false);
        setCommitError(undefined);
        setClamped(false);
      }),
    [],
  );

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

  /* ── incident mode: the saved row hydrates the page (Decision 7) ──
     One read, behind incidents:read like every other source, and the ONLY one
     whose result rewrites `params` rather than adding rows to the timeline. */
  const canIncidentsRead = can("incidents:read");
  const canIncidentsWrite = can("incidents:write");
  const incidentQuery = useQuery({
    queryKey: ["incident", incidentId],
    queryFn: () => getIncident(incidentId as string),
    enabled: incidentId !== null && dbReady && canIncidentsRead,
    retry: false,
  });
  const incident = incidentQuery.data;

  /* The target list decides whether a bare saved scope is a node or a target
     (scopeFromIncidentScope), so hydration WAITS for it rather than racing it:
     hydrating early would frame a target incident on the peer metric family,
     which answers an empty series rather than a wrong number — an outage that
     looks like silence. */
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
    setCursorAt(null);
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
        setPinError(queryErrorMessage(err, "Failed to save the pinned findings"));
        setPinned(incident.pinned);
        setPinDirty(false);
      } finally {
        setPinBusy(false);
      }
    },
    [incident, onIncidentPatched],
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

  /* ── source 4: MTR path changes (mtr:read) ──
     The endpoint REQUIRES both a source and a destination (422 otherwise), so
     the scope decides whether there is a request to make at all. A pair names
     both; a node and a target name one, and the destinations index supplies the
     other side (bounded by MTR_FANOUT); a zone pair and the cluster name
     neither, and the pane says so rather than firing a doomed request. */
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
      const pairs = all.destinations
        .filter((d) => (mtrMode === "by-source" ? d.sourceNode === scope.a : d.destination === scope.a))
        .slice(0, MTR_FANOUT);
      const pages = await Promise.all(
        pairs.map((p) => getMTRSnapshots({ source: p.sourceNode, destination: p.destination, limit: 20 })),
      );
      return pages.flatMap((p) => p.snapshots);
    },
    enabled: dbReady && canMTR && mtrMode !== "none",
  });

  /* ── source 5: diagnostic runs (runs:read) ──
     Two steps, the pages/target-card.tsx precedent: one list request, then a
     detail request per run whose createdAt already falls inside the window —
     the spec (and therefore the scope) is only in the detail body, and
     narrowing by time FIRST keeps the fan-out to the runs that could matter. */
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

  /* ── source 6: K8s events (events:read) ──
     A PAIR asks for BOTH nodes, one name-filtered request each, merged: the
     `name` filter is an exact match, so "what did the cluster do to either end
     of this pair" is genuinely two requests. Everything wider than a node asks
     for the unfiltered window — a target and a zone pair have no node NAME to
     filter by, and inventing one would silently return nothing. */
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
  const lossQuery = useQuery({
    queryKey: ["investigate", "loss", key],
    queryFn: () => promqlQueryRange(investigationLossQuery(scope), params.from, params.to, stepNs),
    enabled: signalsEnabled,
  });
  const rttQuery = useQuery({
    queryKey: ["investigate", "rtt", key],
    queryFn: () => promqlQueryRange(investigationRttQuery(scope), params.from, params.to, stepNs),
    enabled: signalsEnabled,
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

  /* ── source 9: firing alerts (alerts:read + Prometheus) ──
     NOT store-backed and therefore not behind dbReady: the firing set lives in
     Prometheus. The request is skipped entirely when Prometheus is not
     configured — the route would answer 200 with promConfigured:false, and the
     note below already says that without spending a round trip on it.

     There is no window here to ask for: /api/v1/alerts serves CURRENT state
     and no alert history endpoint exists. alertEntries does the placing, and
     the note states the consequence.

     ENGAGED, THIS SOURCE ASKS FOR NOTHING (QA round 3, finding #3). Every other
     source on this page anchors naturally through from/to, which the clamp
     keeps at or before the viewed instant. This one cannot: the firing set is
     Prometheus's NOW, with no history behind it, so at `t` the only two things
     it could do are lie — place today's firing alerts on a window that closed
     last Tuesday — or say so. It says so, in the same sentence pages/
     overview.tsx's Firing alerts card uses, and the request is not made at all
     rather than made and discarded. */
  const canAlerts = can("alerts:read");
  const engaged = at !== null;
  const alertsQuery = useQuery({
    queryKey: ["investigate", "alerts"],
    queryFn: listAlerts,
    enabled: ready && canAlerts && promConfigured && !engaged,
  });
  const firingAlerts = useMemo(() => alertsQuery.data?.alerts ?? [], [alertsQuery.data]);

  /* ── assembly ── */
  const entries = useMemo(
    () =>
      mergeTimeline(
        eventEntries(eventsQuery.data?.events ?? []),
        auditEntries(auditQuery.data?.entries ?? [], params.from, params.to),
        annotationEntries(annotations),
        pathChangeEntries(snapshotsQuery.data ?? [], params.from, params.to),
        runEntries(scopedRuns),
        k8sEntries(k8sQuery.data ?? []),
        maintenanceEntries(windows),
        thresholdCrossings(samples),
        alertEntries(firingAlerts, params.from, params.to),
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
      firingAlerts,
      params.from,
      params.to,
    ],
  );

  const onset = useMemo(() => anomalyOnset(entries), [entries]);
  const causes = useMemo(
    () => (onset === null ? [] : rankCauses(entries, onset).slice(0, CAUSE_TOP_N)),
    [entries, onset],
  );

  /* Every source that was ASKED and did not answer, named the way the source
     list names it (QA round 3, finding #1). A source that was never requested
     is absent from here by construction: react-query holds no error for a
     disabled query, and "you may not read this" already has its own line. */
  const failedSources = useMemo<{ id: string; label: string; error: unknown }[]>(
    () =>
      [
        { id: "events", label: "Events", error: eventsQuery.error },
        { id: "audit", label: "Config changes", error: auditQuery.error },
        { id: "annotations", label: "Annotations", error: annotationsQuery.error },
        { id: "snapshots", label: "Path changes", error: snapshotsQuery.error },
        { id: "runs", label: "Diagnostic runs", error: runsQuery.error ?? runDetailsQuery.error },
        { id: "k8s", label: "Cluster events", error: k8sQuery.error },
        { id: "maintenance", label: "Maintenance windows", error: maintenanceQuery.error },
        { id: "loss", label: "Packet loss series", error: lossQuery.error },
        { id: "rtt", label: "RTT series", error: rttQuery.error },
        { id: "alerts", label: "Firing alerts", error: alertsQuery.error },
      ].filter((s) => s.error !== null && s.error !== undefined),
    [
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
      alertsQuery.error,
    ],
  );

  /* ── the source list: one honest line per absent or bounded source ── */
  const notes = useMemo<SourceNote[]>(() => {
    const out: SourceNote[] = [];
    if (dbResolved && !dbAvailable) {
      out.push({
        id: "database",
        text: "Events, audit, annotations, path history, runs, cluster events and maintenance are all stored — set console.database.mode. None of them was requested.",
      });
    }
    if (!canEvents) {
      out.push({
        id: "events",
        text: "Fleet events and Kubernetes events need events:read — neither was requested.",
      });
    }
    if (!canAudit) {
      /* "Audit rows", not "config changes" (QA round 5, finding #19): the
         source is the audit log, and most of what it records is a READ
         decision, not a change. The timeline badge above was corrected the
         same way — this note names the same rows, so it has to agree. */
      out.push({ id: "audit", text: "Audit rows need audit:read — the audit log was not requested." });
    } else {
      out.push({
        id: "audit-window",
        text: `Config changes come from the newest ${AUDIT_SCAN_LIMIT} audit rows filtered to this window here: GET /api/v1/audit has no time filter, so a very busy console can push older in-range rows off that page.`,
      });
    }
    if (!canAnnotations) {
      out.push({ id: "annotations", text: "Annotations need annotations:read — no note was requested." });
    }
    if (!canMTR) {
      out.push({ id: "mtr", text: "Path changes need mtr:read — no MTR snapshot was requested." });
    } else if (mtrMode === "none") {
      out.push({
        id: "mtr-scope",
        text: "Path history needs a pair, a node or a target: GET /api/v1/mtr/snapshots requires both a source and a destination, and a zone pair or the whole cluster names neither. Nothing was requested.",
      });
    } else if (mtrMode !== "pair") {
      out.push({
        id: "mtr-fanout",
        text: `Path changes cover the ${MTR_FANOUT} most recently traced pairs touching this scope — the snapshots endpoint is per pair, and a whole node's fan-out is not a page's worth of requests.`,
      });
    }
    if (!canRuns) {
      out.push({ id: "runs", text: "Diagnostic runs need runs:read — no run history was requested." });
    } else {
      out.push({
        id: "runs-scan",
        text: `Runs are the newest ${RUN_SCAN_LIMIT}, narrowed to this window and then to this scope by their spec — GET /api/v1/runs has no scope filter.`,
      });
    }
    if (!canMaintenance) {
      out.push({ id: "maintenance", text: "Maintenance windows need maintenance:read — none was requested." });
    }
    if (!canPromQL) {
      out.push({
        id: "promql",
        text: "Threshold crossings need promql:query — the scope's loss and RTT series were not requested, so the timeline carries no derived rows.",
      });
    } else if (!promConfigured) {
      out.push({
        id: "promql-config",
        text: "Threshold crossings read Prometheus — set console.prometheus.address. Nothing was requested.",
      });
    }
    if (!canAlerts) {
      out.push({ id: "alerts", text: "Firing alerts need alerts:read — no alert state was requested." });
    } else if (engaged) {
      /* The honest live-only caption, word for word the one pages/overview.tsx
         gives (QA round 3, finding #3). */
      out.push({
        id: "alerts-live-only",
        text: "Alert state is a live-only signal — Prometheus keeps no firing history here. Nothing was requested for this instant.",
      });
    } else if (!promConfigured) {
      out.push({
        id: "alerts-config",
        text: "Firing alerts read Prometheus — set console.prometheus.address. Nothing was requested.",
      });
    } else {
      out.push({
        id: "alerts-now",
        text: "Alerts are the set firing NOW: a row at activeAt for each one that started inside this window, and a row at the window's start for each one that was already firing. Resolutions are not recorded; only what is firing now is visible.",
      });
    }

    /* ── one line PER FAILED SOURCE (QA round 3, finding #1) ──
       These used to collapse into a single "One of the timeline's sources is
       unavailable" card carrying whichever error `??` reached first, so a
       console where events AND k8s were both 500ing reported one problem and
       silently dropped the other — and the timeline underneath went on
       claiming nothing happened. Each source now says its own name and the
       server's own detail, and every one of them counts towards the partial
       banner the timeline draws from `failed`. */
    for (const source of failedSources) {
      out.push({
        id: `failed-${source.id}`,
        text: `${source.label}: ${queryErrorMessage(source.error, "the request failed.")}`,
        failed: true,
      });
    }
    return out;
  }, [
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
  ]);

  const loading =
    eventsQuery.isLoading ||
    auditQuery.isLoading ||
    annotationsQuery.isLoading ||
    snapshotsQuery.isLoading ||
    k8sQuery.isLoading ||
    maintenanceQuery.isLoading ||
    lossQuery.isLoading ||
    alertsQuery.isLoading;

  /* ── the entry form ── */

  /* The draft's own completeness, recomputed on every keystroke of the selects
     so the button and the reason under it never disagree (finding #6). */
  const draftScope: InvestigationScope = useMemo(
    () => ({ kind: draftKind, a: draftA, b: draftB }),
    [draftKind, draftA, draftB],
  );
  const incompleteReason = useMemo(() => scopeIncompleteReason(draftScope), [draftScope]);

  const apply = useCallback(() => {
    if (incompleteReason !== null) return;
    const now = new Date();
    const chosen = RANGE_PRESETS.find((p) => p.value === preset);
    const rawFrom =
      preset === "custom" ? customFrom : new Date(now.getTime() - (chosen?.seconds ?? DEFAULT_RANGE_SECONDS) * 1000);
    const rawTo = preset === "custom" ? customTo : now;

    /* Time Machine contract for this page: engaging at `t` clamps a committed
       window's `to` down to `t` (a window entirely after `t` is refused
       outright), the header states "Window clamped to the viewed instant."
       when that moved anything, and the firing-alerts source issues ZERO
       requests and says so — every other source anchors through from/to and
       needs no special case. */
    const commit = commitWindow(rawFrom, rawTo, at);
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
    setCursorAt(null);
    /* Leaving incident mode: see writeParams' own comment. hydratedRef is left
       alone deliberately — re-entering the SAME incident later must hydrate
       again, and clearing the id is what makes the next mount do that. */
    setIncidentId(null);
    setSaveOpen(false);
  }, [draftKind, draftA, draftB, preset, customFrom, customTo, at, incompleteReason]);

  /* Save-as-incident sends the CURRENT scope and range verbatim: the incident
     is the investigation on screen, given a name. The 201 body seeds the same
     query key `?incident=` would read, so entering incident mode costs no
     second request — and reloading the permalink lands on the identical view. */
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

  /**
   * onIncidentDeleted lands the page back on the bare entry form (QA round 3,
   * finding #21).
   *
   * Staying put was not an option: the row is gone, so the strip, the pinned
   * findings and the permalink in the address bar would all be describing
   * something that no longer exists, and a reload would render the 404 card.
   * `?at=` is the one parameter kept — the Time Machine is a property of the
   * whole console, not of this investigation, and dropping it here would
   * silently return an operator to Live.
   *
   * The pushState is what resets the page: lib/location.ts's subscription
   * above re-reads the URL, and an empty query string is exactly the entry
   * form's default.
   */
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
        setRunError(queryErrorMessage(err, "Failed to start the run"));
      }
    },
    [runSources, runDestinations, scope.kind, targetIdForScope],
  );

  const refreshAnnotations = useCallback(() => {
    void qc.invalidateQueries({ queryKey: ["investigate", "annotations"] });
  }, [qc]);

  /* The rail's own bar writes, so it has to re-read what THIS page fetched —
     the ["investigate","maintenance"] leg, not the shared ["maintenance"] key
     the standalone hook owns. Both are invalidated: a card open in another tab
     is not this page's problem, but the moment one is, it should not show a
     window that has been deleted here. */
  const refreshMaintenance = useCallback(() => {
    void qc.invalidateQueries({ queryKey: ["investigate", "maintenance"] });
    void qc.invalidateQueries({ queryKey: ["maintenance"] });
  }, [qc]);

  return (
    <PageShell
      title="Investigate"
      description="One window over one scope: every source the console can read, merged into a timeline, with the correlation rules written down rather than guessed at."
      actions={
        <>
          <Badge variant="neutral">{params.kind}</Badge>
          <span className="nums text-sm text-muted-foreground">{scopeHeadline(scope)}</span>
        </>
      }
    >
      {/* ── entry form ── */}
      <Card asChild className="p-5">
        <section aria-label="Investigation scope">
          <div className="flex flex-wrap items-end gap-4">
            <div className="flex flex-col gap-1 text-[13px]">
              <span className="text-muted-foreground">Scope</span>
              <Segmented aria-label="Scope kind" options={SCOPE_OPTIONS} value={draftKind} onChange={changeKind} />
            </div>

            {draftKind === "pair" ? (
              <>
                <Select label="Source node" value={draftA} options={nodeNames} onChange={setDraftA} />
                <Select label="Destination node" value={draftB} options={nodeNames} onChange={setDraftB} />
              </>
            ) : null}
            {draftKind === "node" ? <Select label="Node" value={draftA} options={nodeNames} onChange={setDraftA} /> : null}
            {draftKind === "zone-pair" ? (
              <>
                <Select label="Source zone" value={draftA} options={zoneNames} onChange={setDraftA} />
                <Select label="Destination zone" value={draftB} options={zoneNames} onChange={setDraftB} />
              </>
            ) : null}
            {draftKind === "target" ? (
              <Select label="Target" value={draftA} options={targets.map((t) => t.name)} onChange={setDraftA} />
            ) : null}

            <div className="flex flex-col gap-1 text-[13px]">
              <span className="text-muted-foreground">Range</span>
              <Segmented
                aria-label="Range preset"
                options={RANGE_PRESETS.map((p) => ({ value: p.value, label: p.label }))}
                value={preset}
                onChange={(v) => setPreset(v)}
              />
            </div>

            {preset === "custom" ? (
              <div className="flex items-end gap-2">
                <DateTimePicker aria-label="Range start" value={customFrom} onApply={setCustomFrom} />
                <DateTimePicker aria-label="Range end" value={customTo} onApply={setCustomTo} />
              </div>
            ) : null}

            {/* Disabled until the scope NAMES something (finding #6). Not a
                silent no-op click and not a 422 either: an incomplete scope
                commits perfectly well and produces an empty timeline, which is
                indistinguishable from a healthy fleet. */}
            <Button type="button" size="sm" disabled={incompleteReason !== null} onClick={apply}>
              Investigate
            </Button>
          </div>

          {incompleteReason !== null ? (
            <p data-testid="scope-incomplete" className="mt-3 text-xs text-muted-foreground">
              {incompleteReason}
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
              {CLAMPED_BANNER}
            </p>
          ) : null}

          {draftKind === "target" && !canTargets ? (
            <p className="mt-3 text-xs text-muted-foreground">
              The target list needs targets:read. The scope still works from a permalink — the URL carries the target's
              name, not its id.
            </p>
          ) : null}

          <p className="mt-3 text-[11px] leading-relaxed text-muted-foreground">
            Everything above is in the URL (?kind=&amp;scope=&amp;from=&amp;to=) — this page is shareable as it stands,
            which is also what makes an incident permalink free.
          </p>
        </section>
      </Card>

      {/* ── actions rail ── */}
      <Card asChild className="p-4">
        <section aria-label="Actions">
          <div className="flex flex-wrap items-center gap-2">
            {/* Permission HIDES, time DISABLES — lib/timemachine.tsx's
                useWritesDisabled documents the split and this is the
                composition it prescribes. */}
            {canRun ? (
              <>
                <Button type="button" size="sm" variant="outline" {...guard} onClick={() => void startRun("mtr")}>
                  Run MTR now
                </Button>
                <Button type="button" size="sm" variant="outline" {...guard} onClick={() => void startRun("tcp")}>
                  Run TCP now
                </Button>
              </>
            ) : null}

            <a
              href="/explore"
              className="inline-flex h-8 items-center rounded-md border border-border-strong px-3 text-[13px] hover:bg-accent hover:text-accent-foreground"
            >
              Compare in Explore
            </a>

            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={() => downloadJson(`investigation-${params.from.toISOString()}.json`, buildExportPayload(params, entries, causes))}
            >
              Export JSON
            </Button>

            {/* Permission HIDES, time DISABLES, again. An incident cannot be
                opened for a window while the console is pinned to an instant:
                the write would be filed against the present regardless. */}
            {canIncidentsWrite && incident === undefined ? (
              <Button type="button" size="sm" variant="outline" {...guard} onClick={() => setSaveOpen((v) => !v)}>
                Save as incident
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
            /* What the count sentence CALLS the scope (finding #7): the wide
               scopes were queried unfiltered, so "scope global" named the one
               value they were not filtering by. A window CREATED here is still
               filed globally, and the form says so. */
            scopeCaption={scopeCaptionValue(scope)}
            windows={windows}
            error={maintenanceQuery.error as Error | null}
            onChanged={refreshMaintenance}
            /* The committed window is FROZEN here, so a window declared outside
               it will not appear in the list below and the bar has to say so
               (finding #8). */
            frozenWindow={{ from: params.from, to: params.to }}
            createLabel="Create maintenance"
          />

          <p className="mt-2 text-[11px] leading-relaxed text-muted-foreground">
            Explore&apos;s A/B slots are bound to curated metrics and it reads no range from the URL, so that link opens
            the page and the window has to be chosen there — saying so beats a link that quietly drops half of what it
            promised.
          </p>
          {runStarted ? (
            <p role="status" className="mt-2 text-xs text-muted-foreground">
              Run{" "}
              <a href={`/diagnostics/runs/${runStarted}`} className="text-primary hover:underline">
                {runStarted}
              </a>{" "}
              started.
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
          <p className="text-xs leading-relaxed text-muted-foreground">
            This link names an incident, and reading one needs incidents:read — it was not requested. The investigation
            below is the URL&apos;s own scope and range, not the incident&apos;s.
          </p>
        </Card>
      ) : null}

      {incidentQuery.isError ? (
        <Card role="alert" className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-4">
          <p className="text-sm font-medium">No incident matches this link</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
            {/* endSentence, not the bare detail (QA round 5, finding #10). The
                server's problem details are phrases, not sentences — "no
                incident with that id" carries no full stop — so the two ran
                together into "...with that id The page is showing...", which
                reads as one broken sentence rather than two correct ones. */}
            {endSentence(queryErrorMessage(incidentQuery.error, "The incident could not be read"))} The page is showing
            the default investigation instead — an incident can be deleted, and a stale permalink is not an error state
            worth blanking the page for.
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
          cursorAt={cursorAt}
          onCursor={setCursorAt}
          pinning={pinning}
        />

        <div className="flex flex-col gap-5">
          <SignalPanels
            scopeLabel={scopeHeadline(scope)}
            loss={lossQuery.data}
            /* The REJECTION, not just the envelope (finding #2): a refused
               range query left the pane blank, which reads as "still
               loading" forever. */
            lossError={lossQuery.error as Error | null}
            rtt={rttQuery.data}
            rttError={rttQuery.error as Error | null}
            delta={deltaFromVectors(deltaQuery.data?.before, deltaQuery.data?.after)}
            cursorAt={cursorAt}
            windows={windows}
            annotations={annotations}
            promConfigured={promConfigured}
            gated={!canPromQL}
          />

          <Card asChild className="p-5">
            <section aria-label="Correlation">
              <h3 className="text-sm font-semibold">Likely causes</h3>
              {onset === null ? (
                <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
                  No threshold crossing in range — nothing to rank. The onset is the first crossing of loss above 1% or
                  RTT above twice the range median; without one there is no anchor, and inventing an anchor is how these
                  panels start lying.
                </p>
              ) : (
                <>
                  <p className="mt-1 text-[11px] text-muted-foreground">
                    Onset {onset.toLocaleTimeString()} · candidates within {DEFAULT_CAUSE_WINDOW_SECONDS}s before it
                  </p>
                  {causes.length === 0 ? (
                    <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
                      Nothing scoreable happened in the {DEFAULT_CAUSE_WINDOW_SECONDS} seconds before the onset. An
                      empty ranking is an answer.
                    </p>
                  ) : (
                    <ol aria-label="Ranked causes" className="mt-2 flex flex-col gap-2">
                      {causes.map((c) => {
                        const deltaSeconds = Math.round((onset.getTime() - c.entry.at.getTime()) / 1000);
                        const width = Math.max(2, (c.score / Math.max(...Object.values(CAUSE_WEIGHTS))) * 100);
                        return (
                          <li key={`${c.entry.kind}:${c.entry.ref?.id ?? c.entry.at.getTime()}`} className="text-xs">
                            <div className="flex items-baseline gap-2">
                              <span className="nums w-10 shrink-0 text-muted-foreground">{c.score.toFixed(2)}</span>
                              <span className="min-w-0 flex-1 break-words">{c.entry.title}</span>
                            </div>
                            <div className="mt-1 flex items-center gap-2">
                              <span aria-hidden="true" className="h-1 rounded-full bg-primary" style={{ width: `${width}%` }} />
                              <span className="text-[11px] text-muted-foreground">
                                {deltaSeconds}s before the onset · weight {CAUSE_WEIGHTS[c.entry.kind]}
                              </span>
                            </div>
                          </li>
                        );
                      })}
                    </ol>
                  )}
                </>
              )}
              <p className="mt-3 text-[11px] leading-relaxed text-muted-foreground">
                Ranked by temporal proximity; the weights live in the open — no model, four arithmetic steps,
                reproducible by hand from{" "}
                <a href={DOC_LINK} target="_blank" rel="noreferrer" className="text-primary hover:underline">
                  the scoring source
                </a>
                .
              </p>
            </section>
          </Card>

          <Card asChild className="p-5">
            <section aria-label="Notes">
              <h3 className="text-sm font-semibold">Notes on this scope</h3>
              <AnnotationBar
                scope={eventScope}
                /* finding #7 — see the MaintenanceBar above. */
                scopeCaption={scopeCaptionValue(scope)}
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
