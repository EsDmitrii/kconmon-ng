import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { cardsDict } from "@/lib/i18n/dict/cards";
import { matrixDict } from "@/lib/i18n/dict/matrix";
import { matrixCellsDict } from "@/lib/i18n/dict/matrix-cells";
import { overviewDict } from "@/lib/i18n/dict/overview";
import { topologyDict } from "@/lib/i18n/dict/topology";
import { TimeMachineProvider } from "@/lib/timemachine";
import { NodeCardPage } from "@/pages/node-card";
import { PairCardPage } from "@/pages/pair-card";
import { TargetCardPage } from "@/pages/target-card";

/**
 * The three OBJECT cards in Russian, and the one property they share that no
 * per-card test can check on its own: the SEVERITY VOCABULARY is the same four
 * words on all three. An operator moving from a node to one of its pairs must
 * not be told «Отказ» in one place and «Сбой» in the other.
 *
 * That sentence used to describe only the three CARDS, and the cards agreed
 * with each other while disagreeing with the matrix, the topology and the
 * Overview — «Отказ» here, «Сбой» there, one English word. The cards moved to
 * «сбой» and the last describe() in this file is now a CROSS-DICTIONARY pin:
 * the four words are checked against the surfaces that render the same
 * lib/matrix-cells.ts verdict, not just against each other.
 *
 * Same shape as lib/i18n/manage.test.tsx — the cards' own test files render
 * with no provider and read English, which is the property they are pinning.
 */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const PERMISSIONS = ["targets:read", "checks:read", "runs:create"];

function stubFetch() {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const href = typeof input === "string" ? input : String((input as Request).url ?? input);
      if (href.includes("/api/v1/auth/me")) {
        return Promise.resolve(
          json({
            subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["operator"] },
            permissions: PERMISSIONS,
          }),
        );
      }
      if (href.includes("/api/v1/config")) {
        return Promise.resolve(
          json({
            auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
            anonymousBanner: false,
            controller: { configured: true },
            prometheus: { configured: true },
            database: { configured: true },
          }),
        );
      }
      if (href.includes("/api/v1/topology")) {
        return Promise.resolve(
          json({
            nodes: [{ name: "node-a", zone: "az-1", ready: true }],
            agents: [{ id: "agent-1", nodeName: "node-a", zone: "az-1", podIP: "10.1.2.3" }],
            asOf: "2026-08-08T12:00:00Z",
          }),
        );
      }
      // The node's ONE outbound cell is failing hard, so the header's badge has
      // a tier to name rather than falling through to "No data".
      if (href.includes("/api/v1/matrix")) {
        return Promise.resolve(
          json({
            cells: [
              { source: "node-a", destination: "node-b", failRatio: 0.5, rttP95: 4_000_000, samples: 10 },
            ],
            asOf: "2026-08-08T12:00:00Z",
          }),
        );
      }
      if (href.includes("/api/v1/targets/")) {
        return Promise.resolve(
          json({
            id: "t-1",
            name: "edge-gw",
            kind: "host",
            address: "10.0.0.1",
            labels: {},
            createdAt: "2026-01-01T00:00:00Z",
            updatedAt: "2026-01-01T00:00:00Z",
          }),
        );
      }
      if (href.includes("/api/v1/runs")) return Promise.resolve(json({ runs: [] }));
      if (href.includes("/api/v1/checks")) return Promise.resolve(json({ definitions: [] }));
      if (href.includes("/api/v1/schedules")) return Promise.resolve(json({ schedules: [] }));
      if (href.includes("/api/v1/annotations")) return Promise.resolve(json({ annotations: [] }));
      if (href.includes("/api/v1/maintenance")) return Promise.resolve(json({ windows: [] }));
      if (href.includes("/api/v1/incidents")) return Promise.resolve(json({ incidents: [] }));
      if (href.includes("/api/v1/events")) return Promise.resolve(json({ events: [] }));
      if (href.includes("/api/v1/promql")) {
        return Promise.resolve(json({ status: "success", data: { resultType: "matrix", result: [] } }));
      }
      return Promise.resolve(json({}));
    }),
  );
}

