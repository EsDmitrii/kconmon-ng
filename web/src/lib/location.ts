/**
 * location.ts — "tell me when the URL changed", for the pages that read their
 * parameters straight off `window.location` (QA round 3, finding #10).
 *
 * THE PROBLEM THIS SOLVES. This console reads query parameters through
 * `window.location` and writes them through `window.history` — login.tsx's
 * `?returnTo=`, lib/timemachine.tsx's `?at=`, matrix.tsx's `?protocol=`,
 * investigate.tsx's whole entry contract. That convention is deliberate (no
 * router search-param framework is adopted), and it has one hole: `popstate`
 * fires for the BACK and FORWARD buttons and for nothing else. An in-app
 * navigation — a ⌘K palette action, a sidebar link, a card's "Investigate this"
 * — goes through TanStack Router, which reaches `history.pushState` directly.
 * The URL changes, no popstate is dispatched, and a page already mounted on
 * that route keeps rendering the parameters it read at mount. On /investigate
 * that meant navigating to a fresh investigation and being shown the previous
 * one, with the new URL in the address bar claiming otherwise.
 *
 * WHY WRAPPING history IS THE HONEST FIX HERE. The alternative is to subscribe
 * to the router (`useRouterState`), which would bind these pages to a
 * RouterProvider they currently do not need — they are mounted bare in every
 * test in this repo, and in AppShell's own test seam. Wrapping the two history
 * methods keeps the "window.location is the source of truth" convention whole
 * and catches EVERY writer of the URL, including the router's own (TanStack's
 * browser history wraps the same two methods for exactly this reason) and the
 * page's own writeParams.
 *
 * The wrapper is installed on the first subscriber and removed with the last,
 * and it restores the exact function it replaced — so it composes with
 * TanStack's wrapper in either order and leaves nothing behind on unmount.
 */

type Listener = () => void;

const listeners = new Set<Listener>();

let installed: { pushState: History["pushState"]; replaceState: History["replaceState"] } | null = null;

function notify(): void {
  // A copy: a listener is allowed to unsubscribe itself while being called.
  for (const listener of [...listeners]) listener();
}

function install(): void {
  if (installed !== null) return;
  const { pushState, replaceState } = window.history;
  installed = { pushState, replaceState };
  window.history.pushState = function (...args: Parameters<History["pushState"]>) {
    const result = pushState.apply(window.history, args);
    notify();
    return result;
  };
  window.history.replaceState = function (...args: Parameters<History["replaceState"]>) {
    const result = replaceState.apply(window.history, args);
    notify();
    return result;
  };
  window.addEventListener("popstate", notify);
}

function uninstall(): void {
  if (installed === null) return;
  window.history.pushState = installed.pushState;
  window.history.replaceState = installed.replaceState;
  window.removeEventListener("popstate", notify);
  installed = null;
}

/**
 * subscribeToLocation calls `onChange` after any navigation that changed the
 * URL without a full page load: Back/Forward, a router navigation, or this
 * page's own history write.
 *
 * It does NOT diff the URL — a subscriber that only cares about its own
 * parameters has to compare them itself, because "the URL changed" and "MY
 * parameter changed" are different questions and only the caller knows which
 * keys it owns. investigate.tsx compares the whole search string against the
 * last one it applied, which also makes its own writes cheap no-ops.
 *
 * Returns the unsubscribe.
 */
export function subscribeToLocation(onChange: Listener): () => void {
  listeners.add(onChange);
  install();
  return () => {
    listeners.delete(onChange);
    if (listeners.size === 0) uninstall();
  };
}
