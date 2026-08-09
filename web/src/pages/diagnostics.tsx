import { useCallback, useEffect, useId, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { ClipboardList } from "lucide-react";
import { PageShell } from "@/components/page-shell";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { useTopology } from "@/hooks/use-topology";
import { ApiError, createCheck, createRun, getRuns, goTo, listTargets } from "@/lib/api";
import { scopeNodeOptions } from "@/lib/investigation-sources";
import { useTimeContext, useWriteGuard } from "@/lib/timemachine";
import {
  CHECK_TYPES,
  type CheckDefinitionRequest,
  type CheckType,
  type DestinationKind,
  type RunCreateRequest,
  type RunSummary,
} from "@/lib/types";
import { ADHOC_ADDRESS_ERROR, CHECKBOX_CLASS, cn, isValidAdhocAddress, runsAtOrBefore } from "@/lib/utils";
// The 422-detail -> form-field heuristic and its phrase table live with the
// form that first needed them (pages/targets.tsx). They are imported rather
// than re-implemented so the "Save as definition" action below places a
// rejected field exactly where the Definitions tab would place the same
// message -- one table, one behaviour, no second copy to drift.
import { DEFINITION_FIELD_PHRASES, fieldForDetail } from "./targets";

// MAX_PAIRS mirrors checks.maxPairs (internal/console/checks/checks.go) --
// a client-side echo of the server's ErrTooManyPairs 422 so the operator
// sees the guard before submitting, not after. The server remains the only
// real enforcement point; a Plan run through a stale/empty topology can
// still disagree with this estimate (e.g. duplicate names), and that
// disagreement is the server's 422 to raise, not a bug in this preview.
export const MAX_PAIRS = 400;

/**
 * estimatePairCount previews checks.Plan's own pair count: the deduplicated
 * cross product of sources x destinations, minus self-pairs. It is a
 * client-side approximation only -- Plan additionally dedupes exact
 * duplicate (source, destination) pairs after expansion, which this does
 * not need to reproduce since Set already dedupes each side going in.
 *
 * This is a DISPLAY estimate only -- it is not what gates the submit
 * button. See estimateRawPairCount for that.
 */
export function estimatePairCount(sources: string[], destinations: string[]): number {
  const srcSet = new Set(sources);
  const dstSet = new Set(destinations);
  let count = 0;
  for (const s of srcSet) {
    for (const d of dstSet) {
      if (s !== d) count++;
    }
  }
  return count;
}

/**
 * estimateRawPairCount mirrors Plan's OWN gate exactly: `len(sources) *
 * len(destinations)`, computed before self-pair exclusion or dedup. That
 * raw-product guard is the first thing checks.Plan checks (checks.go), and
 * it runs against the raw slices the server receives -- so this must too,
 * rather than against a deduplicated Set the way estimatePairCount is.
 *
 * The two numbers can disagree: 20 sources and 21 destinations sharing 20
 * names have a raw product of 420 (over the limit) but a self-excluded
 * count of exactly 400 (at the limit, self-pairs subtracted one per shared
 * name) -- estimatePairCount alone would wrongly let that selection
 * through submit, only to have the server 422 it with ErrTooManyPairs.
 */
export function estimateRawPairCount(sources: string[], destinations: string[]): number {
  return sources.length * destinations.length;
}

const STATUS_VARIANT: Record<string, NonNullable<BadgeProps["variant"]>> = {
  pending: "neutral",
  running: "neutral",
  succeeded: "ok",
  failed: "bad",
  partial: "warn",
  // An operator's own decision, not a fault -- see run-detail.tsx's twin.
  cancelled: "neutral",
};

/* ── destination kind ────────────────────────────────────────────────────────
   DESTINATION_KIND_LABELS is the run form's destination selector, and it is
   the SAME vocabulary check definitions use (store.DefinitionInput's
   destinationKind), on purpose: an operator who has written a definition
   already knows this shape.

   "node" is the default and the pre-M4 contract -- and a node run sends NO
   destination* field at all, not an explicit "node", so the body an M3
   console produced and the body this form produces for a node run are the
   same bytes (resolveRunDestination in internal/console/httpapi/runs.go
   treats "" and "node" identically). */
export const DESTINATION_KIND_LABELS: Record<DestinationKind, string> = {
  node: "Nodes",
  target: "Target",
  adhoc: "Ad-hoc",
};

function StatusBadge({ status }: { status: string }) {
  return (
    <Badge variant={STATUS_VARIANT[status] ?? "unknown"} dot>
      {status}
    </Badge>
  );
}

/* NodeSelector, toggleName, FieldLabel, CONTROL_CLASS and
   DESTINATION_KIND_LABELS below are EXPORTED, not because this page needs them
   to be, but because M5 Task 8's MTR Runner builds the same body against the
   same endpoint from pages/mtr.tsx. Exporting the pieces rather than copying
   them is the same call this file already made when it imported targets.tsx's
   422-detail table: one run form's vocabulary, one set of controls, and a
   change to the destination kinds cannot land on one page and not the other.
   Nothing here changed shape — the exports are purely additive. */
export function NodeSelector({
  label,
  nodes,
  all,
  onAllChange,
  selected,
  onToggle,
}: {
  label: string;
  nodes: string[];
  all: boolean;
  onAllChange: (v: boolean) => void;
  selected: string[];
  onToggle: (name: string) => void;
}) {
  /* EXPLICIT htmlFor/id association, not a wrapping <label> (QA round 4,
     finding #16). The nesting was doing the job in the DOM, but it made every
     checkbox's name a property of where it sits rather than of what it is: the
     `truncate` span it depended on is a presentational choice, and a control
     whose accessible name can be lost by a styling edit has no name. Two
     columns of these render at once (Sources and Destinations), so the id is
     seeded per-selector — one useId, one name suffix per node — and the
     fieldset's legend is what tells the two columns' "node-a" apart. */
  const groupId = useId();
  const nodeInputId = (n: string) => `${groupId}-node-${n}`;
  return (
    <fieldset className="rounded-md border border-border p-3">
      <legend className="px-1 text-xs font-medium text-muted-foreground">{label}</legend>
      <label htmlFor={`${groupId}-all`} className="flex items-center gap-2 text-sm">
        <input
          id={`${groupId}-all`}
          type="checkbox"
          checked={all}
          onChange={(e) => onAllChange(e.target.checked)}
          className={CHECKBOX_CLASS}
        />
        All nodes ({nodes.length})
      </label>
      {!all ? (
        <div className="mt-2 flex max-h-40 flex-col gap-1 overflow-y-auto">
          {nodes.length === 0 ? (
            <p className="text-xs text-muted-foreground">No nodes reported by the controller yet.</p>
          ) : (
            nodes.map((n) => (
              <label key={n} htmlFor={nodeInputId(n)} className="flex items-center gap-2 text-sm">
                <input
                  id={nodeInputId(n)}
                  type="checkbox"
                  checked={selected.includes(n)}
                  onChange={() => onToggle(n)}
                  className={CHECKBOX_CLASS}
                />
                <span className="truncate">{n}</span>
              </label>
            ))
          )}
        </div>
      ) : null}
    </fieldset>
  );
}

export function toggleName(list: string[], name: string): string[] {
  return list.includes(name) ? list.filter((n) => n !== name) : [...list, name];
}

/**
 * buildRunRequest is POST /api/v1/runs's body, in ONE place so the node case
 * cannot drift.
 *
 * The node case is a deliberate regression surface: it serialises exactly the
 * four keys it always has (type, plane, sources, destinations) and NOT a
 * destinationKind:"node" — the pre-M4 body, unchanged, for the path that is
 * still the overwhelming majority of runs.
 *
 * Both external kinds send `destinations: []`, because the server refuses a
 * body that names both an external destination and node-name destinations
 * with a 400 ("one run probes either the mesh or one external destination,
 * never a mix") rather than picking one.
 */
export function buildRunRequest(input: {
  type: CheckType;
  sources: string[];
  destinations: string[];
  destinationKind: DestinationKind;
  destinationTargetId: string;
  destinationAddress: string;
}): RunCreateRequest {
  const req: RunCreateRequest = {
    type: input.type,
    plane: "pod",
    sources: input.sources,
    destinations: input.destinationKind === "node" ? input.destinations : [],
  };
  if (input.destinationKind === "target") {
    req.destinationKind = "target";
    req.destinationTargetId = input.destinationTargetId;
  } else if (input.destinationKind === "adhoc") {
    req.destinationKind = "adhoc";
    req.destinationAddress = input.destinationAddress;
  }
  return req;
}

export function FieldLabel({ label, children }: { label: string; children: (id: string) => ReactNode }) {
  const id = useId();
  return (
    <div className="flex flex-col gap-1 text-[13px]">
      <label htmlFor={id} className="text-muted-foreground">
        {label}
      </label>
      {children(id)}
    </div>
  );
}

export const CONTROL_CLASS = "h-9 rounded-md border border-border-strong bg-transparent px-3 text-[13px]";

function RunForm({
  nodeNames,
  canReadTargets,
  canWriteChecks,
}: {
  nodeNames: string[];
  canReadTargets: boolean;
  canWriteChecks: boolean;
}) {
  const [type, setType] = useState<CheckType>("tcp");
  const [sourcesAll, setSourcesAll] = useState(true);
  const [destinationsAll, setDestinationsAll] = useState(true);
  const [sources, setSources] = useState<string[]>([]);
  const [destinations, setDestinations] = useState<string[]>([]);
  const [destinationKind, setDestinationKind] = useState<DestinationKind>("node");
  const [destinationTargetId, setDestinationTargetId] = useState("");
  const [destinationAddress, setDestinationAddress] = useState("");
  /* The in-flight guard, not just a disabled look (QA round 5, finding #17):
     begin() is a REF write, so three clicks in one task produce one request.
     hooks/use-submit-guard.ts says why a useState flag cannot do this. */
  const { submitting, begin, end } = useSubmitGuard();
  const [submitError, setSubmitError] = useState<string>();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's
     useWriteGuard (QA round 2, finding #18; extended here in round 3). Spread it
     onto the control, and compose any local condition AFTER the spread. */
  const guard = useWriteGuard();
  const writesDisabled = guard.disabled;

  /* The target picker's options. Fetched ONLY once "Target" is actually
     selected, and only with targets:read — GET /api/v1/targets is gated on
     that permission, so asking for it without one would be a guaranteed 403
     (same reasoning as targets.tsx's projection call). Shares the ["targets"]
     cache entry with the Targets page rather than inventing a second notion
     of "the targets". */
  const targetsQuery = useQuery({
    queryKey: ["targets"],
    queryFn: () => listTargets(),
    enabled: canReadTargets && destinationKind === "target",
  });
  const targets = targetsQuery.data?.targets ?? [];

  const external = destinationKind !== "node";
  const resolvedSources = sourcesAll ? nodeNames : sources;
  const resolvedDestinations = destinationsAll ? nodeNames : destinations;
  // An external run has exactly ONE destination (the target row, or the typed
  // address), so its fan-out is one pair per source — the node cross product
  // does not apply, and neither does its self-pair exclusion.
  const pairCount = external ? new Set(resolvedSources).size : estimatePairCount(resolvedSources, resolvedDestinations);
  const rawPairCount = external
    ? resolvedSources.length
    : estimateRawPairCount(resolvedSources, resolvedDestinations);
  // Gates on the RAW product, not the self-excluded pairCount below: that is
  // the exact guard checks.Plan runs first, before any dedup or self-pair
  // exclusion (checks.go), so an overlapping selection that would collapse
  // under the limit only after exclusion must still be blocked here -- see
  // estimateRawPairCount's own doc for the 20x21/400 example.
  const overLimit = rawPairCount > MAX_PAIRS;
  const noPairs = pairCount === 0;
  // A target run with nothing picked, or an ad-hoc one with an empty address,
  // is a guaranteed 400 (resolveRunDestination) -- blocked here rather than
  // sent to collect one.
  const incompleteDestination =
    (destinationKind === "target" && destinationTargetId === "") ||
    (destinationKind === "adhoc" && destinationAddress.trim() === "");

  const runRequest = buildRunRequest({
    type,
    sources: sourcesAll ? [] : sources,
    destinations: destinationsAll ? [] : destinations,
    destinationKind,
    destinationTargetId,
    destinationAddress: destinationAddress.trim(),
  });

  /* ONE clearing point for the whole form (QA round 4, finding #10). A 422
     from a rejected submit stayed on screen while the operator edited the very
     field it was complaining about, and survived a switch to a different
     destination MODE entirely — so a banner about an ad-hoc address was still
     being read under a node run. This form has no central state setter to hang
     it off (each control owns its own useState), and an onChange on the <form>
     would miss the two Segmented controls, which are buttons and fire no
     change event at all. An effect over the form's whole value tuple catches
     every one of them, including those. It fires once on mount too, where
     clearing an already-clear error is a no-op. */
  useEffect(() => {
    setSubmitError(undefined);
  }, [type, sourcesAll, destinationsAll, sources, destinations, destinationKind, destinationTargetId, destinationAddress]);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setSubmitError(undefined);
    if (!begin()) return;
    try {
      const res = await createRun(runRequest);
      goTo(`/diagnostics/runs/${res.id}`);
    } catch (err) {
      setSubmitError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Failed to start run");
      end();
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-5">
        <div>
          <span className="mb-2 block text-xs font-medium text-muted-foreground">Check type</span>
          {/* flex-wrap (QA round 4, finding #17): six options is the widest
              segmented control in the console, and under ~700px the track ran
              off the card rather than wrapping — the last two check types were
              simply unreachable. Round 3's finding #20 gave the track shrink-0
              so it wraps AS A WHOLE inside a flex row; that is the right answer
              for a track sitting beside other controls and the wrong one here,
              where the track is alone in its own block and has nothing to wrap
              against. So this one wraps INTERNALLY, and ui/segmented.tsx's
              thumb tracks its option's row (offsetTop) rather than assuming
              one. */}
          <Segmented
            aria-label="Check type"
            options={CHECK_TYPES.map((t) => ({ value: t, label: t.toUpperCase() }))}
            value={type}
            onChange={setType}
            className="flex-wrap"
          />
        </div>

        <label className="flex w-32 flex-col gap-1 text-[13px]">
          <span className="text-muted-foreground">Plane</span>
          {/* The only plane that exists (task-24-brief.md) -- a real,
              disabled form control rather than a decorative badge, so it
              still reads as "part of this form" to a screen reader. */}
          <select
            disabled
            value="pod"
            className="h-9 rounded-md border border-border-strong bg-transparent px-3 text-[13px] text-muted-foreground disabled:opacity-70"
          >
            <option value="pod">pod</option>
          </select>
        </label>

        <div>
          <span className="mb-2 block text-xs font-medium text-muted-foreground">Destination</span>
          {/* "Target" is offered only with targets:read: without it the picker
              would have nothing to list and its own GET would be a guaranteed
              403. Ad-hoc needs no permission of its own — the address is typed
              here, not read from the targets store. */}
          <Segmented
            aria-label="Destination"
            options={(["node", "target", "adhoc"] as DestinationKind[])
              .filter((k) => k !== "target" || canReadTargets)
              .map((k) => ({ value: k, label: DESTINATION_KIND_LABELS[k] }))}
            value={destinationKind}
            onChange={setDestinationKind}
          />
        </div>

        <div className="grid gap-4 sm:grid-cols-2">
          <NodeSelector
            label="Sources"
            nodes={nodeNames}
            all={sourcesAll}
            onAllChange={setSourcesAll}
            selected={sources}
            onToggle={(n) => setSources((prev) => toggleName(prev, n))}
          />
          {destinationKind === "node" ? (
            <NodeSelector
              label="Destinations"
              nodes={nodeNames}
              all={destinationsAll}
              onAllChange={setDestinationsAll}
              selected={destinations}
              onToggle={(n) => setDestinations((prev) => toggleName(prev, n))}
            />
          ) : null}
          {destinationKind === "target" ? (
            <FieldLabel label="Destination target">
              {(id) => (
                <select
                  id={id}
                  value={destinationTargetId}
                  onChange={(e) => setDestinationTargetId(e.target.value)}
                  className={CONTROL_CLASS}
                >
                  <option value="">— pick a target —</option>
                  {targets.map((t) => (
                    <option key={t.id} value={t.id}>
                      {t.name}
                    </option>
                  ))}
                </select>
              )}
            </FieldLabel>
          ) : null}
          {destinationKind === "adhoc" ? (
            <FieldLabel label="Destination address">
              {(id) => (
                <input
                  id={id}
                  /* Belt and braces on the name (QA round 4, finding #16): the
                     visible <label> is the association, and this survives a
                     future refactor that moves the field out of FieldLabel. */
                  aria-label="Destination address"
                  value={destinationAddress}
                  placeholder="10.0.0.1 or https://example.test/health"
                  onChange={(e) => setDestinationAddress(e.target.value)}
                  className={CONTROL_CLASS}
                />
              )}
            </FieldLabel>
          ) : null}
        </div>

        <div className="flex flex-wrap items-center gap-3">
          {destinationKind === "node" ? (
            /* A RESET, and now it says so (QA round 4, finding #15). The glyph
               alone read as "swap the two columns" or "run every pair", and
               pressing it when both pickers were already at All did nothing
               visible — which is fine for a reset and baffling for either of
               the other two readings. The name is the fix; the no-op is not a
               bug. */
            <Button
              type="button"
              variant="outline"
              size="sm"
              title="Reset both pickers to every node"
              aria-label="Reset both pickers to every node"
              onClick={() => {
                setSourcesAll(true);
                setDestinationsAll(true);
              }}
            >
              All ↔ All
            </Button>
          ) : null}
          {/* "~" flags this as an estimate: the server is the only real
              arbiter, and on an overlapping selection this self-excluded
              number can even read as under the limit while the raw S×D
              product the server actually gates on is over it. */}
          <span className={cn("nums text-sm", overLimit ? "text-health-bad" : "text-muted-foreground")}>
            ~{pairCount} pair{pairCount === 1 ? "" : "s"}
            {overLimit
              ? ` — above the ${MAX_PAIRS}-pair limit (server enforces the raw ${resolvedSources.length}×${
                  external ? 1 : resolvedDestinations.length
                } limit of ${MAX_PAIRS}), narrow the selection`
              : ""}
          </span>
        </div>

        {submitError ? (
          <p role="alert" className="text-sm text-health-bad">
            {submitError}
          </p>
        ) : null}

        {/* The Time Machine joins the three reasons this button was already
            allowed to be dead. A run started from a view of the past would run
            NOW against the present fleet, which is exactly the confusion plan
            Decision 8 exists to prevent — disabled, not hidden, because the
            permission to start it has not gone anywhere. */}
        <Button
          type="submit"
          loading={submitting}
          {...guard} disabled={overLimit || noPairs || incompleteDestination || writesDisabled}
          className="self-start"
        >
          Start run
        </Button>

        {canWriteChecks ? (
          <SaveAsDefinition
            checkType={type}
            destinationKind={destinationKind}
            destinationTargetId={destinationTargetId}
            destinationAddress={destinationAddress.trim()}
            incompleteDestination={incompleteDestination}
          />
        ) : null}
      </form>
    </Card>
  );
}

/* ── save as definition ─────────────────────────────────────────────────────
   The run form describes a one-off probe; a check definition describes the
   same probe repeated. This turns one into the other with POST /api/v1/checks
   — the very same endpoint and body the Definitions tab posts — so nothing
   here is a second, parallel way to write a definition.

   Rendered only with checks:write (the endpoint's own permission), never
   disabled-with-a-tooltip. "Attach to incident" is M6 and is deliberately not
   rendered at all.

   Two fields of the run form have NO counterpart in a definition and are
   therefore not carried over, rather than guessed at:

    - the per-node source list. A definition's sourceSelection is
      all|per-zone|one-per-zone, which cannot express "these five nodes", so
      the saved definition always probes from `all` and says so in the hint
      below. The run's own selection stays a property of the run.
    - the node destination list, for the same reason — a definition with
      destinationKind "node" already means "every node".
*/
type SaveField = "name" | "destination" | "form";

function SaveAsDefinition({
  checkType,
  destinationKind,
  destinationTargetId,
  destinationAddress,
  incompleteDestination,
}: {
  checkType: CheckType;
  destinationKind: DestinationKind;
  destinationTargetId: string;
  destinationAddress: string;
  incompleteDestination: boolean;
}) {
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's
     useWriteGuard (QA round 2, finding #18; extended here in round 3). Spread it
     onto the control, and compose any local condition AFTER the spread. */
  const guard = useWriteGuard();
  const writesDisabled = guard.disabled;
  const [name, setName] = useState("");
  const [saving, setSaving] = useState(false);
  const [errors, setErrors] = useState<Partial<Record<SaveField, string>>>({});
  const [saved, setSaved] = useState<string>();

  const draft: CheckDefinitionRequest = {
    name,
    sourceSelection: "all",
    destinationKind,
    destinationTargetId: destinationKind === "target" ? destinationTargetId : undefined,
    destinationAddress: destinationKind === "adhoc" ? destinationAddress : undefined,
    checkType,
    plane: "pod",
    // Same default the Definitions tab's own form uses: saving a definition
    // is asking for it to run. The projection guard applies (an over-limit
    // enabled definition is a 422), and that message renders below verbatim.
    enabled: true,
  };

  async function handleSave() {
    setErrors({});
    setSaved(undefined);
    if (name.trim() === "") {
      setErrors({ name: "a definition needs a name" });
      return;
    }
    /* The client mirror of store.validateAdhocAddress (QA round 4, finding
       #13). Saving a definition PERSISTS the address, and until the store
       learned to check it "sdfsdfsdf !!" was stored happily and then failed as
       a resolver error on every agent, every interval, forever. The server is
       still the arbiter — this only means the refusal arrives at the field the
       operator is looking at instead of two round trips later. */
    if (destinationKind === "adhoc" && destinationAddress !== "" && !isValidAdhocAddress(destinationAddress)) {
      // Empty is already the run form's own `incompleteDestination` gate; this
      // branch is only about a value that IS there and cannot be dialled.
      setErrors({ destination: ADHOC_ADDRESS_ERROR });
      return;
    }
    setSaving(true);
    try {
      const created = await createCheck({ ...draft, name: name.trim() });
      setSaved(created.name);
      setName("");
    } catch (err) {
      if (!(err instanceof ApiError)) {
        setErrors({ form: "Failed to save the definition" });
      } else {
        const detail = err.problem.detail ?? err.problem.title;
        // The same phrase table the Definitions tab uses, collapsed onto the
        // two fields THIS form actually has: anything else (the projection
        // 422 included) still renders in full, one level up, rather than
        // being swallowed because there is no field to hang it on.
        const field = fieldForDetail(detail, DEFINITION_FIELD_PHRASES);
        if (field === "name") setErrors({ name: detail });
        else if (field === "destinationTargetId" || field === "destinationAddress") {
          setErrors({ destination: detail });
        } else setErrors({ form: detail });
      }
    }
    setSaving(false);
  }

  return (
    <div className="flex flex-col gap-2 border-t border-border pt-4">
      <div className="flex flex-wrap items-end gap-3">
        <FieldLabel label="Definition name">
          {(id) => (
            <input
              id={id}
              value={name}
              placeholder="edge-gateway-tcp"
              aria-invalid={errors.name ? true : undefined}
              onChange={(e) => setName(e.target.value)}
              className={cn(CONTROL_CLASS, errors.name ? "border-health-bad" : "")}
            />
          )}
        </FieldLabel>
        <Button
          type="button"
          variant="outline"
          loading={saving}
          {...guard} disabled={incompleteDestination || writesDisabled}
          onClick={handleSave}
        >
          Save as definition
        </Button>
      </div>
      <p className="max-w-prose text-xs leading-relaxed text-muted-foreground">
        Saved enabled and probing from all agents — a definition has no per-node source list. Edit it on Targets &
        Schedules.
      </p>
      {errors.name ? (
        <p role="alert" className="text-sm text-health-bad">
          {errors.name}
        </p>
      ) : null}
      {errors.destination ? (
        <p role="alert" className="text-sm text-health-bad">
          {errors.destination}
        </p>
      ) : null}
      {errors.form ? (
        <p role="alert" className="text-sm text-health-bad">
          {errors.form}
        </p>
      ) : null}
      {saved ? (
        <p role="status" className="text-sm text-muted-foreground">
          Saved definition “{saved}”
        </p>
      ) : null}
    </div>
  );
}

function fmtTime(timestamp?: string): string {
  if (!timestamp) return "—";
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : d.toLocaleString();
}

function HistoryList({ runs, engaged }: { runs: RunSummary[]; engaged: boolean }) {
  if (runs.length === 0) {
    if (engaged) {
      // Engaged with everything filtered out is a DIFFERENT fact from "nobody
      // has ever run one", and offering the form above as the remedy would be
      // wrong twice over — the form is disabled while engaged.
      return (
        <div className="flex flex-col items-center gap-3 px-6 py-14 text-center">
          <span
            aria-hidden="true"
            className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
          >
            <ClipboardList className="size-5" />
          </span>
          <p className="text-sm font-medium">No runs at or before the viewed instant</p>
          <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">
            Every run on the loaded page started later than this. Return to Live, or load older pages.
          </p>
        </div>
      );
    }
    return (
      <div className="flex flex-col items-center gap-3 px-6 py-14 text-center">
        <span
          aria-hidden="true"
          className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
        >
          <ClipboardList className="size-5" />
        </span>
        <p className="text-sm font-medium">No runs yet</p>
        <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">
          Runs started from the form above (or by another operator) show up here.
        </p>
      </div>
    );
  }
  return (
    <ul className="mt-4 divide-y divide-border">
      {runs.map((r) => (
        <li key={r.id} className="flex flex-wrap items-center gap-3 py-3 text-sm">
          <a href={`/diagnostics/runs/${r.id}`} className="font-medium text-primary hover:underline">
            {r.id}
          </a>
          <StatusBadge status={r.status} />
          <span className="text-xs uppercase tracking-wide text-muted-foreground">{r.type}</span>
          <span className="nums ml-auto text-xs text-muted-foreground">
            {r.pairOk}/{r.pairTotal} ok
          </span>
          <span className="text-xs text-muted-foreground">{fmtTime(r.createdAt)}</span>
        </li>
      ))}
    </ul>
  );
}

export function DiagnosticsPage() {
  const { can } = useAuth();
  const topo = useTopology();
  const { available: dbConfigured, resolved: dbResolved } = useDatabaseAvailable();
  const { at } = useTimeContext();

  const canCreate = can("runs:create");
  /* The UNION of the controller's node list and the node names the AGENTS
     report (QA round 4, finding #21; round 3's finding #5 solved the same
     thing for Investigate and this is its helper, imported rather than
     re-derived). `topology.nodes` is the CONTROLLER's view and is empty on
     every console deployed without one — a console that still has agents
     reporting in and every reason to run a diagnostic between them. Reading
     `nodes` alone left both pickers empty there, with no explanation, on the
     page whose entire purpose is starting a run. */
  const nodeNames = useMemo(() => scopeNodeOptions(topo.data), [topo.data]);

  /* Run history (GET /api/v1/runs) is paginated behind the same opaque
     keyset cursor as event scrollback, and is loaded the same way --
     loadHistory in pages/live.tsx is the convention this mirrors.
     `history.nextCursor` doubles as both "there is nothing more to load"
     (exhausted, "" after a real page) and "nothing has loaded yet" (the
     initial value, also ""), which is the right default for a "Load
     older" button that has not yet heard back from its first page. */
  const [runs, setRuns] = useState<RunSummary[]>([]);
  const [history, setHistory] = useState<{ nextCursor: string; loading: boolean; error: unknown }>({
    nextCursor: "",
    loading: false,
    error: null,
  });

  const loadRuns = useCallback(async (cursor?: string) => {
    setHistory((h) => ({ ...h, loading: true, error: null }));
    try {
      const page = await getRuns({ cursor });
      // A cursor-less call is page one -- replace, rather than append, so a
      // remount or a future refresh does not just keep growing the list.
      setRuns((prev) => (cursor ? [...prev, ...page.runs] : page.runs));
      setHistory({ nextCursor: page.nextCursor, loading: false, error: null });
    } catch (err) {
      setHistory({ nextCursor: "", loading: false, error: err });
    }
  }, []);

  useEffect(() => {
    void loadRuns(undefined);
  }, [loadRuns]);

  /* The Time Machine's cut across the history list (QA round 4, finding #4;
     the same treatment round 3 gave the node and pair cards). GET /api/v1/runs
     has no `to` parameter — its query is type/status/cursor/limit and nothing
     else — so the newest page it answers is the newest page NOW, and under a
     banner reading "you are viewing 02:14" this list was showing runs that had
     not happened yet. Client-side over the fetched pages is therefore the
     whole of what is available, and the copy below states the bound rather
     than implying complete history. */
  const visibleRuns = useMemo(() => runsAtOrBefore(runs, at), [runs, at]);

  return (
    <PageShell
      title="Diagnostics"
      description={
        at
          ? `Run on-demand checks against the mesh. History is cut to ${at.toLocaleString()} — runs started later are not listed.`
          : "Run on-demand checks against the mesh, and browse run history."
      }
    >
      {canCreate ? (
        <RunForm nodeNames={nodeNames} canReadTargets={can("targets:read")} canWriteChecks={can("checks:write")} />
      ) : (
        <Card role="status" className="p-6">
          <p className="text-sm font-medium">Starting a run requires the runs:create permission</p>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
            You can still see run history below. Ask an operator to start a new run.
          </p>
        </Card>
      )}

      <Card asChild className="p-6">
        <section>
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <h2 className="text-sm font-semibold">Run history</h2>
          </div>
          {dbResolved && !dbConfigured ? (
            <p role="status" className="mt-1 text-xs leading-relaxed text-muted-foreground">
              History is not persisted — set console.database.mode
            </p>
          ) : null}
          {at ? (
            <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
              GET /api/v1/runs has no time filter, so this cut to the viewed instant happens in the browser over the
              pages loaded here — a run older than them is not reached by paging backwards from this list.
            </p>
          ) : null}

          {history.error ? (
            <p role="alert" className="mt-3 text-sm text-health-bad">
              {history.error instanceof ApiError
                ? (history.error.problem.detail ?? history.error.problem.title)
                : "Run history is unavailable"}
            </p>
          ) : null}

          <HistoryList runs={visibleRuns} engaged={at !== null} />

          {runs.length > 0 ? (
            <div className="mt-4 flex justify-center">
              <Button
                variant="outline"
                size="sm"
                disabled={history.nextCursor === "" || history.loading}
                onClick={() => loadRuns(history.nextCursor)}
              >
                {history.loading ? "Loading older…" : "Load older"}
              </Button>
            </div>
          ) : null}
        </section>
      </Card>
    </PageShell>
  );
}
