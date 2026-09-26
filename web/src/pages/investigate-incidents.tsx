import { useMemo, useState } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { EmptyState } from "@/components/ui/empty-state";
import { Pager, usePager } from "@/components/ui/pager";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { getIncidents, queryErrorMessage } from "@/lib/api";
import { stampFull, useLocale, useT, type Locale } from "@/lib/i18n";
import { investigateIncidentsDict, type InvestigateIncidentsKey } from "@/lib/i18n/dict/investigate-incidents";
import { incidentOpenAt, incidentPermalink } from "@/lib/investigation-sources";
import { withAtParam, useTimeContext } from "@/lib/timemachine";
import type { Incident, IncidentStatus } from "@/lib/types";

type StatusFilter = "all" | IncidentStatus;

const FILTERS: { value: StatusFilter; key: InvestigateIncidentsKey }[] = [
  { value: "all", key: "filter.all" },
  { value: "open", key: "filter.open" },
  { value: "resolved", key: "filter.resolved" },
];

/* One server page per "Load older"; the Pager then pages what has been loaded. */
export const SAVED_INCIDENTS_PAGE = 50;

/**
 * statusAt is an incident's status as the list shows it: `status` is a now fact, so an engaged
 * Time Machine reads the lifecycle stamps instead, and a row saved after the instant has none.
 */
export function statusAt(i: Incident, at: Date | null): IncidentStatus | null {
  if (at === null) return i.status;
  if (!(Date.parse(i.createdAt) <= at.getTime())) return null;
  return incidentOpenAt(i, at) ? "open" : "resolved";
}

function fmtStamp(iso: string, locale: Locale): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : stampFull(d, locale);
}

/**
 * SavedIncidents is the Incidents page's own list: every saved incident, open and resolved, newest
 * first, over GET /api/v1/incidents's keyset cursor. Live, the status filter is the server's;
 * engaged, the rows are read unfiltered and judged at the instant on screen.
 */
export function SavedIncidents({
  enabled,
  canRead,
  dbAvailable,
  configFailed,
}: {
  enabled: boolean;
  canRead: boolean;
  dbAvailable: boolean;
  /** The page already says the configuration could not be read; this list adds nothing to it. */
  configFailed: boolean;
}) {
  const t = useT(investigateIncidentsDict);
  const { locale } = useLocale();
  const { at } = useTimeContext();
  const [filter, setFilter] = useState<StatusFilter>("all");
  const serverStatus = at === null && filter !== "all" ? filter : undefined;

  const query = useInfiniteQuery({
    queryKey: ["incidents", "saved", serverStatus ?? "all"],
    queryFn: ({ pageParam }) =>
      getIncidents({ status: serverStatus, limit: SAVED_INCIDENTS_PAGE, cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (page) => page.nextCursor || undefined,
    enabled: enabled && canRead && dbAvailable,
  });

  const rows = useMemo(() => {
    const out: { incident: Incident; status: IncidentStatus }[] = [];
    for (const page of query.data?.pages ?? []) {
      for (const incident of page.incidents) {
        const status = statusAt(incident, at);
        if (status === null || (filter !== "all" && status !== filter)) continue;
        out.push({ incident, status });
      }
    }
    return out;
  }, [query.data, at, filter]);
  const pager = usePager(rows, { resetKey: `${filter}|${at?.toISOString() ?? ""}` });

  let body;
  if (!enabled || configFailed) {
    body = null;
  } else if (!canRead) {
    body = <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t("denied")}</p>;
  } else if (!dbAvailable) {
    body = <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t("noDatabase")}</p>;
  } else if (query.isPending) {
    body = (
      <div role="status" aria-live="polite" className="mt-3">
        <span className="sr-only">{t("loading")}</span>
        <Skeleton className="h-8 w-full" />
      </div>
    );
  } else if (query.isError && rows.length === 0 && !query.hasNextPage) {
    body = (
      <p role="alert" className="mt-2 text-xs leading-relaxed text-health-bad">
        {t("failed", { error: queryErrorMessage(query.error, t("failed.generic")) })}
      </p>
    );
  } else {
    body = (
      <>
        {rows.length === 0 ? (
          <EmptyState
            compact
            title={t(at !== null ? "empty.at" : filter === "open" ? "empty.open" : filter === "resolved" ? "empty.resolved" : "empty.all")}
          />
        ) : (
          <>
            <ul aria-label={t("title")} className="mt-2 flex flex-col divide-y divide-border">
              {pager.visible.map(({ incident: i, status }) => (
                <li key={i.id} data-testid="saved-incident" className="flex flex-wrap items-center gap-x-3 gap-y-1 py-2">
                  <a
                    href={withAtParam(incidentPermalink(i.id))}
                    className="min-w-0 flex-1 basis-[14rem] truncate text-sm text-primary hover:underline"
                  >
                    {i.title}
                  </a>
                  <Badge variant={status === "open" ? "warn" : "neutral"} dot>
                    {t(status === "open" ? "status.open" : "status.resolved")}
                  </Badge>
                  <span className={i.scope === "" ? "type-meta" : "type-meta mono-data max-w-[16rem] truncate"} title={i.scope}>
                    {i.scope === "" ? t("scope.global") : i.scope}
                  </span>
                  <span className="type-meta nums">
                    {t("opened", { at: fmtStamp(i.createdAt, locale) })}
                    {status === "resolved" && i.resolvedAt ? t("resolvedAt", { at: fmtStamp(i.resolvedAt, locale) }) : ""}
                  </span>
                </li>
              ))}
            </ul>
            <Pager pager={pager} subject={t("subject")} truncated={query.hasNextPage} className="px-0" />
          </>
        )}
        {query.hasNextPage ? (
          <div className="mt-2 flex flex-col items-center gap-2">
            {query.isError ? (
              <p role="alert" className="text-xs text-health-bad">
                {t("olderFailed")}
              </p>
            ) : null}
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={query.isFetchingNextPage}
              onClick={() => void query.fetchNextPage()}
            >
              {query.isFetchingNextPage ? t("loadingOlder") : t("loadOlder")}
            </Button>
          </div>
        ) : null}
      </>
    );
  }

  return (
    <Card asChild className="p-5">
      <section aria-label={t("title")}>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="flex flex-col">
            <h2 className="type-section">{t("title")}</h2>
            {at !== null ? <p className="text-[11px] text-muted-foreground">{t("at", { at: stampFull(at, locale) })}</p> : null}
          </div>
          {canRead && dbAvailable && !configFailed ? (
            <Segmented
              aria-label={t("filter.aria")}
              options={FILTERS.map((f) => ({ value: f.value, label: t(f.key) }))}
              value={filter}
              onChange={setFilter}
            />
          ) : null}
        </div>
        {body}
      </section>
    </Card>
  );
}
