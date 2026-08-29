import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { alertingDict } from "@/lib/i18n/dict/alerting";
import { settingsDict } from "@/lib/i18n/dict/settings";
import { targetsDict } from "@/lib/i18n/dict/targets";
import { TimeMachineProvider } from "@/lib/timemachine";
import { AlertingPage } from "@/pages/alerting";
import { SettingsPage } from "@/pages/settings";
import { TargetsPage } from "@/pages/targets";

/**
 * The three MANAGE pages in Russian — /targets, /alerting, /settings.
 *
 * Modelled on lib/i18n/chrome.test.tsx, and here rather than inside each page's
 * own test file for the reason that file gives: those files render their page
 * with NO LocaleProvider and pass, which is the property being pinned (no
 * provider ⇒ English, and ~1600 assertions depend on it). These mount the same
 * pages WITH the provider and a stored choice, so a t() that quietly reworded
 * a string — or a Russian half that never got wired — is caught here instead of
 * by an operator.
 *
 * The assertions read the DICTIONARY rather than a literal, deliberately: a
 * test that repeated «Цели и расписания» by hand would pass just as happily
 * against a page that hard-codes it, which is the one thing this is checking
 * did not happen.
 */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const ALL = [
  "targets:read",
  "targets:write",
  "checks:read",
  "checks:write",
  "schedules:write",
  "alerts:read",
  "alerts:manage",
  "tokens:manage",
  "webhooks:manage",
  "settings:write",
  "maintenance:write",
];

/** One router for all three pages: each answers the empty, settled shape, so
 *  what renders is the chrome and the EMPTY STATES — which is exactly the copy
 *  most likely to be left untranslated. */
function stubFetch() {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const href = typeof input === "string" ? input : String((input as Request).url ?? input);
      if (href.includes("/api/v1/auth/me")) {
        return Promise.resolve(
          json({
            subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["admin"] },
            permissions: ALL,
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
      if (href.includes("/api/v1/alert-rules/foreign")) return Promise.resolve(json({ foreign: [] }));
      if (href.includes("/api/v1/alert-rules")) return Promise.resolve(json({ rules: [] }));
      if (href.includes("/api/v1/targets")) return Promise.resolve(json({ targets: [] }));
      if (href.includes("/api/v1/checks")) return Promise.resolve(json({ definitions: [] }));
      if (href.includes("/api/v1/schedules")) return Promise.resolve(json({ schedules: [] }));
      if (href.includes("/api/v1/tokens")) return Promise.resolve(json({ tokens: [] }));
      if (href.includes("/api/v1/webhooks")) return Promise.resolve(json({ webhooks: [] }));
      if (href.includes("/api/v1/maintenance")) return Promise.resolve(json({ windows: [] }));
      return Promise.resolve(json({}));
    }),
  );
}

function renderRu(node: React.ReactNode) {
  stubFetch();
  localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <LocaleProvider>
        <TimeMachineProvider>{node}</TimeMachineProvider>
      </LocaleProvider>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  // vitest.setup.ts backs localStorage with one Map per FILE — a locale left
  // behind would leak into every later test here.
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

describe("/targets in Russian", () => {
  it("titles the page with the same words the sidebar's nav.targets uses", async () => {
    renderRu(<TargetsPage />);
    expect(await screen.findByRole("heading", { name: targetsDict.ru["title"] })).toBeInTheDocument();
    expect(screen.getByText(targetsDict.ru["description"])).toBeInTheDocument();
  });

  it("translates the tab strip and its accessible name", async () => {
    renderRu(<TargetsPage />);
    const tabs = await screen.findByRole("radiogroup", { name: targetsDict.ru["tabs.aria"] });
    expect(tabs).toBeInTheDocument();
    for (const key of ["tab.targets", "tab.definitions", "tab.schedules"] as const) {
      expect(screen.getByRole("radio", { name: targetsDict.ru[key] })).toBeInTheDocument();
    }
  });

  it("translates the section heading, the list's accessible name and the empty state", async () => {
    renderRu(<TargetsPage />);
    expect(await screen.findByRole("heading", { name: targetsDict.ru["targets.heading"] })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(targetsDict.ru["targets.empty"])).toBeInTheDocument());
    // The teaching empty state's CTA is its own node, beside the body.
    expect(screen.getByText(targetsDict.ru["targets.empty.cta"])).toBeInTheDocument();
  });

  it("translates the create button", async () => {
    renderRu(<TargetsPage />);
    expect(await screen.findByRole("button", { name: targetsDict.ru["targets.new"] })).toBeInTheDocument();
  });
});

