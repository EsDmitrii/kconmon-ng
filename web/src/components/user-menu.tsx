import { useCallback, useEffect, useId, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ChevronUp, LogOut, KeyRound, Lock } from "lucide-react";
import { useShowPasswordDialog } from "@/components/change-password";
import { useConsoleConfig } from "@/hooks/use-capabilities";
import { logout } from "@/lib/api";
import { useT } from "@/lib/i18n";
import { withAtParam } from "@/lib/timemachine";
import { userMenuDict } from "@/lib/i18n/dict/user-menu";
import type { Me } from "@/lib/types";
import { cn } from "@/lib/utils";

/** UserMenu — display name, roles, "Sign out", (only with tokens:manage) a link to token management,
 *  and (only in auth.mode=local) the user's own password change. */
export function UserMenu({ me, can }: { me: Me; can: (p: string) => boolean }) {
  const [open, setOpen] = useState(false);
  const [signingOut, setSigningOut] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  /* The trigger, so closing hands focus back to it: a menu unmounting with focus inside drops it on
     <body>, and the next Tab restarts at the skip link. */
  const triggerRef = useRef<HTMLButtonElement>(null);
  const queryClient = useQueryClient();
  const t = useT(userMenuDict);
  const groupId = useId();
  /* The same ["config"] entry AppShell already holds, so this is no extra round trip. A password is
     the console's to change only where it is its own identity provider. */
  const { data: config } = useConsoleConfig();
  /* AppShell hosts the dialog so it outlives this menu. */
  const showPassword = useShowPasswordDialog();
  const canChangePassword = config?.auth?.mode === "local" && me.subject.kind === "user" && showPassword !== null;

  /** Closes and puts focus back where it came from. */
  const closeAndRefocus = useCallback(() => {
    setOpen(false);
    triggerRef.current?.focus();
  }, []);

  useEffect(() => {
    if (!open) return;
    function onPointerDown(e: MouseEvent) {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        /* A click OUTSIDE moves focus by itself, so it must not be stolen back; a click landing on
           something unfocusable would otherwise leave focus inside a menu about to unmount. */
        const inside = rootRef.current.contains(document.activeElement);
        setOpen(false);
        if (inside) triggerRef.current?.focus();
      }
    }
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") closeAndRefocus();
    }
    document.addEventListener("mousedown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open, closeAndRefocus]);

  async function handleSignOut() {
    setSigningOut(true);
    try {
      await logout();
    } finally {
      // Invalidate regardless of whether the request itself succeeded.
      await queryClient.invalidateQueries({ queryKey: ["me"] });
      setSigningOut(false);
      closeAndRefocus();
    }
  }

  return (
    <div ref={rootRef} className="relative">
      <button
        ref={triggerRef}
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        aria-controls={open ? groupId : undefined}
        className={cn(
          "flex w-full items-center justify-between gap-2 rounded-md px-1.5 py-1 text-left text-[11px] text-muted-foreground",
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
      {/* NOT role="menu", and no aria-haspopup on the trigger: a menu puts assistive tech into
          application mode and promises arrow-key roving between menuitems, which this does not
          have. A labelled group of ordinary controls is what this is, and Tab already walks it. */}
      {open ? (
        <div
          id={groupId}
          role="group"
          aria-label={me.subject.displayName}
          className="absolute bottom-full left-0 mb-1.5 w-full min-w-56 rounded-md border border-border bg-popover p-1.5 shadow-card"
        >
          <div className="px-2 py-1.5">
            <div className="truncate text-[13px] font-medium text-foreground">{me.subject.displayName}</div>
            <div className="mt-0.5 truncate text-[11px] text-muted-foreground">
              {/* The role NAMES are permission vocabulary and stay as the
                  server resolved them; only the "there are none" line is ours. */}
              {me.subject.roles.length > 0 ? me.subject.roles.join(", ") : t("roles.none")}
            </div>
          </div>
          {can("tokens:manage") ? (
            <a
              // The anchor is pages/settings.tsx's TOKENS_ANCHOR.
              href={withAtParam("/settings#tokens")}
              className="flex items-center gap-2 rounded-sm px-2 py-1.5 text-[13px] text-foreground hover:bg-accent/60"
            >
              <KeyRound aria-hidden="true" className="size-3.5 shrink-0" />
              {t("tokens")}
            </a>
          ) : null}
          {canChangePassword ? (
            <button
              type="button"
              onClick={() => {
                /* Focus goes back to the trigger FIRST: the dialog returns focus to whatever held it
                   when it opened, and this button is about to unmount with the menu. */
                closeAndRefocus();
                showPassword?.();
              }}
              className="flex w-full items-center gap-2 rounded-sm px-2 py-1.5 text-left text-[13px] text-foreground hover:bg-accent/60"
            >
              <Lock aria-hidden="true" className="size-3.5 shrink-0" />
              {t("password")}
            </button>
          ) : null}
          <button
            type="button"
            onClick={handleSignOut}
            disabled={signingOut}
            className="flex w-full items-center gap-2 rounded-sm px-2 py-1.5 text-left text-[13px] text-foreground hover:bg-accent/60 disabled:opacity-50"
          >
            <LogOut aria-hidden="true" className="size-3.5 shrink-0" />
            {signingOut ? t("signOut.pending") : t("signOut")}
          </button>
        </div>
      ) : null}
    </div>
  );
}
