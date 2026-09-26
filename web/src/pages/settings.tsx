import { Fragment, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink } from "lucide-react";
import { DOCS_BASE_URL } from "@/components/page-help";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { TextLink } from "@/components/ui/text-link";
import { EmptyState } from "@/components/ui/empty-state";
import { Pager, usePager } from "@/components/ui/pager";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { Input } from "@/components/ui/input";
import { Segmented, type SegmentedOption } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TBody, Td, Th, THead, Tr } from "@/components/ui/table";
import { ErrorLine, ROW_ACTION, RowActionLabel, SectionCard } from "@/components/settings-section";
import { UsersSection } from "./settings-users";
import { useAuth } from "@/hooks/use-auth";
import { useConsoleConfig } from "@/hooks/use-capabilities";
import { useConfirmStep } from "@/hooks/use-confirm-step";
import { useDisclosureFocus } from "@/hooks/use-disclosure-focus";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import {
  ApiError,
  createToken,
  createWebhook,
  deleteToken,
  deleteWebhook,
  exportConfig,
  getVersion,
  importConfig,
  listTokens,
  listUsers,
  listWebhooks,
  queryErrorMessage,
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
 * WHAT IS HERE: the language switcher, local users (pages/settings-users.tsx), API tokens, webhook
 * endpoints, configuration export/import, and About. Maintenance windows are DECLARED on the chart surfaces and MANAGED on /alerting;
 * a second form for either here would be a second place to get the same thing wrong.
 */

/* ── shared bits (the section building blocks live in components/settings-section.tsx) ── */

/** The locale is required: a bare toLocaleString() reorders the date and swaps in AM/PM from
 *  whatever the browser was installed in — "8/10/2026 3:47 AM" on a Russian page. */
function fmtTime(timestamp: string | null | undefined, locale: Locale): string {
  if (!timestamp) return "—";
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : stampFull(d, locale);
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
    <TextLink href={withAtParam(to)}>
      {children}
    </TextLink>
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
  /* The in-flight guard, not just a disabled look:
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
    <Card asChild className="p-4 sm:p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-4 [&>*]:max-w-2xl">
        <h3 className="type-section">
          {initial ? t("webhooks.form.edit", { name: initial.name }) : t("webhooks.form.create")}
        </h3>
        <div className="grid grid-cols-[minmax(0,1fr)] gap-4 sm:grid-cols-2">
          {/* The message is a SIBLING of the label, not a child of it: text
              inside a wrapping <label> becomes part of the control's accessible
              name, and "Name webhook: name "pd" is already taken" is not what
              the box is called. */}
          <div className="flex min-w-0 flex-col gap-1 text-[13px]">
            <label className="flex flex-col gap-1">
              <span className="text-muted-foreground">{t("webhooks.form.name")}</span>
              {/* The two placeholders are sample VALUES — a receiver's name and
                  a receiver's URL — and stay as they are. */}
              <Input
                value={draft.name}
                placeholder="pagerduty"
                aria-invalid={invalid("name") || undefined}
                aria-describedby={describedBy("name")}
                onChange={(e) => edit("name", { name: e.target.value })}
                className="w-full"
              />
            </label>
            <FieldError field="name" />
          </div>
          <div className="flex min-w-0 flex-col gap-1 text-[13px]">
            <label className="flex flex-col gap-1">
              <span className="text-muted-foreground">{t("webhooks.form.url")}</span>
              <Input
                value={draft.url}
                placeholder="https://hooks.example.test/incidents"
                aria-invalid={invalid("url") || undefined}
                aria-describedby={describedBy("url")}
                onChange={(e) => edit("url", { url: e.target.value })}
                className="w-full"
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
              payload carries — they render as themselves. A grid, not a wrap:
              five boxes in a wrapping row left the fifth alone on a line of
              its own; two columns on a phone and three above it never do. */}
          <div className="grid grid-cols-2 gap-x-4 gap-y-2 sm:grid-cols-3">
            {WEBHOOK_EVENTS.map((event) => (
              <label key={event} className="flex items-center gap-2">
                <input
                  type="checkbox"
                  checked={draft.events.includes(event)}
                  onChange={() => toggleEvent(event)}
                  className={CHECKBOX_CLASS}
                />
                <span className="mono-data">{event}</span>
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
          <Input
            id={secretId}
            type="password"
            value={draft.secret}
            aria-invalid={invalid("secret") || undefined}
            aria-describedby={invalid("secret") ? fieldErrorId("secret") : `${secretId}-help`}
            onChange={(e) => edit("secret", { secret: e.target.value })}
            className="w-full"
          />
          <FieldError field="secret" />
          {/* Write-only, in both directions: the API never returns a secret, so
              this box starts empty even when editing an endpoint that has one.
              While the refusal is showing, the hint steps aside: the two said
              the same thing twice, once in red and once in grey. */}
          {invalid("secret") ? null : (
            <span id={`${secretId}-help`} className="text-xs leading-relaxed text-muted-foreground">
              {initial ? t("webhooks.form.secretKeep") : t("webhooks.form.secretNew")}
            </span>
          )}
        </div>

        {/* The form-level slot, for a refusal that names no field this form
            has. Everything that DOES name one renders at that field instead of
            here, which is the whole point: the message and the box it is about
            are now in the same place. */}
        {errors.form ? <ErrorLine id={errorId}>{errors.form}</ErrorLine> : null}

        <div className="flex flex-wrap gap-2">
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
    /* Two lines of data and a fixed action column: the first line is the
       endpoint (name, URL, the state pills), the second its subscriptions.
       The actions never fall under the data — on a phone they take a line of
       their own, right-aligned; from sm up they are the right column. */
    <li className="flex flex-wrap items-start gap-x-3 gap-y-2 py-2.5 text-sm sm:flex-nowrap">
      <div className="flex min-w-0 flex-1 flex-col gap-1.5">
        <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
          <span className="shrink-0 font-medium">{hook.name}</span>
          {/* The endpoint URL is an identifier, so it wears the data face. It
              is the flexible column: flex-1 with a small floor, so a long one
              truncates and the pills stay on this line rather than wrapping
              under it; the whole value is the title. */}
          <span className="mono-data min-w-[10rem] flex-1 truncate text-muted-foreground" title={hook.url}>
            {hook.url}
          </span>
          {/* Both pills describe a BOOLEAN this page read, so both translate. */}
          <Badge dot variant={hook.enabled ? "ok" : "unknown"}>
            {hook.enabled ? t("webhooks.row.enabled") : t("webhooks.row.disabled")}
          </Badge>
          {/* hasSecret is always true for a stored row, so this reads as the
              contract statement the API intends — "this endpoint signs its
              deliveries" — rather than as a question with two answers. An
              imported endpoint is the one case that can be false (plan
              Decision 9), and that one is a measured bad state with a dot. */}
          <Badge dot={!hook.hasSecret} variant={hook.hasSecret ? "neutral" : "bad"}>
            {hook.hasSecret ? t("webhooks.row.signed") : t("webhooks.row.noSecret")}
          </Badge>
          {/* lastStatus is the DELIVERY LADDER's own string ("ok", "failed: 502").
              This row picks its colour and prints it; it does not rewrite it.
              Empty means the ladder has never tried: one muted sentence, not
              an em-dash pill beside an em-dash stamp. */}
          <span data-testid="last-status" className="flex items-center gap-2">
            {hook.lastStatus === "" ? (
              <span className="type-meta">{t("webhooks.row.neverDelivered")}</span>
            ) : (
              <Badge dot variant={lastStatusTone(hook.lastStatus)}>
                {hook.lastStatus}
              </Badge>
            )}
          </span>
          {hook.lastAttempt ? (
            <span className="mono-data whitespace-nowrap text-muted-foreground">{fmtTime(hook.lastAttempt, locale)}</span>
          ) : null}
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
        </div>
        {/* The subscriptions: machine values, so the chips wear the data face. */}
        <div className="flex flex-wrap gap-2">
          {hook.events.map((event) => (
            <Badge key={event} variant="neutral" className="mono-data">
              {event}
            </Badge>
          ))}
        </div>
        {queued ? (
          <span role="status" className="type-meta">
            {t("webhooks.row.queued")}
          </span>
        ) : null}
        {error ? (
          <span role="alert" className="text-xs leading-relaxed text-health-bad">
            {error}
          </span>
        ) : null}
      </div>

      <span className="flex basis-full shrink-0 items-center justify-end gap-1 sm:basis-auto">
        {confirming ? (
          <>
            {/* Spoken as well as drawn — the row swaps its controls under the reader. */}
            <span role="status" className="sr-only">
              {t("webhooks.row.confirmDelete", { name: hook.name })}
            </span>
            {/* The destructive treatment is reserved for this second click. */}
            <Button
              ref={confirmRef}
              size="sm"
              variant="destructive"
              className={ROW_ACTION}
              loading={busy}
              {...guard}
              aria-label={t("webhooks.row.confirmDelete", { name: hook.name })}
              onClick={handleDelete}
            >
              <RowActionLabel
                text={t("webhooks.row.confirmDelete.verb")}
                title={t("webhooks.row.confirmDelete", { name: hook.name })}
              />
            </Button>
            <Button size="sm" variant="ghost" className={ROW_ACTION} onClick={reset}>
              {t("cancel")}
            </Button>
          </>
        ) : (
          <>
            <Button
              size="sm"
              variant="ghost"
              className={ROW_ACTION}
              {...guard}
              aria-label={t("webhooks.row.test", { name: hook.name })}
              onClick={handleTest}
            >
              <RowActionLabel text={t("webhooks.row.test.verb")} title={t("webhooks.row.test", { name: hook.name })} />
            </Button>
            <Button
              size="sm"
              variant="ghost"
              className={ROW_ACTION}
              {...guard}
              aria-label={t("webhooks.row.edit", { name: hook.name })}
              onClick={onEdit}
            >
              <RowActionLabel text={t("webhooks.row.edit.verb")} title={t("webhooks.row.edit", { name: hook.name })} />
            </Button>
            <Button
              ref={triggerRef}
              size="sm"
              variant="ghost"
              className={ROW_ACTION}
              {...guard}
              aria-label={t("webhooks.row.delete", { name: hook.name })}
              onClick={ask}
            >
              <RowActionLabel text={t("webhooks.row.delete.verb")} title={t("webhooks.row.delete", { name: hook.name })} />
            </Button>
          </>
        )}
      </span>
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

  const listEmpty = query.isSuccess && hooks.length === 0;
  /* The create button sits on the section's heading line, or in the empty
     slate when there is nothing listed, so the one action is never drawn
     twice. Not until the list has settled, though: heading-then-slate is a
     REMOUNT, and a click on the first node between the two renders opened
     nothing. While the form is open there is no button either. */
  const createButton =
    editing.mode === "none" && !query.isPending ? (
      <Button size="sm" {...guard} onClick={() => setEditing({ mode: "create" })}>
        {t("webhooks.new")}
      </Button>
    ) : null;

  return (
    <div className="flex flex-col gap-5">
      {editing.mode !== "none" ? (
        <WebhookForm
          key={editing.mode === "edit" ? editing.hook.id : "create"}
          initial={editing.mode === "edit" ? editing.hook : undefined}
          onDone={() => setEditing({ mode: "none" })}
        />
      ) : null}

      <SectionCard title={t("webhooks.heading")} blurb={t("webhooks.blurb")} action={listEmpty ? null : createButton}>
        {query.isError ? (
          <ErrorLine onRetry={() => void query.refetch()}>
            {queryErrorMessage(query.error, t("webhooks.unavailable"))}
          </ErrorLine>
        ) : null}
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
        {listEmpty ? (
          <EmptyState
            compact
            className="mt-4"
            title={t("webhooks.empty")}
            body={t("webhooks.empty.body")}
            action={createButton}
          />
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

/* ── API tokens ───────────────────────────────── */

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
    <Card asChild className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-4 sm:p-6">
      <section aria-label={t("tokens.secret.aria")}>
        <h3 className="type-section">{t("tokens.secret.title", { name: minted.name })}</h3>
        <p className="mt-2 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("tokens.role")}</p>
        {/* The server's bytes, selectable and wrapped — never truncated, or the
            one copy an operator gets would be a partial token. */}
        <p data-testid="minted-token" className="mono-data mt-3 break-all rounded-md bg-surface-2 p-3">
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
    <Card asChild className="p-4 sm:p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-4 [&>*]:max-w-2xl">
        <h3 className="type-section">{t("tokens.form.create")}</h3>
        <p className="max-w-prose text-xs leading-relaxed text-muted-foreground">{t("tokens.role")}</p>
        <div className="grid grid-cols-[minmax(0,1fr)] gap-4 sm:grid-cols-2">
          <div className="flex min-w-0 flex-col gap-1 text-[13px]">
            <label htmlFor={nameId} className="text-muted-foreground">
              {t("tokens.form.name")}
            </label>
            {/* A sample VALUE, not a word — it stays as it is. */}
            <Input
              id={nameId}
              value={name}
              placeholder="ci-pipeline"
              /* The same 63 the API enforces (httpapi.tokenNameMaxLen). Stopping the typing is
                 kinder than a 422 after the fact — and the name is display text in every row. */
              maxLength={TOKEN_NAME_MAX}
              aria-invalid={invalid("name") || undefined}
              aria-describedby={invalid("name") ? `${errorId} ${nameId}-help` : `${nameId}-help`}
              onChange={(e) => setName(e.target.value)}
              className="w-full"
            />
            <span id={`${nameId}-help`} className="text-xs leading-relaxed text-muted-foreground">
              {t("tokens.form.nameHelp")}
            </span>
          </div>
          <div className="flex min-w-0 flex-col gap-1 text-[13px]">
            <span className="text-muted-foreground">{t("tokens.form.expires")}</span>
            {/* The M5 DateTimePicker, the one way this console asks for an
                instant — and here for the reason the schedule's Run-at field
                takes it: allowFuture lifts the ceiling, disablePast is the
                other half of the same rule. An expiry in the past is refused
                by the server, so a control must not offer one. Its trigger
                takes the field height (h-9) so it lines up with the name box
                beside it. */}
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
                className="h-9"
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

        <div className="flex flex-wrap gap-2">
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

function TokenRow({ token, ownerName }: { token: Token; ownerName?: string }) {
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

  /* The sentence with the name in it is the accessible name and the title;
     the verb alone is what shows. */
  const confirmSentence = t(spent ? "tokens.row.confirmPurge" : "tokens.row.confirmDelete", { name: token.name });
  const askSentence = t(spent ? "tokens.row.purge" : "tokens.row.delete", { name: token.name });

  return (
    <>
      <Tr data-testid="token-row">
        {/* The name is SERVER text and can be anything, including 4 000 characters with no space in
            them. An unbreakable string has no width to lay out against, so the row once grew to
            ~950 000 pixels and every page in the console scrolled sideways. It is bounded here and
            bounded again at the API (tokenNameMaxLen); the whole name stays in the title. */}
        <Td className="font-medium">
          {/* 8rem on a phone (name, state and the action share 311px), 12rem
              from sm up: with three full stamps beside it, a wider name column
              pushed Expires and the action into the scroll wrapper at 1440. */}
          <span className="block max-w-[8rem] truncate sm:max-w-[12rem]" title={token.name}>
            {token.name}
          </span>
        </Td>
        {/* The owner is a SUBJECT ID the server assigned; it prints as the username
            when the users list can resolve it, else as it came — identifiers and
            stamps wear the data face — bounded like the name, the id in the title.
            The secondary columns drop out on narrow screens by class, so a phone
            keeps the name, the state and the action instead of squeezing six keys
            into 311px. */}
        <Td className="hidden lg:table-cell">
          <span className="mono-data block max-w-[9rem] truncate" title={token.owner}>
            {ownerName ?? token.owner}
          </span>
        </Td>
        <Td className="mono-data hidden whitespace-nowrap text-muted-foreground md:table-cell">
          {fmtTime(token.createdAt, locale)}
        </Td>
        {/* An absent lastUsedAt means never used, which is a fact worth stating —
            fmtTime's em-dash would have read as "the API did not say". */}
        <Td data-testid="token-last-used" className="hidden whitespace-nowrap sm:table-cell">
          {token.lastUsedAt ? (
            <span className="mono-data text-muted-foreground">{fmtTime(token.lastUsedAt, locale)}</span>
          ) : (
            <span className="type-meta">{t("tokens.lastUsed.never")}</span>
          )}
        </Td>
        <Td className="hidden whitespace-nowrap md:table-cell">
          {token.expiresAt ? (
            <span className="mono-data text-muted-foreground">{fmtTime(token.expiresAt, locale)}</span>
          ) : (
            <span className="type-meta">{t("tokens.expires.none")}</span>
          )}
        </Td>
        <Td>
          <Badge dot variant={state === "active" ? "ok" : state === "revoked" ? "bad" : "unknown"}>
            {t(state === "active" ? "tokens.state.active" : state === "revoked" ? "tokens.revoked" : "tokens.expired")}
          </Badge>
        </Td>
        <Td className="whitespace-nowrap text-right">
          <span className="inline-flex items-center justify-end gap-1">
            {confirming ? (
              <>
                <span role="status" className="sr-only">
                  {confirmSentence}
                </span>
                {/* The destructive treatment is reserved for this second click. */}
                <Button
                  ref={confirmRef}
                  size="sm"
                  variant="destructive"
                  className={ROW_ACTION}
                  loading={busy}
                  {...guard}
                  aria-label={confirmSentence}
                  onClick={handleDelete}
                >
                  <RowActionLabel
                    text={t(spent ? "tokens.row.confirmPurge.verb" : "tokens.row.confirmDelete.verb")}
                    title={confirmSentence}
                  />
                </Button>
                <Button size="sm" variant="ghost" className={ROW_ACTION} onClick={reset}>
                  {t("cancel")}
                </Button>
              </>
            ) : (
              <Button
                ref={triggerRef}
                size="sm"
                variant="ghost"
                className={ROW_ACTION}
                {...guard}
                title={spent ? t("tokens.row.purgeHint") : undefined}
                aria-label={askSentence}
                onClick={ask}
              >
                <RowActionLabel
                  text={t(spent ? "tokens.row.purge.verb" : "tokens.row.delete.verb")}
                  title={spent ? t("tokens.row.purgeHint") : askSentence}
                />
              </Button>
            )}
          </span>
        </Td>
      </Tr>
      {error ? (
        <Tr>
          <Td colSpan={7}>
            <span role="alert" className="text-xs leading-relaxed text-health-bad">
              {error}
            </span>
          </Td>
        </Tr>
      ) : null}
    </>
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
  /* The users list is the only map from owner id to username; the Users section holds the
     same query, so this adds no request where both render. */
  const { can } = useAuth();
  const { data: config } = useConsoleConfig();
  const users = useQuery({
    queryKey: ["users"],
    queryFn: listUsers,
    enabled: can("users:manage") && config?.auth?.mode === "local",
  });
  const usernames = new Map((users.data ?? []).map((u) => [u.id, u.username]));
  const pager = usePager(tokens);

  const listEmpty = query.isSuccess && tokens.length === 0;
  /* The button is REPLACED by the form, so the keyboard has to be handed over;
     see hooks/use-disclosure-focus. It sits on the section's heading line, or
     in the empty slate when there is nothing listed, so the one action is
     never drawn twice — and not until the list has settled, because
     heading-then-slate is a REMOUNT and a click on the first node between the
     two renders opened nothing. */
  const createButton = creating || query.isPending ? null : (
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
  );

  return (
    <div className="flex flex-col gap-5">
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
      ) : null}

      {minted ? <MintedToken minted={minted} onDismiss={() => setMinted(undefined)} /> : null}

      <SectionCard
        id={TOKENS_ANCHOR}
        title={t("tokens.heading")}
        blurb={t("tokens.blurb")}
        action={listEmpty ? null : createButton}
      >
        {query.isError ? (
          <ErrorLine onRetry={() => void query.refetch()}>
            {queryErrorMessage(query.error, t("tokens.unavailable"))}
          </ErrorLine>
        ) : null}
        {/* isPending / isSuccess, the webhooks list's own guard: a paused retry
            is pending-but-not-fetching and must not read as "no tokens". */}
        {query.isPending ? (
          <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
            <span className="sr-only">{t("loading")}</span>
            <Skeleton className="h-10 w-full" />
          </div>
        ) : null}
        {listEmpty ? (
          <EmptyState
            compact
            className="mt-4"
            title={t("tokens.empty")}
            body={t("tokens.empty.body")}
            action={createButton}
          />
        ) : null}
        {tokens.length > 0 ? (
          <>
            {/* The shared dense table: stamps in tabular figures so the rows
                line up, secondary columns dropped by class on narrow screens
                (each cell says which), the action a fixed right column. */}
            <Table
              variant="dense"
              aria-label={t("tokens.listAria")}
              containerClassName="mt-4"
              className="[&_td:not(:last-child)]:pr-3 [&_th:not(:last-child)]:pr-3"
            >
              <THead>
                <Tr>
                  <Th>{t("tokens.head.name")}</Th>
                  <Th className="hidden lg:table-cell">{t("tokens.head.owner")}</Th>
                  <Th className="hidden md:table-cell">{t("tokens.head.created")}</Th>
                  <Th className="hidden sm:table-cell">{t("tokens.head.lastUsed")}</Th>
                  <Th className="hidden md:table-cell">{t("tokens.head.expires")}</Th>
                  <Th>{t("tokens.head.state")}</Th>
                  <Th className="text-right">
                    <span className="sr-only">{t("tokens.head.actions")}</span>
                  </Th>
                </Tr>
              </THead>
              <TBody>
                {pager.visible.map((token) => (
                  <TokenRow key={token.id} token={token} ownerName={usernames.get(token.owner)} />
                ))}
              </TBody>
            </Table>
            <Pager pager={pager} subject={t("tokens.subject")} className="px-0" />
          </>
        ) : null}
      </SectionCard>
    </div>
  );
}

/* The maintenance-windows section lives in pages/alerting.tsx: a
   window suppresses and annotates the signals Alerting owns, and Explore
   already draws its bands. */

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

/** EXPORT_SECTION_LABELS names the sections a bundle's `omitted` lists (httpapi's exportSectionGates). */
const EXPORT_SECTION_LABELS: ReadonlyMap<string, SettingsKey> = new Map([
  ["targets", "collection.targets"],
  ["checkDefinitions", "collection.checkDefinitions"],
  ["checkSchedules", "collection.checkSchedules"],
  ["alertRules", "collection.alertRules"],
  ["webhooks", "collection.webhooks"],
  ["maintenanceWindows", "collection.maintenanceWindows"],
  ["rbac", "collection.rbac"],
]);

/**
 * omittedSections reads an exported bundle's `omitted`: the sections withheld from a caller without
 * their own read permission. It is absent when nothing was withheld, and a name this build does not
 * know is shown as it came.
 */
function omittedSections(b: ConfigBundle, t: Translate<SettingsKey>): string[] {
  if (!Array.isArray(b.omitted)) return [];
  return b.omitted.map((name) => {
    const key = EXPORT_SECTION_LABELS.get(name);
    return key ? t(key) : name;
  });
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
  /* Notes sharing a sentence are one entry over all their names: the per-binding warning is
     one long sentence, and a bundle with 63 bindings printed it 63 times. */
  const grouped = new Map<string, string[]>();
  for (const note of notes) grouped.set(note.reason, [...(grouped.get(note.reason) ?? []), note.name]);
  return (
    <div className="mt-3">
      <p className={cn("text-xs font-medium", tone)}>{label}</p>
      <dl className="mt-1 flex flex-col gap-1 text-xs leading-relaxed">
        {[...grouped].map(([reason, names], i) => (
          <div key={`${reason}-${i}`} className="flex flex-wrap gap-x-2">
            <dt className="font-mono">{names.join(", ")}</dt>
            {/* Verbatim. The server names the item and says why in one
                sentence; paraphrasing it here would drop the half an operator
                needs to fix the bundle. */}
            <dd className="text-muted-foreground">{reason}</dd>
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
      {/* Five short columns: capped, or the counts drift to the far edge of a wide card. */}
      <Table variant="dense" containerClassName="mt-2 max-w-xl" scrollLabel={t("bundle.table.aria")}>
        <THead>
          <Tr>
            <Th className="pr-4">{t("bundle.col.collection")}</Th>
            <Th numeric className="pr-4">
              {t("bundle.col.created")}
            </Th>
            <Th numeric className="pr-4">
              {t("bundle.col.updated")}
            </Th>
            <Th numeric className="pr-4">
              {t("bundle.col.unchanged")}
            </Th>
            <Th numeric className="pr-4">
              {t("bundle.col.skipped")}
            </Th>
          </Tr>
        </THead>
        <TBody>
          {IMPORT_COLLECTIONS.map(([key, labelKey]) => {
            const c = result[key];
            /* A collection the response OMITS is skipped, not rendered as zeroes and never crashed
               on. The server leaves a section out when the caller may not see it — the rbac ones
               are absent without rbac:manage — and reading .created off undefined took the whole
               Settings page down with it, turning "you cannot see this section" into a blank
               screen. An absent row says the same thing more honestly than a row of zeroes would. */
            if (!c) return null;
            return (
              <Tr key={key} data-testid={`import-row-${key}`}>
                <Th scope="row" className="pr-4 font-normal">
                  {t(labelKey)}
                </Th>
                <Td numeric className="pr-4">
                  {c.created}
                </Td>
                <Td numeric className="pr-4">
                  {c.updated}
                </Td>
                {/* A server older than this count sends none, and a zero would be a claim. */}
                <Td numeric className="pr-4">
                  {typeof c.unchanged === "number" ? c.unchanged : "—"}
                </Td>
                <Td numeric className="pr-4">
                  {c.skipped}
                </Td>
              </Tr>
            );
          })}
        </TBody>
      </Table>
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

/** isClientRefusal is a 4xx problem other than 429: the import handler answers those before it writes. */
function isClientRefusal(err: unknown): boolean {
  const status = err instanceof ApiError ? err.problem.status : undefined;
  return status !== undefined && status >= 400 && status < 500 && status !== 429;
}

/* The cached lists a bundle writes to, by query-key prefix. Invalidating marks each stale, and the
   ones on screen re-read at once. */
const IMPORTED_QUERY_KEYS = [
  ["webhooks"],
  ["targets"],
  ["target"],
  ["definitions"],
  ["schedules"],
  ["checks"],
  ["alert-rules"],
  ["maintenance"],
  ["alerting", "maintenance"],
  ["investigate", "maintenance"],
  ["investigate", "targets"],
  ["rbac-roles"],
];

function ExportImportSection() {
  const t = useT(settingsDict);
  const qc = useQueryClient();
  /* Spread it onto the control; the alias below is for the control that composes it with a local condition. */
  const guard = useWriteGuard();
  const writesDisabled = guard.disabled;
  const fileId = useId();
  const [exporting, setExporting] = useState(false);
  const [exportError, setExportError] = useState<string>();
  const [exportOmitted, setExportOmitted] = useState<string[]>([]);
  const [bundle, setBundle] = useState<ConfigBundle>();
  /* The picked file's NAME, kept because the visually-hidden input no longer shows it. */
  const [fileName, setFileName] = useState<string>();
  const [importing, setImporting] = useState(false);
  /* An Apply in flight holds the file picker: a pick would supersede it and drop its answer. */
  const [applying, setApplying] = useState(false);
  const [importError, setImportError] = useState<string>();
  const [result, setResult] = useState<ConfigImportResult>();
  /* Bumped by every pick and every import call: an answer from a superseded call is dropped, so a
     slow dry run of the previous file can neither replace the current plan nor re-arm Apply. */
  const importSeq = useRef(0);

  async function handleExport() {
    setExporting(true);
    setExportError(undefined);
    setExportOmitted([]);
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
      setExportOmitted(omittedSections(b, t));
    } catch (err) {
      setExportError(queryErrorMessage(err, t("bundle.exportFailed")));
    }
    setExporting(false);
  }

  async function runImport(b: ConfigBundle, dryRun: boolean) {
    const seq = ++importSeq.current;
    setImporting(true);
    if (!dryRun) setApplying(true);
    setImportError(undefined);
    const invalidateImported = () => {
      for (const queryKey of IMPORTED_QUERY_KEYS) void qc.invalidateQueries({ queryKey });
    };
    try {
      const answer = await importConfig(b, dryRun);
      // An applied import has written whatever it wrote, even when a newer pick superseded it.
      if (!dryRun) invalidateImported();
      if (seq !== importSeq.current) return;
      setResult(answer);
    } catch (err) {
      // The server writes section by section with no single transaction: a 5xx or a lost answer
      // may follow a partial write. A 4xx is refused before anything is written.
      if (!dryRun && !isClientRefusal(err)) invalidateImported();
      if (seq !== importSeq.current) return;
      setImportError(queryErrorMessage(err, t("bundle.importRefused")));
    } finally {
      if (!dryRun) setApplying(false);
    }
    setImporting(false);
  }

  async function handleFile(file: File | undefined) {
    const seq = ++importSeq.current;
    setImporting(false);
    setResult(undefined);
    setImportError(undefined);
    setBundle(undefined);
    setFileName(file?.name);
    if (!file) return;
    const text = await file.text();
    if (seq !== importSeq.current) return;
    const parsed = parseBundle(text, t);
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
    <SectionCard title={t("bundle.heading")} blurb={t("bundle.blurb")}>
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
        {exportOmitted.length > 0 ? (
          <p role="status" className="max-w-prose text-xs leading-relaxed text-muted-foreground">
            {t("bundle.exportOmitted", { sections: exportOmitted.join(", ") })}
          </p>
        ) : null}
      </div>

      <div className="mt-6 flex flex-col gap-2">
        <span className="text-[13px] text-muted-foreground">{t("bundle.field")}</span>
        {/* The native file input is VISUALLY HIDDEN, not replaced.
            `<input type="file">` renders as the browser's own
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
            {...guard} disabled={writesDisabled || applying}
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
            /* Outline until there is a bundle to apply: a primary that cannot
               act yet would be the page's one blue promising something the
               operator has not yet given it. With a bundle loaded it is the
               primary, and a Time-Machine lock on it wears the muted disabled
               primary the Button defines. */
            variant={bundle === undefined ? "outline" : "default"}
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
    <SectionCard title={t("language.title")} blurb={t("language.description")}>
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

/** shortCommit is the twelve characters a full hash is known by on screen; anything
 *  that short already ("dev", "unknown", a seven-char short hash) prints as it came.
 *  A suffix such as "-dirty" says the build is not that commit, so only the hash in
 *  front of it is shortened. The caller keeps the full value in the title. */
export function shortCommit(commit: string): string {
  const dash = commit.indexOf("-");
  const hash = dash < 0 ? commit : commit.slice(0, dash);
  const suffix = dash < 0 ? "" : commit.slice(dash);
  return (hash.length > 12 ? hash.slice(0, 12) : hash) + suffix;
}

/* The generated OpenAPI shape of GET /api/v1/config. lib/types.ts's hand-written Config predates
   the scheduler/retention fields; drop this alias once it re-exports the schema. */
type ApiConfig = components["schemas"]["Config"];

/** Where "Source on GitHub" points. The site itself is page-help.tsx's
 *  DOCS_BASE_URL, so either destination moves by editing one line. */
export const SOURCE_URL = "https://github.com/EsDmitrii/kconmon-ng";

/* The About row's three destinations. Release notes are the site's rendering
   of RELEASE_NOTES.md (docs/reference/release-notes.md), not the raw file. */
const ABOUT_LINKS: ReadonlyArray<{ key: SettingsKey; href: string }> = [
  { key: "about.links.docs", href: DOCS_BASE_URL },
  { key: "about.links.releaseNotes", href: `${DOCS_BASE_URL}reference/release-notes/` },
  { key: "about.links.source", href: SOURCE_URL },
];

function AboutSection() {
  const t = useT(settingsDict);
  const { me } = useAuth();
  const { data } = useConsoleConfig();
  const config = data as ApiConfig | undefined;
  /* Same ["version"] entry useCapabilities polls, so this costs no extra round
     trip. The section that answers "what am I looking at" could not say WHICH
     BUILD it was — the first question of any bug report. */
  const { data: version } = useQuery({ queryKey: ["version"], queryFn: getVersion });
  /* Absent until the query answers, and absent from an older server's body
     too; either way the row says so rather than reading .length off nothing. */
  const commit = typeof version?.commit === "string" ? version.commit : undefined;
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
            treatment lib/investigation-sources.ts's auditDetailLine gives. An anonymous subject
            has no displayName, and the fixed template printed the separator
            anyway: "anonymous · " reads as a name that failed to load. A
            separator is a joint between two things. */}
        <Fact label={t("about.subject")}>{me ? subjectLine(me.subject.kind, me.subject.displayName) : "—"}</Fact>
        {/* The server's own strings, verbatim — including "dev" and "unknown",
            which are the honest answer for a locally built binary and must not
            be dressed up as anything else. */}
        <Fact label={t("about.version")}>
          <span className="mono-data" data-testid="about-version">
            {version?.version ?? "—"}
          </span>
        </Fact>
        <Fact label={t("about.commit")}>
          {/* The short hash on screen, the full one a hover away: forty hex
              characters wrapped onto two lines on a phone and said nothing the
              first twelve do not. */}
          <span
            className="mono-data"
            data-testid="about-commit"
            title={commit !== undefined && commit.length > 12 ? commit : undefined}
          >
            {commit === undefined ? "—" : shortCommit(commit)}
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
        {/* Outbound, so a new tab with no opener: the console stays where the
            operator left it, and the site gets no handle on this window. */}
        <Fact label={t("about.links")}>
          <span className="flex flex-wrap gap-x-3 gap-y-1">
            {ABOUT_LINKS.map((link) => (
              <a
                key={link.key}
                href={link.href}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 text-primary hover:underline"
              >
                {t(link.key)}
                <ExternalLink aria-hidden="true" className="size-3" />
              </a>
            ))}
          </span>
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
          alerting: <SurfaceLink to="/alerting">{t("link.alerting")}</SurfaceLink>,
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
  /* Users exist only where the console is its own identity provider; under header or oidc the
     provider owns the accounts and the API answers 404. */
  const { data: config } = useConsoleConfig();
  const canUsers = can("users:manage") && config?.auth?.mode === "local";

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
        {!canTokens && !canWebhooks && !canBundle && !canUsers ? (
          <Card role="status" className="p-4 sm:p-6">
            <p className="text-sm font-medium">{t("nothing.title")}</p>
            <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("nothing.body")}</p>
          </Card>
        ) : null}
        {/* First of the gated sections: the user menu links straight at it. */}
        {canTokens ? <TokensSection /> : null}
        {canUsers ? <UsersSection /> : null}
        {canWebhooks ? <WebhooksSection /> : null}
        {canBundle ? <ExportImportSection /> : null}
        <AboutSection />
      </>
    );
  }

  return (
    /* The title is the same word the sidebar's nav.settings uses. */
    <PageShell title={t("title")} help={{ body: t("help.body"), slug: "settings" }} description={t("description")}>
      {body}
    </PageShell>
  );
}