describe("/alerting in Russian", () => {
  it("titles the page with the same word the sidebar's nav.alerting uses", async () => {
    renderRu(<AlertingPage />);
    expect(await screen.findByRole("heading", { name: alertingDict.ru["title"] })).toBeInTheDocument();
  });

  it("translates the section headings and empty states — the maintenance list included (M3-14)", async () => {
    renderRu(<AlertingPage />);
    expect(await screen.findByRole("heading", { name: alertingDict.ru["rules.heading"] })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: alertingDict.ru["foreign.heading"] })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: alertingDict.ru["maintenance.heading"] })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(alertingDict.ru["rules.empty"])).toBeInTheDocument());
    expect(screen.getByText(alertingDict.ru["foreign.empty"])).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(alertingDict.ru["maintenance.empty"])).toBeInTheDocument());
  });

  it("renders the maintenance blurb's links in Russian words, both anchors intact", async () => {
    renderRu(<AlertingPage />);
    const investigate = await screen.findByRole("link", { name: alertingDict.ru["link.investigate"] });
    expect(investigate).toHaveAttribute("href", "/investigate");
    expect(screen.getByRole("link", { name: alertingDict.ru["link.explore"] })).toHaveAttribute("href", "/explore");
  });

  it("translates the builder's own labels once it is open", async () => {
    renderRu(<AlertingPage />);
    fireEvent.click(await screen.findByRole("button", { name: alertingDict.ru["rules.new"] }));
    await waitFor(() => expect(screen.getByLabelText(alertingDict.ru["form.name"])).toBeInTheDocument());
    expect(screen.getByLabelText(alertingDict.ru["form.severity"])).toBeInTheDocument();
    expect(screen.getByLabelText(alertingDict.ru["form.for"])).toBeInTheDocument();
    // The default kind's own param, from KIND_PARAMS' translated labelKey.
    expect(screen.getByLabelText(alertingDict.ru["param.thresholdPercent.loss"])).toBeInTheDocument();
  });

  it("keeps `severity` and `kind` OPTIONS in the wire values a routing tree is written against", async () => {
    renderRu(<AlertingPage />);
    fireEvent.click(await screen.findByRole("button", { name: alertingDict.ru["rules.new"] }));
    await waitFor(() => expect(screen.getByLabelText(alertingDict.ru["form.severity"])).toBeInTheDocument());
    const severity = screen.getByLabelText(alertingDict.ru["form.severity"]) as HTMLSelectElement;
    expect([...severity.options].map((o) => o.value)).toEqual(["info", "warning", "critical"]);
    expect([...severity.options].map((o) => o.textContent)).toEqual(["info", "warning", "critical"]);
    // The kind option keeps its identifier and translates only the blurb.
    const kind = screen.getByLabelText(alertingDict.ru["form.kind"]) as HTMLSelectElement;
    expect(kind.options[0].value).toBe("pair-loss");
    expect(kind.options[0].textContent).toBe(`pair-loss — ${alertingDict.ru["kind.pair-loss"]}`);
  });
});

describe("/settings in Russian", () => {
  it("titles the page with the same word the sidebar's nav.settings uses", async () => {
    renderRu(<SettingsPage />);
    expect(await screen.findByRole("heading", { name: settingsDict.ru["title"] })).toBeInTheDocument();
  });

  it("keeps the language switcher itself translated, beside the rest of the page", async () => {
    renderRu(<SettingsPage />);
    expect(await screen.findByRole("heading", { name: settingsDict.ru["language.title"] })).toBeInTheDocument();
    expect(screen.getByRole("radiogroup", { name: settingsDict.ru["language.aria"] })).toBeInTheDocument();
    // The two language NAMES are endonyms and never translate.
    expect(screen.getByRole("radio", { name: "English" })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Русский" })).toBeInTheDocument();
  });

  /* The maintenance list moved to /alerting (M3-14) — its Russian strings are
     asserted in that page's describe above. */
  it("translates the three gated sections' headings and their empty states", async () => {
    renderRu(<SettingsPage />);
    expect(await screen.findByRole("heading", { name: settingsDict.ru["tokens.heading"] })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: settingsDict.ru["webhooks.heading"] })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: settingsDict.ru["bundle.heading"] })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: settingsDict.ru["about.heading"] })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(settingsDict.ru["webhooks.empty"])).toBeInTheDocument());
    expect(screen.getByText(settingsDict.ru["tokens.empty"])).toBeInTheDocument();
  });

  /* The tokens section arrived last and is the one most likely to ship half
     translated (QA round 6, finding #14). */
  it("translates the token form, including the one-time-secret warning", async () => {
    renderRu(<SettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: settingsDict.ru["tokens.new"] }));
    await waitFor(() => expect(screen.getByLabelText(settingsDict.ru["tokens.form.name"])).toBeInTheDocument());
    expect(screen.getByLabelText(settingsDict.ru["tokens.form.expires"])).toBeInTheDocument();
    expect(screen.getByRole("button", { name: settingsDict.ru["tokens.form.createButton"] })).toBeInTheDocument();
    expect(screen.getByText(settingsDict.ru["tokens.blurb"])).toBeInTheDocument();
  });

  it("renders a sentence with LINKS in it without gluing fragments — all three anchors, Russian words", async () => {
    renderRu(<SettingsPage />);
    // Once each, in About's closing paragraph (the maintenance blurb moved to /alerting).
    const investigate = await screen.findAllByRole("link", { name: settingsDict.ru["link.investigate"] });
    const explore = screen.getAllByRole("link", { name: settingsDict.ru["link.explore"] });
    const alerting = screen.getAllByRole("link", { name: settingsDict.ru["link.alerting"] });
    expect(investigate).toHaveLength(1);
    expect(explore).toHaveLength(1);
    expect(alerting).toHaveLength(1);
    expect(investigate[0]).toHaveAttribute("href", "/investigate");
    expect(explore[0]).toHaveAttribute("href", "/explore");
    expect(alerting[0]).toHaveAttribute("href", "/alerting");
  });

  it("keeps webhook EVENT ids in the wire values the payload carries", async () => {
    renderRu(<SettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: settingsDict.ru["webhooks.new"] }));
    await waitFor(() => expect(screen.getByText("incident.created")).toBeInTheDocument());
    expect(screen.getByText("alert.fired")).toBeInTheDocument();
    // …while the field around them is translated.
    expect(screen.getByLabelText(settingsDict.ru["webhooks.form.secret"])).toBeInTheDocument();
  });
});
