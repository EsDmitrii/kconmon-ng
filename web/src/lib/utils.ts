import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

/**
 * escapeLabelValue guards a hand-built PromQL selector against a node name,
 * target name or destination containing a `"` or `\` — PromQL's string-literal
 * escaping matches Go's and JSON's, so the two replacements below are the whole
 * rule.
 *
 * It lives here because three pages now build selectors by hand (pair-card,
 * target-card and the MTR changes timeline) and each had grown its own private
 * copy. One escaping rule, one place: a fourth call site cannot quietly ship a
 * fifth interpretation of what a quote in a name means.
 *
 * Escaping is NOT the security boundary — POST /api/v1/promql/* is a guarded
 * proxy with its own limits, and the server remains the only real arbiter of
 * what a query is allowed to do. This keeps an operator's legal-but-awkward
 * name from producing a syntactically broken query.
 */
export function escapeLabelValue(v: string): string {
  return v.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}

/**
 * fmtEventTime is THE clock for an event row, wherever it is rendered — the
 * Live feed and the Overview's recent-events card both call this and nothing
 * else (QA round 1, finding #10: the two pages showed the same event at two
 * different times, one in UTC and one local, with no marking on either).
 *
 * Local wall clock, 24h, to the SECOND. Local because an operator correlating
 * a row against a graph, a pager or their own memory reads the clock on the
 * wall, not the one in Zulu; 24h because a feed line is dense and " PM" is
 * three characters spent on nothing; seconds because two events inside one
 * minute is the normal case in this feed and a minute-precision column would
 * flatten their order into a wall of identical stamps.
 *
 * An unparseable timestamp comes back verbatim rather than as "Invalid Date":
 * the wire's own bytes are more use to whoever has to explain them.
 */
