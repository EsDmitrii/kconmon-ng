import * as React from "react";
import { cn } from "@/lib/utils";

/* EmptyState: a short sentence saying why a panel is empty plus the next
   action, never a bare "No data" — the BlankSlate pattern from overview.tsx,
   lifted so every page renders the same slate. */

/* The neutral "nothing here" glyph BlankSlate always drew — kept as the
   default so existing slates look identical after migration. */
function DefaultIcon({ compact = false }: { compact?: boolean }) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      className={compact ? "size-4" : "size-5"}
    >
      <circle cx="12" cy="12" r="9" />
      <path d="M9 12h6" strokeLinecap="round" />
    </svg>
  );
}

export interface EmptyStateProps extends React.HTMLAttributes<HTMLDivElement> {
  title: string;
  body?: React.ReactNode;
  /* Replaces the default glyph inside the circle; size itself (size-5). */
  icon?: React.ReactNode;
  /* Optional CTA slot under the body — a Button or a link. */
  action?: React.ReactNode;
  /* The slate for a panel or a rail rather than a whole page: tighter
     padding, a smaller glyph, a 13px title. Same three parts, same order. */
  compact?: boolean;
}

export function EmptyState({ title, body, icon, action, compact = false, className, ...props }: EmptyStateProps) {
  return (
    <div
      className={cn(
        "flex flex-col items-center text-center",
        compact ? "gap-1.5 px-4 py-6" : "gap-2 px-6 py-10",
        className,
      )}
      {...props}
    >
      <span
        aria-hidden="true"
        className={cn(
          "mb-1 flex items-center justify-center rounded-full bg-surface-2 text-muted-foreground",
          compact ? "size-8" : "size-10",
        )}
      >
        {icon ?? <DefaultIcon compact={compact} />}
      </span>
      <p className={cn("font-medium", compact ? "text-[13px]" : "text-sm")}>{title}</p>
      {body != null ? <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">{body}</p> : null}
      {action != null ? <div className="mt-2">{action}</div> : null}
    </div>
  );
}
