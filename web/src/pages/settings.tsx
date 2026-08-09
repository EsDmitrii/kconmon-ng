import { useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { MaintenanceRow } from "@/components/maintenance";
import {
  ApiError,
  createWebhook,
  deleteWebhook,
  exportConfig,
  getConfig,
  getMaintenance,
  importConfig,
  listWebhooks,
  testWebhook,
  updateWebhook,
} from "@/lib/api";
// Read in each mutating component rather than threaded down as a prop, the same
// way pages/targets.tsx does it: it is a context read, it costs nothing, and
// every affordance then states its own dependency. See lib/timemachine.tsx for
// the rule these all follow — a permission decides whether a control EXISTS,
// this decides whether it is usable right now.
import { useWriteGuard } from "@/lib/timemachine";
import type {
  ConfigBundle,
  ConfigImportCollectionResult,
  ConfigImportResult,
  Webhook,
  WebhookEvent,
  WebhookRequest,
} from "@/lib/types";
import { CHECKBOX_CLASS, cn } from "@/lib/utils";

/**
 * The Settings page (M7 Task 10, plan Decision 10).
 *
 * WHAT IS HERE, and why the nav description promises more than this file
 * delivers. NAV_ITEMS calls /settings "Auth, RBAC, retention, maintenance,
 * webhooks, export/import", and that sentence predates the decision this page
 * implements. What ships is:
 *
 *   1. Webhooks — full CRUD plus test delivery, behind webhooks:manage.
 *   2. Configuration export / import — behind settings:write.
 *   3. About this console — read-only, everyone who can see the page.
 *
 * And what deliberately does NOT ship here, each for its own reason:
 *
 *   RBAC and token administration. There is no UI for either, in this file or
 *   anywhere else, and adding one is not a page-layout decision: rbac:manage
 *   and tokens:manage are the credential-issuing permissions, a token is shown
 *   exactly once at creation, and a half-built version of that surface is worse
 *   than none. The nav sentence keeps saying "RBAC" until MILESTONES' as-built
 *   pass rewrites it; this comment is the honest record in the meantime.
 *
 *   Maintenance windows — CREATING them. They already have surfaces — the
 *   MaintenanceBar on /investigate, /explore and the object cards
 *   (components/maintenance.tsx) — where a window is declared next to the chart
 *   it explains. A second create form here would be a second place to get the
 *   same thing wrong, so About LINKS to those surfaces instead.
 *
 *   LISTING AND DELETING THEM DOES SHIP HERE (QA round 3, finding #9), and it
 *   is the exception that proves the paragraph above rather than a reversal of
 *   it. Every MaintenanceBar is bounded to the RANGE of the chart it sits under
 *   — that is what makes the bands honest — so a window declared for next
 *   Tuesday appeared on no surface in this console the moment it was saved: QA
 *   created one and then could not find it, edit it or delete it anywhere. An
 *   orphaned write is worse than a missing form. So the section below is a
 *   deliberately minimal one: the UNBOUNDED list (GET /api/v1/maintenance with
 *   no from/to), each row's scope, reason and span, and the same confirm-delete
 *   every destructive control in this console wears. No create form — creation
 *   stays next to the chart it explains, exactly as the paragraph above says.
 *
 *   Retention numbers. GET /api/v1/config does not serve them (httpapi's
 *   handleConfig: auth mode/role/loginPath, the anonymous banner flag, and
 *   three `configured` booleans — that is the whole body). Inventing a route to
 *   fetch them is a backend change this task does not own, so About says
 *   plainly that the browser is not told, and names where the numbers live.
 *
 * DEGRADED MODE. Neither gated section pre-checks `database.configured` the way
 * pages/targets.tsx does. The 503 both sets of routes answer with names
 * console.database.mode in its own detail, in better words than a second copy
 * here would, and rendering the server's sentence verbatim keeps one authority
 * on what is missing instead of two that can drift.
 */

/* ── shared bits ────────────────────────────────────────────────────────── */

function queryErrorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiError ? (error.problem.detail ?? error.problem.title) : fallback;
}

function fmtTime(timestamp?: string | null): string {
  if (!timestamp) return "—";
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : d.toLocaleString();
}

function fieldClasses(invalid: boolean): string {
  return cn(
    "h-9 rounded-md border bg-transparent px-3 text-[13px]",
    invalid ? "border-health-bad" : "border-border-strong",
  );
}

function SectionCard({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Card asChild className="p-6">
      <section>
        <h2 className="text-sm font-semibold">{title}</h2>
        {children}
      </section>
    </Card>
  );
}

function ErrorLine({ children, id }: { children: ReactNode; id?: string }) {
  return (
    <p id={id} role="alert" className="mt-3 text-sm leading-relaxed text-health-bad">
      {children}
    </p>
  );
}

/* ── webhooks ───────────────────────────────────────────────────────────── */

/**
 * WEBHOOK_EVENTS is the closed subscribable set as of THIS build, in the order
 * the checkbox group renders it: the incident family first, then the alert
 * family, each grouped so the list reads the way the two payload shapes divide.
 * It is typed as WebhookEvent[] rather than inferred, deliberately — the array
 * has to fail typechecking the day the enum narrows, and an inferred one never
 * would.
 *
 * There is no runtime enum to derive this from: openapi-typescript emits
 * WebhookEvent as a TYPE union, which vanishes at build time.
 */
