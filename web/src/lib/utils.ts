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

/* ── the pair arrow nobody can type ──────────────────────────────────────── */

/**
 * PAIR_ARROW is U+2192, the ONE separator this console writes a pair scope with
 * — the same character internal/console/events/live_event.go's pairScope emits
 * and lib/investigation-sources.ts re-exports as PAIR_SEPARATOR.
 */
export const PAIR_ARROW = "→";

/**
 * TYPEABLE_SEPARATOR is every separator a keyboard CAN produce, plus the arrow
 * itself, each with any surrounding whitespace collapsed into it.
 *
 * Whitespace ALONE is deliberately not a separator, and that is the one rule
 * here worth arguing about. A scope is not always a hostname: this console's own
 * round-trip test pins "ns/pod a&b" as a node scope, so reading a bare space as
 * an arrow would quietly cut a legitimate name in half — a worse failure than
 * the one being fixed, because it corrupts a name that was typed correctly. A
 * space is only a separator when it sits next to an arrow somebody wrote.
 *
 * A hyphen NOT followed by `>` is left alone, which is what keeps "edge-gw-01"
 * one name.
 */
const TYPEABLE_SEPARATOR = /\s*(?:→|-+>|=+>|>)\s*/g;

/**
 * normalizePairInput rewrites whatever an operator typed into the canonical pair
 * scope, so every scope box matches what it draws.
 *
 * A pair reads "node-a→node-b" everywhere in this console, and U+2192 is on no
 * keyboard: the Live filter and the ?scope= permalink both compared the typed
 * text literally, so "node-a->node-b" matched nothing and the arrow had to be
 * copied out of a row to be used. Normalising the INPUT is the fix rather than a
 * placeholder teaching the arrow — the reader should not have to learn a
 * character to use a filter.
 *
 * A single name comes back untouched (trimmed) — spaces and all — so substring
 * matching over one node keeps working exactly as it did, and a half-written
 * pair ("->node-b") normalises too: it is how "anything into node-b" is typed.
 */
