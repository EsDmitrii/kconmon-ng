import * as React from "react";
import { cn } from "@/lib/utils";

/* EmptyState: a short sentence saying why a panel is empty plus the next
   action, never a bare "No data" — the BlankSlate pattern from overview.tsx,
   lifted so every page renders the same slate. */

/* The neutral "nothing here" glyph BlankSlate always drew — kept as the
   default so existing slates look identical after migration. */
function DefaultIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" className="size-5">
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
}

export function EmptyState({ title, body, icon, action, className, ...props }: EmptyStateProps) {
  return (
    <div className={cn("flex flex-col items-center gap-2 px-6 py-10 text-center", className)} {...props}>
      <span
        aria-hidden="true"
        className="mb-1 flex size-10 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
      >
        {icon ?? <DefaultIcon />}
      </span>
      <p className="text-sm font-medium">{title}</p>
      {body != null ? <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">{body}</p> : null}
      {action != null ? <div className="mt-2">{action}</div> : null}
    </div>
  );
}
