import { useCallback, useEffect, useRef, useState } from "react";
import { Menu, X } from "lucide-react";
import { AppSidebar } from "@/components/app-sidebar";
import { useT } from "@/lib/i18n";
import { chromeDict } from "@/lib/i18n/dict/chrome";
import { cn } from "@/lib/utils";

/**
 * NavDrawer is the sidebar BELOW 768px, where a fixed 16rem column left a
 * 375px viewport 6rem of page and pushed everything else into a horizontal
 * scroll (QA scope 2, finding #16).
 *
 * The column and the drawer are the same component — AppSidebar — rendered
 * twice, and CSS decides which one exists at a given width: `hidden md:flex`
 * on the column, `md:hidden` on the trigger. No JS breakpoint listener, so
 * there is no width at which both are mounted and no resize during which
 * neither is.
 *
 * The open drawer is a real modal: `role="dialog" aria-modal`, Escape closes
 * it, focus moves in on open and back to the trigger on close, and Tab cycles
 * inside it. That last part is hand-rolled rather than borrowed because this
 * kit ships no dialog primitive — components/maintenance.tsx and the
 * annotation form both went the OTHER way and dropped the dialog role rather
 * than claim behaviour they did not have. Here the behaviour is implemented,
 * so the role is honest.
 */

/** The tabbables inside the panel, in document order. */
function focusables(root: HTMLElement): HTMLElement[] {
  return [
    ...root.querySelectorAll<HTMLElement>(
      'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
    ),
  ];
}

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
      if (e.key === "Escape") {
        e.stopPropagation();
        close();
        return;
      }
      if (e.key !== "Tab") return;
      const panel = panelRef.current;
      if (!panel) return;
      const items = focusables(panel);
      if (items.length === 0) {
        /* Nothing to cycle through: keep focus on the panel rather than
           letting Tab escape to the page behind. */
        e.preventDefault();
        panel.focus();
        return;
      }
      const first = items[0];
      const last = items[items.length - 1];
      const active = document.activeElement;
      if (e.shiftKey && (active === first || active === panel)) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKey, true);
    return () => document.removeEventListener("keydown", onKey, true);
  }, [open, close]);

  return (
    <>
      <button
        ref={triggerRef}
        type="button"
        aria-label={open ? t("shell.menu.close") : t("shell.menu.open")}
        aria-expanded={open}
        onClick={() => (open ? close() : setOpen(true))}
        className={cn(
          "flex size-9 shrink-0 items-center justify-center rounded-md text-muted-foreground md:hidden",
          "hover:bg-accent/60 hover:text-foreground",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
        )}
      >
        {open ? <X aria-hidden="true" className="size-5" /> : <Menu aria-hidden="true" className="size-5" />}
      </button>

      {open ? (
        <div className="fixed inset-0 z-40 md:hidden">
          {/* The scrim is a button so a pointer user can dismiss by tapping
              beside the drawer; aria-hidden because Escape and the trigger are
              the keyboard's two ways out and a third, unlabelled stop in the
              tab order would only be noise. */}
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
            <AppSidebar onNavigate={close} />
          </div>
        </div>
      ) : null}
    </>
  );
}
