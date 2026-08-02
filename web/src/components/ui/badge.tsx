import * as React from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

/* Badge: a small status pill. The -soft token fills the pill and keeps
   --foreground legible on top; the optional saturated dot adds the colour
   channel. The label is part of the component — a badge is never colour
   alone (index.css rule 1). */
const badgeVariants = cva(
  "inline-flex items-center gap-1.5 whitespace-nowrap rounded-full px-2.5 py-0.5 text-xs font-medium text-foreground",
  {
    variants: {
      variant: {
        neutral: "bg-surface-2",
        ok: "bg-health-ok-soft",
        warn: "bg-health-warn-soft",
        bad: "bg-health-bad-soft",
        unknown: "bg-health-unknown-soft",
      },
    },
    defaultVariants: { variant: "neutral" },
  },
);

const dotFill: Record<NonNullable<BadgeProps["variant"]>, string> = {
  neutral: "bg-muted-foreground",
  ok: "bg-health-ok",
  warn: "bg-health-warn",
  bad: "bg-health-bad",
  unknown: "bg-health-unknown",
};

export interface BadgeProps
  extends React.HTMLAttributes<HTMLSpanElement>,
    VariantProps<typeof badgeVariants> {
  dot?: boolean;
}

export function Badge({ className, variant = "neutral", dot = false, children, ...props }: BadgeProps) {
  return (
    <span className={cn(badgeVariants({ variant }), className)} {...props}>
      {dot ? (
        <span
          aria-hidden="true"
          className={cn("size-1.5 shrink-0 rounded-full", dotFill[variant ?? "neutral"])}
        />
      ) : null}
      {children}
    </span>
  );
}