export function fmtEventTime(timestamp: string): string {
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : d.toLocaleTimeString(undefined, { hour12: false });
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
 * runsAtOrBefore is the Time Machine's cut across a run list.
 *
 * It exists because GET /api/v1/runs has NO time filter — its query is
 * type/status/cursor/limit and nothing else (internal/console/httpapi/runs.go's
 * handleRunsList), so the newest page it returns is the newest page NOW, and a
 * card rendering it under a banner reading "you are viewing 12:00" was listing
 * runs that had not happened yet (QA round 2, finding #6).
 *
 * Client-side over the fetched page is therefore the whole of what is
 * available, and it has a real limitation the panels state in words: a run
 * older than that page is not reached by paging backwards here. A server-side
 * `?to=` would fix both halves and is the right eventual answer.
 *
 * Live (`at === null`) it is the identity function — no filtering, no copy.
 */
export function runsAtOrBefore<T extends RunInstant>(runs: T[], at: Date | null): T[] {
  if (at === null) return runs;
  const cutoff = at.getTime();
  return runs.filter((r) => runInstant(r) <= cutoff);
}

/**
 * plural renders "1 hop" / "2 hops" — the count and its noun, agreeing (QA
 * round 4, finding #12).
 *
 * English-only and deliberately naive: this console pluralises by appending an
 * "s", and every call site it exists for ("hop", "trace", "path") is regular.
 * An irregular noun gets its own plural spelled out rather than a suffix
 * parameter nobody would read at the call site; a surface that needs real
 * plural RULES should reach for Intl.PluralRules instead of widening this.
 */
export function plural(n: number, singular: string, pluralForm = `${singular}s`): string {
  return `${n} ${n === 1 ? singular : pluralForm}`;
}

/**
 * CHECKBOX_CLASS is this console's ONE styling for a native checkbox (QA round
 * 5, finding #14).
 *
 * There is no <Checkbox> component and this deliberately does not add one: a
 * checkbox styled from scratch means reimplementing the indeterminate state,
 * the label association and the space-key behaviour that the native control
 * already has correct. What the native control lacks is only THEMING — it
 * renders in the OS accent colour, which is the blue rectangle that looked
 * pasted-on beside this console's own controls, and in dark mode a bright
 * white box.
 *
 * `accent-primary` is the whole fix: CSS accent-color tints the native check
 * with the theme's own token, and it follows light/dark because the token
 * does. The size and ring match the pickers' controls so a checkbox in a row
 * is the same object as a checkbox in a form. Applied verbatim at every
 * `type="checkbox"` in web/src — the three that already carried a partial
 * version of this, and the ones that carried nothing.
 */
export const CHECKBOX_CLASS =
  "size-4 shrink-0 cursor-pointer rounded border-border-strong accent-primary " +
  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring " +
  "focus-visible:ring-offset-2 focus-visible:ring-offset-background " +
  "disabled:cursor-not-allowed disabled:opacity-50";

/**
 * endSentence closes a phrase with a full stop so the console's own sentence
 * can follow it (QA round 5, finding #10).
 *
 * RFC 7807 `detail` values are phrases — "no incident with that id", "failed
 * to query schedules" — because they are written to be embedded, not read
 * aloud. A UI that concatenates one with a sentence of its own therefore
 * produces a run-on: "...with that id The page is showing...". Appending the
 * stop here rather than at every call site keeps the rule in one place.
 *
 * A phrase that already ENDS in sentence punctuation is returned untouched:
 * some details are full sentences, and "...deleted.." is worse than the
 * run-on. An empty string stays empty — a lone full stop is not a sentence.
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

/** The one message both the definition form and the run forms show. It repeats
 *  the four accepted shapes rather than saying "invalid": an operator who typed
 *  a wrong thing needs to know what a right thing looks like. It also contains
 *  the phrase "destination address", so targets.tsx's fieldForDetail places the
 *  SERVER's version of this refusal on the same field. */
export const ADHOC_ADDRESS_ERROR =
  "destination address must be a host, an IP, host:port, or an http(s) URL — " +
  "the agent resolves and dials exactly this string";

function isHostLiteralIP(host: string): boolean {
  // Bracketed IPv6 takes the literal path too, exactly as the agent's
  // allowlist does (checker.parseLiteral strips the brackets first).
  const bare = host.length > 1 && host.startsWith("[") && host.endsWith("]") ? host.slice(1, -1) : host;
  if (bare === "") return false;
  // No IP parser in the browser: the two literal families are matched
  // structurally instead. IPv4 is four in-range octets; IPv6 is the hex/colon
  // alphabet, which is enough to tell a literal from a name here — the agent's
  // netip.ParseAddr remains the real arbiter, and anything this lets through
  // still has to survive the allowlist.
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
 * isValidAdhocAddress mirrors store.validateAdhocAddress (internal/console/
 * store/targets.go) — the client half of QA round 4's finding #13, where
 * "sdfsdfsdf !!" was accepted, persisted, and only ever failed as a resolver
 * error minutes later on every agent.
 *
 * The rule is DERIVED FROM WHAT THE AGENT ACCEPTS, not invented:
 *
 *  - an http:// or https:// URL with a host. checker.validateExternalHTTP is
 *    the only code path that parses the address as a URL, and it demands
 *    exactly that scheme and a non-empty hostname.
 *  - an IP literal, bracketed or bare. checker.Allowlist.parseLiteral takes it
 *    verbatim and never asks DNS.
 *  - a DNS name, which is what ResolveAllowed hands to LookupNetIP.
 *  - either of the last two with a `:port` suffix, because BOTH senders split
 *    it off at the boundary (checks.externalTarget for the continuous path,
 *    agent.approveExternalTarget for the one-shot one) and the port must parse
 *    as a number in [1,65535] for that split to happen at all.
 *
 * Nothing stricter than that: a shape the agent could dial is accepted here
 * even if the allowlist would then refuse the address, because a refusal is a
 * POLICY answer that belongs to the agent, not a typo this form can spot.
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
  // The port split, only where the senders would find one: the last colon,
  // with a bracketed IPv6 host kept whole. A bare IPv6 literal has several
  // colons and no brackets, which is exactly the case net.SplitHostPort
  // refuses — so it falls through to the literal check below, untouched.
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
