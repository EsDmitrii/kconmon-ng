import * as React from "react";
import { cn } from "@/lib/utils";

/* Table: the semantic table primitives every data table renders through, so
   density is a variant instead of per-page padding arithmetic. "dense" is the
   monitoring default the roadmap wants: ~30-32px rows, 13px text. */

export type TableVariant = "default" | "dense";

/* Th/Td read the variant from context so a call site sets density ONCE on
   <Table> and every cell follows — that is what keeps migration mechanical. */
const TableVariantContext = React.createContext<TableVariant>("default");

export interface TableProps extends React.TableHTMLAttributes<HTMLTableElement> {
  variant?: TableVariant;
  /* Skip the built-in overflow-x-auto wrapper for call sites that own their
     scroll container (mtr-hop-table measures its own scroll position). */
  bare?: boolean;
  /* Extra classes for the scroll wrapper (ignored when bare). */
  containerClassName?: string;
}

export const Table = React.forwardRef<HTMLTableElement, TableProps>(
  ({ className, variant = "default", bare = false, containerClassName, ...props }, ref) => {
    const table = (
      <table
        ref={ref}
        className={cn("w-full", variant === "dense" ? "text-[13px]" : "text-sm", className)}
        {...props}
      />
    );
    return (
      <TableVariantContext.Provider value={variant}>
        {bare ? table : <div className={cn("overflow-x-auto", containerClassName)}>{table}</div>}
      </TableVariantContext.Provider>
    );
  },
);
Table.displayName = "Table";

/* Header typography lives on <thead> and inherits into its cells; the border
   goes on the row via an arbitrary variant because a border on the row group
   itself does not render in every engine. */
export const THead = React.forwardRef<HTMLTableSectionElement, React.HTMLAttributes<HTMLTableSectionElement>>(
  ({ className, ...props }, ref) => {
    const variant = React.useContext(TableVariantContext);
    return (
      <thead
        ref={ref}
        className={cn(
          "text-left text-muted-foreground [&_tr]:border-b [&_tr]:border-border",
          variant === "dense" ? "text-xs" : "text-[11px] uppercase tracking-[0.07em]",
          className,
        )}
        {...props}
      />
    );
  },
);
THead.displayName = "THead";

export const TBody = React.forwardRef<HTMLTableSectionElement, React.HTMLAttributes<HTMLTableSectionElement>>(
  ({ className, ...props }, ref) => (
    <tbody ref={ref} className={cn("divide-y divide-border", className)} {...props} />
  ),
);
TBody.displayName = "TBody";

/* Tr carries no styling of its own — hover/selected treatments differ per
   table and stay at the call site. */
export const Tr = React.forwardRef<HTMLTableRowElement, React.HTMLAttributes<HTMLTableRowElement>>(
  ({ className, ...props }, ref) => <tr ref={ref} className={cn(className)} {...props} />,
);
Tr.displayName = "Tr";

export interface ThProps extends React.ThHTMLAttributes<HTMLTableCellElement> {
  /* Right-aligns the header over a numeric column. */
  numeric?: boolean;
}

export const Th = React.forwardRef<HTMLTableCellElement, ThProps>(
  ({ className, numeric = false, scope = "col", ...props }, ref) => {
    const variant = React.useContext(TableVariantContext);
    return (
      <th
        ref={ref}
        scope={scope}
        className={cn(
          "text-left",
          variant === "dense" ? "py-1.5 font-medium" : "py-3 font-semibold",
          numeric && "text-right",
          className,
        )}
        {...props}
      />
    );
  },
);
Th.displayName = "Th";

export interface TdProps extends React.TdHTMLAttributes<HTMLTableCellElement> {
  /* Numeric cells right-align and read in the data face: mono-data is the
     numeric type treatment index.css defines, .nums pins tabular figures. */
  numeric?: boolean;
}

export const Td = React.forwardRef<HTMLTableCellElement, TdProps>(
  ({ className, numeric = false, ...props }, ref) => {
    const variant = React.useContext(TableVariantContext);
    return (
      <td
        ref={ref}
        className={cn(
          variant === "dense" ? "py-1.5" : "py-3",
          numeric && "mono-data nums text-right",
          className,
        )}
        {...props}
      />
    );
  },
);
Td.displayName = "Td";
