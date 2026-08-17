import { useCallback, useEffect, useId, useRef, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { X } from "lucide-react";
import { useT } from "@/lib/i18n";
import { sharedDict } from "@/lib/i18n/dict/shared";
import { cn } from "@/lib/utils";

/**
 * Modal — the kit's dialog primitive, and the reason detail panes stop competing for the page.
 *
 * The console kept putting everything side by side: on the MTR Explorer three panes shared one row,
 * so on a laptop the route history was a column too narrow to read a route in and the trace detail
 * was a column too narrow to read a hop in (owner report). A detail is not a thing you scan next to
 * the list — it is a thing you open, read, and close. That is what this is for.
 *
 * WHEN NOT TO USE IT: anything the reader must see AT THE SAME TIME as what is behind it. A modal
 * hides its context by design, so it is the wrong shape for "watch this while I change that". The
 * comparison view is a legitimate modal because the two things being compared are BOTH inside it.
 *
 * The contract is the WAI-ARIA dialog one, implemented rather than claimed — this kit shipped no
 * dialog primitive, and two surfaces had already dropped the `dialog` role rather than assert
 * behaviour they did not have:
 *   - role="dialog" aria-modal, labelled by its own title
 *   - focus moves inside on open and returns to the opener on close
 *   - Escape closes; so does a click on the backdrop
 *   - Tab and Shift+Tab cycle within the panel
 *   - the page behind it does not scroll
 */
export function Modal({
  open,
  onClose,
  title,
  description,
  size = "md",
  footer,
  children,
}: {
  open: boolean;
  onClose: () => void;
  /** The dialog's accessible name, and its heading. */
  title: string;
  /** One line under the title: what this dialog is about, not how to use it. */
  description?: string;
  /** `wide` is for content that is a TABLE or a diff — the cases that were unreadable in a column. */
  size?: "md" | "wide";
  footer?: ReactNode;
  children: ReactNode;
}) {
  const t = useT(sharedDict);
  const panelRef = useRef<HTMLDivElement>(null);
  /* Where focus was when the dialog opened, so closing puts it back rather than
     dropping the reader at the top of the document. */
  const openerRef = useRef<HTMLElement | null>(null);
  const titleId = useId();
  const descriptionId = useId();

  const focusables = useCallback((): HTMLElement[] => {
    const panel = panelRef.current;
    if (!panel) return [];
    return [
      ...panel.querySelectorAll<HTMLElement>(
        'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
      ),
    ].filter((el) => el.offsetParent !== null || el === document.activeElement);
  }, []);

  useEffect(() => {
    if (!open) return;
    openerRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    /* The panel itself takes focus first: the first control would otherwise be
       read before the title, and a dialog that announces its Close button as
       its opening line tells the reader nothing about what opened. */
    panelRef.current?.focus();

    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = previousOverflow;
      openerRef.current?.focus();
    };
  }, [open]);

  const onKeyDown = (event: React.KeyboardEvent<HTMLDivElement>) => {
    if (event.key === "Escape") {
      event.stopPropagation();
      onClose();
      return;
    }
    if (event.key !== "Tab") return;
    const items = focusables();
    if (items.length === 0) {
      // Nothing to cycle through: keep focus on the panel rather than letting
      // Tab walk out into the page the dialog is covering.
      event.preventDefault();
      panelRef.current?.focus();
      return;
    }
    const first = items[0];
    const last = items[items.length - 1];
    const active = document.activeElement;
    if (!event.shiftKey && active === last) {
      event.preventDefault();
      first.focus();
    } else if (event.shiftKey && (active === first || active === panelRef.current)) {
      event.preventDefault();
      last.focus();
    }
  };

  if (!open) return null;

  /* PORTALLED to the document body, and not for tidiness: `position: fixed` is relative to the
     nearest ancestor carrying a transform, filter or perspective — not to the viewport — and this
     console's page shell animates in under `.page-enter` (a fade-up that holds a transform while it
     runs). A dialog rendered inside that subtree anchored itself to the PAGE instead of the screen,
     so a tall one hung past the bottom edge with its header pushed off the top: the title and the
     Close button were simply not on screen (owner report). At the top of the document there is
     nothing to be relative to but the viewport. */
  return createPortal(
    /* No scrolling on THIS element. It used to carry overflow-y-auto, and a dialog taller than the
       viewport then overflowed a centred flex child in both directions at once: the top — the title
       and the Close button — ended up above the scroll origin, unreachable at any scroll position
       (owner report, a trace with fifty rows). The panel below bounds its own height instead, so it
       can never be taller than this box and centring stays safe. */
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4 sm:p-8">
      {/* The backdrop is a button so a pointer can dismiss, and aria-hidden with
          no tab stop because Escape and the Close control are the keyboard's two
          ways out — a third, unlabelled stop would only be noise.

          It BLURS rather than merely dims: the page behind stays recognisable as
          the place you came from while ceasing to compete for the eye, which is
          the whole reason this is a dialog and not a column. */}
      <button
        type="button"
        aria-hidden="true"
        tabIndex={-1}
        onClick={onClose}
        data-testid="modal-backdrop"
        className="fixed inset-0 bg-background/60 backdrop-blur-md"
      />
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description ? descriptionId : undefined}
        tabIndex={-1}
        onKeyDown={onKeyDown}
        className={cn(
          /* Centred, and lifted off the page it covers: a larger radius than a
             card, a real shadow (the one place the elevation ladder allows one)
             and the kit's own 150ms scale-in, which the reduced-motion block
             already switches off. */
          "pop-enter relative z-10 w-full overflow-hidden rounded-2xl border border-border bg-surface outline-none",
          /* A COLUMN with a hard ceiling: the header and the footer keep their places and only the
             body scrolls, however long the content is. dvh rather than vh because a mobile browser's
             vh is the address-bar-collapsed height, which is taller than what is actually visible. */
          "flex max-h-[calc(100dvh-2rem)] flex-col sm:max-h-[calc(100dvh-4rem)]",
          "shadow-[0_24px_64px_-12px_rgb(0_0_0/0.55)]",
          size === "wide" ? "max-w-5xl" : "max-w-2xl",
        )}
      >
        <div className="flex shrink-0 items-start justify-between gap-4 border-b border-border px-5 py-3.5">
          <div className="min-w-0">
            <h2 id={titleId} className="truncate text-sm font-semibold">
              {title}
            </h2>
            {description ? (
              <p id={descriptionId} className="mt-0.5 truncate text-xs text-muted-foreground" title={description}>
                {description}
              </p>
            ) : null}
          </div>
          <button
            type="button"
            aria-label={t("modal.close")}
            onClick={onClose}
            className={cn(
              "-mr-1 -mt-1 shrink-0 rounded-md p-1.5 text-muted-foreground",
              "transition-colors duration-(--dur-fast) hover:bg-accent hover:text-foreground",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
            )}
          >
            <X aria-hidden="true" className="size-4" />
          </button>
        </div>

        {/* The body scrolls, not the page: a long hop table must not push the dialog's own header
            off the screen. min-h-0 is what makes that true inside a flex column — without it a flex
            item refuses to shrink below its content and the overflow moves back up to the panel. */}
        <div className="min-h-0 flex-1 overflow-auto px-5 py-4">{children}</div>

        {footer ? <div className="shrink-0 border-t border-border px-5 py-3">{footer}</div> : null}
      </div>
    </div>,
    document.body,
  );
}
