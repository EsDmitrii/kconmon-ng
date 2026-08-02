import * as React from "react";
import { cn } from "@/lib/utils";

/* Card: the surface primitive. Depth comes from bg-card + shadow-card, never
   a 1px outline (index.css rule 2). Default p-6; callers override with their
   own p-* utility. `asChild` merges the card classes onto the single child
   element instead of wrapping it — used where the card is semantically a
   <section> or other landmark. */
export interface CardProps extends React.HTMLAttributes<HTMLDivElement> {
  asChild?: boolean;
  /* Adds a whisper of hover lift (translateY + raised shadow) — for cards the
     user reads as objects (stat tiles, chart cards), not for containers. */
  interactive?: boolean;
}

export const Card = React.forwardRef<HTMLDivElement, CardProps>(
  ({ className, asChild, interactive, children, ...props }, ref) => {
    const classes = cn(
      "rounded-lg bg-card p-6 text-card-foreground shadow-card",
      interactive && "card-interactive",
      className,
    );
    if (asChild && React.isValidElement(children)) {
      const child = children as React.ReactElement<React.HTMLAttributes<HTMLElement>>;
      return React.cloneElement(child, {
        ...props,
        className: cn(classes, child.props.className),
      });
    }
    return (
      <div ref={ref} className={classes} {...props}>
        {children}
      </div>
    );
  },
);
Card.displayName = "Card";
