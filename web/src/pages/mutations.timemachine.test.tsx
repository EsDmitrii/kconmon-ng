import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { TimeMachineProvider } from "@/lib/timemachine";
import { AlertingPage } from "./alerting";
import { DiagnosticsPage } from "./diagnostics";
import { MTRPage } from "./mtr";
import { PairCardPage } from "./pair-card";
import { RunDetailPage } from "./run-detail";
import { SettingsPage } from "./settings";
import { TargetsPage } from "./targets";

/**
 * Plan Decision 8, one case per mutating surface, each asserted in BOTH
 * directions: engaged the control is present and disabled, live it is present
 * and enabled.
 *
 * Both halves matter. "Disabled while engaged" alone would pass just as well
 * for a control that is disabled always, and the rule this task implements is
 * not "hide the write affordances" — permissions hide, time disables
 * (lib/timemachine.tsx). Every assertion below therefore checks the button is
 * IN the document as well as what its disabled state is.
 */

const AT = "2026-08-01T12:00:00Z";

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const configBody = {
  auth: { mode: "anonymous", role: "admin", loginPath: "" },
  anonymousBanner: true,
  controller: { configured: true },
  prometheus: { configured: true },
  database: { configured: true },
};

/* M7 Task 12b joined Settings and Alerting to this matrix, so the subject has
   to hold their write permissions too — the rule under test is "permissions
   HIDE, time DISABLES", and a control the subject cannot hold would be absent
   for the wrong reason. */
const ADMIN = [
  "runs:create",
  "targets:read",
  "targets:write",
  "checks:read",
  "checks:write",
  "schedules:write",
  "mtr:read",
  "webhooks:manage",
  "settings:write",
  "alerts:read",
  "alerts:manage",
];

const meBody = {
  subject: { kind: "user", id: "u", displayName: "u", groups: [], roles: ["admin"] },
  permissions: ADMIN,
};

const targetRow = { id: "t-1", name: "edge-gw", kind: "host", address: "10.0.0.1", labels: {} };

const definitionRow = {
  id: "d-1",
  name: "edge-tcp",
  checkType: "tcp",
  sourceSelection: "all",
  destinationKind: "target",
  destinationTargetId: "t-1",
  destinationAddress: "",
  params: {},
  enabled: true,
  createdAt: AT,
  updatedAt: AT,
};

const scheduleRow = {
  id: "s-1",
  definitionId: "d-1",
  kind: "interval",
  intervalNs: 60_000_000_000,
  runAt: null,
  enabled: true,
  nextFireAt: null,
  lastFiredAt: null,
  createdAt: AT,
  updatedAt: AT,
};

const webhookRow = {
  id: "w-1",
  name: "pagerduty",
  url: "https://hooks.example.test/pd",
  events: ["incident.created"],
  enabled: true,
  hasSecret: true,
  lastStatus: "",
  failures: 0,
  createdAt: AT,
};

const alertRuleRow = {
  id: "11111111-1111-1111-1111-111111111111",
  name: "PairLossHigh",
  kind: "pair-loss",
  params: { protocol: "udp", thresholdPercent: 5 },
  severity: "warning",
  forNs: 300_000_000_000,
  labels: {},
  annotations: {},
  enabled: true,
  renderedExpr: "kconmon_ng_udp_packet_loss_ratio * 100 > 5",
  syncStatus: "synced",
  syncMessage: "",
  lastSyncedAt: AT,
  createdAt: AT,
  updatedAt: AT,
};

const foreignRuleRow = { name: "kube-prometheus-rules", groups: 2, rules: 7, managedBy: "prometheus-operator" };

const runningRun = {
  id: "run-1",
  type: "tcp",
  status: "running",
  createdAt: AT,
  startedAt: AT,
  finishedAt: null,
  pairTotal: 1,
  pairDone: 0,
  results: [],
};

