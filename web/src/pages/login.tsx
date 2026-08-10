import { useEffect, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, getConfig, goTo, login } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { useT } from "@/lib/i18n";
import { loginDict } from "@/lib/i18n/dict/login";
import { cn } from "@/lib/utils";

// currentReturnTo reads ?returnTo= off the current URL.
function currentReturnTo(): string {
  const returnTo = new URLSearchParams(window.location.search).get("returnTo");
  // "//host/path" is a protocol-relative open redirect, and a leading "\" is too.
  if (!returnTo || !returnTo.startsWith("/") || returnTo.startsWith("//") || returnTo.includes("\\")) return "/";
  return returnTo;
}

/**
 * LoginPage feature-detects auth.mode from GET /api/v1/config (the same ["config"] cache entry
 * AppShell/useDatabaseAvailable use) rather than hardcoding it.
 */
export function LoginPage() {
  const { data: config, isPending } = useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });
  const queryClient = useQueryClient();
  // Before every early return below — this page has three of them (pending,
  // redirect-home, and the whole OIDC card), and a hook after one of those is
  // a hook that runs on some renders and not others.
  const t = useT(loginDict);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [submitting, setSubmitting] = useState(false);

  const mode = config?.auth.mode;
  const redirectHome = !isPending && (mode === "header" || mode === "anonymous");

  // Side effect, not a render-time mutation — redirect only once we actually
  // know the mode (isPending false) confirms it needs no login step.
  useEffect(() => {
    if (redirectHome) goTo("/");
  }, [redirectHome]);

  if (isPending || redirectHome) return null;

  if (mode === "oidc") {
    const href = `/api/v1/auth/oidc/start?returnTo=${encodeURIComponent(currentReturnTo())}`;
    return (
      <div className="flex h-full items-center justify-center">
        <div className="w-full max-w-sm rounded-lg border border-border bg-surface p-6 text-center shadow-card">
          <h1 className="text-[15px] font-semibold text-foreground">{t("title")}</h1>
          <p className="mt-1 text-[13px] text-muted-foreground">{t("oidc.lead")}</p>
          {/* A real browser navigation, not a fetch: /api/v1/auth/oidc/start
              302s to the IdP, which fetch cannot follow into a rendered
              page. Styled to match Button's default variant (button.tsx
              does not export buttonVariants for an <a> to reuse). */}
          <a
            href={href}
            className={cn(
              "mt-5 inline-flex h-9 w-full items-center justify-center gap-2 rounded-md bg-primary px-4 py-2",
              "text-sm font-medium text-primary-foreground transition-colors duration-(--dur) ease-(--ease) hover:bg-primary/90",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background",
            )}
          >
            {t("oidc.action")}
          </a>
        </div>
      </div>
    );
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    setSubmitting(true);
    try {
      await login(username, password);
      // Invalidate rather than refetch-and-wait: every mounted consumer
      // (the sidebar's user menu, this page were it to stay mounted)
      // refreshes off the one shared ["me"] key use-auth.ts reads.
      await queryClient.invalidateQueries({ queryKey: ["me"] });
      goTo(currentReturnTo());
    } catch (err) {
      // The server's own sentence wins whenever there is one, verbatim and in
      // either language; t() only covers the failure that carried no problem
      // document at all (a dropped connection, a parse error).
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : t("error.fallback"));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="flex h-full items-center justify-center">
      <form
        onSubmit={handleSubmit}
        className="w-full max-w-sm rounded-lg border border-border bg-surface p-6 shadow-card"
      >
        <h1 className="text-[15px] font-semibold text-foreground">{t("title")}</h1>
        <div className="mt-4 flex flex-col gap-3">
          <label className="flex flex-col gap-1 text-[13px]">
            <span className="text-muted-foreground">{t("field.username")}</span>
            <input
              className="h-9 rounded-md border border-border-strong bg-transparent px-3 text-[13px] text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
              autoFocus
            />
          </label>
          <label className="flex flex-col gap-1 text-[13px]">
            <span className="text-muted-foreground">{t("field.password")}</span>
            <input
              type="password"
              className="h-9 rounded-md border border-border-strong bg-transparent px-3 text-[13px] text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
            />
          </label>
        </div>
        {error ? (
          <p role="alert" className="mt-3 text-[13px] text-health-bad">
            {error}
          </p>
        ) : null}
        <Button type="submit" loading={submitting} className="mt-5 w-full">
          {t("submit")}
        </Button>
      </form>
    </div>
  );
}
