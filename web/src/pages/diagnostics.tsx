import { useCallback, useEffect, useId, useMemo, useRef, useState, type FormEvent, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { ClipboardList } from "lucide-react";
import { PageShell } from "@/components/page-shell";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Pager, usePager } from "@/components/ui/pager";
import { Segmented } from "@/components/ui/segmented";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { useTopology } from "@/hooks/use-topology";
import { ApiError, createCheck, createRun, getRuns, goTo, listAllTargets } from "@/lib/api";
import { localeTag, stampFull, useLocale, useT, type Locale, type Translate } from "@/lib/i18n";
import { countForm, diagnosticsDict, type DiagnosticsKey } from "@/lib/i18n/dict/diagnostics";
/* The ad-hoc address refusal is lib/utils.ts's, shared with the definition
   form on /targets — one rule, one sentence, one table. */
import { validationDict } from "@/lib/i18n/dict/validation";
import { scopeNodeOptions } from "@/lib/investigation-sources";
import {
  formatCadenceProse,
  perPairBudgetNs,
  planCadenceFor,
  type PlannedCadence,
} from "@/lib/run-samples";
import { withAtParam, useTimeContext, useWriteGuard } from "@/lib/timemachine";
import {
  CHECK_TYPES,
  type CheckDefinitionRequest,
  type CheckType,
  type DestinationKind,
  type RunCreateRequest,
  type RunSummary,
} from "@/lib/types";
import { CHECKBOX_CLASS, cn, isValidAdhocAddress, runsAtOrBefore } from "@/lib/utils";
// The 422-detail -> form-field heuristic and its phrase table live with the form that first needed
// them (pages/targets.tsx).
import { DEFINITION_FIELD_PHRASES, fieldForDetail } from "./targets";

// MAX_PAIRS mirrors checks.maxPairs (internal/console/checks/checks.go); the server remains the
// only real enforcement point.
export const MAX_PAIRS = 400;

/**
 * estimatePairCount previews checks.Plan's own pair count; it is a client-side approximation only
 * -- Plan additionally dedupes exact duplicate (source, destination) pairs after expansion.
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
   The run form's destination selector names the SAME vocabulary check
   definitions use (store.DefinitionInput's destinationKind), on purpose: an
   operator who has written a definition already knows this shape.

   The map holds dict KEYS rather than words (it used to be
   DESTINATION_KIND_LABELS, an exported English table): the wire values are the
   thing this page owns, the words belong to lib/i18n/dict/diagnostics.ts.
   pages/mtr.tsx's Runner carries the same three options and its own map into
   its own dictionary, per the README's one-file-per-surface rule.

   "node" is the default and the pre-M4 contract -- and a node run sends NO
   destination* field at all, not an explicit "node", so the body an M3
   console produced and the body this form produces for a node run are the
   same bytes (resolveRunDestination in internal/console/httpapi/runs.go
   treats "" and "node" identically). */
const DESTINATION_KIND_KEYS: Record<DestinationKind, DiagnosticsKey> = {
  node: "destination.kind.node",
  target: "destination.kind.target",
  adhoc: "destination.kind.adhoc",
};

function StatusBadge({ status }: { status: string }) {
  return (
    <Badge variant={STATUS_VARIANT[status] ?? "unknown"} dot>
      {status}
    </Badge>
  );
}

