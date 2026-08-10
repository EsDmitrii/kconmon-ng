import { Link } from "@tanstack/react-router";
import {
  Activity,
  Bell,
  LayoutDashboard,
  LayoutGrid,
  LineChart,
  Microscope,
  Network,
  Route,
  Settings,
  SquareTerminal,
  Stethoscope,
  Target,
  type LucideIcon,
} from "lucide-react";
import { NAV_ITEMS, type NavItem } from "@/nav";
import { ThemeToggle } from "@/components/theme-toggle";
import { UserMenu } from "@/components/user-menu";
import { useAuth } from "@/hooks/use-auth";
import { useT } from "@/lib/i18n";
import { chromeDict, NAV_KEYS, type ChromeKey } from "@/lib/i18n/dict/chrome";
import { NAV_DESC_KEYS, paletteDict } from "@/lib/i18n/dict/palette";
import { cn } from "@/lib/utils";

/* The sidebar derives its grouping and icons from NAV_ITEMS by path, without
   changing nav.ts's exported shape. Unknown paths land in the last group so a
   new nav entry can never silently disappear. */
const ICONS: Record<string, LucideIcon> = {
  "/": LayoutDashboard,
  "/live": Activity,
  "/investigate": Microscope,
  "/matrix": LayoutGrid,
  "/topology": Network,
  "/mtr": Route,
  "/diagnostics": Stethoscope,
  "/targets": Target,
  "/explore": LineChart,
  "/alerting": Bell,
  "/console": SquareTerminal,
  "/settings": Settings,
};

/* The header is a translation KEY rather than a literal: the group names are
   the one piece of sidebar text with no NavItem behind it to fall back to. */
const GROUPS: { key: ChromeKey; paths: string[] }[] = [
  { key: "nav.group.monitor", paths: ["/", "/live", "/matrix", "/topology"] },
  { key: "nav.group.investigate", paths: ["/investigate", "/mtr", "/diagnostics", "/explore", "/console"] },
  { key: "nav.group.manage", paths: ["/targets", "/alerting", "/settings"] },
];

function groupItems(): { key: ChromeKey; items: NavItem[] }[] {
  const byPath = new Map(NAV_ITEMS.map((i) => [i.path, i]));
  const seen = new Set<string>();
  const groups = GROUPS.map((g) => ({
    key: g.key,
    items: g.paths.flatMap((p) => {
      const item = byPath.get(p);
      if (!item) return [];
      seen.add(p);
      return [item];
    }),
  }));
  const rest = NAV_ITEMS.filter((i) => !seen.has(i.path));
  if (rest.length > 0) groups[groups.length - 1].items.push(...rest);
  return groups;
}

function NavLink({ item, onNavigate }: { item: NavItem; onNavigate?: () => void }) {
  const t = useT(chromeDict);
  /* The tooltip is the palette's search corpus, so it lives in the PALETTE's
     table and the two can never say different things about one link (the pair
     was the named gap the palette's own translation closed). */
  const tDesc = useT(paletteDict);
  const Icon = ICONS[item.path];
  /* Same fallback rule as ICONS and GROUPS above: a path this build's chrome
     dictionary has never heard of renders nav.ts's English label — and its
     English description — rather than nothing at all. */
  const key = NAV_KEYS[item.path];
  const descKey = NAV_DESC_KEYS[item.path];
  return (
    <Link
      to={item.path}
      activeOptions={{ exact: item.path === "/" }}
      onClick={onNavigate}
      title={descKey ? tDesc(descKey) : item.description}
      className={cn(
        "group flex h-9 items-center gap-2.5 rounded-md px-2.5 text-[13.5px] text-muted-foreground",
        "transition-colors duration-(--dur-fast) ease-(--ease)",
        "hover:bg-accent/60 hover:text-foreground",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
        "[&.active]:bg-surface-2 [&.active]:font-medium [&.active]:text-foreground [&.active]:shadow-card",
      )}
    >
      {Icon ? (
        <Icon
          aria-hidden="true"
          className="size-4 shrink-0 opacity-70 transition-opacity duration-(--dur-fast) group-hover:opacity-100 group-[.active]:opacity-100 group-[.active]:text-primary"
        />
      ) : null}
      <span className="truncate">{key ? t(key) : item.label}</span>
    </Link>
  );
}

/**
 * AppSidebar is the column itself. `onNavigate` fires on every link click and is
 * how the narrow-viewport drawer closes after a navigation — a drawer that
 * stayed open over the page it just loaded would hide the thing it was used to
 * reach.
 */
export function AppSidebar({ onNavigate }: { onNavigate?: () => void } = {}) {
  const { me, can, isAnonymous } = useAuth();
  const t = useT(chromeDict);
  return (
    <aside className="flex h-full w-64 flex-col bg-surface">
      <div className="flex items-center justify-between px-4 pb-2 pt-4">
        <span className="flex items-center gap-2.5">
          <span
            aria-hidden="true"
            className="size-2.5 rounded-full bg-gradient-to-br from-primary to-chart-3 shadow-[0_0_10px_hsl(var(--primary)/0.6)]"
          />
          <span className="text-[15px] font-semibold tracking-tight">kconmon-ng</span>
        </span>
        <ThemeToggle />
      </div>
      {/* Named, like every other role in this kit (the palette's listbox is
          "Commands", the picker's grid is "Calendar"): "navigation" alone is
          what a landmark list would otherwise announce. */}
      <nav aria-label={t("shell.nav.aria")} className="flex-1 overflow-y-auto px-3 pb-4 pt-2">
        {groupItems().map((group) => (
          <div key={group.key} className="mb-5">
            <div className="mb-1.5 px-2.5 text-[10.5px] font-semibold uppercase tracking-[0.1em] text-muted-foreground/70">
              {t(group.key)}
            </div>
            <div className="flex flex-col gap-0.5">
              {group.items.map((item) => (
                <NavLink key={item.path} item={item} onNavigate={onNavigate} />
              ))}
            </div>
          </div>
        ))}
      </nav>
      <div className="border-t border-border px-4 py-3">
        {/* me is undefined until GET /api/v1/auth/me answers, and isAnonymous
            reads false in that gap (use-auth.ts's fail-closed default) — so
            this only ever shows the real user menu once a genuinely
            non-anonymous subject is confirmed, never mid-load. */}
        {me && !isAnonymous ? (
          <UserMenu me={me} can={can} />
        ) : (
          <span className="text-[11px] text-muted-foreground/70">{t("sidebar.footer")}</span>
        )}
      </div>
    </aside>
  );
}
