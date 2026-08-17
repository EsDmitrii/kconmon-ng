import { useCallback, useLayoutEffect, useRef, useState } from "react";

/**
 * useConfirmStep is the two-press destructive action, with the keyboard kept whole.
 *
 * Every "Delete → Confirm delete" row in this console swaps one button for a pair of them, and React
 * sees a different element type at that slot: the focused button's DOM node is destroyed. Focus fell
 * back to `<body>`, so the next Tab restarted at the skip link and a keyboard user had to walk the
 * sidebar, the page header and every row above to reach the confirm button they had just summoned.
 * A screen reader was told nothing at all, so pressing Delete appeared to do nothing.
 *
 * So the step hands focus over, the way ui/pager.tsx already hands it over across a page change: the
 * ref goes on the confirm control, and a layout effect focuses it the moment it mounts — before the
 * browser paints, so there is no frame with focus on the body. Cancelling hands it back to where the
 * row's action started.
 *
 * `announce` is the row's live region: the same words the button carries, in a `role="status"` the
 * reader hears without moving. It is a separate node rather than an aria-live on the button because
 * the button did not exist a moment ago, and a live region that mounts with its message is not
 * reliably announced.
 */
export function useConfirmStep(): {
  /** True while the confirm control should be on screen. */
  confirming: boolean;
  /** Put on the CONFIRM control; it takes focus as soon as it mounts. */
  confirmRef: React.RefObject<HTMLButtonElement | null>;
  /** Put on the control that STARTS the step, so cancelling can return focus to it. */
  triggerRef: React.RefObject<HTMLButtonElement | null>;
  /** Call from the trigger's onClick. */
  ask: () => void;
  /** Call from Cancel, and after the action itself resolves. */
  reset: () => void;
} {
  const [confirming, setConfirming] = useState(false);
  const confirmRef = useRef<HTMLButtonElement | null>(null);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  /* Which direction the last change went, so focus is only moved for a change the READER made —
     never for a row that merely re-rendered while they were somewhere else. */
  const moving = useRef<"none" | "toConfirm" | "toTrigger">("none");

  useLayoutEffect(() => {
    if (moving.current === "toConfirm") confirmRef.current?.focus();
    else if (moving.current === "toTrigger") triggerRef.current?.focus();
    moving.current = "none";
  }, [confirming]);

  const ask = useCallback(() => {
    moving.current = "toConfirm";
    setConfirming(true);
  }, []);

  const reset = useCallback(() => {
    moving.current = "toTrigger";
    setConfirming(false);
  }, []);

  return { confirming, confirmRef, triggerRef, ask, reset };
}

/**
 * useKeyedConfirmStep is the same step for a LIST, where the row asking "are you sure?" is named by
 * a key rather than by a boolean.
 *
 * One row at a time can be confirming, so one pair of refs is enough; what changes is that `ask`
 * takes the key, and `confirming` is that key or null. The keys matter here because the list is
 * re-keyed by every save, and an index would move the confirm onto a different finding under the
 * operator's cursor.
 */
export function useKeyedConfirmStep(): {
  /** The key of the row currently asking, or null. */
  confirming: string | null;
  confirmRef: React.RefObject<HTMLButtonElement | null>;
  triggerRef: React.RefObject<HTMLButtonElement | null>;
  ask: (key: string) => void;
  reset: () => void;
} {
  const [confirming, setConfirming] = useState<string | null>(null);
  const confirmRef = useRef<HTMLButtonElement | null>(null);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const moving = useRef<"none" | "toConfirm" | "toTrigger">("none");

  useLayoutEffect(() => {
    if (moving.current === "toConfirm") confirmRef.current?.focus();
    else if (moving.current === "toTrigger") triggerRef.current?.focus();
    moving.current = "none";
  }, [confirming]);

  const ask = useCallback((key: string) => {
    moving.current = "toConfirm";
    setConfirming(key);
  }, []);

  const reset = useCallback(() => {
    moving.current = "toTrigger";
    setConfirming(null);
  }, []);

  return { confirming, confirmRef, triggerRef, ask, reset };
}
