import { AlertTriangle } from "lucide-react";

// M0 ran anonymous-only, so the banner was always shown; M3 finally wires it
// to GET /api/v1/config's `auth.mode` via `mode` (routes.tsx's AppShell
// passes `config?.auth.mode`). Defaulting to "anonymous" keeps `<AnonymousBanner
// />` with no prop — the original M0/M2 call shape, still exercised by the
// untouched test below — showing exactly as before, and also fails safe
// (shown) for the brief instant before the first config response lands.
// Slim and quiet: it must inform without shouting over the page it sits on.
export function AnonymousBanner({ mode = "anonymous" }: { mode?: string }) {
  if (mode !== "anonymous") return null;
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
