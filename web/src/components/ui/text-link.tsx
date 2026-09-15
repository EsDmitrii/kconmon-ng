import * as React from "react";
import { cn } from "@/lib/utils";

/* A link inside running text carries a persistent underline: blue against muted grey is 1.13:1,
   so colour alone does not mark it (WCAG 1.4.1). Nav links and button-like links do not use this. */
export const textLinkClass =
  "text-primary underline decoration-primary/40 underline-offset-2 transition-[text-decoration-color] duration-(--dur-fast) ease-(--ease) hover:decoration-primary";

export const TextLink = React.forwardRef<HTMLAnchorElement, React.AnchorHTMLAttributes<HTMLAnchorElement>>(
  ({ className, ...props }, ref) => <a ref={ref} className={cn(textLinkClass, className)} {...props} />,
);
TextLink.displayName = "TextLink";
