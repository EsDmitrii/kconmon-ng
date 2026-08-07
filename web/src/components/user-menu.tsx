import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ChevronUp, LogOut, KeyRound } from "lucide-react";
import { logout } from "@/lib/api";
import type { Me } from "@/lib/types";
import { cn } from "@/lib/utils";

/**
 * UserMenu — display name, roles, "Sign out", and (only with tokens:manage)
 * a link to token management. Rendered by AppSidebar in place of the M0/M1
 * static footer text, and only when the caller already knows `me` is a real
 * (non-anonymous) subject — see app-sidebar.tsx.
 *
 * No route for standalone token management exists yet (only the
 * /api/v1/tokens REST surface does — see internal/console/httpapi/tokens.go);
 * the link below points at /settings, the nearest existing NAV_ITEMS entry
 * ("Auth, RBAC, retention, maintenance, webhooks, export/import"), pending a
 * dedicated page in a later task.
 */
export function UserMenu({ me, can }: { me: Me; can: (p: string) => boolean }) {
  const [open, setOpen] = useState(false);
  const [signingOut, setSigningOut] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const queryClient = useQueryClient();

  useEffect(() => {
    if (!open) return;
    function onPointerDown(e: MouseEvent) {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    }
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  async function handleSignOut() {
    setSigningOut(true);
    try {
      await logout();
    } finally {
      // Invalidate regardless of whether the request itself succeeded —
      // handleAuthLogout is idempotent server-side and always clears the
      // cookies client-side isn't guaranteed on a network failure, but the
      // UI must not get stuck showing a stale signed-in identity either way.
      await queryClient.invalidateQueries({ queryKey: ["me"] });
      setSigningOut(false);
      setOpen(false);
    }
  }

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        aria-haspopup="menu"
        className={cn(
          "flex w-full items-center justify-between gap-2 rounded-md px-1.5 py-1 text-left text-[11px] text-muted-foreground/70",
          "transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent/60 hover:text-foreground",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
        )}
      >
        <span className="min-w-0 truncate font-medium text-foreground">{me.subject.displayName}</span>
        <ChevronUp
          aria-hidden="true"
          className={cn("size-3.5 shrink-0 transition-transform duration-(--dur-fast)", open ? "" : "rotate-180")}
        />
      </button>
      {open ? (
        <div
          role="menu"
          className="absolute bottom-full left-0 mb-1.5 w-full min-w-56 rounded-md border border-border bg-popover p-1.5 shadow-card"
        >
          <div className="px-2 py-1.5">
            <div className="truncate text-[13px] font-medium text-foreground">{me.subject.displayName}</div>
            <div className="mt-0.5 truncate text-[11px] text-muted-foreground">
              {me.subject.roles.length > 0 ? me.subject.roles.join(", ") : "no roles bound"}
            </div>
          </div>
          {can("tokens:manage") ? (
            <a
              href="/settings"
              role="menuitem"
              className="flex items-center gap-2 rounded-sm px-2 py-1.5 text-[13px] text-foreground hover:bg-accent/60"
            >
              <KeyRound aria-hidden="true" className="size-3.5 shrink-0" />
              Token management
            </a>
          ) : null}
          <button
            type="button"
            role="menuitem"
            onClick={handleSignOut}
            disabled={signingOut}
            className="flex w-full items-center gap-2 rounded-sm px-2 py-1.5 text-left text-[13px] text-foreground hover:bg-accent/60 disabled:opacity-50"
          >
            <LogOut aria-hidden="true" className="size-3.5 shrink-0" />
            {signingOut ? "Signing out…" : "Sign out"}
          </button>
        </div>
      ) : null}
    </div>
  );
}