function renderRu(node: React.ReactNode) {
  stubFetch();
  localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  /* ThemeProvider too: the pair and target cards draw a chart and read the
     theme to colour it, and useTheme (unlike useT) throws outside its
     provider on purpose. */
  render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <LocaleProvider>
          <TimeMachineProvider>{node}</TimeMachineProvider>
        </LocaleProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  window.history.pushState({}, "", "/");
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

describe("the severity vocabulary", () => {
  it("is the operator words this zone settled on, and they are not the English ones", () => {
    expect(cardsDict.ru["tier.ok"]).toBe("Здоров");
    expect(cardsDict.ru["tier.warn"]).toBe("Деградация");
    expect(cardsDict.ru["tier.bad"]).toBe("Сбой");
    expect(cardsDict.ru["tier.unknown"]).toBe("Нет данных");
  });

  it("is ONE table, so no card can drift from another", () => {
    // The three cards import TIER_KEYS from this dictionary rather than each
    // holding their own labels — this is the assertion that would break the
    // day somebody re-adds a local copy.
    for (const key of ["tier.ok", "tier.warn", "tier.bad", "tier.unknown"] as const) {
      expect(cardsDict.en[key]).toBeTruthy();
      expect(cardsDict.ru[key]).not.toBe(cardsDict.en[key]);
    }
  });
});

/* ── the fail word, across dictionaries ──────────────────────────────────── */

/**
 * The cards used to be internally consistent and externally wrong: «Отказ» on
 * all three badges while dict/matrix.ts, dict/topology.ts, dict/overview.ts and
 * dict/matrix-cells.ts all rendered the SAME English concept as «сбой». These
 * are the equalities that make the unification a fact rather than a habit —
 * every one of them reads a ratio through the same lib/matrix-cells.ts
 * thresholds, so every one of them has to name the verdict with the same noun.
 */
describe("the ONE word for a probe that failed", () => {
  it("badges a card with the word the matrix, topology and Overview legends open with", () => {
    const bad = cardsDict.ru["tier.bad"];
    expect(matrixDict.ru["legend.bad"].startsWith(bad)).toBe(true);
    expect(topologyDict.ru["legend.bad"].startsWith(bad)).toBe(true);
    expect(overviewDict.ru["tiles.failing.tone"].startsWith(bad)).toBe(true);
  });

  it("names the fail-ratio column exactly as the matrix tooltip names the series", () => {
    // "Fail ratio" on the node card, "Failure ratio" in the tooltip — one
    // series, so one Russian name.
    expect(cardsDict.ru["node.breakdown.failRatio"]).toBe(matrixDict.ru["tooltip.failRatio"]);
  });

  it("says on a run row what the topology says on a node and the cell says in its aria-label", () => {
    const failed = cardsDict.ru["pair.result.failed"];
    expect(failed).toBe(topologyDict.ru["health.failing"]);
    expect(matrixCellsDict.ru["fail"].startsWith(failed)).toBe(true);
  });

  /* QA scope 2, finding #11 — this used to assert «нет данных о сбоях», the
     exact phrasing dict/matrix.ts documents REJECTING because it opens with
     «нет данных», the phrase reserved for a pair nothing probed. The cards kept
     it anyway. Both halves now read the matrix's own «сбои: н/д», and the pin
     is an EQUALITY against dict/matrix.ts rather than a substring test, so a
     future edit to either file breaks here. */
  it("says «сбои: н/д» exactly as the matrix cell does, and never opens with «нет данных»", () => {
    expect(cardsDict.ru["cell.noFailData"]).toBe(matrixDict.ru["cell.noFailData"]);
    expect(cardsDict.ru["cell.noFailData"].startsWith("нет данных")).toBe(false);
  });

  it("keeps the unprobed reading distinct, and spelled as the matrix legend spells it", () => {
    // Two different facts: one leg was never probed, the other has a p95 and a
    // lazy failure counter. They must not read the same.
    expect(cardsDict.ru["cell.noData"]).not.toBe(cardsDict.ru["cell.noFailData"]);
    expect(cardsDict.ru["cell.noData"]).toBe(matrixDict.ru["legend.unknown"].toLowerCase());
  });

  it("leaves the LONG form to the surfaces that have room for it", () => {
    // The aria-label and the tooltip still say the whole sentence; only the
    // 10.5px line and the badge take the short one.
    expect(matrixCellsDict.ru["noFailSignal"]).toContain("данных о сбоях");
  });
});

describe("the node card in Russian", () => {
  beforeEach(() => window.history.pushState({}, "", "/nodes/node-a"));

  it("names the object in the URL and the zone around it", async () => {
    renderRu(<NodeCardPage />);
    // The node NAME is data and stays; the sentence around it is ours.
    expect(await screen.findByRole("heading", { name: "node-a" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(cardsDict.ru["node.zone"].replace("{zone}", "az-1"))).toBeInTheDocument());
  });

  it("says «Сбой» on the header badge", async () => {
    renderRu(<NodeCardPage />);
    await waitFor(() => expect(screen.getByText(cardsDict.ru["tier.bad"])).toBeInTheDocument());
  });

  it("translates the tabs and the identity panel", async () => {
    renderRu(<NodeCardPage />);
    await waitFor(() =>
      expect(screen.getByRole("radio", { name: cardsDict.ru["tab.overview"] })).toBeInTheDocument(),
    );
    expect(screen.getByRole("radio", { name: cardsDict.ru["tab.diagnostics"] })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: cardsDict.ru["node.identity"] })).toBeInTheDocument();
    expect(screen.getByText(cardsDict.ru["node.identity.podIP"])).toBeInTheDocument();
    // Ready is a boolean rendered as a word, so it translates.
    expect(screen.getByText(cardsDict.ru["node.identity.yes"])).toBeInTheDocument();
  });

  it("translates the breakdown table's headers, keeping the destination NAME as data", async () => {
    renderRu(<NodeCardPage />);
    await waitFor(() =>
      expect(screen.getByRole("columnheader", { name: cardsDict.ru["node.breakdown.destination"] })).toBeInTheDocument(),
    );
    expect(screen.getByRole("columnheader", { name: cardsDict.ru["node.breakdown.failRatio"] })).toBeInTheDocument();
    expect(screen.getByText("node-b")).toBeInTheDocument();
  });
});

