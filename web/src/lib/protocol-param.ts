import { PROTOCOLS, type Protocol } from "./types";

/* The matrix protocol in the URL, shared by the matrix page and the object cards. It lives here, not
   in pages/matrix.tsx, so a card that reads the parameter does not pull the whole matrix page in. */

/*
 * PROTOCOL_PARAM is the matrix page's own URL key, carried the way lib/timemachine's `?at=` is carried;
 * TanStack Router owns navigation here but no route declares a search schema (timemachine.tsx
 * documents that decision).
 */
const PROTOCOL_PARAM = "protocol";

/** readProtocolFromLocation resolves ?protocol= into one of the protocols the
 *  console probes. Anything else — a typo, a stale link, a protocol this build
 *  does not know — degrades to tcp rather than rendering an empty grid for a
 *  protocol nothing will ever answer for. */
export function readProtocolFromLocation(search: string): Protocol {
  const raw = new URLSearchParams(search).get(PROTOCOL_PARAM);
  return PROTOCOLS.includes(raw as Protocol) ? (raw as Protocol) : "tcp";
}

/**
 * degradedProtocolParam answers "is the URL still claiming something this page
 * is not showing?" — a `?protocol=sctp` that silently became TCP left the lie
 * in the address bar, which is the string an operator copies and shares (QA
 * scope 2, finding #17). Null when the URL and the view already agree, which
 * includes the ordinary no-param case: the default needs no spelling out.
 */
export function degradedProtocolParam(search: string): Protocol | null {
  const raw = new URLSearchParams(search).get(PROTOCOL_PARAM);
  if (raw === null) return null;
  const resolved = readProtocolFromLocation(search);
  return raw === resolved ? null : resolved;
}

/** matrixHref is a link to the Matrix on protocol `p`; the default stays unspelled, as it does in
 *  the Matrix's own address bar. */
export function matrixHref(p: Protocol): string {
  return p === "tcp" ? "/matrix" : `/matrix?${PROTOCOL_PARAM}=${p}`;
}

/** writeProtocol is the ONE writer of ?protocol=, shared with the object cards
 *  so a second surface cannot invent a second spelling of the same key. */
export function writeProtocol(p: Protocol): void {
  const url = new URL(window.location.href);
  url.searchParams.set(PROTOCOL_PARAM, p);
  window.history.replaceState(window.history.state, "", `${url.pathname}${url.search}${url.hash}`);
}
