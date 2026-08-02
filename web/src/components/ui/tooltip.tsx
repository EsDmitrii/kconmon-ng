import * as React from "react";
import { createPortal } from "react-dom";
import { cn } from "@/lib/utils";

/* Tooltip: a real hover layer (not a title attribute). The bubble renders in a
   body portal at a fixed position measured from the trigger, so it survives
   scroll containers and never gets clipped; it enters with the shared
   scale-in animation. Shows on hover AND focus, hides on leave/blur/Escape.
   Content is plain nodes — callers compose their own rows. */
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
  const open = rect !== null;

  const show = (e: React.SyntheticEvent<HTMLElement>) => {
    setRect(e.currentTarget.getBoundingClientRect());
  };
  const hide = () => setRect(null);

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
            <div
              role="tooltip"
              className={cn(
                "pop-enter pointer-events-none fixed z-50 -translate-x-1/2 rounded-md bg-popover px-3 py-2 text-xs text-popover-foreground shadow-pop",
                className,
              )}
              style={{
                left: rect.left + rect.width / 2,
                top: side === "top" ? rect.top - 8 : rect.bottom + 8,
                transform: `translate(-50%, ${side === "top" ? "-100%" : "0"})`,
              }}
            >
              {content}
            </div>,
            document.body,
          )
        : null}
    </>
  );
}
