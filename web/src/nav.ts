export interface NavItem {
  path: string;
  label: string;
  description: string;
}

// DESIGN.md §6.2 navigation.
export const NAV_ITEMS: NavItem[] = [
  { path: "/", label: "Overview", description: "Health summary, worst pairs, firing alerts, recent events." },
  { path: "/live", label: "Events", description: "Real-time event feed." },
  { path: "/investigate", label: "Incidents", description: "Investigation Mode entry and saved incidents." },
  { path: "/matrix", label: "Matrix", description: "Live/historical N×N heatmap." },
  { path: "/topology", label: "Topology", description: "Interactive connectivity map." },
  { path: "/mtr", label: "Routes · MTR", description: "MTR Explorer." },
  { path: "/diagnostics", label: "Run checks", description: "Run checks and run history." },
  { path: "/targets", label: "Scheduled checks", description: "External targets, definitions, schedules." },
  { path: "/explore", label: "Metrics", description: "Curated metrics and A/B compare." },
  { path: "/alerting", label: "Alerting", description: "Rule list, builder, maintenance windows." },
  { path: "/console", label: "PromQL", description: "PromQL dev-tools." },
  { path: "/settings", label: "Settings", description: "Language, API tokens, webhooks, config export/import, about." },
];

/**
 * navPath widens a literal route path to the `string` TanStack's <Link> accepts.
 *
 * The nav routes are BUILT from NAV_ITEMS at module load, so they are not literal
 * members of the router's registered path union and `to="/live"` does not
 * typecheck — while `to={item.path}`, which is a `string`, does. This is the same
 * widening the sidebar gets for free, named so a reader knows it is deliberate.
 */
export const navPath = (path: string): string => path;
