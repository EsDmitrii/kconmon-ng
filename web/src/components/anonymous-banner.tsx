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
  const body = named ? t("banner.anonymous.body.role", { role: role as string }) : t("banner.anonymous.body");
  /* Below sm the full sentence took four lines of a 375px viewport before the page title. One
     clause fits; the sentence rides on title. Two spans toggled by class, so the role/text
     assertions see the same DOM at every width. */
  const short = named ? t("banner.anonymous.body.short.role", { role: role as string }) : t("banner.anonymous.body.short");
  return (
    <div
      role="status"
      title={`${t("banner.anonymous.title")} ${body}`}
      className="flex items-center gap-2.5 border-b border-border bg-health-warn-soft/60 px-4 py-1.5 text-[13px] text-foreground sm:px-5"
    >
      <AlertTriangle aria-hidden="true" className="size-3.5 shrink-0 text-health-warn" />
      <span className="min-w-0">
        <span className="font-medium">{t("banner.anonymous.title")}</span>{" "}
        <span className="text-muted-foreground sm:hidden">{short}</span>
        <span className="hidden text-muted-foreground sm:inline">{body}</span>
      </span>
    </div>
  );
}
