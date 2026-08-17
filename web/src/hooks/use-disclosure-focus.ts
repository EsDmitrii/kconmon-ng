import { useCallback, useLayoutEffect, useRef } from "react";

/**
 * useDisclosureFocus keeps the keyboard when a button is REPLACED by the panel it opens.
 *
 * "New token", "New target" and "New rule" are not toggles that stay on screen: activating one
 * unmounts the button and mounts a form in its place. React destroys the focused node, focus falls
 * back to `<body>`, and the next Tab restarts at the skip link — so a keyboard user who just asked
 * for a form has to walk the whole page to reach it, and a screen-reader user is told nothing at all
 * happened. Closing the form has the mirror problem: the panel disappears and focus is nowhere.
 *
 * The same handover useConfirmStep performs for a two-press delete, for the other shape.
 *
 * `panelRef` goes on the form's own container (give it `tabIndex={-1}` — it is a programmatic focus
 * target, not a tab stop); `triggerRef` goes on the button that opens it.
 */
export function useDisclosureFocus(open: boolean): {
  panelRef: React.RefObject<HTMLDivElement | null>;
  triggerRef: React.RefObject<HTMLButtonElement | null>;
  /** Call from the trigger's onClick, in addition to whatever opens the panel. */
  onOpen: () => void;
  /** Call when the panel closes, in addition to whatever closes it. */
  onClose: () => void;
} {
  const panelRef = useRef<HTMLDivElement | null>(null);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  /* Which direction the last change went, so focus moves only for a change the READER made — never
     because a query settled and re-rendered the page while they were somewhere else. */
  const moving = useRef<"none" | "toPanel" | "toTrigger">("none");

  useLayoutEffect(() => {
    if (moving.current === "toPanel") {
      const panel = panelRef.current;
      // The first real control if the form has one; the panel itself otherwise. Either way the next
      // Tab continues from inside the thing that just appeared.
      const first = panel?.querySelector<HTMLElement>(
        "input:not([type=hidden]):not([disabled]), select:not([disabled]), textarea:not([disabled]), button:not([disabled])",
      );
      (first ?? panel)?.focus();
    } else if (moving.current === "toTrigger") {
      triggerRef.current?.focus();
    }
    moving.current = "none";
  }, [open]);

  const onOpen = useCallback(() => {
    moving.current = "toPanel";
  }, []);

  const onClose = useCallback(() => {
    moving.current = "toTrigger";
  }, []);

  return { panelRef, triggerRef, onOpen, onClose };
}
