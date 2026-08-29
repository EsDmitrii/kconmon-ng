import { useState } from "react";
import { CircleHelp, ExternalLink } from "lucide-react";
import { Modal } from "@/components/ui/modal";
import { useT } from "@/lib/i18n";
import { sharedDict } from "@/lib/i18n/dict/shared";

/** Where "Learn more" points. ONE constant so the docs site can move (a custom
 *  domain is M7-6) by editing one line rather than twelve. */
export const DOCS_BASE_URL = "https://esdmitrii.github.io/kconmon-ng/";

/** The docs page for a console route. `slug` must name a real file under the
 *  repo's docs/console/ — page-help.test.tsx pins the shape, the docs build
 *  (`mkdocs build --strict`) owns the files. */
export function docsConsoleUrl(slug: string): string {
  return `${DOCS_BASE_URL}console/${slug}/`;
}

/**
 * PageHelp — the "?" after a page title, opening a few sentences of orientation
 * and a "Learn more" link to that page's chapter on the docs site (M7-5).
 *
 * The body arrives ALREADY TRANSLATED: each page words its own `help.body` in
 * its own dictionary (lib/i18n's rule — the dictionary is passed, not named,
 * so a shell component cannot resolve a page's key itself). Only the chrome
 * around the body — button label, dialog title, link text — is shared wording,
 * and it lives in dict/shared.ts like the rest of the one-component-many-mounts
 * strings.
 *
 * The opened layer is ui/modal.tsx rather than a bespoke popover: the Modal
 * already implements the whole dialog contract this affordance needs — Escape
 * closes, focus returns to the opener, the panel is aria-labelled — and help
 * text is precisely a thing you open, read and close, not something to study
 * beside the page (the Modal's own doc draws that line). A floating bubble
 * would re-implement all three guarantees to save a backdrop.
 */
export function PageHelp({ body, slug }: {
  /** 3–5 sentences from the page's own dictionary (`help.body`), translated by the page. */
  body: string;
  /** The docs/console/ page for this route, e.g. "matrix" or "routes-mtr". */
  slug: string;
}) {
  const t = useT(sharedDict);
  const [open, setOpen] = useState(false);
  return (
    <>
      <button
        type="button"
        aria-label={t("help.open")}
        aria-haspopup="dialog"
        onClick={() => setOpen(true)}
        className="rounded-md p-1 text-muted-foreground transition-colors duration-(--dur-fast) hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        <CircleHelp aria-hidden="true" className="size-4" />
      </button>
      <Modal open={open} onClose={() => setOpen(false)} title={t("help.title")}>
        <p className="text-sm leading-relaxed">{body}</p>
        <a
          href={docsConsoleUrl(slug)}
          target="_blank"
          rel="noreferrer"
          className="mt-3 inline-flex items-center gap-1.5 text-sm font-medium text-primary hover:underline"
        >
          {t("help.learnMore")}
          <ExternalLink aria-hidden="true" className="size-3.5" />
        </a>
      </Modal>
    </>
  );
}
