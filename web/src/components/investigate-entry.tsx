import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { queryErrorMessage } from "@/lib/api";
import { stampFull, useLocale, useT } from "@/lib/i18n";
import { cardsDict } from "@/lib/i18n/dict/cards";
import { investigateEntryDict } from "@/lib/i18n/dict/investigate-entry";
import {
  buildInvestigateURL,
  incidentPermalink,
  openIncidents,
  scopeFilterValue,
  type InvestigationScope,
} from "@/lib/investigation-sources";
import { useTimeContext, withAtParam } from "@/lib/timemachine";
import { Badge } from "./ui/badge";
import { Card } from "./ui/card";
import { EmptyState } from "./ui/empty-state";
import { Skeleton } from "./ui/skeleton";

const ACTION_CLASS =
  "inline-flex h-8 items-center rounded-md border border-border-strong px-3 text-[13px] hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

/**
 * InvestigateLink is a real <a href>, not a click handler. Its window is fixed at render time, which
 * only matters for a tab left open overnight.
 *
 * The window ends at the INSTANT BEING VIEWED, not at now: opened while the Time Machine is engaged,
 * an Investigate link that anchored on the wall clock walked the reader out of the moment they were
 * investigating and into the present, without saying so.
 */
export function InvestigateLink({ scope, now }: { scope: InvestigationScope; now?: Date }) {
  const t = useT(investigateEntryDict);
  const { at } = useTimeContext();
  return (
    <a href={withAtParam(buildInvestigateURL(scope, now ?? at ?? new Date()))} className={ACTION_CLASS}>
      {t("investigate")}
    </a>
  );
}

/** How many open incidents to scan before filtering to this object. The list
 *  is fleet-wide and short (an open incident is a human decision, not a
 *  metric), so one page covers a console with far more of them than anybody
 *  would want. */
export const RELATED_INCIDENTS_SCAN = 50;

/**
 * RelatedIncidents is the rail entry: the OPEN incidents filed against exactly this object; the
 * filter is CLIENT-side over one shared fleet-wide page rather than a per-object `?scope=` request.
 *
 * Engaged, "open" means open AT the instant on screen: `status` is a now fact, so the rail scans
 * the list (as the Overview does) for the incidents declared by `at` and not resolved by then.
 */
export function RelatedIncidents({ scope }: { scope: InvestigationScope }) {
  const { me, can, meError } = useAuth();
  const { available, resolved, error: configError } = useDatabaseAvailable();
  const { at } = useTimeContext();
  const { locale } = useLocale();
  const t = useT(investigateEntryDict);
  /* The rail sits on the node, pair and target cards and says "loading" in their words. */
  const tCards = useT(cardsDict);
  const canRead = can("incidents:read");
  const enabled = me !== undefined && canRead && resolved && available;
  const filter = scopeFilterValue(scope);

  /* Engaged, the scan pages past incidents not open at t (see scanIncidents) until as many OPEN
     ones as the live list holds have been found. */
  const query = useQuery({
    queryKey: at ? ["incidents", "open", "at", at.toISOString()] : ["incidents", "open"],
    queryFn: () => openIncidents(at, RELATED_INCIDENTS_SCAN),
    enabled,
  });
  const related = (query.data?.incidents ?? []).filter((i) => i.scope === filter);

  return (
    <Card asChild className="p-4">
      <aside aria-label={t("aria")}>
        {/* No second Investigate link here on purpose: the card header already
            carries exactly one, and two controls with the same name in one page
            is an accessibility problem before it is a design one. */}
        <h2 className="text-sm font-semibold">{t("title")}</h2>
        {at ? <p className="mt-0.5 text-[11px] text-muted-foreground">{t("at", { at: stampFull(at, locale) })}</p> : null}

        {me === undefined && meError !== null ? (
          <p role="alert" className="mt-2 text-xs leading-relaxed text-health-bad">
            {t("meFailed", { error: queryErrorMessage(meError, t("failed.generic")) })}
          </p>
        ) : me !== undefined && !canRead ? (
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t("denied")}</p>
        ) : configError !== null ? (
          <p role="alert" className="mt-2 text-xs leading-relaxed text-health-bad">
            {t("configFailed", { error: queryErrorMessage(configError, t("failed.generic")) })}
          </p>
        ) : resolved && !available ? (
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t("noDatabase")}</p>
        ) : !enabled || query.isPending ? (
          <div role="status" aria-live="polite" className="mt-2">
            <span className="sr-only">{tCards("loading")}</span>
            <Skeleton className="h-8 w-full" />
          </div>
        ) : query.isError ? (
          <p role="alert" className="mt-2 text-xs leading-relaxed text-health-bad">
            {t("failed", { error: queryErrorMessage(query.error, t("failed.generic")) })}
          </p>
        ) : related.length === 0 ? (
          <EmptyState compact title={t(at ? "empty.at" : "empty")} body={t(at ? "empty.at.body" : "empty.body")} />
        ) : (
          <ul className="mt-2 flex flex-col divide-y divide-border">
            {related.map((i) => (
              <li key={i.id} data-testid="related-incident" className="flex items-center gap-2 py-2">
                <a href={incidentPermalink(i.id)} className="min-w-0 flex-1 truncate text-xs text-primary hover:underline">
                  {i.title}
                </a>
                <Badge variant="warn" dot>
                  {t("open")}
                </Badge>
              </li>
            ))}
          </ul>
        )}
        {query.data?.truncated ? <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t("scanCapped")}</p> : null}
      </aside>
    </Card>
  );
}
