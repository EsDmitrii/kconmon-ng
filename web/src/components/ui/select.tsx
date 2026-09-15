import * as React from "react";
import { ChevronDown } from "lucide-react";
import { cn } from "@/lib/utils";
import { fieldClasses } from "@/components/ui/input";

/* Select: the native <select> in the shared field look (input.tsx defines it).
   Options stay children so existing call sites move over unchanged.

   Two variants. "field" is the bordered form control. "filter" is the toolbar
   idiom the Live feed hand-rolled: the platform picker keeps its keyboard,
   mobile and screen-reader behaviour, only its closed face is dressed like the
   40px Segmented track beside it (recessed surface-2, no border, a real
   chevron instead of the UA arrow). Only the filter variant gets the wrapper
   span the chevron needs, so a field call site's DOM is unchanged. */
export type SelectVariant = "field" | "filter";

export interface SelectProps extends React.SelectHTMLAttributes<HTMLSelectElement> {
  invalid?: boolean;
  variant?: SelectVariant;
}

const FILTER_CLASSES =
  "h-10 min-w-0 max-w-full appearance-none rounded-md border-0 bg-surface-2 py-1 pl-3.5 pr-8 text-sm text-foreground transition-colors duration-(--dur-fast) ease-(--ease) focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-70";

export const Select = React.forwardRef<HTMLSelectElement, SelectProps>(
  ({ className, invalid = false, variant = "field", "aria-invalid": ariaInvalid, ...props }, ref) => {
    const isInvalid = invalid || ariaInvalid === true || ariaInvalid === "true";
    if (variant === "filter") {
      return (
        <span className="relative inline-flex max-w-full">
          <select
            ref={ref}
            aria-invalid={ariaInvalid ?? (invalid || undefined)}
            className={cn(FILTER_CLASSES, isInvalid && "ring-2 ring-health-bad", className)}
            {...props}
          />
          <ChevronDown
            aria-hidden="true"
            className="pointer-events-none absolute right-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
          />
        </span>
      );
    }
    return (
      <select
        ref={ref}
        aria-invalid={ariaInvalid ?? (invalid || undefined)}
        className={cn(fieldClasses(isInvalid), className)}
        {...props}
      />
    );
  },
);
Select.displayName = "Select";