const WEBHOOK_EVENTS: WebhookEvent[] = [
  "incident.created",
  "incident.resolved",
  "incident.reopened",
  "alert.fired",
  "alert.resolved",
];

/**
 * TEST_REFETCH_DELAY_MS is how long the page waits after a 202 before re-reading
 * the endpoint list.
 *
 * It is a nicety, not a mechanism. The delivery ladder is asynchronous and the
 * console makes no promise about when the outcome row is written, so this
 * refetch is a convenience for the common case where the receiver answers
 * immediately — the copy next to it says the outcome lands on the row either
 * way, and a slower receiver's result arrives on the next natural read rather
 * than being lost. Kept well under vitest's waitFor budget so the tests
 * exercise the real timer instead of a fake clock.
 */
export const TEST_REFETCH_DELAY_MS = 400;

/** WebhookDraft is the form's state. `secret` is a plain string here and a
 *  three-state value on the wire — webhookRequestFrom is the one place that
 *  translation happens. */
export interface WebhookDraft {
  name: string;
  url: string;
  events: WebhookEvent[];
  enabled: boolean;
  secret: string;
}

/**
 * webhookRequestFrom turns the draft into the body that goes on the wire, and
 * the ONLY interesting thing it does is with a blank secret box: it omits the
 * KEY, rather than sending "" or null.
 *
 * That is the API's rule, not a preference (docs/console-api.yaml's PUT
 * description): absent means KEEP the stored ciphertext, "" is 422 on both
 * create and update because neither "keep" nor "clear" would be more than a
 * guess about what an operator meant by an empty box. So an operator editing a
 * URL never has to re-type a signing key they may not have, and a create with a
 * blank box is refused CLIENT-side (see WebhookForm) rather than sent to
 * collect a 422 that says the same thing one round trip later.
 *
 * The secret is not trimmed. Leading and trailing bytes are part of a key.
 */
export function webhookRequestFrom(draft: WebhookDraft): WebhookRequest {
  const req: WebhookRequest = {
    name: draft.name,
    url: draft.url,
    events: draft.events,
    enabled: draft.enabled,
  };
  if (draft.secret !== "") req.secret = draft.secret;
  return req;
}

/* ── 422 → form field ───────────────────────────────────────────────────────
   The same treatment pages/targets.tsx's three forms already have, applied to
   this one in QA round 5 (finding #13). A refused endpoint used to render its
   whole reason as one banner above the buttons — "webhook: url ... must start
   with http:// or https://" printed a long way from the URL box, with nothing
   marking which of the four fields was wrong. On a five-field form that is a
   reading exercise.

   Problem+json carries NO field pointer (docs/console-api.yaml's Problem has
   exactly four members), so the field is recovered from the noun the server's
   own message leads with: store/webhooks.go builds every message as
   "webhook: <field> ...". This is a PRESENTATION heuristic — a detail matching
   no phrase still renders verbatim as a form-level error, which is what the
   banner already did. */

export type WebhookField = "name" | "url" | "secret" | "events";

/** Most specific first, and "event" (singular) rather than "events" so the
 *  indexed form store writes for a bad member — "events[0]: ..." — lands here
 *  too. `url` before `name` because a duplicate-name 422 says "webhook names
 *  are unique" and carries no url, while a bad url message carries no name. */
export const WEBHOOK_FIELD_PHRASES: readonly (readonly [WebhookField, string])[] = [
  ["secret", "secret"],
  ["events", "event"],
  ["url", "url"],
  ["name", "name"],
];

/** webhookFieldForDetail returns the field a 422 names, or null when the
 *  message names none this form has. Exported for the same reason
 *  targets.tsx's fieldForDetail is: the table is the thing worth pinning. */
export function webhookFieldForDetail(detail: string): WebhookField | null {
  const haystack = detail.toLowerCase();
  for (const [field, phrase] of WEBHOOK_FIELD_PHRASES) {
    if (haystack.includes(phrase)) return field;
  }
  return null;
}

/** lastStatusTone maps the endpoint row's own string onto a badge colour. The
 *  string itself is always rendered verbatim; this only decides which of the
 *  four tokens carries it, and an unrecognised value gets "unknown" rather than
 *  being optimistically read as success. */
function lastStatusTone(lastStatus: string): "neutral" | "ok" | "bad" | "unknown" {
  if (lastStatus === "") return "neutral";
  if (lastStatus === "ok") return "ok";
  if (lastStatus.startsWith("failed")) return "bad";
  return "unknown";
}