export function normalizePairInput(raw: string): string {
  return raw.trim().replace(TYPEABLE_SEPARATOR, PAIR_ARROW);
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

/** isIPv4Dotted matches four dotted-decimal octets, each 0-255 -- the structural
 *  half of Go's netip.ParseAddr for an IPv4 literal. Leading zeros are tolerated
 *  (Go's own IPv4 fold is stricter, but a value like "010.0.0.1" that netip
 *  refuses as an IP is then accepted by the hostname rule below anyway, so both
 *  sides still reach the same verdict). */
function isIPv4Dotted(s: string): boolean {
  const parts = s.split(".");
  return parts.length === 4 && parts.every((p) => /^\d{1,3}$/.test(p) && Number(p) <= 255);
}

/**
 * isIPv6Literal validates an IPv6 address the way Go's netip.ParseAddr does (an
 * optional %zone included): 1-4 hex digits per group, exactly eight groups OR a
 * single "::" run standing for the rest, with an optional embedded IPv4 tail. It
 * replaces the old "contains a colon and only hex/colon/dot chars" guess, which
 * accepted strings Go refuses outright -- ":" and "::::" among them.
 */
function isIPv6Literal(host: string): boolean {
  let s = host;
  const zone = s.indexOf("%");
  if (zone !== -1) {
    if (zone === s.length - 1) return false; // a "%" with an empty zone is not an address
    s = s.slice(0, zone);
  }
  if (!s.includes(":")) return false;

  // "::" is the ONE zero-run compression a literal may carry; a second is invalid.
  const dc = s.indexOf("::");
  if (dc !== s.lastIndexOf("::")) return false;

  let head: string[];
  let tail: string[];
  if (dc === -1) {
    head = s.split(":");
    tail = [];
  } else {
    const before = s.slice(0, dc);
    const after = s.slice(dc + 2);
    head = before === "" ? [] : before.split(":");
    tail = after === "" ? [] : after.split(":");
  }
  // A stray ":" (":::", or a lone leading/trailing colon) leaves an empty group.
  if (head.includes("") || tail.includes("")) return false;

  const groups = [...head, ...tail];
  let total = groups.length;
  // Only the LAST group may be a dotted-quad IPv4, and it stands for two groups.
  const last = groups[groups.length - 1];
  let hexGroups = groups;
  if (last !== undefined && last.includes(".")) {
    if (!isIPv4Dotted(last)) return false;
    hexGroups = groups.slice(0, -1);
    total += 1;
  }
  if (!hexGroups.every((g) => /^[0-9A-Fa-f]{1,4}$/.test(g))) return false;

  // No "::" demands exactly eight groups; "::" stands for at least one zero group.
  return dc === -1 ? total === 8 : total < 8;
}

/** isIPLiteral mirrors Go's isIPLiteral (store/targets.go): strip one pair of
 *  surrounding brackets, then accept an IPv4 or IPv6 literal. */
function isIPLiteral(host: string): boolean {
  const bare = host.length > 1 && host.startsWith("[") && host.endsWith("]") ? host.slice(1, -1) : host;
  if (bare === "") return false;
  return isIPv4Dotted(bare) || isIPv6Literal(bare);
}

function isHostname(host: string): boolean {
  if (host === "" || host.length > HOSTNAME_MAX) return false;
  // One trailing dot is the fully-qualified spelling and is legal; anything
  // else empty is a doubled dot.
  const labels = (host.endsWith(".") ? host.slice(0, -1) : host).split(".");
  return labels.every((l) => l.length > 0 && l.length <= HOST_LABEL_MAX && HOST_LABEL.test(l));
}

/**
 * splitHostPort ports net.SplitHostPort's SUCCESS/FAILURE decision exactly: it
 * returns {host, port} only for the strings the Go senders would themselves
 * split, and null (host stays whole, no port) for every string Go rejects with
 * missingPort/tooManyColons. The whole point is the colon grammar -- a bare host
 * has no colon so it returns null; a non-bracketed host:port splits ONLY when
 * there is exactly one colon (Go rejects two as "too many colons"), and it DOES
 * split a leading-colon ":80" into an empty host; a bracketed host must have ']'
 * immediately before the final colon.
 */
function splitHostPort(hostport: string): { host: string; port: string } | null {
  const i = hostport.lastIndexOf(":");
  if (i < 0) return null; // no colon -> missing port -> Go does not split, host stays whole

  if (hostport.startsWith("[")) {
    const end = hostport.indexOf("]");
    if (end < 0) return null; // missing ']'
    if (end + 1 === hostport.length) return null; // "[::1]" -> missing port
    if (end + 1 !== i) return null; // ']' not right before the last ':' -> tooManyColons/missingPort
    if (hostport.slice(1).includes("[")) return null; // stray '['
    if (hostport.slice(end + 1).includes("]")) return null; // stray ']'
    return { host: hostport.slice(1, end), port: hostport.slice(i + 1) };
  }

  const host = hostport.slice(0, i);
  if (host.includes(":")) return null; // more than one colon -> too many colons
  if (hostport.includes("[") || hostport.includes("]")) return null; // stray bracket
  return { host, port: hostport.slice(i + 1) };
}

/** isDecimalPort mirrors strconv.ParseUint(p, 10, 16) plus Go's n==0 rejection:
 *  DECIMAL digits ONLY (no hex "0x50", no exponent "1e3", no sign "+80", no
 *  whitespace " 80" -- every one of which a JS Number() would otherwise swallow)
 *  and a value in [1, 65535]. */
function isDecimalPort(p: string): boolean {
  if (!/^[0-9]+$/.test(p)) return false;
  const n = Number(p);
  return n >= 1 && n <= 65535;
}

/**
 * isValidAdhocAddress mirrors store.validateAdhocAddress
 * (internal/console/store/targets.go) EXACTLY -- the Go validator is the source
 * of truth, and this must not accept a shape Go refuses. The port grammar is
 * what made the two drift: Go parses a port with strconv.ParseUint(base 10), so
 * ":0x50", ":1e3", ":+80" and ": 80" are all refused, where a JS Number() would
 * accept every one of them; and Go's net.SplitHostPort rejects ":" and "::::"
 * that the old structural IPv6 guess let through. Nothing here is stricter than
 * Go: a shape the agent could dial is accepted even if the allowlist would then
 * refuse the address.
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
  const split = splitHostPort(value);
  if (split !== null) {
    if (!isDecimalPort(split.port)) return false;
    host = split.host;
  }

  return isIPLiteral(host) || isHostname(host);
}
