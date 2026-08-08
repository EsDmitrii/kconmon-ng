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
