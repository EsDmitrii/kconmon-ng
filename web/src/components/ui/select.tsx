import * as React from "react";
import { cn } from "@/lib/utils";
import { fieldClasses } from "@/components/ui/input";

/* Select: the native <select> in the shared field look (input.tsx defines it).
   Options stay children so existing call sites move over unchanged. */
export interface SelectProps extends React.SelectHTMLAttributes<HTMLSelectElement> {
  invalid?: boolean;
}

export const Select = React.forwardRef<HTMLSelectElement, SelectProps>(
  ({ className, invalid = false, "aria-invalid": ariaInvalid, ...props }, ref) => (
    <select
      ref={ref}
      aria-invalid={ariaInvalid ?? (invalid || undefined)}
      className={cn(
        fieldClasses(invalid || ariaInvalid === true || ariaInvalid === "true"),
        className,
      )}
      {...props}
    />
  ),
);
Select.displayName = "Select";
