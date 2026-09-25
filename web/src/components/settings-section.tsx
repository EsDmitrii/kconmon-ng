import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { ApiError } from "@/lib/api";
import { useT } from "@/lib/i18n";
import { settingsDict } from "@/lib/i18n/dict/settings";

/* The building blocks every Settings section is drawn from, shared by pages/settings.tsx and the
   sections that live in their own files. */

/**
 * queryErrorMessage is what this page says when something failed: the server's
 * own words whenever it wrote any, and this section's own sentence when it did
 * not.
 *
 * The `?? title` alone was not enough. A refusal that never reached the console
 * — a gateway's HTML 502, a proxy's empty problem document — arrives with an
 * ABSENT detail and a title that is the empty statusText, and `"" ` is a string,
 * so every error slot on this page rendered a red paragraph with nothing in it:
 * the list stayed empty, the button un-spun, and the operator was looking at a
 * page that had silently given up. A blank message is worse than a generic one.
 */
export function queryErrorMessage(error: unknown, fallback: string): string {
  if (!(error instanceof ApiError)) return fallback;
  const said = [error.problem.detail, error.problem.title].map((s) => s?.trim() ?? "").find((s) => s !== "");
  return said ?? fallback;
}

export function SectionCard({
  id,
  title,
  blurb,
  action,
  children,
}: {
  id?: string;
  title: string;
  /** The one-paragraph explanation under the heading. */
  blurb?: ReactNode;
  /** The section's own create button, on the heading line at the right. */
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Card asChild className="p-4 sm:p-6">
      {/* `id` is the anchor the sidebar's user menu links a deep link at. */}
      <section id={id}>
        <div className="flex items-start justify-between gap-4">
          <h2 className="type-section">{title}</h2>
          {action ? <div className="shrink-0">{action}</div> : null}
        </div>
        {blurb ? <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{blurb}</p> : null}
        {children}
      </section>
    </Card>
  );
}

export function ErrorLine({
  children,
  id,
  onRetry,
}: {
  children: ReactNode;
  id?: string;
  /** Re-runs the read that failed; a small ghost button beside the sentence. */
  onRetry?: () => void;
}) {
  const t = useT(settingsDict);
  /* The sentence and the role stay on ONE element — tests and assistive tech
     both find the alert by its text — and the button rides inside it. */
  return (
    <p id={id} role="alert" className="mt-3 text-sm leading-relaxed text-health-bad">
      {children}
      {onRetry ? (
        <Button type="button" size="sm" variant="ghost" className="ml-3 h-7 px-2 align-middle" onClick={onRetry}>
          {t("error.retry")}
        </Button>
      ) : null}
    </p>
  );
}

/**
 * RowActionLabel is the VISIBLE half of a row button whose accessible name
 * carries the object's own name — pages/alerting.tsx's component, verbatim.
 * `text` is the verb that shows, `title` the whole sentence; the span stays
 * bounded and truncating because a name is operator bytes of any length.
 */
export function RowActionLabel({ text, title }: { text: string; title?: string }) {
  return (
    <span aria-hidden="true" className="block max-w-[14rem] truncate" title={title ?? text}>
      {text}
    </span>
  );
}

/** ROW_ACTION is the compact ghost button every row action on this page is
 *  drawn as; a touch tighter on a phone, the same value pages/alerting.tsx uses. */
export const ROW_ACTION = "h-7 px-1.5 sm:px-2";