/* NodeSelector, toggleName, FieldLabel and CONTROL_CLASS below are EXPORTED,
   not because this page needs them to be, but because M5 Task 8's MTR Runner
   builds the same body against the same endpoint from pages/mtr.tsx.
   Exporting the pieces rather than copying them is the same call this file
   already made when it imported targets.tsx's 422-detail table: one run form's
   set of controls, and a change to them cannot land on one page and not the
   other. NodeSelector's own two strings therefore come from THIS surface's
   dictionary on both pages. */
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
  const t = useT(diagnosticsDict);
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
        {t("nodes.all", { count: nodes.length })}
      </label>
      {!all ? (
        <div className="mt-2 flex max-h-40 flex-col gap-1 overflow-y-auto">
          {nodes.length === 0 ? (
            <p className="text-xs text-muted-foreground">{t("nodes.empty")}</p>
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
/**
 * RUN_DURATIONS is the duration selector's vocabulary. "Instant" is FIRST and
 * is the default: the overwhelming majority of runs are still one probe per
 * pair, and a form that quietly defaulted to an hour of fleet traffic would be
 * a trap.
 *
 * Every non-zero value sits inside the server's own [10s, 24h] window
 * (checks.MinRunDuration/MaxRunDuration), so no option on offer here can
 * produce the 422 — the server stays the enforcement point, but the UI does
 * not lead an operator into a refusal.
 *
 * `label` is the DURATION token and renders as it stands in both languages.
 * The one exception is "Instant", which is a word: the two forms that offer
 * this table (the run form below and pages/mtr.tsx's Runner) each swap it for
 * their own dictionary's, so editing the literal here changes nothing on
 * screen. It stays as the option's description, and as the interval captions'
 * "{label}" placeholder, which only ever sees a non-zero row.
 */
export const RUN_DURATIONS: { value: string; label: string; ns: number }[] = [
  { value: "instant", label: "Instant", ns: 0 },
  { value: "1m", label: "1m", ns: 60e9 },
  { value: "5m", label: "5m", ns: 300e9 },
  { value: "15m", label: "15m", ns: 900e9 },
  { value: "1h", label: "1h", ns: 3600e9 },
  { value: "6h", label: "6h", ns: 21600e9 },
  { value: "24h", label: "24h", ns: 86400e9 },
];

export function durationNsFor(value: string): number {
  return RUN_DURATIONS.find((d) => d.value === value)?.ns ?? 0;
}

/**
 * RUN_SAMPLE_INTERVALS is the cadence selector, and "Auto" is FIRST and is the
 * default for the same reason "Instant" is: it posts nothing, so every run made
 * without touching this control behaves exactly as it did before the control
 * existed.
 *
 * The control exists because the cadence used to be un-dialable — the planner's
 * own doc said the base cadence "is not something an operator can dial" — while
 * three different surfaces each reported a different number for it. An operator
 * who can set it can also be told, in one sentence, what setting it will do.
 *
 * `label` is a duration token and stands in both languages, exactly as
 * RUN_DURATIONS' labels do; only "Auto" is a word, and each form swaps it for
 * its own dictionary's.
 */
export const RUN_SAMPLE_INTERVALS: { value: string; label: string; ns: number }[] = [
  { value: "auto", label: "Auto", ns: 0 },
  { value: "1s", label: "1s", ns: 1e9 },
  { value: "5s", label: "5s", ns: 5e9 },
  { value: "15s", label: "15s", ns: 15e9 },
  { value: "30s", label: "30s", ns: 30e9 },
  { value: "1m", label: "1m", ns: 60e9 },
  { value: "5m", label: "5m", ns: 300e9 },
  { value: "15m", label: "15m", ns: 900e9 },
];

export function sampleIntervalNsFor(value: string): number {
  return RUN_SAMPLE_INTERVALS.find((d) => d.value === value)?.ns ?? 0;
}

/**
 * sampleIntervalOptionsFor drops every preset LONGER than the run.
 *
 * The server refuses a cadence above durationNs with a 422 (a cadence that long
 * collapses to a single sample, which is an instant run the caller did not ask
 * for), and RUN_DURATIONS' own rule applies here: the server stays the
 * enforcement point, but the UI does not lead an operator into a refusal. Auto
 * always survives, and so does the equal case — "every 1m for 1m" is one honest
 * sample, not an error.
 */
export function sampleIntervalOptionsFor(durationNs: number): typeof RUN_SAMPLE_INTERVALS {
  if (!(durationNs > 0)) return RUN_SAMPLE_INTERVALS.slice(0, 1);
  return RUN_SAMPLE_INTERVALS.filter((o) => o.ns === 0 || o.ns <= durationNs);
}

/* ── the external destination, per check type ────────────────────────────────
   One field served all six check types with one label, one placeholder and no
   hint (QA scope 4, finding #10) — so it named the wrong thing for most of
   them, and a value typed for one type survived a switch to another without a
   word. These four shapes are the AGENT's own behaviour, read off
   internal/agent/tasks.go:

     tcp   externalCapableChecks + externalPort defaults the port to 80
     udp   externalCapableChecks, no default port: 0 is what gets dialled
     icmp  externalCapableChecks, ICMP has no ports and one is ignored
     mtr   same as icmp
     dns   NOT in externalCapableChecks — "external destinations support only
     http  tcp, udp, icmp and mtr checks", refused before any checker runs

   The console does not enforce; it says what the agent will do, and refuses to
   send a body whose refusal is already certain. */
export type AdhocShape = "hostPort" | "hostPortRequired" | "hostOnly" | "unsupported";

export const ADHOC_SHAPE: Record<CheckType, AdhocShape> = {
  tcp: "hostPort",
  udp: "hostPortRequired",
  icmp: "hostOnly",
  mtr: "hostOnly",
  dns: "unsupported",
  http: "unsupported",
};

/* Addresses are syntax and do not translate; the CONNECTIVE between two of them
   is a word, and it used to read "example.test or 10.0.0.1" in the Russian
   interface. The examples stay here, the joining word comes from the caller. */
export const ADHOC_PLACEHOLDER: Record<AdhocShape, readonly string[]> = {
  hostPort: ["example.test", "10.0.0.1:8443"],
  hostPortRequired: ["10.0.0.1:53"],
  hostOnly: ["example.test", "10.0.0.1"],
  unsupported: [],
};

/** adhocPlaceholder joins the examples with the interface language's own "or". */
export function adhocPlaceholder(shape: AdhocShape, or: string): string {
  return ADHOC_PLACEHOLDER[shape].join(` ${or} `);
}

/**
 * AdhocIssue is what is wrong with (check type, address) as a pair, evaluated on every render — so
 * a type switch RE-JUDGES the value the operator already typed instead of leaving it standing.
 * `null` is "nothing the console can know is wrong"; the server is still the arbiter.
 */
export type AdhocIssue = "unsupported" | "shape" | "url" | "port";

export function adhocAddressIssue(checkType: CheckType, raw: string): AdhocIssue | null {
  // The whole check type is refused for an external destination, whatever the
  // address says — so this one outranks every address-shaped complaint.
  if (ADHOC_SHAPE[checkType] === "unsupported") return "unsupported";
  const value = raw.trim();
  // Empty is the form's own incompleteDestination gate, not a mismatch.
  if (value === "") return null;
  if (!isValidAdhocAddress(value)) return "shape";
  // The agent hands this string to the allowlist and dials the host it
  // resolves; a URL is only ever dialled by the http checker, which cannot
  // take an external destination at all.
  if (/^https?:\/\//i.test(value)) return "url";
  // udp has no default port (externalPort returns 0 for everything but tcp),
  // so an address without one names a port nothing listens on.
  if (ADHOC_SHAPE[checkType] === "hostPortRequired" && !hasExplicitPort(value)) return "port";
  return null;
}

/** hasExplicitPort mirrors isValidAdhocAddress's own port split: the LAST colon, with a bracketed
 *  IPv6 host kept whole. */
function hasExplicitPort(value: string): boolean {
  const colon = value.lastIndexOf(":");
  if (colon <= 0) return false;
  return value.startsWith("[") ? value.includes("]:") && value.indexOf("]:") + 1 === colon : value.indexOf(":") === colon;
}

export function buildRunRequest(input: {
  type: CheckType;
  sources: string[];
  destinations: string[];
  destinationKind: DestinationKind;
  destinationTargetId: string;
  destinationAddress: string;
  durationNs?: number;
  sampleIntervalNs?: number;
}): RunCreateRequest {
  const req: RunCreateRequest = {
    type: input.type,
    plane: "pod",
    sources: input.sources,
    destinations: input.destinationKind === "node" ? input.destinations : [],
  };
  /* Omitted entirely for an instant run, not sent as 0: the node case's body
     is a deliberate regression surface (see this function's own doc above),
     and an instant run must keep serialising exactly the four keys it always
     had. */
  if (input.durationNs && input.durationNs > 0) {
    req.durationNs = input.durationNs;
    /* Nested, and omitted at zero for the same reason: "Auto" must post the
       body an interval run posted before this control existed, byte for byte —
       and a cadence without a duration is a 422 the form has no business
       collecting. */
    if (input.sampleIntervalNs && input.sampleIntervalNs > 0) {
      req.sampleIntervalNs = input.sampleIntervalNs;
    }
  }
  if (input.destinationKind === "target") {
    req.destinationKind = "target";
    req.destinationTargetId = input.destinationTargetId;
  } else if (input.destinationKind === "adhoc") {
    req.destinationKind = "adhoc";
    req.destinationAddress = input.destinationAddress;
  }
  return req;
}

/**
 * cadenceCaption is the ONE sentence both run builders say about a duration
 * run's cadence — this form's, and pages/mtr.tsx's Runner.
 *
 * It exists because they said different things about the same run. The Runner's
 * caption named the BASE cadence («раз в 5 с») for a check type that cannot keep
 * it, while this form had already been made type-aware; a 5m MTR over ten pairs
 * was advertised at 5s by one screen and 3m by another, and did neither. There
 * is now one mirror of the planner (planCadenceFor) and one sentence built from
 * it, so the two surfaces cannot drift again.
 *
 * `prefix` rather than a shared key union, for lib/i18n/README.md's reason: two
 * surfaces, two dictionaries. The keys under each prefix are the same four.
 *
 * Order is deliberate. When the plan is not what was PICKED, the adjustment
 * leads: an operator who has just clicked "1s" needs to learn it will not be 1s
 * before they are told what it will be. With nothing picked there is nothing to
 * contradict, and the "mtr" sentence already explains its own stretch by naming
 * the trace budget — so the note is suppressed rather than made to say the run
 * betrayed a request nobody made.
 */
export function cadenceCaption<K extends string>(
  t: Translate<K>,
  locale: string,
  plan: PlannedCadence,
  checkType: CheckType,
  durationValue: string,
  prefix = "duration.caption",
): string {
  const budgetNs = perPairBudgetNs(checkType);
  const vars = {
    interval: formatCadenceProse(plan.intervalNs, locale),
    requested: formatCadenceProse(plan.requestedNs, locale),
    label: RUN_DURATIONS.find((d) => d.value === durationValue)?.label ?? "",
    samples: plan.samplesPerPair,
    budget: formatCadenceProse(budgetNs, locale),
  };
  const base = t(`${prefix}.interval${budgetNs > 0 ? ".mtr" : ""}` as K, vars);
  if (plan.adjusted === "" || plan.requestedNs <= 0) return base;
  return `${t(`${prefix}.adjusted.${plan.adjusted}` as K, vars)} ${base}`;
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
  const t = useT(diagnosticsDict);
  const tv = useT(validationDict);
  const { locale } = useLocale();
  const [type, setType] = useState<CheckType>("tcp");
  const [sourcesAll, setSourcesAll] = useState(true);
  const [destinationsAll, setDestinationsAll] = useState(true);
  const [sources, setSources] = useState<string[]>([]);
  const [destinations, setDestinations] = useState<string[]>([]);
  const [destinationKind, setDestinationKind] = useState<DestinationKind>("node");
  const [destinationTargetId, setDestinationTargetId] = useState("");
  const [destinationAddress, setDestinationAddress] = useState("");
  const [duration, setDuration] = useState("instant");
  /* "auto" is today's behaviour exactly: nothing is posted, and the console
     derives the cadence as it always did. */
  const [sampleInterval, setSampleInterval] = useState("auto");
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
    // Every page: a picker that stops at the server's first 100 hides targets that exist.
    queryFn: () => listAllTargets(),
    enabled: canReadTargets && destinationKind === "target",
  });
  const targets = targetsQuery.data?.items ?? [];

  const external = destinationKind !== "node";
  const resolvedSources = sourcesAll ? nodeNames : sources;
  const resolvedDestinations = destinationsAll ? nodeNames : destinations;
  // A target run with nothing picked, or an ad-hoc one with an empty address,
  // is a guaranteed 400 (resolveRunDestination) -- blocked here rather than
  // sent to collect one.
  const incompleteDestination =
    (destinationKind === "target" && destinationTargetId === "") ||
    (destinationKind === "adhoc" && destinationAddress.trim() === "");
  /* A run's fan-out is sources x destinations, and with the destination side
     unresolved there is no second factor. The preview used to print one pair
     per source anyway -- "~10 pairs" for a form with no target picked and no
     address typed (QA scope 4, finding #9), a number for a run the server
     would refuse outright. Zero is the true estimate, and pairsReason below
     says which side is missing. */
  // An external run has exactly ONE destination (the target row, or the typed
  // address), so its fan-out is one pair per source — the node cross product
  // does not apply, and neither does its self-pair exclusion.
  const pairCount = incompleteDestination
    ? 0
    : external
      ? new Set(resolvedSources).size
      : estimatePairCount(resolvedSources, resolvedDestinations);
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
  /* The reason a zero estimate is zero. A dead button owes an explanation and
     "~0 pairs" alone is not one (QA scope 4, finding #8) -- /mtr's Runner has
     said this since it shipped, and this is the same sentence. Sources first:
     with nothing to probe FROM, which destination is missing does not matter
     yet. */
  const pairsReason: DiagnosticsKey | null = !noPairs
    ? null
    : resolvedSources.length === 0
      ? "pairs.noSources"
      : destinationKind === "target" && destinationTargetId === ""
        ? "pairs.noTarget"
        : destinationKind === "adhoc" && destinationAddress.trim() === ""
          ? "pairs.noAddress"
          : /* Sources and destinations both non-empty and still no pair means
               every pair the cross product produced was a node against itself —
               a one-node cluster with both pickers on All is the whole of it.
               Saying "no destinations picked" there named the one thing the
               operator could see was false, and pointed them at a picker that
               was already right. */
            resolvedDestinations.length > 0
            ? "pairs.selfOnly"
            : "pairs.noDestinations";

  /* The external destination's per-type vocabulary and its live verdict. Both
     are derived, never stored: a check-type switch must re-judge whatever is
     already typed rather than leave a stale caption or a stale error. */
  const adhocShape = ADHOC_SHAPE[type];
  const adhocLabelKey = `adhoc.label.${adhocShape}` as DiagnosticsKey;
  const adhocHintKey = `adhoc.hint.${adhocShape}` as DiagnosticsKey;
  const adhocIssue = external ? adhocAddressIssue(type, destinationKind === "adhoc" ? destinationAddress : "") : null;

  const durationNs = durationNsFor(duration);
  /* The presets that FIT this duration, and the picked one re-judged against
     them: shortening the run from 15m to 1m while "5m" was selected must not
     leave a value the server would refuse standing in a control that still
     looks chosen. It falls back to Auto, which is always offered. */
  const intervalOptions = sampleIntervalOptionsFor(durationNs);
  const intervalValue = intervalOptions.some((o) => o.value === sampleInterval) ? sampleInterval : "auto";
  const requestedIntervalNs = sampleIntervalNsFor(intervalValue);
  const runRequest = buildRunRequest({
    type,
    sources: sourcesAll ? [] : sources,
    destinations: destinationsAll ? [] : destinations,
    destinationKind,
    destinationTargetId,
    destinationAddress: destinationAddress.trim(),
    durationNs,
    sampleIntervalNs: requestedIntervalNs,
  });
  /* ONE mirror of the server's planner for the whole caption: what the run will
     keep, and whether that is what was asked for. */
  const plan = planCadenceFor(durationNs, type, pairCount, resolvedSources.length, requestedIntervalNs);

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
  }, [type, sourcesAll, destinationsAll, sources, destinations, destinationKind, destinationTargetId, destinationAddress, duration, sampleInterval]);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setSubmitError(undefined);
    if (!begin()) return;
    try {
      const res = await createRun(runRequest);
      goTo(`/diagnostics/runs/${res.id}`);
    } catch (err) {
      // problem+json is the SERVER's refusal, verbatim; only the network-level
      // fallback is the console's own sentence.
      setSubmitError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : t("form.submitFailed"));
      end();
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-5">
        <div>
          <span className="mb-2 block text-xs font-medium text-muted-foreground">{t("form.checkType")}</span>
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
            aria-label={t("form.checkType.aria")}
            /* The check types are protocol names — TCP, UDP, ICMP, DNS, HTTP,
               MTR — and stay Latin in both languages. */
            options={CHECK_TYPES.map((ct) => ({ value: ct, label: ct.toUpperCase() }))}
            value={type}
            onChange={setType}
            className="flex-wrap"
          />
        </div>

        <div>
          <span className="mb-2 block text-xs font-medium text-muted-foreground">{t("form.duration")}</span>
          {/* Instant stays the default and the first option. Anything else
              re-probes every pair on a server-derived cadence for that long,
              and the run stays cancellable the whole time — which is why the
              caption below spells out what was chosen rather than leaving the
              operator to discover the fan-out after pressing Run. */}
          <Segmented
            aria-label={t("form.duration.aria")}
            /* Only "Instant" is a word; 1m … 24h are durations and stay. */
            options={RUN_DURATIONS.map((d) => ({
              value: d.value,
              label: d.value === "instant" ? t("duration.instant") : d.label,
            }))}
            value={duration}
            onChange={setDuration}
            className="flex-wrap"
          />
          <p className="mt-2 text-xs text-muted-foreground">
            {durationNs === 0 ? t("duration.caption.instant") : cadenceCaption(t, locale, plan, type, duration)}
          </p>
        </div>

        {/* The cadence control, only for a run that HAS one. It is rendered
            beneath Duration rather than beside it because it is a refinement of
            that choice: "Auto" is the whole of what existed before, and every
            preset above it is bounded by the duration already picked. */}
        {durationNs > 0 ? (
          <div data-testid="sample-interval">
            <span className="mb-2 block text-xs font-medium text-muted-foreground">{t("form.sampleInterval")}</span>
            <Segmented
              aria-label={t("form.sampleInterval.aria")}
              /* Only "Auto" is a word; 1s … 15m are durations and stand. */
              options={intervalOptions.map((o) => ({
                value: o.value,
                label: o.value === "auto" ? t("sampleInterval.auto") : o.label,
              }))}
              value={intervalValue}
              onChange={setSampleInterval}
              className="flex-wrap"
            />
          </div>
        ) : null}

        <div className="flex w-32 flex-col gap-1 text-[13px]">
          <span className="text-muted-foreground">{t("form.plane")}</span>
          {/* pod is the only plane that exists (task-24-brief.md), and one
              option is not a choice: a disabled <select> still LOOKS like a
              control, so it reads as something the operator failed to use.
              A chip states the fact instead — the treatment pages/matrix.tsx
              already gives the same value (QA scope 4, finding #17). The
              run body's `plane` comes from a constant either way, never from
              this element. */}
          <span className="flex h-9 items-center">
            <Badge variant="neutral">pod</Badge>
          </span>
        </div>

        <div>
          <span className="mb-2 block text-xs font-medium text-muted-foreground">{t("form.destination")}</span>
          {/* "Target" is offered only with targets:read: without it the picker
              would have nothing to list and its own GET would be a guaranteed
              403. Ad-hoc needs no permission of its own — the address is typed
              here, not read from the targets store. */}
          <Segmented
            aria-label={t("form.destination.aria")}
            options={(["node", "target", "adhoc"] as DestinationKind[])
              .filter((k) => k !== "target" || canReadTargets)
              .map((k) => ({ value: k, label: t(DESTINATION_KIND_KEYS[k]) }))}
            value={destinationKind}
            onChange={setDestinationKind}
          />
        </div>

        <div className="grid gap-4 sm:grid-cols-2">
          <NodeSelector
            label={t("form.sources")}
            nodes={nodeNames}
            all={sourcesAll}
            onAllChange={setSourcesAll}
            selected={sources}
            onToggle={(n) => setSources((prev) => toggleName(prev, n))}
          />
          {destinationKind === "node" ? (
            <NodeSelector
              label={t("form.destinations")}
              nodes={nodeNames}
              all={destinationsAll}
              onAllChange={setDestinationsAll}
              selected={destinations}
              onToggle={(n) => setDestinations((prev) => toggleName(prev, n))}
            />
          ) : null}
          {destinationKind === "target" ? (
            <FieldLabel label={t("form.destinationTarget")}>
              {(id) => (
                <select
                  id={id}
                  value={destinationTargetId}
                  onChange={(e) => setDestinationTargetId(e.target.value)}
                  className={CONTROL_CLASS}
                >
                  <option value="">{t("form.destinationTarget.placeholder")}</option>
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
            /* The label, the placeholder and the hint all follow the CHECK
               TYPE (QA scope 4, finding #10): tcp defaults a missing port to
               80, udp has no default at all, icmp and mtr ignore one, and dns
               and http cannot take an external destination in the first
               place. One field with one caption named the wrong thing for
               four of the six. */
            <FieldLabel label={t(adhocLabelKey)}>
              {(id) => (
                <>
                  <input
                    id={id}
                    /* Belt and braces on the name (QA round 4, finding #16): the
                       visible <label> is the association, and this survives a
                       future refactor that moves the field out of FieldLabel. */
                    aria-label={t(adhocLabelKey)}
                    value={destinationAddress}
                    placeholder={adhocPlaceholder(adhocShape, t("adhoc.or"))}
                    onChange={(e) => setDestinationAddress(e.target.value)}
                    className={CONTROL_CLASS}
                  />
                  <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t(adhocHintKey)}</p>
                </>
              )}
            </FieldLabel>
          ) : null}
        </div>

        {/* Recomputed every render, so switching the check type re-judges the
            address already in the box and the mismatch appears at once rather
            than surviving the switch in silence. */}
        {adhocIssue ? (
          <p role="alert" className="text-sm text-health-bad">
            {adhocIssue === "shape" ? tv("adhoc.address") : t(`adhoc.mismatch.${adhocIssue}` as DiagnosticsKey)}
          </p>
        ) : null}

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
              title={t("form.resetPickers")}
              aria-label={t("form.resetPickers")}
              onClick={() => {
                setSourcesAll(true);
                setDestinationsAll(true);
              }}
            >
              {t("form.resetPickers.label")}
            </Button>
          ) : null}
          {/* "~" flags this as an estimate: the server is the only real
              arbiter, and on an overlapping selection this self-excluded
              number can even read as under the limit while the raw S×D
              product the server actually gates on is over it. */}
          <span className={cn("nums text-sm", overLimit || noPairs ? "text-health-bad" : "text-muted-foreground")}>
            {t(`pairs.${countForm(locale, pairCount)}` as DiagnosticsKey, { count: pairCount })}
            {overLimit
              ? t("pairs.overLimit", {
                  max: MAX_PAIRS,
                  sources: resolvedSources.length,
                  destinations: external ? 1 : resolvedDestinations.length,
                })
              : ""}
            {pairsReason ? t(pairsReason) : ""}
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
          /* adhocIssue joins the list for the same reason incompleteDestination is on it: the
             refusal is already certain, so sending the body only collects it later. */
          {...guard} disabled={overLimit || noPairs || incompleteDestination || adhocIssue !== null || writesDisabled}
          className="self-start"
        >
          {t("form.submit")}
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
  const t = useT(diagnosticsDict);
  const tv = useT(validationDict);
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
      setErrors({ name: t("definition.nameRequired") });
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
      setErrors({ destination: tv("adhoc.address") });
      return;
    }
    setSaving(true);
    try {
      const created = await createCheck({ ...draft, name: name.trim() });
      setSaved(created.name);
      setName("");
    } catch (err) {
      if (!(err instanceof ApiError)) {
        setErrors({ form: t("definition.saveFailed") });
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
        <FieldLabel label={t("definition.name")}>
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
          {t("definition.save")}
        </Button>
      </div>
      <p className="max-w-prose text-xs leading-relaxed text-muted-foreground">{t("definition.hint")}</p>
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
          {t("definition.saved", { name: saved })}
        </p>
      ) : null}
    </div>
  );
}

