import { useCallback, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useRouterState } from "@tanstack/react-router";
import { Menu } from "lucide-react";
import { AppSidebar } from "@/components/app-sidebar";
import { useT } from "@/lib/i18n";
import { chromeDict } from "@/lib/i18n/dict/chrome";
import { cn } from "@/lib/utils";
import { cycleTab } from "./ui/focus-trap";

/**
 * NavDrawer is the sidebar below Tailwind's md (48rem, 768px at the default font size), where a
 * fixed 16rem column would leave a phone a sliver of page and a horizontal scroll.
 *
 * The column and the drawer are the same component — AppSidebar — rendered
 * twice, and CSS decides which one exists at a given width: `hidden md:flex`
 * on the column, `md:hidden` on the trigger. CSS alone decides which one is
 * shown, so there is no width at which both are visible and no resize during
 * which neither is; the one breakpoint listener only closes an open drawer
 * that md has hidden.
 *
 * The open drawer keeps ui/modal's WAI-ARIA dialog contract and shares its Tab trap. The panel
 * covers the trigger, so it carries its own Close control.
 */

/** Tailwind's `md` exactly as it compiles `md:hidden`: in rem, so a larger browser font moves it
 *  (48rem is 960px at 20px), and a px constant would close the drawer where the trigger still shows. */
const DRAWER_HIDDEN_QUERY = "(width >= 48rem)";

export function NavDrawer() {
  const t = useT(chromeDict);
  const [open, setOpen] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);

  /* Focus goes back to the control that opened the drawer — the same contract
     AnnotationBar and MaintenanceBar keep, and without it a keyboard user is
     dropped on <body>. */
  const close = useCallback(() => {
    setOpen(false);
    triggerRef.current?.focus();
  }, []);

  useEffect(() => {
    if (!open) return;
    /* The panel takes focus itself rather than its first link: a screen reader
       then announces the dialog and its name before the navigation. */
    panelRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      /* A dialog opened from inside the drawer (Change password) sits above it and owns its keys:
         Escape there closes that dialog, not the drawer under it. */
      const target = e.target instanceof Element ? e.target : null;
      if (target?.closest('[aria-modal="true"]') && !panelRef.current?.contains(target)) return;
      if (e.key === "Escape") {
        e.stopPropagation();
        close();
        return;
      }
      if (panelRef.current) cycleTab(panelRef.current, e);
    };
    document.addEventListener("keydown", onKey, true);
    return () => document.removeEventListener("keydown", onKey, true);
  }, [open, close]);

  /* A route change the drawer did not start (Back/Forward, the Android back gesture, a ⌘K jump)
     closes it too: the shell focuses the new page's <main>, which an open drawer would cover. */
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  useEffect(() => setOpen(false), [pathname]);

  /* CSS hides the drawer at md and up, but `open` and the capture-phase Escape listener above would
     outlive it and swallow the next Escape meant for a dialog. Crossing md closes it for real. */
  useEffect(() => {
    if (!open || typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia(DRAWER_HIDDEN_QUERY);
    if (mq.matches) {
      setOpen(false);
      return;
    }
    const onChange = (e: { matches: boolean }) => {
      if (e.matches) setOpen(false);
    };
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, [open]);

  return (
    <>
      <button
        ref={triggerRef}
        type="button"
        aria-label={t("shell.menu.open")}
        aria-expanded={open}
        onClick={() => setOpen(true)}
        className={cn(
          "flex size-9 shrink-0 items-center justify-center rounded-md text-muted-foreground md:hidden",
          "hover:bg-accent/60 hover:text-foreground",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
        )}
      >
        <Menu aria-hidden="true" className="size-5" />
      </button>

      {/* Portalled to <body>, like ui/modal: the trigger sits in the mobile <header>, and the
          sidebar's <aside> may not nest inside that banner landmark. */}
      {open ? createPortal(
        <div className="fixed inset-0 z-40 h-[100dvh] md:hidden">
          {/* The scrim is a button so a pointer user can dismiss by tapping
              beside the drawer; aria-hidden because Escape and the panel's
              Close control are the keyboard's ways out and a third, unlabelled
              stop in the tab order would only be noise. */}
          <button
            type="button"
            aria-hidden="true"
            tabIndex={-1}
            onClick={close}
            className="absolute inset-0 bg-background/70"
          />
          <div
            ref={panelRef}
            role="dialog"
            aria-modal="true"
            aria-label={t("shell.menu.aria")}
            tabIndex={-1}
            className="absolute inset-y-0 left-0 w-64 max-w-[85vw] shadow-pop outline-none"
          >
            <AppSidebar onNavigate={close} onClose={close} />
          </div>
        </div>,
        document.body,
      ) : null}
    </>
  );
}
