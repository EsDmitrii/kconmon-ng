import * as React from "react";
import { cn } from "@/lib/utils";

/* fieldClasses: the ONE look for a native form field — previously copied
   verbatim in targets.tsx, alerting.tsx and settings.tsx. Focus ring mirrors
   button.tsx so keyboard focus reads the same on every control; hover raises
   the edge quietly in both themes. An invalid field keeps its bad border on
   hover — the error must not fade under the pointer. */
export function fieldClasses(invalid = false): string {
  return cn(
    "h-9 rounded-md border bg-transparent px-3 text-[13px]",
    "transition-[border-color,box-shadow] duration-(--dur-fast) ease-(--ease)",
    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background",
    "disabled:cursor-not-allowed disabled:opacity-70",
    invalid ? "border-health-bad" : "border-border-strong hover:border-muted-foreground/50",
  );
}

/* A caller may flag the error through `invalid` or through its own
   aria-invalid; either flips the border, and aria-invalid is emitted for
   the prop so screen readers hear what the border shows. */
function isInvalid(invalid: boolean, ariaInvalid: React.AriaAttributes["aria-invalid"]): boolean {
  return invalid || ariaInvalid === true || ariaInvalid === "true";
}

export interface InputProps extends React.InputHTMLAttributes<HTMLInputElement> {
  invalid?: boolean;
}

export const Input = React.forwardRef<HTMLInputElement, InputProps>(
  ({ className, invalid = false, "aria-invalid": ariaInvalid, ...props }, ref) => (
    <input
      ref={ref}
      aria-invalid={ariaInvalid ?? (invalid || undefined)}
      className={cn(fieldClasses(isInvalid(invalid, ariaInvalid)), className)}
      {...props}
    />
  ),
);
Input.displayName = "Input";

export interface TextareaProps extends React.TextareaHTMLAttributes<HTMLTextAreaElement> {
  invalid?: boolean;
}

/* Textarea shares the field look; height comes from `rows`, not h-9. */
export const Textarea = React.forwardRef<HTMLTextAreaElement, TextareaProps>(
  ({ className, invalid = false, "aria-invalid": ariaInvalid, ...props }, ref) => (
    <textarea
      ref={ref}
      aria-invalid={ariaInvalid ?? (invalid || undefined)}
      className={cn(fieldClasses(isInvalid(invalid, ariaInvalid)), "h-auto py-2", className)}
      {...props}
    />
  ),
);
Textarea.displayName = "Textarea";
