import { AlertTriangle } from "lucide-react";
import { useT } from "@/lib/i18n";
import { chromeDict } from "@/lib/i18n/dict/chrome";

// ran anonymous-only, so the banner was always shown.
export function AnonymousBanner({ mode = "anonymous", role }: { mode?: string; role?: string }) {
  // Before the early return: `mode` is a prop.
  const t = useT(chromeDict);
  if (mode !== "anonymous") return null;
  /* The role NAME is a config value (console.auth.anonymous.role) and goes in
     verbatim; without one the banner says less rather than guessing. */
  const named = role !== undefined && role !== "";
  return (
    <div
      role="status"
      className="flex items-center gap-2.5 border-b border-border bg-health-warn-soft/60 px-5 py-1.5 text-[13px] text-foreground"
    >
      <AlertTriangle aria-hidden="true" className="size-3.5 shrink-0 text-health-warn" />
      <span className="min-w-0">
        <span className="font-medium">{t("banner.anonymous.title")}</span>{" "}
        <span className="text-muted-foreground">
          {named ? t("banner.anonymous.body.role", { role: role as string }) : t("banner.anonymous.body")}
        </span>
      </span>
    </div>
  );
}