/** The locale is required: a bare toLocaleString() reorders the date and swaps in AM/PM from
 *  whatever the browser was installed in, which is not what a Russian page promised. */
function fmtTime(timestamp: string | undefined, locale: Locale): string {
  if (!timestamp) return "—";
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : stampFull(d, locale);
}

/* RUN_STATUSES is the store's own terminal-plus-live set, in the order a run
   moves through it; it is the same vocabulary STATUS_VARIANT above keys on and
   the same the server accepts in ?status=. */
const RUN_STATUSES = ["pending", "running", "succeeded", "partial", "failed", "cancelled"] as const;

/** FILTER_CLASS is CONTROL_CLASS at list-header size — a filter is a smaller thing than a form field. */
const FILTER_CLASS =
  "h-8 rounded-md border border-border-strong bg-transparent px-2 text-xs " +
  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

/**
 * okOfTotal is a run row's "1/2 ok", and an em dash when either counter did not
 * arrive.
 *
 * The interpolation prints whatever it is handed: a row missing pairTotal read
 * "1/undefined ok" in the one number an operator scans a history list for. Zero
 * would be worse than the dash — "1/0 ok" is a claim, the dash is an absence.
 */
function okOfTotal(t: Translate<DiagnosticsKey>, r: RunSummary): string {
  if (!Number.isFinite(r.pairOk) || !Number.isFinite(r.pairTotal)) return "—";
  return t("history.run.okOfTotal", { ok: r.pairOk, total: r.pairTotal });
}

