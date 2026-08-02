export interface NavItem {
  path: string;
  label: string;
  description: string;
}

// DESIGN.md §6.2 navigation.
export const NAV_ITEMS: NavItem[] = [
  { path: "/", label: "Overview", description: "Health summary, worst pairs, firing alerts, recent events." },
  { path: "/live", label: "Live", description: "Real-time event feed." },
  { path: "/investigate", label: "Investigate", description: "Investigation Mode entry and saved incidents." },
  { path: "/matrix", label: "Matrix", description: "Live/historical N×N heatmap." },
  { path: "/topology", label: "Topology", description: "Interactive connectivity map." },
  { path: "/mtr", label: "MTR", description: "MTR Explorer." },
  { path: "/diagnostics", label: "Diagnostics", description: "Run checks and run history." },
  { path: "/targets", label: "Targets & Schedules", description: "External targets, definitions, schedules." },
  { path: "/explore", label: "Explore", description: "Curated metrics and A/B compare." },
  { path: "/alerting", label: "Alerting", description: "Rule list and builder." },
  { path: "/console", label: "Console", description: "PromQL dev-tools." },
  { path: "/settings", label: "Settings", description: "Auth, RBAC, retention, maintenance, webhooks, export/import." },
];