function WebhookForm({ initial, onDone }: { initial?: Webhook; onDone: () => void }) {
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's
     useWriteGuard (QA round 2, finding #18). Spread it onto the control. */
  const guard = useWriteGuard();
  const secretId = useId();
  const [draft, setDraft] = useState<WebhookDraft>({
    name: initial?.name ?? "",
    url: initial?.url ?? "",
    events: initial?.events ?? [],
    enabled: initial?.enabled ?? true,
    secret: "",
  });
  /* The in-flight guard, not just a disabled look (QA round 5, finding #17):
     begin() is a REF write, so three clicks in one task produce one request.
     hooks/use-submit-guard.ts says why a useState flag cannot do this. */
  const { submitting, begin, end } = useSubmitGuard();
  const [error, setError] = useState<string>();
  /* Which field the last refusal named, or null for a form-level one
     (finding #13). Separate from `error`, which still carries the server's
     words verbatim — the field only decides WHERE they render. */
  const [errorField, setErrorField] = useState<WebhookField | null>(null);
  const errorId = useId();

  /** fail routes one refusal: the message always shows, and it shows AT the
   *  field when the server named one. */
  function fail(message: string, field: WebhookField | null) {
    setError(message);
    setErrorField(field);
  }

  /** invalid marks a field for aria-invalid and the red border. */
  const invalid = (field: WebhookField) => errorField === field;
  /** describedBy points a field's assistive description at the one message,
   *  so a screen reader reading the URL box hears why it was refused. */
  const describedBy = (field: WebhookField) => (errorField === field ? errorId : undefined);

  function toggleEvent(event: WebhookEvent) {
    setDraft((d) => ({
      ...d,
      events: d.events.includes(event) ? d.events.filter((e) => e !== event) : [...d.events, event],
    }));
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    setErrorField(null);
    // The one rule mirrored client-side. Everything else — the name charset,
    // the url scheme, an empty event list — is left to the server's 422, whose
    // wording is better than a second copy of the rule would be. This one is
    // mirrored because a blank box on CREATE is the single case where the
    // wire body would be ambiguous rather than merely invalid.
    if (!initial && draft.secret === "") {
      fail("A secret is required: every delivery is signed, so an endpoint without one cannot exist.", "secret");
      return;
    }
    if (!begin()) return;
    try {
      const req = webhookRequestFrom(draft);
      if (initial) await updateWebhook(initial.id, req);
      else await createWebhook(req);
      await qc.invalidateQueries({ queryKey: ["webhooks"] });
      onDone();
    } catch (err) {
      const message = queryErrorMessage(err, "Failed to save the endpoint");
      // A non-ApiError has no server words to route, so it stays form-level.
      fail(message, err instanceof ApiError ? webhookFieldForDetail(message) : null);
      end();
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-4">
        <h3 className="text-sm font-semibold">{initial ? `Edit ${initial.name}` : "New endpoint"}</h3>
        <div className="grid gap-4 sm:grid-cols-2">
          <label className="flex flex-col gap-1 text-[13px]">
            <span className="text-muted-foreground">Name</span>
            <input
              value={draft.name}
              placeholder="pagerduty"
              aria-invalid={invalid("name") || undefined}
              aria-describedby={describedBy("name")}
              onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
              className={fieldClasses(invalid("name"))}
            />
          </label>
          <label className="flex flex-col gap-1 text-[13px]">
            <span className="text-muted-foreground">URL</span>
            <input
              value={draft.url}
              placeholder="https://hooks.example.test/incidents"
              aria-invalid={invalid("url") || undefined}
              aria-describedby={describedBy("url")}
              onChange={(e) => setDraft((d) => ({ ...d, url: e.target.value }))}
              className={fieldClasses(invalid("url"))}
            />
          </label>
        </div>

        {/* The event list has no single input to mark, so the GROUP carries it:
            aria-invalid on the fieldset, which is what a "no events selected"
            422 is actually about. */}
        <fieldset
          className="flex flex-col gap-2 text-[13px]"
          aria-invalid={invalid("events") || undefined}
          aria-describedby={describedBy("events")}
        >
          <legend className={cn("text-muted-foreground", invalid("events") && "text-health-bad")}>Events</legend>
          <div className="flex flex-wrap gap-4">
            {WEBHOOK_EVENTS.map((event) => (
              <label key={event} className="flex items-center gap-2">
                <input
                  type="checkbox"
                  checked={draft.events.includes(event)}
                  onChange={() => toggleEvent(event)}
                  className={CHECKBOX_CLASS}
                />
                <span>{event}</span>
              </label>
            ))}
          </div>
        </fieldset>

        <label className="flex items-center gap-2 text-[13px]">
          <input
            type="checkbox"
            checked={draft.enabled}
            onChange={(e) => setDraft((d) => ({ ...d, enabled: e.target.checked }))}
            className={CHECKBOX_CLASS}
          />
          <span>Enabled</span>
        </label>

        <div className="flex max-w-md flex-col gap-1 text-[13px]">
          <label htmlFor={secretId} className="text-muted-foreground">
            Secret
          </label>
          <input
            id={secretId}
            type="password"
            value={draft.secret}
            aria-invalid={invalid("secret") || undefined}
            aria-describedby={invalid("secret") ? `${errorId} ${secretId}-help` : `${secretId}-help`}
            onChange={(e) => setDraft((d) => ({ ...d, secret: e.target.value }))}
            className={fieldClasses(invalid("secret"))}
          />
          {/* Write-only, in both directions: the API never returns a secret, so
              this box starts empty even when editing an endpoint that has one. */}
          <span id={`${secretId}-help`} className="text-xs leading-relaxed text-muted-foreground">
            {initial
              ? "Leave blank to keep the current secret. Typing here replaces it."
              : "Required — every delivery is signed with it. It is never shown again."}
          </span>
        </div>

        {/* One message, one id. It keeps its place above the buttons whether or
            not a field claimed it — the server's words are never moved out of
            reading order — and aria-describedby points the marked field here
            rather than duplicating the text beside it. */}
        {error ? <ErrorLine id={errorId}>{error}</ErrorLine> : null}

        <div className="flex gap-2">
          <Button type="submit" loading={submitting} {...guard}>
            {initial ? "Save endpoint" : "Create endpoint"}
          </Button>
          {/* Cancel closes a form and touches nothing, so it stays live even
              while engaged — a form an operator cannot dismiss would be the
              mode holding the page hostage. */}
          <Button type="button" variant="outline" onClick={onDone}>
            Cancel
          </Button>
        </div>
      </form>
    </Card>
  );
}

function WebhookRow({ hook, onEdit }: { hook: Webhook; onEdit: () => void }) {
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's
     useWriteGuard (QA round 2, finding #18). Spread it onto the control. */
  const guard = useWriteGuard();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [queued, setQueued] = useState(false);
  const [error, setError] = useState<string>();
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => () => clearTimeout(timer.current), []);

  async function handleDelete() {
    setBusy(true);
    setError(undefined);
    try {
      await deleteWebhook(hook.id);
      await qc.invalidateQueries({ queryKey: ["webhooks"] });
    } catch (err) {
      setError(queryErrorMessage(err, "Failed to delete the endpoint"));
      setBusy(false);
      setConfirming(false);
    }
  }

  async function handleTest() {
    setBusy(true);
    setError(undefined);
    setQueued(false);
    try {
      await testWebhook(hook.id);
    } catch (err) {
      // A 503 here names console.webhooks.encryptionKey — the key the ping has
      // to be signed with. Worth every word the server wrote.
      setError(queryErrorMessage(err, "Failed to enqueue the test delivery"));
      setBusy(false);
      return;
    }
    setBusy(false);
    setQueued(true);
    // 202 means QUEUED, so nothing here may claim an outcome. The refetch below
    // is what eventually shows one, on the row, as lastStatus.
    timer.current = setTimeout(() => {
      void qc.invalidateQueries({ queryKey: ["webhooks"] });
    }, TEST_REFETCH_DELAY_MS);
  }

  return (
    <li className="flex flex-wrap items-center gap-3 py-3 text-sm">
      <span className="font-medium">{hook.name}</span>
      <span className="truncate text-xs text-muted-foreground">{hook.url}</span>
      {hook.events.map((event) => (
        <Badge key={event} variant="neutral">
          {event}
        </Badge>
      ))}
      <Badge variant={hook.enabled ? "ok" : "unknown"}>{hook.enabled ? "enabled" : "disabled"}</Badge>
      {/* hasSecret is always true for a stored row, so this reads as the
          contract statement the API intends — "this endpoint signs its
          deliveries" — rather than as a question with two answers. An imported
          endpoint is the one case that can be false (plan Decision 9). */}
      <Badge variant={hook.hasSecret ? "neutral" : "bad"}>{hook.hasSecret ? "signed" : "no secret"}</Badge>
      <span data-testid="last-status" className="text-xs">
        <Badge variant={lastStatusTone(hook.lastStatus)}>{hook.lastStatus === "" ? "—" : hook.lastStatus}</Badge>
      </span>
      <span className="text-xs text-muted-foreground">{fmtTime(hook.lastAttempt)}</span>
      {hook.failures > 0 ? (
        <span className="text-xs text-health-bad">{hook.failures} consecutive failures</span>
      ) : null}

      <span className="ml-auto flex flex-wrap items-center gap-2">
        {confirming ? (
          <>
            <Button size="sm" variant="outline" loading={busy} {...guard} onClick={handleDelete}>
              Confirm delete {hook.name}
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
              Cancel
            </Button>
          </>
        ) : (
          <>
            <Button size="sm" variant="ghost" {...guard} onClick={handleTest}>
              Send test to {hook.name}
            </Button>
            <Button size="sm" variant="ghost" {...guard} onClick={onEdit}>
              Edit {hook.name}
            </Button>
            <Button size="sm" variant="ghost" {...guard} onClick={() => setConfirming(true)}>
              Delete {hook.name}
            </Button>
          </>
        )}
      </span>
      {queued ? (
        <span role="status" className="w-full text-xs text-muted-foreground">
          Test queued; the outcome lands on this row.
        </span>
      ) : null}
      {error ? (
        <span role="alert" className="w-full text-xs leading-relaxed text-health-bad">
          {error}
        </span>
      ) : null}
    </li>
  );
}

function WebhooksSection() {
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's
     useWriteGuard (QA round 2, finding #18). Spread it onto the control. */
  const guard = useWriteGuard();
  const [editing, setEditing] = useState<{ mode: "none" } | { mode: "create" } | { mode: "edit"; hook: Webhook }>({
    mode: "none",
  });
  const query = useQuery({ queryKey: ["webhooks"], queryFn: listWebhooks });
  const hooks = query.data?.webhooks ?? [];

  return (
    <div className="flex flex-col gap-4">
      {editing.mode === "none" ? (
        <div>
          <Button size="sm" {...guard} onClick={() => setEditing({ mode: "create" })}>
            New endpoint
          </Button>
        </div>
      ) : (
        <WebhookForm
          key={editing.mode === "edit" ? editing.hook.id : "create"}
          initial={editing.mode === "edit" ? editing.hook : undefined}
          onDone={() => setEditing({ mode: "none" })}
        />
      )}

      <SectionCard title="Webhooks">
        <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
          Outbound endpoints the console signs and POSTs incident events to. Delivery is asynchronous with a retry
          ladder, so the last outcome below is what actually happened, not what was attempted.
        </p>
        {query.isError ? <ErrorLine>{queryErrorMessage(query.error, "Webhooks are unavailable")}</ErrorLine> : null}
        {/* isPending / isSuccess, not !isLoading && !isError: a paused retry
            (react-query pauses while the browser thinks it is offline) is
            pending-but-not-fetching, and the old guard would present "no
            endpoints" as a settled answer. M7 final-gate finding. */}
        {query.isPending ? (
          <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
            <span className="sr-only">Loading…</span>
            <Skeleton className="h-10 w-full" />
          </div>
        ) : null}
        {query.isSuccess && hooks.length === 0 ? (
          <p className="px-1 py-10 text-center text-xs text-muted-foreground">
            No endpoints yet. Nothing is being notified.
          </p>
        ) : null}
        {hooks.length > 0 ? (
          <ul aria-label="Webhook endpoints" className="mt-4 divide-y divide-border">
            {hooks.map((hook) => (
              <WebhookRow key={hook.id} hook={hook} onEdit={() => setEditing({ mode: "edit", hook })} />
            ))}
          </ul>
        ) : null}
      </SectionCard>
    </div>
  );
}

/* ── maintenance windows (QA round 3, finding #9) ───────────────────────── */

/**
 * MaintenanceWindowsSection is the ONLY unbounded view of the declared windows
 * in this console, and the reason it exists is at the top of this file.
 *
 * THE REQUEST CARRIES NO RANGE. Every other maintenance request in the console
 * passes from/to, because every other one is drawing bands on a chart. This one
 * is answering "what has been declared, ever" — a window whose whole span is in
 * the future is exactly the row an operator comes here for, and a range would
 * hide it again. Nor is `scope` sent: the point is every scope at once.
 *
 * It is gated on maintenance:WRITE rather than :read, the same HIDE the
 * webhooks section makes. A subject who can only read windows already sees them
 * beside the charts they explain, in context; this list exists for the one act
 * that has nowhere else to happen, so without the permission to perform it the
 * section is not rendered and NOTHING is requested.
 */
function MaintenanceWindowsSection() {
  const qc = useQueryClient();
  const query = useQuery({ queryKey: ["settings", "maintenance"], queryFn: () => getMaintenance() });
  const windows = query.data?.windows ?? [];

  const onChanged = () => {
    void qc.invalidateQueries({ queryKey: ["settings", "maintenance"] });
    // The range-bounded lists elsewhere hold the same rows; a delete here must
    // not leave a card in another tab drawing a band for a window that is gone.
    void qc.invalidateQueries({ queryKey: ["maintenance"] });
    void qc.invalidateQueries({ queryKey: ["investigate", "maintenance"] });
  };

  return (
    <SectionCard title="Maintenance windows">
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
        Every declared window, with no time range — including the ones entirely in the future, which the bars beside the
        charts cannot show because those are bounded to what the chart plots. Declaring a window still happens next to
        the chart it explains, on{" "}
        <a href="/investigate" className="text-primary hover:underline">
          Investigate
        </a>{" "}
        or{" "}
        <a href="/explore" className="text-primary hover:underline">
          Explore
        </a>
        ; this list is for finding and removing one.
      </p>
      {query.isError ? (
        <ErrorLine>{queryErrorMessage(query.error, "Maintenance windows are unavailable")}</ErrorLine>
      ) : null}
      {/* isPending / isSuccess, the same guard the webhooks list uses: a paused
          retry is pending-but-not-fetching, and presenting that as "none
          declared" would be a settled answer nobody gave. */}
      {query.isPending ? (
        <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
          <span className="sr-only">Loading…</span>
          <Skeleton className="h-10 w-full" />
        </div>
      ) : null}
      {query.isSuccess && windows.length === 0 ? (
        <p className="px-1 py-10 text-center text-xs text-muted-foreground">
          No maintenance windows have been declared.
        </p>
      ) : null}
      {windows.length > 0 ? (
        <ul aria-label="All maintenance windows" className="mt-4 divide-y divide-border">
          {windows.map((w) => (
            /* The SHARED row (components/maintenance.tsx): same confirm-delete,
               same compact stamp, same write guard. canWrite is true by
               construction — this whole section is behind maintenance:write. */
            <MaintenanceRow key={w.id} window={w} canWrite onChanged={onChanged} />
          ))}
        </ul>
      ) : null}
    </SectionCard>
  );
}

/* ── export / import ────────────────────────────────────────────────────── */

/** The ONE bundle version this build reads or writes, matching httpapi's
 *  exportBundleVersion. Checked for EQUALITY, never ">=": a bundle from a
 *  future console may describe collections this build has never heard of, and
 *  importing the recognised subset would be a partial restore presented as a
 *  complete one. */
export const BUNDLE_VERSION = 1;

export type BundleParse = { ok: true; bundle: ConfigBundle } | { ok: false; message: string };

/**
 * parseBundle is the WHOLE of the client's validation, and its shortness is the
 * point: it checks that the file is a JSON object and that it declares the
 * version this build reads. Nothing else.
 *
 * The server is the authority on everything past that — which collections
 * exist, whether a schedule's definition is in the bundle, whether a name
 * collides — and it reports all of it per item through the dry run. A second,
 * weaker copy of those rules here would reject bundles the console would have
 * accepted, and its verdicts would drift from the real ones the first time the
 * importer changed.
 *
 * The two checks that ARE here earn their place by being about the FILE rather
 * than its contents: an operator who picked the wrong file, or a bundle from a
 * console this one cannot speak to, should hear so without a round trip.
 */
export function parseBundle(text: string): BundleParse {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { ok: false, message: "That file is not valid JSON. A configuration bundle is the JSON this page exports." };
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { ok: false, message: "That file is valid JSON but not a bundle: a bundle is a JSON object." };
  }
  const version = (parsed as { version?: unknown }).version;
  if (version !== BUNDLE_VERSION) {
    return {
      ok: false,
      message:
        `This console reads bundle version ${BUNDLE_VERSION}; that file declares ` +
        `${JSON.stringify(version ?? null)}. Importing the part this build recognises would be a partial ` +
        `restore presented as a complete one, so it is refused.`,
    };
  }
  return { ok: true, bundle: parsed as ConfigBundle };
}

/** exportFilename names the DAY, not the instant: a bundle is a restore point
 *  an operator files away, and two exports on the same day overwriting each
 *  other in the downloads folder is the behaviour they expect from a
 *  date-stamped config dump. */
export function exportFilename(now: Date): string {
  return `kconmon-ng-config-${now.toISOString().slice(0, 10)}.json`;
}

/**
 * IMPORT_COLLECTIONS is the result table's row order, and it is the BUNDLE's
 * own dependency order (targets before the definitions that point at them,
 * definitions before their schedules) rather than alphabetical: the importer
 * walks them in this order, so a reader scanning down the table is reading the
 * sequence of events, and an error high up explains the skips below it.
 */
const IMPORT_COLLECTIONS: readonly (readonly [keyof Omit<ConfigImportResult, "dryRun">, string])[] = [
  ["targets", "Targets"],
  ["checkDefinitions", "Check definitions"],
  ["checkSchedules", "Check schedules"],
  ["alertRules", "Alert rules"],
  ["webhooks", "Webhooks"],
  ["maintenanceWindows", "Maintenance windows"],
];

function ImportNotes({ label, notes, tone }: { label: string; notes: ConfigImportCollectionResult["errors"]; tone: string }) {
  if (notes.length === 0) return null;
  return (
    <div className="mt-3">
      <p className={cn("text-xs font-medium", tone)}>{label}</p>
      <dl className="mt-1 flex flex-col gap-1 text-xs leading-relaxed">
        {notes.map((note, i) => (
          <div key={`${note.name}-${i}`} className="flex flex-wrap gap-x-2">
            <dt className="font-mono">{note.name}</dt>
            {/* Verbatim. The server names the item and says why in one
                sentence; paraphrasing it here would drop the half an operator
                needs to fix the bundle. */}
            <dd className="text-muted-foreground">{note.reason}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}

/* role="status", like the webhook row's "Test queued" line: the dry run fires
   the moment a file is chosen and this table is its whole answer, arriving
   asynchronously well below the file input. Without a live region the one
   sentence that matters — dry run, or applied — is silent. */
function ImportResultTable({ result }: { result: ConfigImportResult }) {
  return (
    <div role="status" className="mt-4">
      <p className="text-sm font-medium">
        {result.dryRun ? "Dry run — nothing was written." : "Applied — these writes happened."}
      </p>
      <div className="mt-2 overflow-x-auto">
        <table className="w-full text-left text-xs">
          <thead className="text-muted-foreground">
            <tr>
              <th scope="col" className="py-1 pr-4 font-medium">
                Collection
              </th>
              <th scope="col" className="py-1 pr-4 font-medium">
                Created
              </th>
              <th scope="col" className="py-1 pr-4 font-medium">
                Updated
              </th>
              <th scope="col" className="py-1 pr-4 font-medium">
                Skipped
              </th>
            </tr>
          </thead>
          <tbody className="divide-y divide-border">
            {IMPORT_COLLECTIONS.map(([key, label]) => {
              const c = result[key];
              return (
                <tr key={key} data-testid={`import-row-${key}`}>
                  <th scope="row" className="py-1.5 pr-4 font-normal">
                    {label}
                  </th>
                  <td className="py-1.5 pr-4 tabular-nums">{c.created}</td>
                  <td className="py-1.5 pr-4 tabular-nums">{c.updated}</td>
                  <td className="py-1.5 pr-4 tabular-nums">{c.skipped}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {IMPORT_COLLECTIONS.map(([key, label]) => {
        const c = result[key];
        if (c.errors.length === 0 && c.warnings.length === 0) return null;
        return (
          <div key={key} className="mt-4">
            <p className="text-xs font-semibold">{label}</p>
            <ImportNotes label="Errors" notes={c.errors} tone="text-health-bad" />
            <ImportNotes label="Warnings" notes={c.warnings} tone="text-health-warn" />
          </div>
        );
      })}
    </div>
  );
}

function ExportImportSection() {
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's
     useWriteGuard (QA round 2, finding #18). Spread it onto the control; the
     alias below is for the control that composes it with a local condition,
     which must be applied AFTER the spread to win. */
  const guard = useWriteGuard();
  const writesDisabled = guard.disabled;
  const fileId = useId();
  const [exporting, setExporting] = useState(false);
  const [exportError, setExportError] = useState<string>();
  const [bundle, setBundle] = useState<ConfigBundle>();
  /* The picked file's NAME, kept because the visually-hidden input no longer
     shows it (finding #8). Set even for a bundle that failed to parse: the
     operator needs to know WHICH file the error below is about. */
  const [fileName, setFileName] = useState<string>();
  const [importing, setImporting] = useState(false);
  const [importError, setImportError] = useState<string>();
  const [result, setResult] = useState<ConfigImportResult>();

  async function handleExport() {
    setExporting(true);
    setExportError(undefined);
    try {
      const b = await exportConfig();
      // Blob + object URL rather than navigating the tab to /api/v1/export: a
      // 403 or a 503 has to render on this page, and a navigation would replace
      // the console with raw problem+json in the address bar.
      const href = URL.createObjectURL(new Blob([JSON.stringify(b, null, 2)], { type: "application/json" }));
      const anchor = document.createElement("a");
      anchor.href = href;
      anchor.download = exportFilename(new Date());
      document.body.appendChild(anchor);
      anchor.click();
      anchor.remove();
      URL.revokeObjectURL(href);
    } catch (err) {
      setExportError(queryErrorMessage(err, "Failed to export the configuration"));
    }
    setExporting(false);
  }

  async function runImport(b: ConfigBundle, dryRun: boolean) {
    setImporting(true);
    setImportError(undefined);
    try {
      setResult(await importConfig(b, dryRun));
    } catch (err) {
      setImportError(queryErrorMessage(err, "The import was refused"));
    }
    setImporting(false);
  }

  async function handleFile(file: File | undefined) {
    setResult(undefined);
    setImportError(undefined);
    setBundle(undefined);
    setFileName(file?.name);
    if (!file) return;
    const parsed = parseBundle(await file.text());
    if (!parsed.ok) {
      setImportError(parsed.message);
      return;
    }
    setBundle(parsed.bundle);
    // ALWAYS the dry run first, without being asked. A bundle is the whole
    // declarative configuration, so the first thing an operator should see is
    // what it WOULD do — and since the dry-run and apply responses are
    // identical in shape, the preview is the apply's own prediction rather than
    // a separate estimate.
    await runImport(parsed.bundle, true);
  }

  return (
    <SectionCard title="Configuration export / import">
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
        Targets, check definitions, schedules, alert rules, webhook endpoints and maintenance windows — what was
        declared, never what was observed. A bundle never carries a webhook secret; imported endpoints arrive without
        one and stay unusable until a secret is set here.
      </p>

      <div className="mt-4 flex flex-col gap-2">
        {/* Export is a READ, so the Time Machine does not touch it. Engaging the
            Time Machine blocks WRITES to avoid the confusion of editing the
            fleet from a historical view; downloading the current configuration
            from that view changes nothing and hides nothing. (What it exports
            is the LIVE configuration either way — /api/v1/export takes no `at`,
            and config tables are not time-folded.) */}
        <div>
          <Button size="sm" loading={exporting} onClick={handleExport}>
            Export configuration
          </Button>
        </div>
        {exportError ? <ErrorLine>{exportError}</ErrorLine> : null}
      </div>

      <div className="mt-6 flex flex-col gap-2">
        <span className="text-[13px] text-muted-foreground">Configuration bundle</span>
        {/* The native file input is VISUALLY HIDDEN, not replaced (QA round 5,
            finding #8). `<input type="file">` renders as the browser's own
            chrome — a grey "Choose File / no file selected" that matches
            nothing else on this page and cannot be themed at all, so in dark
            mode it was a light rectangle in the middle of a dark card.

            sr-only rather than display:none or opacity-0: the element stays in
            the accessibility tree AND in the tab order, so the keyboard path
            is the real one (Tab to the input, Space to open the picker) and
            the label is only the POINTER affordance. `peer` carries the
            input's disabled and focus states onto the label, so there is one
            source of truth for both — the guard still disables the input
            itself, not just its skin. */}
        <div className="flex flex-wrap items-center gap-2">
          <input
            id={fileId}
            type="file"
            accept="application/json,.json"
            /* The accessible name stays the FIELD's name, not the button's
               text: the label element below is the pointer affordance ("Choose
               bundle…" is an instruction), while a screen reader user landing
               on the input needs to hear what the field IS. */
            aria-label="Configuration bundle"
            {...guard}
            onChange={(e) => void handleFile(e.target.files?.[0])}
            className="peer sr-only"
          />
          <label
            htmlFor={fileId}
            data-testid="bundle-file-label"
            className={cn(
              "inline-flex h-8 cursor-pointer items-center justify-center rounded-md border border-border-strong",
              "bg-transparent px-3 text-sm font-medium transition-colors duration-(--dur) ease-(--ease)",
              "hover:bg-accent hover:text-accent-foreground",
              "peer-focus-visible:ring-2 peer-focus-visible:ring-ring peer-focus-visible:ring-offset-2 peer-focus-visible:ring-offset-background",
              "peer-disabled:pointer-events-none peer-disabled:opacity-50",
            )}
          >
            Choose bundle…
          </label>
          {/* The name the native control used to show on the operator's
              behalf. Without it, a hidden input means a picked file leaves no
              trace at all until the dry-run table lands. */}
          <span data-testid="bundle-file-name" className="min-w-0 truncate text-xs text-muted-foreground">
            {fileName ?? "No file chosen"}
          </span>
        </div>
        <p className="max-w-prose text-xs leading-relaxed text-muted-foreground">
          Choosing a file runs a dry run immediately: it writes nothing and predicts, per collection, exactly what
          Apply would do.
        </p>
        <div>
          <Button
            size="sm"
            loading={importing}
            /* Enabled the moment a bundle is loaded, and NOT gated on what the
               dry run predicted: an all-zero result is a valid no-op (a bundle
               already applied), and a result carrying errors is still worth
               applying for the items that succeeded. The operator decides;
               this button does not. */
            {...guard} disabled={writesDisabled || bundle === undefined}
            onClick={() => bundle && void runImport(bundle, false)}
          >
            Apply import
          </Button>
        </div>
        {importError ? <ErrorLine>{importError}</ErrorLine> : null}
        {result ? <ImportResultTable result={result} /> : null}
      </div>
    </SectionCard>
  );
}

/* ── About ──────────────────────────────────────────────────────────────── */

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="text-sm">{children}</dd>
    </div>
  );
}

/**
 * subjectLine joins the subject's kind and display name, skipping whatever is
 * missing (QA round 5, finding #9). Exported so the rule is testable on its
 * own: "anonymous" is a real subject kind with no display name at all, and it
 * is the DEFAULT deployment, so the dangling-separator case was the one most
 * operators saw first.
 */
export function subjectLine(kind: string, displayName: string): string {
  return [kind, displayName].map((s) => s.trim()).filter((s) => s !== "").join(" · ");
}

function AboutSection() {
  const { me } = useAuth();
  const { data: config } = useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });
  const mode = config?.auth.mode ?? "—";
  const roles = me?.subject.roles ?? [];

  return (
    <SectionCard title="About this console">
      <dl className="mt-3 grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <Fact label="Auth mode">{mode}</Fact>
        <Fact label="Your roles">{roles.length > 0 ? roles.join(", ") : "—"}</Fact>
        {/* Empty segments are DROPPED, not rendered as a gap — the same
            treatment lib/investigation-sources.ts's auditDetailLine got in
            round 3, applied here in round 5 (finding #9). An anonymous subject
            has no displayName, and the fixed template printed the separator
            anyway: "anonymous · " reads as a name that failed to load. A
            separator is a joint between two things. */}
        <Fact label="Your subject">{me ? subjectLine(me.subject.kind, me.subject.displayName) : "—"}</Fact>
        <Fact label="Controller">{config?.controller.configured ? "configured" : "not configured"}</Fact>
        <Fact label="Prometheus">{config?.prometheus.configured ? "configured" : "not configured"}</Fact>
        <Fact label="Database">{config?.database.configured ? "configured" : "not configured"}</Fact>
      </dl>

      {/* The role SOURCE differs by mode, and only in anonymous mode is it a
          config value the browser is told. In local/oidc mode the roles above
          come from the authenticated subject, which is what "Your roles"
          already says. */}
      {mode === "anonymous" ? (
        <p className="mt-4 max-w-prose text-xs leading-relaxed text-muted-foreground">
          Anonymous mode: every unauthenticated request is the {config?.auth.role} role
          (console.auth.anonymous.role). There is no sign-in.
        </p>
      ) : null}

      <p className="mt-4 max-w-prose text-xs leading-relaxed text-muted-foreground">
        Retention: GET /api/v1/config does not serve the retention windows to the browser, so this page will not
        print numbers it was never told. They are console.retention.* in the console config (Helm:
        console.retention), and the pruner logs what it swept.
      </p>
      <p className="mt-3 max-w-prose text-xs leading-relaxed text-muted-foreground">
        Maintenance windows are declared where they explain something — on{" "}
        <a href="/investigate" className="text-primary hover:underline">
          Investigate
        </a>{" "}
        and{" "}
        <a href="/explore" className="text-primary hover:underline">
          Explore
        </a>
        , next to the chart they cover — rather than a second time here. The section above lists every declared window
        with no range, which is the only place a future one can be found and removed. Roles and API tokens are not
        administered from this console at all.
      </p>
    </SectionCard>
  );
}

/* ── page ───────────────────────────────────────────────────────────────── */

/**
 * SettingsPage renders for ANY authenticated subject; the two gated sections
 * hide per permission, and About is what is left.
 *
 * The gate waits for `me` before deciding. `can()` fails closed while GET
 * /api/v1/auth/me is in flight, so rendering on the un-resolved value would
 * flash "your role can view none of this" on every cold load — the same
 * resolved-vs-false split pages/targets.tsx makes.
 */
export function SettingsPage() {
  const { me, can } = useAuth();
  const canWebhooks = can("webhooks:manage");
  const canBundle = can("settings:write");
  /* maintenance:WRITE, not :read — see MaintenanceWindowsSection. */
  const canMaintenance = can("maintenance:write");

  let body: ReactNode;
  if (me === undefined) {
    body = (
      <Card role="status" aria-live="polite" className="p-6">
        <span className="sr-only">Loading…</span>
        <Skeleton className="h-10 w-full" />
      </Card>
    );
  } else {
    body = (
      <>
        {!canWebhooks && !canBundle && !canMaintenance ? (
          <Card role="status" className="p-6">
            <p className="text-sm font-medium">Your role can view none of the console's settings.</p>
            <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
              Webhook endpoints need webhooks:manage, configuration export/import needs settings:write, and the
              maintenance-window list needs maintenance:write. The first two are admin-only in the built-in roles. What
              is below is everything this role can read here.
            </p>
          </Card>
        ) : null}
        {canWebhooks ? <WebhooksSection /> : null}
        {canMaintenance ? <MaintenanceWindowsSection /> : null}
        {canBundle ? <ExportImportSection /> : null}
        <AboutSection />
      </>
    );
  }

  return (
    <PageShell
      title="Settings"
      description="Webhook endpoints, configuration export/import, and what this console is running as."
    >
      {body}
    </PageShell>
  );
}
