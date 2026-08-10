import { useCallback, useRef, useState } from "react";

/**
 * useSubmitGuard makes a form's submit RE-ENTRANT-SAFE, not merely disabled-looking; a `useState`
 * flag cannot be read by the handler that is about to set.
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