function stubFetch() {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody));
      if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody));
      if (href.startsWith("/api/v1/topology")) {
        return Promise.resolve(json({
          nodes: [
            { name: "node-a", zone: "z", ready: true },
            { name: "node-b", zone: "z", ready: true },
          ],
          agents: [],
          timestamp: AT,
        }));
      }
      if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events: [], nextCursor: "" }));
      if (href.startsWith("/api/v1/targets")) return Promise.resolve(json({ targets: [targetRow], nextCursor: "" }));
      if (href.startsWith("/api/v1/checks")) return Promise.resolve(json({ definitions: [definitionRow], nextCursor: "" }));
      if (href.startsWith("/api/v1/schedules")) return Promise.resolve(json({ schedules: [scheduleRow], nextCursor: "" }));
      if (href.startsWith("/api/v1/webhooks")) return Promise.resolve(json({ webhooks: [webhookRow] }));
      // Foreign BEFORE the collection: "/api/v1/alert-rules/foreign" also
      // starts with "/api/v1/alert-rules".
      if (href.startsWith("/api/v1/alert-rules/foreign")) return Promise.resolve(json({ foreign: [foreignRuleRow] }));
      if (href.startsWith("/api/v1/alert-rules")) return Promise.resolve(json({ rules: [alertRuleRow] }));
      if (href.startsWith("/api/v1/mtr/destinations")) return Promise.resolve(json({ destinations: [] }));
      if (href.startsWith("/api/v1/mtr/snapshots")) return Promise.resolve(json({ snapshots: [], nextCursor: "" }));
      if (href === "/api/v1/runs/run-1") return Promise.resolve(json(runningRun));
      if (href.startsWith("/api/v1/runs")) return Promise.resolve(json({ runs: [], nextCursor: "" }));
      if (href.includes("/api/v1/promql")) {
        return Promise.resolve(json({ status: "success", data: { resultType: "vector", result: [] } }));
      }
      return Promise.resolve(json({}));
    }),
  );
}

