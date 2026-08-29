import { Fragment, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Pager, usePager } from "@/components/ui/pager";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { Segmented, type SegmentedOption } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import { useConfirmStep } from "@/hooks/use-confirm-step";
import { useDisclosureFocus } from "@/hooks/use-disclosure-focus";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { MaintenanceRow } from "@/components/maintenance";
import {
  ApiError,
  createToken,
  createWebhook,
  deleteToken,
  deleteWebhook,
  exportConfig,
  getConfig,
  getVersion,
  getMaintenance,
  importConfig,
  listTokens,
  listWebhooks,
  testWebhook,
  updateWebhook,
} from "@/lib/api";
import type { components } from "@/lib/api-types";
// Read in each mutating component rather than threaded down as a prop, the same way
// pages/targets.tsx does it.
import { stampFull, translate, useLocale, useT, type Locale, type Translate } from "@/lib/i18n";
import { pluralKey, settingsDict, type SettingsKey } from "@/lib/i18n/dict/settings";
import { withAtParam, useWriteGuard } from "@/lib/timemachine";
import type {
  ConfigBundle,
  ConfigImportCollectionResult,
  ConfigImportResult,
  Token,
  TokenCreateResponse,
  Webhook,
  WebhookEvent,
  WebhookRequest,
} from "@/lib/types";
import { CHECKBOX_CLASS, cn } from "@/lib/utils";

/**
 * WHAT IS HERE, and why the nav description promises more than this file delivers; a second create
 * form here would be a second place to get the same thing wrong.
 */

/* ── shared bits ────────────────────────────────────────────────────────── */

/**
 * queryErrorMessage is what this page says when something failed: the server's
 * own words whenever it wrote any, and this section's own sentence when it did
 * not.
 *
 * The `?? title` alone was not enough. A refusal that never reached the console
 * — a gateway's HTML 502, a proxy's empty problem document — arrives with an
 * ABSENT detail and a title that is the empty statusText, and `"" ` is a string,
 * so every error slot on this page rendered a red paragraph with nothing in it:
 * the list stayed empty, the button un-spun, and the operator was looking at a
 * page that had silently given up. A blank message is worse than a generic one.
 */
function queryErrorMessage(error: unknown, fallback: string): string {
  if (!(error instanceof ApiError)) return fallback;
  const said = [error.problem.detail, error.problem.title].map((s) => s?.trim() ?? "").find((s) => s !== "");
  return said ?? fallback;
}

/** The locale is required: a bare toLocaleString() reorders the date and swaps in AM/PM from
 *  whatever the browser was installed in — "8/10/2026 3:47 AM" on a Russian page. */
function fmtTime(timestamp: string | null | undefined, locale: Locale): string {
  if (!timestamp) return "—";
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : stampFull(d, locale);
}

function fieldClasses(invalid: boolean): string {
  return cn(
    "h-9 rounded-md border bg-transparent px-3 text-[13px]",
    invalid ? "border-health-bad" : "border-border-strong",
  );
}

