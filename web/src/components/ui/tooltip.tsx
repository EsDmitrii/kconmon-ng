import * as React from "react";
import { createPortal } from "react-dom";
import { cn } from "@/lib/utils";

/** The gap between the trigger and the bubble, and the margin the bubble keeps
 *  from every viewport edge. One number for both: the bubble is never nearer a
 *  wall than it is to the thing it describes. */
const GAP = 8;

/*
 * Tooltip: a real hover layer (not a title attribute); the bubble renders in a body portal at a
 * fixed position measured from the trigger.
 *
 * `side` is the PREFERENCE, not the outcome. A matrix cell in the top row had
 * no room above it, and the bubble that could not fit there ended up over the
 * value the operator was pointing at (QA scope 2, finding #23). After the
 * bubble mounts it is measured once and, when the preferred side cannot hold
 * it, flipped to the other one; the horizontal centre is clamped into the
 * viewport by the same margin, so a first- or last-column header no longer
 * pushes half the bubble off screen.
 */
export function Tooltip({
  content,
  children,
  className,
  side = "top",
}: {
  content: React.ReactNode;
  children: React.ReactElement<React.HTMLAttributes<HTMLElement>>;
  className?: string;
  side?: "top" | "bottom";
}) {
  const [rect, setRect] = React.useState<DOMRect | null>(null);
  const bubbleRef = React.useRef<HTMLDivElement>(null);
  /* null until the bubble has been measured — the first paint uses `side` and
     the layout effect below corrects it before the browser shows anything. */
  const [placed, setPlaced] = React.useState<{ side: "top" | "bottom"; left: number } | null>(null);
  const open = rect !== null;

  const show = (e: React.SyntheticEvent<HTMLElement>) => {
    setPlaced(null);
    setRect(e.currentTarget.getBoundingClientRect());
  };
  const hide = () => {
    setPlaced(null);
    setRect(null);
  };

  React.useLayoutEffect(() => {
    const bubble = bubbleRef.current;
    if (!rect || !bubble) return;
    const { width, height } = bubble.getBoundingClientRect();
    /* Flip only when the preferred side genuinely cannot hold the bubble AND
       the other one can — a bubble taller than the whole viewport stays put
       rather than oscillating. */
    const fitsAbove = rect.top - GAP - height >= GAP;
    const fitsBelow = rect.bottom + GAP + height <= window.innerHeight - GAP;
    const resolved: "top" | "bottom" =
      side === "top" ? (fitsAbove || !fitsBelow ? "top" : "bottom") : fitsBelow || !fitsAbove ? "bottom" : "top";
    const centre = rect.left + rect.width / 2;
    const half = width / 2;
    const max = Math.max(GAP + half, window.innerWidth - GAP - half);
    const left = Math.min(Math.max(centre, GAP + half), max);
    setPlaced({ side: resolved, left });
  }, [rect, side]);

  React.useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") hide();
    };
    window.addEventListener("keydown", onKey);
    window.addEventListener("scroll", hide, true);
    return () => {
      window.removeEventListener("keydown", onKey);
      window.removeEventListener("scroll", hide, true);
    };
  }, [open]);

  const child = React.cloneElement(children, {
    onMouseEnter: (e: React.MouseEvent<HTMLElement>) => {
      children.props.onMouseEnter?.(e);
      show(e);
    },
    onMouseLeave: (e: React.MouseEvent<HTMLElement>) => {
      children.props.onMouseLeave?.(e);
      hide();
    },
    onFocus: (e: React.FocusEvent<HTMLElement>) => {
      children.props.onFocus?.(e);
      show(e);
    },
    onBlur: (e: React.FocusEvent<HTMLElement>) => {
      children.props.onBlur?.(e);
      hide();
    },
  });

  return (
    <>
      {child}
      {open && rect
        ? createPortal(
            (() => {
              const resolved = placed?.side ?? side;
              return (
                <div
                  ref={bubbleRef}
                  role="tooltip"
                  data-side={resolved}
                  className={cn(
                    "pop-enter pointer-events-none fixed z-50 -translate-x-1/2 rounded-md bg-popover px-3 py-2 text-xs text-popover-foreground shadow-pop",
                    className,
                  )}
                  style={{
                    left: placed?.left ?? rect.left + rect.width / 2,
                    top: resolved === "top" ? rect.top - GAP : rect.bottom + GAP,
                    transform: `translate(-50%, ${resolved === "top" ? "-100%" : "0"})`,
                  }}
                >
                  {content}
                </div>
              );
            })(),
            document.body,
          )
        : null}
    </>
  );
}
