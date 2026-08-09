import { useCallback, useRef, useState } from "react";

/**
 * useSubmitGuard makes a form's submit RE-ENTRANT-SAFE, not merely
 * disabled-looking (QA round 5, finding #17).
 *
 * Every create/save form in this console already carried a `submitting` piece
 * of state that reached its button, and `Button` disables itself while
 * `loading` — so the control LOOKED guarded. It was not. A `useState` flag
 * cannot be read by the handler that is about to set it: three clicks
 * dispatched before React re-renders all run the same closure, all see
 * `submitting === false`, and all three POST. That is not a hypothetical
 * double-submit — it is a doubled target, a doubled schedule, a doubled
 * incident, and for POST /api/v1/runs three fan-outs at once.
 *
 * The fix is a REF beside the state. The ref is written synchronously, so the
 * second call in the same task sees it; the state exists only to re-render the
 * button. Both are updated together and neither is exported on its own, so
 * they cannot drift.
 *
 * Usage keeps each form's existing shape — begin() goes exactly where
 * setSubmitting(true) was (AFTER the synchronous validation, which must still
 * be free to return early), and end() replaces setSubmitting(false):
 *
 *   const { submitting, begin, end } = useSubmitGuard();
 *   async function handleSubmit(e: FormEvent) {
 *     e.preventDefault();
 *     if (!valid) return;          // early returns need no end(): begin() has not run
 *     if (!begin()) return;        // a submit is already in flight
 *     try { await save(); onDone(); } catch (err) { setError(err); end(); }
 *   }
 *
 * end() is deliberately NOT automatic on success: a form that navigates away
 * or unmounts on success must not re-enable a button that is about to
 * disappear, and calling setState on an unmounted component is the warning
 * this avoids. Forms that stay mounted call end() themselves.
 */
export function useSubmitGuard(): {
  /** True while a submit is in flight — pass to Button's `loading`. */
  submitting: boolean;
  /** Claims the in-flight slot. False means one is already claimed: return. */
  begin: () => boolean;
  /** Releases it, re-enabling the control. */
  end: () => void;
} {
  const inFlight = useRef(false);
  const [submitting, setSubmitting] = useState(false);

  const begin = useCallback(() => {
    if (inFlight.current) return false;
    inFlight.current = true;
    setSubmitting(true);
    return true;
  }, []);

  const end = useCallback(() => {
    inFlight.current = false;
    setSubmitting(false);
  }, []);

  return { submitting, begin, end };
}
