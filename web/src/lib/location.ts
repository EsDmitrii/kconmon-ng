/** On /investigate that meant navigating to a fresh investigation and being shown the previous. */

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
 * subscribeToLocation calls `onChange` after any navigation that changed the URL without a full
 * page load; it does NOT diff the URL — a subscriber that only cares about its own parameters has
 * to compare them itself.
 */
export function subscribeToLocation(onChange: Listener): () => void {
  listeners.add(onChange);
  install();
  return () => {
    listeners.delete(onChange);
    if (listeners.size === 0) uninstall();
  };
}
