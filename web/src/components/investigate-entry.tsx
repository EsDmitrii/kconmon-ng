import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { getIncidents } from "@/lib/api";
import { useT } from "@/lib/i18n";
import { investigateEntryDict } from "@/lib/i18n/dict/investigate-entry";
import {
  buildInvestigateURL,
  incidentPermalink,
  scopeFilterValue,
  type InvestigationScope,
} from "@/lib/investigation-sources";
import { useTimeContext, withAtParam } from "@/lib/timemachine";
import { Badge } from "./ui/badge";
import { Card } from "./ui/card";
import { cn } from "@/lib/utils";

/** Both are parameterised by ONE InvestigationScope rather than by a name and a kind. */

const ACTION_CLASS =
  "inline-flex h-8 items-center rounded-md border border-border-strong px-3 text-[13px] hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

/**
 * It is a real <a href>, not a click handler; the difference only matters for a tab left open
 * overnight.
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
 */
export function RelatedIncidents({ scope }: { scope: InvestigationScope }) {
  const { me, can } = useAuth();
  const { available, resolved } = useDatabaseAvailable();
  const t = useT(investigateEntryDict);
  const canRead = can("incidents:read");
  const enabled = me !== undefined && canRead && resolved && available;
  const filter = scopeFilterValue(scope);

  const query = useQuery({
    queryKey: ["incidents", "open"],
    queryFn: () => getIncidents({ status: "open", limit: RELATED_INCIDENTS_SCAN }),
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

        {me !== undefined && !canRead ? (
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t("denied")}</p>
        ) : resolved && !available ? (
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t("noDatabase")}</p>
        ) : related.length === 0 ? (
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t("empty")}</p>
        ) : (
          <ul className="mt-2 flex flex-col divide-y divide-border">
            {related.map((i) => (
              <li key={i.id} data-testid="related-incident" className="flex items-center gap-2 py-2">
                <a
                  href={incidentPermalink(i.id)}
                  className={cn("min-w-0 flex-1 truncate text-xs text-primary hover:underline")}
                >
                  {i.title}
                </a>
                <Badge variant="warn" dot>
                  {t("open")}
                </Badge>
              </li>
            ))}
          </ul>
        )}
      </aside>
    </Card>
  );
}
