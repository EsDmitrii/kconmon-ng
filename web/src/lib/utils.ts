import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";
import { validationDict } from "./i18n/dict/validation";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

/**
 * escapeLabelValue guards a hand-built PromQL selector against a node name; one escaping rule, one
 * place: a fourth call site cannot quietly ship a fifth interpretation of what a quote in a name
 * means.
 */
export function escapeLabelValue(v: string): string {
  return v.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}

/**
 * fmtEventTime is THE clock for an event row, wherever it is rendered; local because an operator
 * correlating a row against a graph.
 *
 * `locale` is passed by a caller whose chrome speaks a language: a row stamp
 * stands on its own and normally keeps the VIEWER's locale (lib/i18n's own
 * rule), but 24-hour is this console's house form and `hour12: false` is what
 * pins it whichever tag arrives.
 */
export function fmtEventTime(timestamp: string, locale?: string): string {
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : d.toLocaleTimeString(locale, { hour12: false });
}

/**
 * fmtEventStamp is fmtEventTime plus the DAY, for a row that is not from today.
 *
 * A rail showing bare times turns yesterday's 15:12 into this afternoon's at a
 * glance, which is the one reading an operator must not make from a change feed
 * (QA scope 2, finding #10). The compact month/day is the house form
 * components/annotations.tsx and components/maintenance.tsx already print.
 * `now` is a parameter so the boundary is testable without moving the clock.
 */
export function fmtEventStamp(timestamp: string, locale?: string, now: Date = new Date()): string {
  const d = new Date(timestamp);
  if (Number.isNaN(d.getTime())) return timestamp;
  const sameDay =
    d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
  const time = d.toLocaleTimeString(locale, { hour12: false });
  return sameDay ? time : `${d.toLocaleDateString(locale, { month: "short", day: "numeric" })} ${time}`;
}

/** RunInstant is the shape runsAtOrBefore reads — the two timestamps every run
 *  summary and detail carries. Structural, so both RunSummary and RunDetail
 *  satisfy it without either page importing the other's types. */
export interface RunInstant {
  createdAt: string;
  startedAt?: string;
}

/**
 * runInstant is "when did this run happen", in one place: the moment it
 * STARTED, falling back to the moment it was created for a run that is still
 * pending and has no start yet.
 *
 * An unparseable stamp answers +Infinity rather than 0. That is deliberate and
 * it is the safe direction: the filter below keeps runs at or before `t`, so a
 * run this console cannot place in time is EXCLUDED from a historical view
 * rather than smuggled into it.
 */
export function runInstant(run: RunInstant): number {
  const parsed = new Date(run.startedAt ?? run.createdAt).getTime();
  return Number.isNaN(parsed) ? Number.POSITIVE_INFINITY : parsed;
}

/**
 * runsAtOrBefore is the Time Machine's cut across a run list; a server-side `?to=` would fix both
 * halves and is the right eventual answer.
 */
export function runsAtOrBefore<T extends RunInstant>(runs: T[], at: Date | null): T[] {
  if (at === null) return runs;
  const cutoff = at.getTime();
  return runs.filter((r) => runInstant(r) <= cutoff);
}

/**
 * plural renders "1 hop" / "2 hops" — the count and its noun; english-only and deliberately naive:
 * this console pluralises by appending an "s".
 */
export function plural(n: number, singular: string, pluralForm = `${singular}s`): string {
  return `${n} ${n === 1 ? singular : pluralForm}`;
}

/** CHECKBOX_CLASS is this console's ONE styling for a native checkbox. */
export const CHECKBOX_CLASS =
  "size-4 shrink-0 cursor-pointer rounded border-border-strong accent-primary " +
  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring " +
  "focus-visible:ring-offset-2 focus-visible:ring-offset-background " +
  "disabled:cursor-not-allowed disabled:opacity-50";

/**
 * endSentence closes a phrase with a full stop so the console's own sentence can follow it;
 * appending the stop here rather than at every call site keeps the rule in one place.
 */
export function endSentence(text: string): string {
  const trimmed = text.trim();
  if (trimmed === "") return "";
  return /[.!?:;]$/.test(trimmed) ? trimmed : `${trimmed}.`;
}

/* ── ad-hoc destination address (QA round 4, finding #13) ────────────────── */

/** The shape a hostname label may take. Underscores are permitted because Go's
 *  own resolver accepts them (net.isDomainName), and this rule must not refuse
 *  a name the agent would happily resolve. */
const HOST_LABEL = /^[A-Za-z0-9_]([A-Za-z0-9_-]*[A-Za-z0-9_])?$/;
const HOSTNAME_MAX = 253;
const HOST_LABEL_MAX = 63;

/**
 * The one message both the definition form and the run forms show; it repeats the four accepted
 * shapes rather than saying "invalid".
 */
export const ADHOC_ADDRESS_ERROR = validationDict.en["adhoc.address"];

function isHostLiteralIP(host: string): boolean {
  // Bracketed IPv6 takes the literal path too, exactly as the agent's
  // allowlist does (checker.parseLiteral strips the brackets first).
  const bare = host.length > 1 && host.startsWith("[") && host.endsWith("]") ? host.slice(1, -1) : host;
  if (bare === "") return false;
  // No IP parser in the browser: the two literal families are matched structurally instead.
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(bare)) {
    return bare.split(".").every((o) => Number(o) <= 255);
  }
  return bare.includes(":") && /^[0-9A-Fa-f:.%]+$/.test(bare);
}

function isHostname(host: string): boolean {
  if (host === "" || host.length > HOSTNAME_MAX) return false;
  // One trailing dot is the fully-qualified spelling and is legal; anything
  // else empty is a doubled dot.
  const labels = (host.endsWith(".") ? host.slice(0, -1) : host).split(".");
  return labels.every((l) => l.length > 0 && l.length <= HOST_LABEL_MAX && HOST_LABEL.test(l));
}

/**
 * isValidAdhocAddress mirrors store.validateAdhocAddress (internal/console/ store/targets.go);
 * nothing stricter than that: a shape the agent could dial is accepted here even if the allowlist
 * would then refuse the address.
 */
export function isValidAdhocAddress(raw: string): boolean {
  const value = raw.trim();
  if (value === "") return false;
  const lower = value.toLowerCase();

  if (lower.startsWith("http://") || lower.startsWith("https://")) {
    try {
      return new URL(value).hostname !== "";
    } catch {
      return false;
    }
  }

  let host = value;
  // The port split, only where the senders would find one: the last colon, with a bracketed IPv6
  // host kept whole.
  const colon = value.lastIndexOf(":");
  const splittable = value.startsWith("[")
    ? value.includes("]:") && value.indexOf("]:") + 1 === colon
    : colon > 0 && value.indexOf(":") === colon;
  if (splittable) {
    const port = Number(value.slice(colon + 1));
    if (!Number.isInteger(port) || port < 1 || port > 65535) return false;
    host = value.slice(0, colon);
  }

  return isHostLiteralIP(host) || isHostname(host);
}