function SectionCard({ id, title, children }: { id?: string; title: string; children: ReactNode }) {
  return (
    <Card asChild className="p-6">
      {/* `id` is the anchor the sidebar's user menu links a deep link at. */}
      <section id={id}>
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

/** enT is the English translator this file's PURE helpers default to, so
 *  parseBundle keeps the signature (and the output) its unit tests read. */
const enT: Translate<SettingsKey> = (key, vars) => translate(settingsDict, "en", key, vars);

/**
 * withNodes renders a translated sentence that contains LINKS; two paragraphs on this page say "…on
 * Investigate or Explore…" with each name an anchor.
 */
function withNodes(template: string, nodes: Record<string, ReactNode>): ReactNode[] {
  return template.split(/(\{\w+\})/).map((chunk, i) => {
    const name = /^\{(\w+)\}$/.exec(chunk)?.[1];
    const node = name === undefined ? undefined : nodes[name];
    return node === undefined ? chunk : <Fragment key={i}>{node}</Fragment>;
  });
}

/** The two surface names that appear as links in those sentences. Their words
 *  come from this dictionary and match what the sidebar calls the same pages. */
/**
 * SurfaceLink is a plain in-app link, and it applies withAtParam ITSELF so its callers cannot forget.
 *
 * Every one of them passed a bare string ("/explore"), which is a full document load: the Time
 * Machine provider unmounts, the new one finds no ?at=, and a reader pinned to an instant was
 * silently returned to Live. Doing it here rather than at four call sites is what keeps the fifth
 * one honest.
 */
function SurfaceLink({ to, children }: { to: string; children: ReactNode }) {
  return (
    <a href={withAtParam(to)} className="text-primary hover:underline">
      {children}
    </a>
  );
}

/* ── webhooks ───────────────────────────────────────────────────────────── */

/**
 * WEBHOOK_EVENTS is the closed subscribable set as of THIS build; it is typed as WebhookEvent[]
 * rather than inferred.
 */
const WEBHOOK_EVENTS: WebhookEvent[] = [
  "incident.created",
  "incident.resolved",
  "incident.reopened",
  "alert.fired",
  "alert.resolved",
];

/**
 * TEST_REFETCH_DELAY_MS is how long the page waits after a 202 before re-reading the endpoint list;
 * the delivery ladder is asynchronous and the console makes no promise about when the outcome row
 * is written.
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

/** webhookRequestFrom turns the draft into the body that goes on the wire. */
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

/* 422 → form field The same treatment pages/targets.tsx's three forms already have. */

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

/** FieldErrors is targets.tsx's shape: a message per field, plus "form" for
 *  anything that names no field this form has. */
type FieldErrors<F extends string> = Partial<Record<F | "form", string>>;

/** WEBHOOK_NAME_RE is store.webhookNameRE, mirrored: lowercase alphanumerics and hyphens. */
const WEBHOOK_NAME_RE = /^[a-z0-9-]+$/;

/**
 * webhookDraftErrors returns EVERY basic problem in a draft at once.
 *
 * The server refuses one thing at a time — it returns on the first failure —
 * so a draft with an empty name, a bare-hostname URL and no events took three
 * submits to learn three facts. These are only the checks a browser can be
 * certain about; anything subtler is still the server's call, and its words
 * are what render for it.
 */
export function webhookDraftErrors(
  draft: WebhookDraft,
  editing: boolean,
  t: Translate<SettingsKey>,
): FieldErrors<WebhookField> {
  const errors: FieldErrors<WebhookField> = {};
  const name = draft.name.trim();
  if (name === "") errors.name = t("webhooks.form.nameRequired");
  else if (!WEBHOOK_NAME_RE.test(name)) errors.name = t("webhooks.form.nameCharset");

  const url = draft.url.trim();
  if (url === "") errors.url = t("webhooks.form.urlRequired");
  else if (!url.startsWith("http://") && !url.startsWith("https://")) errors.url = t("webhooks.form.urlScheme");

  if (draft.events.length === 0) errors.events = t("webhooks.form.eventsRequired");

  // Only on CREATE: editing with an empty box keeps the stored secret.
  if (!editing && draft.secret === "") errors.secret = t("webhooks.form.secretRequired");

  return errors;
}

function WebhookForm({ initial, onDone }: { initial?: Webhook; onDone: () => void }) {
  const t = useT(settingsDict);
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
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
  /* The draft as it was handed over, for the discard prompt below — the same
     shape the rule builder uses, and for the same reason: these two are the
     forms with enough in them to be worth a question. */
  const pristine = useRef(
    JSON.stringify({
      name: initial?.name ?? "",
      url: initial?.url ?? "",
      events: initial?.events ?? [],
      enabled: initial?.enabled ?? true,
      secret: "",
    }),
  );
  const dirty = JSON.stringify(draft) !== pristine.current;
  const [discarding, setDiscarding] = useState(false);
  /* A MAP, not one message: the client checks say everything wrong with the
     draft at once, and each message renders at the field it is about rather
     than 256px below it in a single slot. A server refusal still lands here
     verbatim, routed by the phrase it names. */
  const [errors, setErrors] = useState<FieldErrors<WebhookField>>({});
  const errorId = useId();

  /** invalid marks a field for aria-invalid and the red border. */
  const invalid = (field: WebhookField) => errors[field] !== undefined;
  /** fieldErrorId is the id of the message rendered beside a field. */
  const fieldErrorId = (field: WebhookField) => `${errorId}-${field}`;
  /** describedBy points a field's assistive description at its OWN message. */
  const describedBy = (field: WebhookField) => (invalid(field) ? fieldErrorId(field) : undefined);

  /** FieldError renders one field's message right under it. */
  function FieldError({ field }: { field: WebhookField }) {
    const message = errors[field];
    if (message === undefined) return null;
    return (
      <span id={fieldErrorId(field)} role="alert" className="text-xs leading-relaxed text-health-bad">
        {message}
      </span>
    );
  }

  /* Editing answers an error: a field the reader has just changed must stop
     carrying a verdict about what it used to hold. */
  function edit(field: WebhookField, next: Partial<WebhookDraft>) {
    setErrors((prev) => {
      if (prev[field] === undefined && prev.form === undefined) return prev;
      const rest = { ...prev };
      delete rest[field];
      delete rest.form;
      return rest;
    });
    setDraft((d) => ({ ...d, ...next }));
  }

  function toggleEvent(event: WebhookEvent) {
    const next = draft.events.includes(event)
      ? draft.events.filter((e) => e !== event)
      : [...draft.events, event];
    edit("events", { events: next });
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const basics = webhookDraftErrors(draft, initial !== undefined, t);
    setErrors(basics);
    // Every basic problem is now on screen at once; there is nothing to learn
    // from a round trip that would only report the first of them again.
    if (Object.keys(basics).length > 0) return;
    if (!begin()) return;
    try {
      const req = webhookRequestFrom(draft);
      if (initial) await updateWebhook(initial.id, req);
      else await createWebhook(req);
      await qc.invalidateQueries({ queryKey: ["webhooks"] });
      onDone();
    } catch (err) {
      const message = queryErrorMessage(err, t("webhooks.form.failed"));
      // The server's words, verbatim, at the field its phrase names; a
      // non-ApiError has none to route, so it stays form-level.
      const field = err instanceof ApiError ? webhookFieldForDetail(message) : null;
      setErrors(field ? { [field]: message } : { form: message });
      end();
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-4">
        <h3 className="text-sm font-semibold">
          {initial ? t("webhooks.form.edit", { name: initial.name }) : t("webhooks.form.create")}
        </h3>
        <div className="grid gap-4 sm:grid-cols-2">
          {/* The message is a SIBLING of the label, not a child of it: text
              inside a wrapping <label> becomes part of the control's accessible
              name, and "Name webhook: name "pd" is already taken" is not what
              the box is called. */}
          <div className="flex flex-col gap-1 text-[13px]">
            <label className="flex flex-col gap-1">
              <span className="text-muted-foreground">{t("webhooks.form.name")}</span>
              {/* The two placeholders are sample VALUES — a receiver's name and
                  a receiver's URL — and stay as they are. */}
              <input
                value={draft.name}
                placeholder="pagerduty"
                aria-invalid={invalid("name") || undefined}
                aria-describedby={describedBy("name")}
                onChange={(e) => edit("name", { name: e.target.value })}
                className={fieldClasses(invalid("name"))}
              />
            </label>
            <FieldError field="name" />
          </div>
          <div className="flex flex-col gap-1 text-[13px]">
            <label className="flex flex-col gap-1">
              <span className="text-muted-foreground">{t("webhooks.form.url")}</span>
              <input
                value={draft.url}
                placeholder="https://hooks.example.test/incidents"
                aria-invalid={invalid("url") || undefined}
                aria-describedby={describedBy("url")}
                onChange={(e) => edit("url", { url: e.target.value })}
                className={fieldClasses(invalid("url"))}
              />
            </label>
            <FieldError field="url" />
          </div>
        </div>

        {/* The event list has no single input to mark, so the GROUP carries it:
            aria-invalid on the fieldset, which is what a "no events selected"
            422 is actually about. */}
        <fieldset
          className="flex flex-col gap-2 text-[13px]"
          aria-invalid={invalid("events") || undefined}
          aria-describedby={describedBy("events")}
        >
          <legend className={cn("text-muted-foreground", invalid("events") && "text-health-bad")}>
            {t("webhooks.form.events")}
          </legend>
          {/* The event IDs are the wire values the checkbox writes and the
              payload carries — they render as themselves. */}
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
          <FieldError field="events" />
        </fieldset>

        <label className="flex items-center gap-2 text-[13px]">
          <input
            type="checkbox"
            checked={draft.enabled}
            onChange={(e) => setDraft((d) => ({ ...d, enabled: e.target.checked }))}
            className={CHECKBOX_CLASS}
          />
          <span>{t("webhooks.form.enabled")}</span>
        </label>

        <div className="flex max-w-md flex-col gap-1 text-[13px]">
          <label htmlFor={secretId} className="text-muted-foreground">
            {t("webhooks.form.secret")}
          </label>
          <input
            id={secretId}
            type="password"
            value={draft.secret}
            aria-invalid={invalid("secret") || undefined}
            aria-describedby={invalid("secret") ? `${fieldErrorId("secret")} ${secretId}-help` : `${secretId}-help`}
            onChange={(e) => edit("secret", { secret: e.target.value })}
            className={fieldClasses(invalid("secret"))}
          />
          <FieldError field="secret" />
          {/* Write-only, in both directions: the API never returns a secret, so
              this box starts empty even when editing an endpoint that has one. */}
          <span id={`${secretId}-help`} className="text-xs leading-relaxed text-muted-foreground">
            {initial ? t("webhooks.form.secretKeep") : t("webhooks.form.secretNew")}
          </span>
        </div>

        {/* The form-level slot, for a refusal that names no field this form
            has. Everything that DOES name one renders at that field instead of
            here, which is the whole point: the message and the box it is about
            are now in the same place. */}
        {errors.form ? <ErrorLine id={errorId}>{errors.form}</ErrorLine> : null}

        <div className="flex gap-2">
          <Button type="submit" loading={submitting} {...guard}>
            {initial ? t("webhooks.form.save") : t("webhooks.form.createButton")}
          </Button>
          {/* Cancel closes a form and touches nothing, so it stays live even
              while engaged — a form an operator cannot dismiss would be the
              mode holding the page hostage. It asks first only when there is
              unsaved work to lose. */}
          {discarding ? (
            <>
              <Button type="button" variant="outline" onClick={onDone}>
                {t("webhooks.form.discard")}
              </Button>
              <Button type="button" variant="ghost" onClick={() => setDiscarding(false)}>
                {t("webhooks.form.keepEditing")}
              </Button>
              <span role="status" className="self-center text-xs text-muted-foreground">
                {t("webhooks.form.discardConfirm")}
              </span>
            </>
          ) : (
            <Button type="button" variant="outline" onClick={() => (dirty ? setDiscarding(true) : onDone())}>
              {t("cancel")}
            </Button>
          )}
        </div>
      </form>
    </Card>
  );
}

function WebhookRow({ hook, onEdit }: { hook: Webhook; onEdit: () => void }) {
  const t = useT(settingsDict);
  const { locale } = useLocale();
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
  const guard = useWriteGuard();
  const { confirming, confirmRef, triggerRef, ask, reset } = useConfirmStep();
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
      setError(queryErrorMessage(err, t("webhooks.row.deleteFailed")));
      setBusy(false);
      reset();
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
      setError(queryErrorMessage(err, t("webhooks.row.testFailed")));
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
      {/* Both pills describe a BOOLEAN this page read, so both translate. */}
      <Badge variant={hook.enabled ? "ok" : "unknown"}>
        {hook.enabled ? t("webhooks.row.enabled") : t("webhooks.row.disabled")}
      </Badge>
      {/* hasSecret is always true for a stored row, so this reads as the
          contract statement the API intends — "this endpoint signs its
          deliveries" — rather than as a question with two answers. An imported
          endpoint is the one case that can be false (plan Decision 9). */}
      <Badge variant={hook.hasSecret ? "neutral" : "bad"}>
        {hook.hasSecret ? t("webhooks.row.signed") : t("webhooks.row.noSecret")}
      </Badge>
      {/* lastStatus is the DELIVERY LADDER's own string ("ok", "failed: 502").
          This row picks its colour and prints it; it does not rewrite it. */}
      <span data-testid="last-status" className="text-xs">
        <Badge variant={lastStatusTone(hook.lastStatus)}>{hook.lastStatus === "" ? "—" : hook.lastStatus}</Badge>
      </span>
      <span className="text-xs text-muted-foreground">{fmtTime(hook.lastAttempt, locale)}</span>
      {hook.failures > 0 ? (
        <span className="text-xs text-health-bad">
          {t("webhooks.row.failures", {
            count: hook.failures,
            /* The locale, because 21 is singular in Russian and plural in
               English — pluralKey's own doc comment has the case. */
            word: t(pluralKey(locale, hook.failures, "count.failures.one", "count.failures.few", "count.failures.many")),
          })}
        </span>
      ) : null}

      <span className="ml-auto flex flex-wrap items-center gap-2">
        {confirming ? (
          <>
            {/* Spoken as well as drawn — the row swaps its controls under the reader. */}
            <span role="status" className="sr-only">
              {t("webhooks.row.confirmDelete", { name: hook.name })}
            </span>
            <Button ref={confirmRef} size="sm" variant="outline" loading={busy} {...guard} onClick={handleDelete}>
              {t("webhooks.row.confirmDelete", { name: hook.name })}
            </Button>
            <Button size="sm" variant="ghost" onClick={reset}>
              {t("cancel")}
            </Button>
          </>
        ) : (
          <>
            <Button size="sm" variant="ghost" {...guard} onClick={handleTest}>
              {t("webhooks.row.test", { name: hook.name })}
            </Button>
            <Button size="sm" variant="ghost" {...guard} onClick={onEdit}>
              {t("webhooks.row.edit", { name: hook.name })}
            </Button>
            <Button ref={triggerRef} size="sm" variant="ghost" {...guard} onClick={ask}>
              {t("webhooks.row.delete", { name: hook.name })}
            </Button>
          </>
        )}
      </span>
      {queued ? (
        <span role="status" className="w-full text-xs text-muted-foreground">
          {t("webhooks.row.queued")}
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
  const t = useT(settingsDict);
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
  const guard = useWriteGuard();
  const [editing, setEditing] = useState<{ mode: "none" } | { mode: "create" } | { mode: "edit"; hook: Webhook }>({
    mode: "none",
  });
  const query = useQuery({ queryKey: ["webhooks"], queryFn: listWebhooks });
  const hooks = query.data?.webhooks ?? [];
  const pager = usePager(hooks);

  return (
    <div className="flex flex-col gap-4">
      {editing.mode === "none" ? (
        <div>
          <Button size="sm" {...guard} onClick={() => setEditing({ mode: "create" })}>
            {t("webhooks.new")}
          </Button>
        </div>
      ) : (
        <WebhookForm
          key={editing.mode === "edit" ? editing.hook.id : "create"}
          initial={editing.mode === "edit" ? editing.hook : undefined}
          onDone={() => setEditing({ mode: "none" })}
        />
      )}

      <SectionCard title={t("webhooks.heading")}>
        <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("webhooks.blurb")}</p>
        {query.isError ? <ErrorLine>{queryErrorMessage(query.error, t("webhooks.unavailable"))}</ErrorLine> : null}
        {/* isPending / isSuccess, not !isLoading && !isError: a paused retry
            (react-query pauses while the browser thinks it is offline) is
            pending-but-not-fetching, and the old guard would present "no
            endpoints" as a settled answer. M7 final-gate finding. */}
        {query.isPending ? (
          <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
            <span className="sr-only">{t("loading")}</span>
            <Skeleton className="h-10 w-full" />
          </div>
        ) : null}
        {query.isSuccess && hooks.length === 0 ? (
          <p className="px-1 py-10 text-center text-xs text-muted-foreground">{t("webhooks.empty")}</p>
        ) : null}
        {hooks.length > 0 ? (
          <>
          <ul aria-label={t("webhooks.listAria")} className="mt-4 divide-y divide-border">
            {pager.visible.map((hook) => (
              <WebhookRow key={hook.id} hook={hook} onEdit={() => setEditing({ mode: "edit", hook })} />
            ))}
          </ul>
          <Pager pager={pager} subject={t("webhooks.subject")} className="px-0" />
          </>
        ) : null}
      </SectionCard>
    </div>
  );
}

/* ── API tokens (QA round 6, finding #14) ───────────────────────────────── */

/** TOKENS_ANCHOR is what components/user-menu.tsx's "Token management" points
 *  at; the link used to land on /settings, which had no tokens section at all. */
export const TOKENS_ANCHOR = "tokens";

export type TokenField = "name" | "expiresAt";

/** tokenFieldForDetail routes a 422/400 onto the field it names, the same table
 *  idiom webhookFieldForDetail uses. */
export function tokenFieldForDetail(detail: string): TokenField | null {
  const haystack = detail.toLowerCase();
  if (haystack.includes("expire")) return "expiresAt";
  if (haystack.includes("name")) return "name";
  return null;
}

/** tokenState is the row's own status; `revokedAt` and a past `expiresAt` are
 *  two different reasons a token no longer works and the row says which. */
export function tokenState(token: Token, now: Date): "active" | "revoked" | "expired" {
  if (token.revokedAt) return "revoked";
  if (token.expiresAt && new Date(token.expiresAt).getTime() <= now.getTime()) return "expired";
  return "active";
}

/**
 * MintedToken is the ONE render of a raw token in this console. It is not stored, not re-fetchable
 * and not in the list — so it stays until the operator dismisses it.
 */
function MintedToken({ minted, onDismiss }: { minted: TokenCreateResponse; onDismiss: () => void }) {
  const t = useT(settingsDict);
  const [note, setNote] = useState<string>();

  async function copy() {
    const clipboard = navigator.clipboard;
    if (!clipboard || typeof clipboard.writeText !== "function") {
      setNote(t("tokens.secret.noClipboard"));
      return;
    }
    try {
      await clipboard.writeText(minted.token);
      setNote(t("tokens.secret.copied"));
    } catch {
      setNote(t("tokens.secret.refused"));
    }
  }

  return (
    <Card asChild className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-6">
      <section aria-label={t("tokens.secret.aria")}>
        <h3 className="text-sm font-semibold">{t("tokens.secret.title", { name: minted.name })}</h3>
        {/* The server's bytes, selectable and wrapped — never truncated, or the
            one copy an operator gets would be a partial token. */}
        <p data-testid="minted-token" className="mt-3 break-all rounded-md bg-surface-2 p-3 font-mono text-[13px]">
          {minted.token}
        </p>
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <Button size="sm" variant="outline" onClick={() => void copy()}>
            {t("tokens.secret.copy")}
          </Button>
          <Button size="sm" variant="ghost" onClick={onDismiss}>
            {t("tokens.secret.dismiss")}
          </Button>
          {note ? (
            <span role="status" className="text-xs text-muted-foreground">
              {note}
            </span>
          ) : null}
        </div>
      </section>
    </Card>
  );
}

/** TOKEN_NAME_MAX mirrors httpapi.tokenNameMaxLen; see the input's own comment. */
const TOKEN_NAME_MAX = 63;

function TokenForm({ onMinted, onDone }: { onMinted: (minted: TokenCreateResponse) => void; onDone: () => void }) {
  const t = useT(settingsDict);
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
  const guard = useWriteGuard();
  const nameId = useId();
  const expiresId = useId();
  const errorId = useId();
  const [name, setName] = useState("");
  /* null, not "": "no expiry chosen" is a real state — a token with no expiry
     is valid until revoked, which the help line says. */
  const [expires, setExpires] = useState<Date | null>(null);
  const { submitting, begin, end } = useSubmitGuard();
  const [error, setError] = useState<string>();
  const [errorField, setErrorField] = useState<TokenField | null>(null);

  function fail(message: string, field: TokenField | null) {
    setError(message);
    setErrorField(field);
  }

  /* Both fields carry a help line, so a marked one points at the message AND
     keeps its own description rather than replacing it. */
  const invalid = (field: TokenField) => errorField === field;

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    setErrorField(null);
    if (name.trim() === "") {
      fail(t("tokens.form.nameRequired"), "name");
      return;
    }
    if (expires !== null && Number.isNaN(expires.getTime())) {
      fail(t("tokens.form.badExpiry"), "expiresAt");
      return;
    }
    /* A token that has already expired can never authenticate, so minting one
       hands back a secret for nothing. The picker's disablePast blocks past
       DAYS; on today it still yields a time already gone, and the server's 422
       stays the net behind this. */
    if (expires !== null && expires.getTime() <= Date.now()) {
      fail(t("tokens.form.pastExpiry"), "expiresAt");
      return;
    }
    if (!begin()) return;
    try {
      const minted = await createToken({
        name: name.trim(),
        ...(expires !== null ? { expiresAt: expires.toISOString() } : {}),
      });
      await qc.invalidateQueries({ queryKey: ["tokens"] });
      onMinted(minted);
    } catch (err) {
      const message = queryErrorMessage(err, t("tokens.form.failed"));
      fail(message, err instanceof ApiError ? tokenFieldForDetail(message) : null);
      end();
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-4">
        <h3 className="text-sm font-semibold">{t("tokens.form.create")}</h3>
        <div className="grid gap-4 sm:grid-cols-2">
          <div className="flex flex-col gap-1 text-[13px]">
            <label htmlFor={nameId} className="text-muted-foreground">
              {t("tokens.form.name")}
            </label>
            {/* A sample VALUE, not a word — it stays as it is. */}
            <input
              id={nameId}
              value={name}
              placeholder="ci-pipeline"
              /* The same 63 the API enforces (httpapi.tokenNameMaxLen). Stopping the typing is
                 kinder than a 422 after the fact — and the name is display text in every row. */
              maxLength={TOKEN_NAME_MAX}
              aria-invalid={invalid("name") || undefined}
              aria-describedby={invalid("name") ? `${errorId} ${nameId}-help` : `${nameId}-help`}
              onChange={(e) => setName(e.target.value)}
              className={fieldClasses(invalid("name"))}
            />
            <span id={`${nameId}-help`} className="text-xs leading-relaxed text-muted-foreground">
              {t("tokens.form.nameHelp")}
            </span>
          </div>
          <div className="flex flex-col gap-1 text-[13px]">
            <span className="text-muted-foreground">{t("tokens.form.expires")}</span>
            {/* The M5 DateTimePicker, the one way this console asks for an
                instant — and here for the reason the schedule's Run-at field
                takes it: allowFuture lifts the ceiling, disablePast is the
                other half of the same rule. An expiry in the past is refused
                by the server, so a control must not offer one. */}
            <div className="flex items-center gap-1">
              <DateTimePicker
                aria-label={t("tokens.form.expires")}
                aria-invalid={invalid("expiresAt")}
                aria-describedby={invalid("expiresAt") ? `${errorId} ${expiresId}-help` : `${expiresId}-help`}
                value={expires}
                label={expires === null ? t("tokens.form.expiresNotSet") : undefined}
                allowFuture
                disablePast
                onApply={setExpires}
              />
              {expires !== null ? (
                <Button
                  type="button"
                  size="sm"
                  variant="ghost"
                  aria-label={t("tokens.form.expiresClearAria")}
                  onClick={() => setExpires(null)}
                >
                  {t("tokens.form.expiresClear")}
                </Button>
              ) : null}
            </div>
            <span id={`${expiresId}-help`} className="text-xs leading-relaxed text-muted-foreground">
              {t("tokens.form.expiresHelp")}
            </span>
          </div>
        </div>

        {error ? <ErrorLine id={errorId}>{error}</ErrorLine> : null}

        <div className="flex gap-2">
          <Button type="submit" loading={submitting} {...guard}>
            {t("tokens.form.createButton")}
          </Button>
          {/* Cancel touches nothing, so it stays live while engaged — the same
              rule the webhook form's Cancel follows. */}
          <Button type="button" variant="outline" onClick={onDone}>
            {t("cancel")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

function TokenRow({ token }: { token: Token }) {
  const t = useT(settingsDict);
  const { locale } = useLocale();
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
  const guard = useWriteGuard();
  const { confirming, confirmRef, triggerRef, ask, reset } = useConfirmStep();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const state = tokenState(token, new Date());

  /* Spent = revoked, or past its expiry. DELETE means REVOKE on a live token
     and PURGE on a spent one — the server reads the row's state and acts on it
     — so the row asks for the act it is actually about to perform. Without
     this, a revoked row was permanent and the list only ever grew. */
  const spent = state !== "active";

  async function handleDelete() {
    setBusy(true);
    setError(undefined);
    try {
      await deleteToken(token.id);
      await qc.invalidateQueries({ queryKey: ["tokens"] });
    } catch (err) {
      setError(queryErrorMessage(err, t(spent ? "tokens.row.purgeFailed" : "tokens.row.deleteFailed")));
    }
    /* Cleared on BOTH paths, because a SUCCESS does not necessarily unmount this
       row: DELETE on an active token revokes it, and the revoked row comes
       straight back from the refetch — offering the purge, but with `busy` still
       true, which is `loading` on the confirm button, which is disabled. The
       purge then needed a reload to become clickable. */
    setBusy(false);
    reset();
  }

  return (
    <li data-testid="token-row" className="flex flex-wrap items-center gap-x-3 gap-y-1 py-3 text-sm">
      {/* The name is SERVER text and can be anything, including 4 000 characters with no space in
          them. An unbreakable string in a flex row has no width to lay out against, so the row grew
          to ~950 000 pixels and every page in the console scrolled sideways. It is bounded here and
          bounded again at the API (tokenNameMaxLen); the whole name stays in the title. */}
      <span className="min-w-0 max-w-full truncate font-medium" title={token.name}>
        {token.name}
      </span>
      {state === "active" ? null : (
        <Badge variant={state === "revoked" ? "bad" : "unknown"}>
          {t(state === "revoked" ? "tokens.revoked" : "tokens.expired")}
        </Badge>
      )}
      {/* The owner is a SUBJECT ID the server assigned; it prints as it came. */}
      <span className="text-xs text-muted-foreground">
        {t("tokens.col.owner")} {token.owner}
      </span>
      <span className="text-xs text-muted-foreground">
        {t("tokens.col.created")} {fmtTime(token.createdAt, locale)}
      </span>
      {/* An absent lastUsedAt means never used, which is a fact worth stating —
          fmtTime's em-dash would have read as "the API did not say". */}
      <span data-testid="token-last-used" className="text-xs text-muted-foreground">
        {token.lastUsedAt ? `${t("tokens.col.lastUsed")} ${fmtTime(token.lastUsedAt, locale)}` : t("tokens.lastUsed.never")}
      </span>
      {token.expiresAt ? (
        <span className="text-xs text-muted-foreground">
          {t("tokens.col.expires")} {fmtTime(token.expiresAt, locale)}
        </span>
      ) : null}

      <span className="ml-auto flex flex-wrap items-center gap-2">
        {confirming ? (
          <>
            <span role="status" className="sr-only">
              {t(spent ? "tokens.row.confirmPurge" : "tokens.row.confirmDelete", { name: token.name })}
            </span>
            <Button
              ref={confirmRef}
              size="sm"
              variant="outline"
              loading={busy}
              {...guard}
              aria-label={t(spent ? "tokens.row.confirmPurge" : "tokens.row.confirmDelete", { name: token.name })}
              onClick={handleDelete}
            >
              <span className="block max-w-[18rem] truncate">
                {t(spent ? "tokens.row.confirmPurge" : "tokens.row.confirmDelete", { name: token.name })}
              </span>
            </Button>
            <Button size="sm" variant="ghost" onClick={reset}>
              {t("cancel")}
            </Button>
          </>
        ) : (
          <Button
            ref={triggerRef}
            size="sm"
            variant="ghost"
            {...guard}
            title={spent ? t("tokens.row.purgeHint") : undefined}
            aria-label={t(spent ? "tokens.row.purge" : "tokens.row.delete", { name: token.name })}
            onClick={ask}
          >
            {/* Truncated for the same reason the name above is: this label CARRIES the name. */}
            <span className="block max-w-[18rem] truncate">
              {t(spent ? "tokens.row.purge" : "tokens.row.delete", { name: token.name })}
            </span>
          </Button>
        )}
      </span>
      {error ? (
        <span role="alert" className="w-full text-xs leading-relaxed text-health-bad">
          {error}
        </span>
      ) : null}
    </li>
  );
}

function TokensSection() {
  const t = useT(settingsDict);
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
  const guard = useWriteGuard();
  const [creating, setCreating] = useState(false);
  // The keyboard across the button↔form swap; see hooks/use-disclosure-focus.
  const createFocus = useDisclosureFocus(creating);
  /* The one-time secret lives HERE rather than in the form, so dismissing the
     form does not take the only copy of it with it. */
  const [minted, setMinted] = useState<TokenCreateResponse>();
  const query = useQuery({ queryKey: ["tokens"], queryFn: listTokens });
  const tokens = query.data?.tokens ?? [];
  const pager = usePager(tokens);

  return (
    <div className="flex flex-col gap-4">
      {creating ? (
        <div ref={createFocus.panelRef} tabIndex={-1}>
          <TokenForm
            onMinted={(m) => {
              setMinted(m);
              createFocus.onClose();
              setCreating(false);
            }}
            onDone={() => {
              createFocus.onClose();
              setCreating(false);
            }}
          />
        </div>
      ) : (
        <div>
          {/* The button is REPLACED by the form, so the keyboard has to be handed over; see
              hooks/use-disclosure-focus. */}
          <Button
            ref={createFocus.triggerRef}
            size="sm"
            {...guard}
            onClick={() => {
              createFocus.onOpen();
              setCreating(true);
            }}
          >
            {t("tokens.new")}
          </Button>
        </div>
      )}

      {minted ? <MintedToken minted={minted} onDismiss={() => setMinted(undefined)} /> : null}

      <SectionCard id={TOKENS_ANCHOR} title={t("tokens.heading")}>
        <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("tokens.blurb")}</p>
        {query.isError ? <ErrorLine>{queryErrorMessage(query.error, t("tokens.unavailable"))}</ErrorLine> : null}
        {/* isPending / isSuccess, the webhooks list's own guard: a paused retry
            is pending-but-not-fetching and must not read as "no tokens". */}
        {query.isPending ? (
          <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
            <span className="sr-only">{t("loading")}</span>
            <Skeleton className="h-10 w-full" />
          </div>
        ) : null}
        {query.isSuccess && tokens.length === 0 ? (
          <p className="px-1 py-10 text-center text-xs text-muted-foreground">{t("tokens.empty")}</p>
        ) : null}
        {tokens.length > 0 ? (
          <>
          <ul aria-label={t("tokens.listAria")} className="mt-4 divide-y divide-border">
            {pager.visible.map((token) => (
              <TokenRow key={token.id} token={token} />
            ))}
          </ul>
          <Pager pager={pager} subject={t("tokens.subject")} className="px-0" />
          </>
        ) : null}
      </SectionCard>
    </div>
  );
}

/* ── maintenance windows (QA round 3, finding #9) ───────────────────────── */

/**
 * MaintenanceWindowsSection is the ONLY unbounded view of the declared windows in this console —
 * and it has to actually be unbounded.
 *
 * It used to issue one unparameterised GET and paginate client-side over whatever came back. The API
 * answers 100 rows plus a nextCursor, ordered start_at DESC, so with more than 100 declared windows
 * the ones silently missing were the running and the past ones — exactly what an operator opens this
 * page to find — while future windows stayed. The blurb promised every declared window.
 */
function MaintenanceWindowsSection() {
  const t = useT(settingsDict);
  const qc = useQueryClient();
  const query = useInfiniteQuery({
    queryKey: ["settings", "maintenance"],
    queryFn: ({ pageParam }) => getMaintenance({ limit: 100, cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (page) => page.nextCursor || undefined,
  });
  const windows = query.data?.pages.flatMap((page) => page.windows ?? []) ?? [];
  const pager = usePager(windows);

  const onChanged = () => {
    void qc.invalidateQueries({ queryKey: ["settings", "maintenance"] });
    // The range-bounded lists elsewhere hold the same rows; a delete here must
    // not leave a card in another tab drawing a band for a window that is gone.
    void qc.invalidateQueries({ queryKey: ["maintenance"] });
    void qc.invalidateQueries({ queryKey: ["investigate", "maintenance"] });
  };

  return (
    <SectionCard title={t("maintenance.heading")}>
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
        {withNodes(t("maintenance.blurb"), {
          investigate: <SurfaceLink to="/investigate">{t("link.investigate")}</SurfaceLink>,
          explore: <SurfaceLink to="/explore">{t("link.explore")}</SurfaceLink>,
        })}
      </p>
      {query.isError ? (
        <ErrorLine>{queryErrorMessage(query.error, t("maintenance.unavailable"))}</ErrorLine>
      ) : null}
      {/* isPending / isSuccess, the same guard the webhooks list uses: a paused
          retry is pending-but-not-fetching, and presenting that as "none
          declared" would be a settled answer nobody gave. */}
      {query.isPending ? (
        <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
          <span className="sr-only">{t("loading")}</span>
          <Skeleton className="h-10 w-full" />
        </div>
      ) : null}
      {query.isSuccess && windows.length === 0 ? (
        <p className="px-1 py-10 text-center text-xs text-muted-foreground">{t("maintenance.empty")}</p>
      ) : null}
      {windows.length > 0 ? (
        <>
        <ul aria-label={t("maintenance.listAria")} className="mt-4 divide-y divide-border">
          {pager.visible.map((w) => (
            /* The SHARED row (components/maintenance.tsx): same confirm-delete,
               same compact stamp, same write guard. canWrite is true by
               construction — this whole section is behind maintenance:write. */
            <MaintenanceRow key={w.id} window={w} canWrite onChanged={onChanged} />
          ))}
        </ul>
        <Pager pager={pager} subject={t("maintenance.subject")} className="px-0" />
        {query.hasNextPage ? (
          <div className="mt-3 flex items-center gap-3">
            <Button
              size="sm"
              variant="outline"
              loading={query.isFetchingNextPage}
              onClick={() => void query.fetchNextPage()}
            >
              {t("maintenance.loadMore")}
            </Button>
            {/* A failed page is a note BESIDE the button, never the loss of the pages that
                succeeded — the rule mtr-trace-list.tsx states for the same shape. */}
            {query.isError ? (
              <span role="alert" className="text-xs text-health-bad">
                {queryErrorMessage(query.error, t("maintenance.unavailable"))}
              </span>
            ) : null}
          </div>
        ) : null}
        </>
      ) : null}
    </SectionCard>
  );
}

/* ── export / import ────────────────────────────────────────────────────── */

/**
 * Checked for EQUALITY, never ">=": a bundle from a future console may describe collections this
 * build has never heard.
 */
export const BUNDLE_VERSION = 1;

export type BundleParse = { ok: true; bundle: ConfigBundle } | { ok: false; message: string };

/**
 * parseBundle is the WHOLE of the client's validation, and its shortness is the point; a second,
 * weaker copy of those rules here would reject bundles the console would have accepted.
 */
export function parseBundle(text: string, t: Translate<SettingsKey> = enT): BundleParse {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { ok: false, message: t("bundle.notJson") };
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { ok: false, message: t("bundle.notObject") };
  }
  const version = (parsed as { version?: unknown }).version;
  if (version !== BUNDLE_VERSION) {
    return {
      ok: false,
      /* Both numbers go in as they are — the version the build reads and the
         one the file declares, JSON-quoted so `null` reads as null. */
      message: t("bundle.versionMismatch", {
        expected: BUNDLE_VERSION,
        found: JSON.stringify(version ?? null),
      }),
    };
  }
  return { ok: true, bundle: parsed as ConfigBundle };
}

/** exportFilename names the DAY, not the instant: a bundle is a restore point an operator files away. */
export function exportFilename(now: Date): string {
  return `kconmon-ng-config-${now.toISOString().slice(0, 10)}.json`;
}

/** IMPORT_COLLECTIONS is the result table's row order. */
const IMPORT_COLLECTIONS: readonly (readonly [keyof Omit<ConfigImportResult, "dryRun">, SettingsKey])[] = [
  ["targets", "collection.targets"],
  ["checkDefinitions", "collection.checkDefinitions"],
  ["checkSchedules", "collection.checkSchedules"],
  ["alertRules", "collection.alertRules"],
  ["webhooks", "collection.webhooks"],
  ["maintenanceWindows", "collection.maintenanceWindows"],
  /* The bundle carries these two as well, and the ledger silently dropped them: an import that
     created or skipped custom roles reported neither, so the one section whose outcome an operator
     most needs to see — the access map — was the one the result table did not mention. */
  ["rbacRoles", "collection.rbacRoles"],
  ["rbacBindings", "collection.rbacBindings"],
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

/* role="status", like the webhook row's "Test queued" line. */
function ImportResultTable({ result }: { result: ConfigImportResult }) {
  const t = useT(settingsDict);
  return (
    <div role="status" className="mt-4">
      <p className="text-sm font-medium">{result.dryRun ? t("bundle.dryRun") : t("bundle.applied")}</p>
      <div className="mt-2 overflow-x-auto">
        <table className="w-full text-left text-xs">
          <thead className="text-muted-foreground">
            <tr>
              <th scope="col" className="py-1 pr-4 font-medium">
                {t("bundle.col.collection")}
              </th>
              <th scope="col" className="py-1 pr-4 font-medium">
                {t("bundle.col.created")}
              </th>
              <th scope="col" className="py-1 pr-4 font-medium">
                {t("bundle.col.updated")}
              </th>
              <th scope="col" className="py-1 pr-4 font-medium">
                {t("bundle.col.skipped")}
              </th>
            </tr>
          </thead>
          <tbody className="divide-y divide-border">
            {IMPORT_COLLECTIONS.map(([key, labelKey]) => {
              const c = result[key];
              /* A collection the response OMITS is skipped, not rendered as zeroes and never crashed
                 on. The server leaves a section out when the caller may not see it — the rbac ones
                 are absent without rbac:manage — and reading .created off undefined took the whole
                 Settings page down with it, turning "you cannot see this section" into a blank
                 screen. An absent row says the same thing more honestly than a row of zeroes would. */
              if (!c) return null;
              return (
                <tr key={key} data-testid={`import-row-${key}`}>
                  <th scope="row" className="py-1.5 pr-4 font-normal">
                    {t(labelKey)}
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
      {IMPORT_COLLECTIONS.map(([key, labelKey]) => {
        const c = result[key];
        if (!c) return null;
        if (c.errors.length === 0 && c.warnings.length === 0) return null;
        return (
          <div key={key} className="mt-4">
            <p className="text-xs font-semibold">{t(labelKey)}</p>
            <ImportNotes label={t("bundle.errors")} notes={c.errors} tone="text-health-bad" />
            <ImportNotes label={t("bundle.warnings")} notes={c.warnings} tone="text-health-warn" />
          </div>
        );
      })}
    </div>
  );
}

function ExportImportSection() {
  const t = useT(settingsDict);
  /* Spread it onto the control; the alias below is for the control that composes it with a local condition. */
  const guard = useWriteGuard();
  const writesDisabled = guard.disabled;
  const fileId = useId();
  const [exporting, setExporting] = useState(false);
  const [exportError, setExportError] = useState<string>();
  const [bundle, setBundle] = useState<ConfigBundle>();
  /* The picked file's NAME, kept because the visually-hidden input no longer shows it. */
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
      setExportError(queryErrorMessage(err, t("bundle.exportFailed")));
    }
    setExporting(false);
  }

  async function runImport(b: ConfigBundle, dryRun: boolean) {
    setImporting(true);
    setImportError(undefined);
    try {
      setResult(await importConfig(b, dryRun));
    } catch (err) {
      setImportError(queryErrorMessage(err, t("bundle.importRefused")));
    }
    setImporting(false);
  }

  async function handleFile(file: File | undefined) {
    setResult(undefined);
    setImportError(undefined);
    setBundle(undefined);
    setFileName(file?.name);
    if (!file) return;
    const parsed = parseBundle(await file.text(), t);
    if (!parsed.ok) {
      setImportError(parsed.message);
      return;
    }
    setBundle(parsed.bundle);
    // ALWAYS the dry run first, without being asked; a bundle is the whole declarative
    // configuration.
    await runImport(parsed.bundle, true);
  }

  return (
    <SectionCard title={t("bundle.heading")}>
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("bundle.blurb")}</p>

      <div className="mt-4 flex flex-col gap-2">
        {/* Export is a READ, so the Time Machine does not touch it. Engaging the
            Time Machine blocks WRITES to avoid the confusion of editing the
            fleet from a historical view; downloading the current configuration
            from that view changes nothing and hides nothing. (What it exports
            is the LIVE configuration either way — /api/v1/export takes no `at`,
            and config tables are not time-folded.) */}
        <div>
          <Button size="sm" loading={exporting} onClick={handleExport}>
            {t("bundle.export")}
          </Button>
        </div>
        {exportError ? <ErrorLine>{exportError}</ErrorLine> : null}
      </div>

      <div className="mt-6 flex flex-col gap-2">
        <span className="text-[13px] text-muted-foreground">{t("bundle.field")}</span>
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
            /* The accessible name stays the FIELD's name, not the button's text. */
            aria-label={t("bundle.field")}
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
            {t("bundle.choose")}
          </label>
          {/* The name the native control used to show on the operator's
              behalf. Without it, a hidden input means a picked file leaves no
              trace at all until the dry-run table lands. The FILE NAME itself
              is the operator's own bytes and is printed as it is. */}
          <span data-testid="bundle-file-name" className="min-w-0 truncate text-xs text-muted-foreground">
            {fileName ?? t("bundle.noFile")}
          </span>
        </div>
        <p className="max-w-prose text-xs leading-relaxed text-muted-foreground">{t("bundle.dryRunNote")}</p>
        <div>
          <Button
            size="sm"
            loading={importing}
            /* Enabled the moment a bundle is loaded, and NOT gated on what the dry run predicted. */
            {...guard} disabled={writesDisabled || bundle === undefined}
            onClick={() => bundle && void runImport(bundle, false)}
          >
            {t("bundle.apply")}
          </Button>
        </div>
        {importError ? <ErrorLine>{importError}</ErrorLine> : null}
        {result ? <ImportResultTable result={result} /> : null}
      </div>
    </SectionCard>
  );
}

/* ── language ───────────────────────────────────────────────────────────── */

/** LANGUAGE_OPTIONS names each language IN THAT LANGUAGE. */
const LANGUAGE_OPTIONS: readonly SegmentedOption<Locale>[] = [
  { value: "en", label: "English" },
  { value: "ru", label: "Русский" },
];

/**
 * LanguageSection is the console's language switch (lib/i18n); ungated and unconditional: it is the
 * one control on this page that belongs to the PERSON rather than to their role.
 */
function LanguageSection() {
  const { locale, setLocale } = useLocale();
  const t = useT(settingsDict);
  return (
    <SectionCard title={t("language.title")}>
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("language.description")}</p>
      <div className="mt-4">
        <Segmented options={LANGUAGE_OPTIONS} value={locale} onChange={setLocale} aria-label={t("language.aria")} />
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

/** subjectLine joins the subject's kind and display name, skipping whatever is missing. */
export function subjectLine(kind: string, displayName: string): string {
  return [kind, displayName].map((s) => s.trim()).filter((s) => s !== "").join(" · ");
}

/* The generated OpenAPI shape of GET /api/v1/config. lib/types.ts's hand-written Config predates
   the scheduler/retention fields; drop this alias once it re-exports the schema. */
type ApiConfig = components["schemas"]["Config"];

function AboutSection() {
  const t = useT(settingsDict);
  const { me } = useAuth();
  const { data } = useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });
  const config = data as ApiConfig | undefined;
  /* Same ["version"] entry useCapabilities polls, so this costs no extra round
     trip. The section that answers "what am I looking at" could not say WHICH
     BUILD it was — the first question of any bug report. */
  const { data: version } = useQuery({ queryKey: ["version"], queryFn: getVersion });
  const mode = config?.auth.mode ?? "—";
  const roles = me?.subject.roles ?? [];

  return (
    <SectionCard title={t("about.heading")}>
      {/* The LABELS are ours; every value beside them — the auth mode, the role
          names, the subject — is what the server said it is. */}
      <dl className="mt-3 grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <Fact label={t("about.authMode")}>{mode}</Fact>
        <Fact label={t("about.roles")}>{roles.length > 0 ? roles.join(", ") : "—"}</Fact>
        {/* Empty segments are DROPPED, not rendered as a gap — the same
            treatment lib/investigation-sources.ts's auditDetailLine got in
            round 3, applied here in round 5 (finding #9). An anonymous subject
            has no displayName, and the fixed template printed the separator
            anyway: "anonymous · " reads as a name that failed to load. A
            separator is a joint between two things. */}
        <Fact label={t("about.subject")}>{me ? subjectLine(me.subject.kind, me.subject.displayName) : "—"}</Fact>
        {/* The server's own strings, verbatim — including "dev" and "unknown",
            which are the honest answer for a locally built binary and must not
            be dressed up as anything else. */}
        <Fact label={t("about.version")}>
          <span className="font-mono text-[12px]" data-testid="about-version">
            {version?.version ?? "—"}
          </span>
        </Fact>
        <Fact label={t("about.commit")}>
          <span className="font-mono text-[12px] break-all" data-testid="about-commit">
            {version?.commit ?? "—"}
          </span>
        </Fact>
        <Fact label={t("about.controller")}>
          {config?.controller.configured ? t("about.configured") : t("about.notConfigured")}
        </Fact>
        <Fact label={t("about.prometheus")}>
          {config?.prometheus.configured ? t("about.configured") : t("about.notConfigured")}
        </Fact>
        {/* «База данных» is feminine, so it takes the feminine participle;
            the two above are masculine and keep the plain key. */}
        <Fact label={t("about.database")}>
          {config?.database.configured ? t("about.configured.f") : t("about.notConfigured.f")}
        </Fact>
      </dl>

      {/* The role SOURCE differs by mode, and only in anonymous mode is it a
          config value the browser is told. In local/oidc mode the roles above
          come from the authenticated subject, which is what "Your roles"
          already says. */}
      {mode === "anonymous" ? (
        <p className="mt-4 max-w-prose text-xs leading-relaxed text-muted-foreground">
          {/* The ROLE is a config value and goes in as it is. */}
          {t("about.anonymous", { role: config?.auth.role ?? "" })}
        </p>
      ) : null}

      {/* Real numbers, printed only when the server actually told them: without a database there
          is nothing retained, and an older server omits the field entirely. */}
      {config?.database.configured && typeof config.database.retentionDays === "number" ? (
        <p className="mt-4 max-w-prose text-xs leading-relaxed text-muted-foreground">
          {config.database.retentionDays > 0
            ? t("about.retention", { days: config.database.retentionDays })
            : t("about.retention.off")}
        </p>
      ) : null}
      <p className="mt-3 max-w-prose text-xs leading-relaxed text-muted-foreground">
        {withNodes(t("about.maintenance"), {
          investigate: <SurfaceLink to="/investigate">{t("link.investigate")}</SurfaceLink>,
          explore: <SurfaceLink to="/explore">{t("link.explore")}</SurfaceLink>,
        })}
      </p>
    </SectionCard>
  );
}

/* ── page ───────────────────────────────────────────────────────────────── */

/** SettingsPage renders for ANY authenticated subject; `can` fails closed while GET /api/v1/auth/me is in flight. */
export function SettingsPage() {
  const t = useT(settingsDict);
  const { me, can } = useAuth();
  const canTokens = can("tokens:manage");
  const canWebhooks = can("webhooks:manage");
  const canBundle = can("settings:write");
  /* maintenance:WRITE, not :read — see MaintenanceWindowsSection. */
  const canMaintenance = can("maintenance:write");

  let body: ReactNode;
  if (me === undefined) {
    body = (
      <Card role="status" aria-live="polite" className="p-6">
        <span className="sr-only">{t("loading")}</span>
        <Skeleton className="h-10 w-full" />
      </Card>
    );
  } else {
    body = (
      <>
        {/* First, and for everyone — see LanguageSection. */}
        <LanguageSection />
        {!canTokens && !canWebhooks && !canBundle && !canMaintenance ? (
          <Card role="status" className="p-6">
            <p className="text-sm font-medium">{t("nothing.title")}</p>
            <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("nothing.body")}</p>
          </Card>
        ) : null}
        {/* First of the gated sections: the user menu links straight at it. */}
        {canTokens ? <TokensSection /> : null}
        {canWebhooks ? <WebhooksSection /> : null}
        {canMaintenance ? <MaintenanceWindowsSection /> : null}
        {canBundle ? <ExportImportSection /> : null}
        <AboutSection />
      </>
    );
  }

  return (
    /* The title is the same word the sidebar's nav.settings uses. */
    <PageShell title={t("title")} description={t("description")}>
      {body}
    </PageShell>
  );
}