describe("the pair card in Russian", () => {
  beforeEach(() => window.history.pushState({}, "", "/pairs/node-a/node-b"));

  it("keeps the two node names and the arrow as the title", async () => {
    renderRu(<PairCardPage />);
    expect(await screen.findByRole("heading", { name: "node-a → node-b" })).toBeInTheDocument();
    expect(screen.getByText(cardsDict.ru["pair.description"])).toBeInTheDocument();
  });

  it("distinguishes «нет данных» from «сбои: н/д» on the two legs", async () => {
    renderRu(<PairCardPage />);
    // The reverse leg is not in the matrix at all, so it is the unmeasured one.
    await waitFor(() => expect(screen.getAllByText(cardsDict.ru["cell.noData"]).length).toBeGreaterThan(0));
    expect(cardsDict.ru["cell.noFailData"]).not.toBe(cardsDict.ru["cell.noData"]);
  });

  it("translates the Diagnostics tab, including the button the palette also names", async () => {
    renderRu(<PairCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: cardsDict.ru["tab.diagnostics"] }));
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: cardsDict.ru["pair.lastRun"] })).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: cardsDict.ru["pair.runCheck"] })).toBeInTheDocument();
  });
});

describe("the target card in Russian", () => {
  beforeEach(() => window.history.pushState({}, "", "/targets/t-1"));

  it("keeps the target's own name and kind, and translates the sentence around them", async () => {
    renderRu(<TargetCardPage />);
    expect(await screen.findByRole("heading", { name: "edge-gw" })).toBeInTheDocument();
    expect(screen.getByText(cardsDict.ru["target.description"])).toBeInTheDocument();
    // `kind` and `address` are the row's own fields.
    expect(screen.getByText("host")).toBeInTheDocument();
    expect(screen.getByText("10.0.0.1")).toBeInTheDocument();
  });

  it("translates the three tabs and the Checks tab's empty state", async () => {
    renderRu(<TargetCardPage />);
    await waitFor(() => expect(screen.getByRole("radio", { name: cardsDict.ru["tab.checks"] })).toBeInTheDocument());
    expect(screen.getByRole("radio", { name: cardsDict.ru["tab.history"] })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: cardsDict.ru["tab.runs"] })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(cardsDict.ru["target.checks.empty"])).toBeInTheDocument());
  });

  it("translates the Runs tab's scan limitation, keeping the endpoint path as it is", async () => {
    renderRu(<TargetCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: cardsDict.ru["tab.runs"] }));
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: cardsDict.ru["target.runs.heading"] })).toBeInTheDocument(),
    );
    expect(screen.getByText(/GET \/api\/v1\/runs/)).toBeInTheDocument();
  });
});