function renderPage(node: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>{node}</TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

/** at(path) engages the Time Machine; live(path) does not. Both go through the
 *  URL, which is the only way the app itself sets the mode. */
const engaged = (path: string) => window.history.pushState({}, "", `${path}?at=${AT}`);
const live = (path: string) => window.history.pushState({}, "", path);

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
  stubFetch();
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

describe("Diagnostics run form", () => {
  it("disables Start run and Save as definition while engaged", async () => {
    engaged("/diagnostics");
    renderPage(<DiagnosticsPage />);
    expect(await screen.findByRole("button", { name: "Start run" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save as definition" })).toBeDisabled();
  });

  it("leaves both enabled while live", async () => {
    live("/diagnostics");
    renderPage(<DiagnosticsPage />);
    expect(await screen.findByRole("button", { name: "Start run" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Save as definition" })).toBeEnabled();
  });
});

describe("Run detail cancel", () => {
  it("disables Cancel run while engaged, without removing it", async () => {
    engaged("/diagnostics/runs/run-1");
    renderPage(<RunDetailPage />);
    const cancel = await screen.findByRole("button", { name: "Cancel run" });
    expect(cancel).toBeInTheDocument();
    expect(cancel).toBeDisabled();
  });

  it("leaves Cancel run enabled while live", async () => {
    live("/diagnostics/runs/run-1");
    renderPage(<RunDetailPage />);
    expect(await screen.findByRole("button", { name: "Cancel run" })).toBeEnabled();
  });
});

describe("MTR runner", () => {
  it("disables Start MTR while engaged", async () => {
    engaged("/mtr");
    renderPage(<MTRPage />);
    (await screen.findByRole("radio", { name: "Runner" })).click();
    expect(await screen.findByRole("button", { name: "Start MTR" })).toBeDisabled();
  });

  it("leaves Start MTR enabled while live", async () => {
    live("/mtr");
    renderPage(<MTRPage />);
    (await screen.findByRole("radio", { name: "Runner" })).click();
    expect(await screen.findByRole("button", { name: "Start MTR" })).toBeEnabled();
  });
});

describe("Pair card run check", () => {
  it("disables Run check while engaged", async () => {
    engaged("/pairs/node-a/node-b");
    renderPage(<PairCardPage />);
    (await screen.findByRole("radio", { name: "Diagnostics" })).click();
    expect(await screen.findByRole("button", { name: "Run check" })).toBeDisabled();
  });

  it("leaves Run check enabled while live", async () => {
    live("/pairs/node-a/node-b");
    renderPage(<PairCardPage />);
    (await screen.findByRole("radio", { name: "Diagnostics" })).click();
    expect(await screen.findByRole("button", { name: "Run check" })).toBeEnabled();
  });
});

describe("Targets, definitions and schedules CRUD", () => {
  it("disables create, edit and delete on targets while engaged", async () => {
    engaged("/targets");
    renderPage(<TargetsPage />);
    expect(await screen.findByRole("button", { name: "Edit edge-gw" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "New target" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Delete edge-gw" })).toBeDisabled();
  });

  it("leaves them enabled while live", async () => {
    live("/targets");
    renderPage(<TargetsPage />);
    expect(await screen.findByRole("button", { name: "Edit edge-gw" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "New target" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Delete edge-gw" })).toBeEnabled();
  });

  it("disables the definitions tab's writes while engaged", async () => {
    engaged("/targets");
    renderPage(<TargetsPage />);
    (await screen.findByRole("radio", { name: "Definitions" })).click();
    expect(await screen.findByRole("button", { name: "Edit edge-tcp" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "New definition" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Delete edge-tcp" })).toBeDisabled();
  });

  it("disables the schedules tab's create, toggle and delete while engaged", async () => {
    engaged("/targets");
    renderPage(<TargetsPage />);
    (await screen.findByRole("radio", { name: "Schedules" })).click();
    expect(await screen.findByRole("button", { name: "Disable edge-tcp" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "New schedule" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Delete edge-tcp" })).toBeDisabled();
  });

  it("leaves the schedules tab's controls enabled while live", async () => {
    live("/targets");
    renderPage(<TargetsPage />);
    (await screen.findByRole("radio", { name: "Schedules" })).click();
    expect(await screen.findByRole("button", { name: "Disable edge-tcp" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "New schedule" })).toBeEnabled();
  });
});

/* M7 Task 12b (plan Decision 12, checklist item 8) added the two surfaces this
   matrix was missing. Both pages already implemented the rule; what was absent
   was the SHARED pin — the file that fails when a new mutating page forgets
   it, rather than each page's own suite deciding for itself. */

describe("Settings webhooks and configuration import", () => {
  it("disables webhook create, edit, delete and test while engaged", async () => {
    engaged("/settings");
    renderPage(<SettingsPage />);
    expect(await screen.findByRole("button", { name: "Edit pagerduty" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "New endpoint" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Delete pagerduty" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Send test to pagerduty" })).toBeDisabled();
  });

  it("leaves them enabled while live", async () => {
    live("/settings");
    renderPage(<SettingsPage />);
    expect(await screen.findByRole("button", { name: "Edit pagerduty" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "New endpoint" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Delete pagerduty" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Send test to pagerduty" })).toBeEnabled();
  });

  /* Export is a READ and stays live from a historical view — pages/settings.tsx
     says so in as many words. Pinning it here keeps a later "disable
     everything while engaged" sweep from quietly taking it out. */
  it("leaves Export configuration alone in both modes", async () => {
    engaged("/settings");
    renderPage(<SettingsPage />);
    expect(await screen.findByRole("button", { name: "Export configuration" })).toBeEnabled();
  });
});

describe("Alerting rules", () => {
  it("disables create, edit, delete, sync, the enable toggle and import while engaged", async () => {
    engaged("/alerting");
    renderPage(<AlertingPage />);
    expect(await screen.findByRole("button", { name: "Edit PairLossHigh" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "New rule" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Delete PairLossHigh" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Sync PairLossHigh now" })).toBeDisabled();
    expect(screen.getByRole("checkbox", { name: "Enabled PairLossHigh" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Import kube-prometheus-rules" })).toBeDisabled();
  });

  it("leaves them enabled while live", async () => {
    live("/alerting");
    renderPage(<AlertingPage />);
    expect(await screen.findByRole("button", { name: "Edit PairLossHigh" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "New rule" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Delete PairLossHigh" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Sync PairLossHigh now" })).toBeEnabled();
    expect(screen.getByRole("checkbox", { name: "Enabled PairLossHigh" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Import kube-prometheus-rules" })).toBeEnabled();
  });

  /* Details is a read of a row already on screen, and Cancel dismisses a form
     — neither writes, so neither is time's business. */
  it("leaves the read-only row controls alone while engaged", async () => {
    engaged("/alerting");
    renderPage(<AlertingPage />);
    expect(await screen.findByRole("button", { name: "Details for PairLossHigh" })).toBeEnabled();
  });
});