function HistoryList({
  runs,
  engaged,
  filtered,
  scope,
}: {
  runs: RunSummary[];
  engaged: boolean;
  filtered: boolean;
  /** The list's IDENTITY — the two filters and the viewed instant. See the
   *  pager's resetKey below. */
  scope: string;
}) {
  const t = useT(diagnosticsDict);
  const { locale } = useLocale();
  /* The history is as long as the fleet has been running diagnostics; the list
     is paged rather than scrolled forever (the owner's rule for every list).

     resetKey is the two filters and the Time Machine's instant, because each of
     them REPLACES the rows rather than extending them. Without it, a reader on
     page 9 of the whole history who narrowed the status to "failed" landed on
     page 2 of twelve rows — a page they never asked for, holding the tail of a
     list they had not read the head of. "Load older" is the other case and is
     deliberately not in the key: it appends to the same list, and the page the
     reader is on still addresses the same runs. */
  const pager = usePager(runs, { resetKey: scope });
  if (runs.length === 0) {
    /* A filter that matched nothing is a different fact from "nobody has ever
       run one", and the run form above is not the remedy for it. Checked
       BEFORE the engaged branch: a filter is something the reader just did,
       and it is the likelier cause of the two. */
    if (filtered) {
      return (
        <div className="flex flex-col items-center gap-3 px-6 py-14 text-center">
          <span
            aria-hidden="true"
            className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
          >
            <ClipboardList className="size-5" />
          </span>
          <p className="text-sm font-medium">{t("history.emptyFiltered.title")}</p>
          <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">{t("history.emptyFiltered.body")}</p>
        </div>
      );
    }
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
          <p className="text-sm font-medium">{t("history.emptyAt.title")}</p>
          <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">{t("history.emptyAt.body")}</p>
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
        <p className="text-sm font-medium">{t("history.empty.title")}</p>
        <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">{t("history.empty.body")}</p>
      </div>
    );
  }
  return (
    <>
    <ul className="mt-4 divide-y divide-border">
      {pager.visible.map((r) => (
        <li key={r.id} className="flex flex-wrap items-center gap-3 py-3 text-sm">
          <a href={withAtParam(`/diagnostics/runs/${r.id}`)} className="font-medium text-primary hover:underline">
            {r.id}
          </a>
          <StatusBadge status={r.status || "—"} />
          <span className="text-xs uppercase tracking-wide text-muted-foreground">{r.type || "—"}</span>
          <span className="nums ml-auto text-xs text-muted-foreground">{okOfTotal(t, r)}</span>
          <span className="text-xs text-muted-foreground">{fmtTime(r.createdAt, locale)}</span>
        </li>
      ))}
    </ul>
    <Pager pager={pager} subject={t("history.subject")} className="px-0" />
    </>
  );
}

