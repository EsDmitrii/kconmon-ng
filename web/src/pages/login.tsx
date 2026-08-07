import { useEffect, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, getConfig, goTo, login } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// currentReturnTo reads ?returnTo= off the current URL — the same value
// apiFetch's redirectToLogin (lib/api.ts) put there on a 401 — and refuses
// anything that is not a same-origin absolute path, mirroring the server's
// own isSafeReturnTo (internal/console/httpapi/auth.go) for the same
// open-redirect reason: this value came back off the query string, i.e.
// from the URL, i.e. attacker-controllable.
function currentReturnTo(): string {
  const returnTo = new URLSearchParams(window.location.search).get("returnTo");
  // "//host/path" is a protocol-relative open redirect, and a leading "\"
  // is too: the WHATWG URL parser treats "\" as "/" for special schemes
  // (http/https), so "/\evil.example" parses the same as "//evil.example".
  // Same three checks as the server's isSafeReturnTo
  // (internal/console/httpapi/auth.go) for the identical reason — this
  // value is unvalidated attacker-controllable input straight off the URL.
  if (!returnTo || !returnTo.startsWith("/") || returnTo.startsWith("//") || returnTo.includes("\\")) return "/";
  return returnTo;
}

/**
 * LoginPage feature-detects auth.mode from GET /api/v1/config (the same
 * ["config"] cache entry AppShell/useDatabaseAvailable use) rather than
 * hardcoding it:
 *  - local:     username/password form, POSTs to lib/api.ts's `login`.
 *  - oidc:      a single "Sign in with SSO" link — a real browser
 *               navigation to /api/v1/auth/oidc/start (a 302 to the IdP),
 *               never a fetch; fetch can't follow a cross-origin redirect
 *               into a login form the browser needs to render.
 *  - header/anonymous: no login step exists (a trusted proxy authenticates
 *               header mode on every request; anonymous has no credentials
 *               at all) — redirect home.
 */
export function LoginPage() {
  const { data: config, isPending } = useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });
  const queryClient = useQueryClient();
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
          <h1 className="text-[15px] font-semibold text-foreground">Sign in</h1>
          <p className="mt-1 text-[13px] text-muted-foreground">Authenticate through your identity provider.</p>
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
            Sign in with SSO
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
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Sign in failed");
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
        <h1 className="text-[15px] font-semibold text-foreground">Sign in</h1>
        <div className="mt-4 flex flex-col gap-3">
          <label className="flex flex-col gap-1 text-[13px]">
            <span className="text-muted-foreground">Username</span>
            <input
              className="h-9 rounded-md border border-border-strong bg-transparent px-3 text-[13px] text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
              autoFocus
            />
          </label>
          <label className="flex flex-col gap-1 text-[13px]">
            <span className="text-muted-foreground">Password</span>
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
          Sign in
        </Button>
      </form>
    </div>
  );
}
