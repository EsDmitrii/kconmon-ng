import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { getIncidents } from "@/lib/api";
import {
  buildInvestigateURL,
  incidentPermalink,
  scopeFilterValue,
  type InvestigationScope,
} from "@/lib/investigation-sources";
import { Badge } from "./ui/badge";
import { Card } from "./ui/card";
import { cn } from "@/lib/utils";

/**
 * investigate-entry.tsx — the two things every object card grows in M6 (plan
 * Decision 11): the way IN to Investigation Mode, and the incidents already
 * open about this object.
 *
 * Both are parameterised by ONE InvestigationScope rather than by a name and a
 * kind, because that is the value both halves need to agree on: the link's
 * `?kind=&scope=` and the rail's exact-match filter are two readings of the
 * same object, and a card that spelled them apart would offer an Investigate
 * button for a pair and a rail for a node.
 */

const ACTION_CLASS =
  "inline-flex h-8 items-center rounded-md border border-border-strong px-3 text-[13px] hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

/**
 * InvestigateLink is the header action. It is a real <a href>, not a click
 * handler: the whole point of Decision 11 is that the destination is a URL an
 * operator can copy, middle-click and paste into chat.
 *
 * `now` is resolved at RENDER, not at click, so the range is the hour before
 * the card was opened. The difference only matters for a tab left open
 * overnight, and the alternative (an onClick that computes the range) would
 * cost the copyable href that is the entire feature.
 */
export function InvestigateLink({ scope, now = new Date() }: { scope: InvestigationScope; now?: Date }) {
  return (
    <a href={buildInvestigateURL(scope, now)} className={ACTION_CLASS}>
      Investigate
    </a>
  );
}

/** How many open incidents to scan before filtering to this object. The list
 *  is fleet-wide and short (an open incident is a human decision, not a
 *  metric), so one page covers a console with far more of them than anybody
 *  would want. */
export const RELATED_INCIDENTS_SCAN = 50;

/**
 * RelatedIncidents is the rail entry: the OPEN incidents filed against exactly
 * this object.
 *
 * The filter is CLIENT-side over one shared fleet-wide page rather than a
 * per-object `?scope=` request. Two reasons, one of them the whole design: the
 * incident scope vocabulary matches EXACTLY, so a rail asking `?scope=node-a`
 * silently drops the global incidents an operator would also want — and one
 * cached list is shared by every card in a session, whereas a scoped request
 * refetches for each object visited. What is filtered here is therefore visible
 * and correctable, not hidden inside a query parameter.
 */
export function RelatedIncidents({ scope }: { scope: InvestigationScope }) {
  const { me, can } = useAuth();
  const { available, resolved } = useDatabaseAvailable();
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
      <aside aria-label="Open incidents">
        {/* No second Investigate link here on purpose: the card header already
            carries exactly one, and two controls with the same name in one page
            is an accessibility problem before it is a design one. */}
        <h2 className="text-sm font-semibold">Open incidents</h2>

        {me !== undefined && !canRead ? (
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
            Incidents need incidents:read — none was requested.
          </p>
        ) : resolved && !available ? (
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
            Incidents are stored — set console.database.mode. Nothing was requested.
          </p>
        ) : related.length === 0 ? (
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
            No open incident names this object.
          </p>
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
                  Open
                </Badge>
              </li>
            ))}
          </ul>
        )}
      </aside>
    </Card>
  );
}