export function DiagnosticsPage() {
  const t = useT(diagnosticsDict);
  const { locale } = useLocale();
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
  /* "" is the no-filter value on both, and both are sent to the SERVER. */
  const [typeFilter, setTypeFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [history, setHistory] = useState<{ nextCursor: string; loading: boolean; error: unknown }>({
    nextCursor: "",
    loading: false,
    error: null,
  });

  /* The request TOKEN, and the race it settles: two filter changes in the same
     second are two in-flight GETs, and whichever the network answers last wins
     the list — so picking tcp and then mtr could leave tcp's rows on screen
     under a select reading "mtr", with no way to tell. Only the newest request
     may write; an older answer is dropped on arrival, error included. */
  const requestSeq = useRef(0);

  const loadRuns = useCallback(async (cursor?: string) => {
    const seq = (requestSeq.current += 1);
    setHistory((h) => ({ ...h, loading: true, error: null }));
    try {
      // "" is "no filter" and must not reach the query string: the server reads
      // an empty ?type= as no filter too, but sending it is a lie about what
      // was asked for.
      const page = await getRuns({ cursor, type: typeFilter || undefined, status: statusFilter || undefined });
      if (seq !== requestSeq.current) return;
      // A cursor-less call is page one -- replace, rather than append, so a
      // remount or a future refresh does not just keep growing the list.
      setRuns((prev) => (cursor ? [...prev, ...page.runs] : page.runs));
      setHistory({ nextCursor: page.nextCursor, loading: false, error: null });
    } catch (err) {
      if (seq !== requestSeq.current) return;
      setHistory({ nextCursor: "", loading: false, error: err });
    }
  }, [typeFilter, statusFilter]);

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
      timeMachine
      title={t("title")}
      /* {at} lands INSIDE a translated sentence, so it takes that sentence's
         language — lib/i18n's localeTag. Computed here, never formatted by the
         dictionary (QA scope 2, finding #8). */
      description={at ? t("description.at", { at: at.toLocaleString(localeTag(locale)) }) : t("description")}
    >
      {canCreate ? (
        <RunForm nodeNames={nodeNames} canReadTargets={can("targets:read")} canWriteChecks={can("checks:write")} />
      ) : (
        <Card role="status" className="p-6">
          <p className="text-sm font-medium">{t("gate.title")}</p>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("gate.body")}</p>
        </Card>
      )}

      <Card asChild className="p-6">
        <section>
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h2 className="text-sm font-semibold">{t("history.title")}</h2>
            {/* Wired to the SERVER's own ?type=&status= (runs.go's
                handleRunsList), not to a client-side pass over the loaded
                pages: filtering in the browser would silently mean "of the
                hundred runs already fetched", which is the trap the Time
                Machine note beside it exists to admit to. A filter change
                re-fetches page one — loadRuns's identity depends on both,
                and the cursor-less call replaces rather than appends.
                Selects, not Segmented: seven options a side. */}
            <div className="flex flex-wrap items-center gap-2">
              <select
                aria-label={t("history.filter.type.aria")}
                value={typeFilter}
                onChange={(e) => setTypeFilter(e.target.value)}
                className={FILTER_CLASS}
              >
                <option value="">{t("history.filter.type.all")}</option>
                {CHECK_TYPES.map((ct) => (
                  <option key={ct} value={ct}>
                    {ct}
                  </option>
                ))}
              </select>
              <select
                aria-label={t("history.filter.status.aria")}
                value={statusFilter}
                onChange={(e) => setStatusFilter(e.target.value)}
                className={FILTER_CLASS}
              >
                <option value="">{t("history.filter.status.all")}</option>
                {RUN_STATUSES.map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </select>
            </div>
          </div>
          {dbResolved && !dbConfigured ? (
            <p role="status" className="mt-1 text-xs leading-relaxed text-muted-foreground">
              {t("history.notPersisted")}
            </p>
          ) : null}
          {at ? (
            <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("history.atNote")}</p>
          ) : null}

          {history.error ? (
            <p role="alert" className="mt-3 text-sm text-health-bad">
              {/* problem+json is the server's own sentence — verbatim. */}
              {history.error instanceof ApiError
                ? (history.error.problem.detail ?? history.error.problem.title)
                : t("history.unavailable")}
            </p>
          ) : null}

          <HistoryList
            runs={visibleRuns}
            engaged={at !== null}
            filtered={typeFilter !== "" || statusFilter !== ""}
            scope={`${typeFilter}|${statusFilter}|${at?.toISOString() ?? ""}`}
          />

          {runs.length > 0 ? (
            <div className="mt-4 flex justify-center">
              <Button
                variant="outline"
                size="sm"
                disabled={history.nextCursor === "" || history.loading}
                onClick={() => loadRuns(history.nextCursor)}
              >
                {history.loading ? t("history.loadingOlder") : t("history.loadOlder")}
              </Button>
            </div>
          ) : null}
        </section>
      </Card>
    </PageShell>
  );
}
