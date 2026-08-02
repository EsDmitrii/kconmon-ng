import { AlertTriangle } from "lucide-react";

// M0 runs anonymous-only, so the banner is always shown. M1+ will drive this
// from GET /api/v1/config (anonymousBanner). Slim and quiet: it must inform
// without shouting over the page it sits on.
export function AnonymousBanner() {
  return (
    <div
      role="alert"
      className="flex items-center gap-2.5 border-b border-border bg-health-warn-soft/60 px-5 py-1.5 text-[13px] text-foreground"
    >
      <AlertTriangle aria-hidden="true" className="size-3.5 shrink-0 text-health-warn" />
      <span className="min-w-0">
        <span className="font-medium">Anonymous mode.</span>{" "}
        <span className="text-muted-foreground">
          Authentication is disabled — everyone has the fixed role. Do not use in production.
        </span>
      </span>
    </div>
  );
}
