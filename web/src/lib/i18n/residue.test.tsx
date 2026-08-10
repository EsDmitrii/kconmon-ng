import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { InvestigateLink, RelatedIncidents } from "@/components/investigate-entry";
import { RecentChanges } from "@/components/recent-changes";
import { StubPage } from "@/components/stub-page";
import { UserMenu } from "@/components/user-menu";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { investigateDict } from "@/lib/i18n/dict/investigate";
import { investigateEntryDict } from "@/lib/i18n/dict/investigate-entry";
import { loginDict } from "@/lib/i18n/dict/login";
import { overviewDict } from "@/lib/i18n/dict/overview";
import { recentChangesDict } from "@/lib/i18n/dict/recent-changes";
import { stubPageDict } from "@/lib/i18n/dict/stub-page";
import { userMenuDict } from "@/lib/i18n/dict/user-menu";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { InvestigationScope } from "@/lib/investigation-sources";
import type { Me } from "@/lib/types";
import { LoginPage } from "@/pages/login";

/**
 * THE RESIDUE, closed — the five surfaces lib/i18n/README.md's "Known gaps"
 * list still named after the wave: the login page, the sidebar's user menu, the
 * Recent-changes rail, the Open-incidents rail and the stub page.
 *
 * One file rather than five, because what they have in common is the only thing
 * that is not obvious about them: NONE is a page with a dictionary of its own
 * making. Two are shared rails mounted by three cards each, one is chrome the
 * sidebar owns the frame of, one is a fallback for a route that does not exist
 * yet, and one is the page an operator sees BEFORE the console knows who they
 * are. Each borrows words from a table someone else wrote, and the last
 * describe() here is the check that they borrowed rather than re-invented.
 *
 * Same division of labour as lib/i18n/cards.test.tsx and manage.test.tsx: the
 * components' own test files (pages/login.test.tsx, components/user-menu.test.tsx,
 * components/recent-changes.test.tsx, the three card tests) render with NO
 * LocaleProvider and read English, which is the property THEY pin. These mount
 * the provider and assert both halves.
 */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function configBody(opts: { authMode?: string; database?: boolean } = {}) {
  const { authMode = "local", database = true } = opts;
  return {
    auth: {
      mode: authMode,
      role: "",
      loginPath: authMode === "local" ? "/api/v1/auth/login" : authMode === "oidc" ? "/api/v1/auth/oidc/start" : "",
    },
    anonymousBanner: authMode === "anonymous",
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: database },
  };
}

function renderRu(node: React.ReactNode, seed?: (qc: QueryClient) => void) {
  localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
  return renderWithProvider(node, seed);
}

function renderWithProvider(node: React.ReactNode, seed?: (qc: QueryClient) => void) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  seed?.(qc);
  return {
    qc,
    ...render(
      <QueryClientProvider client={qc}>
        <LocaleProvider>{node}</LocaleProvider>
      </QueryClientProvider>,
    ),
  };
}

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
});

/* The README's cleanup rule: vitest.setup.ts backs localStorage with ONE Map
   per test file, so a locale left behind leaks into every later test here. */
afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
  localStorage.removeItem(LOCALE_STORAGE_KEY);
  window.history.pushState({}, "", "/");
});

/* ── /login ──────────────────────────────────────────────────────────────── */

describe("the login page in Russian", () => {
  it("splits the one English «Sign in» into a heading noun and a button verb", () => {
    renderRu(<LoginPage />, (qc) => qc.setQueryData(["config"], configBody()));
    expect(screen.getByRole("heading", { name: loginDict.ru["title"] })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: loginDict.ru["submit"] })).toBeInTheDocument();
    // …and they are genuinely two words, which is the whole reason for two keys.
    expect(loginDict.ru["title"]).not.toBe(loginDict.ru["submit"]);
    expect(loginDict.en["title"]).toBe(loginDict.en["submit"]);
  });

  it("names both form fields, so the labels a password manager reads are Russian too", () => {
    renderRu(<LoginPage />, (qc) => qc.setQueryData(["config"], configBody()));
    expect(screen.getByLabelText(loginDict.ru["field.username"])).toBeInTheDocument();
    expect(screen.getByLabelText(loginDict.ru["field.password"])).toBeInTheDocument();
  });

  it("translates the SSO card, keeping SSO itself and the start URL untouched", () => {
    renderRu(<LoginPage />, (qc) => qc.setQueryData(["config"], configBody({ authMode: "oidc" })));
    expect(screen.getByText(loginDict.ru["oidc.lead"])).toBeInTheDocument();
    const link = screen.getByRole("link", { name: loginDict.ru["oidc.action"] });
    expect(link).toHaveAttribute("href", "/api/v1/auth/oidc/start?returnTo=%2F");
    expect(loginDict.ru["oidc.action"]).toContain("SSO");
  });

  it("shows the SERVER's refusal verbatim, in Russian chrome — it is data, not chrome", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ type: "about:blank", title: "invalid credentials", status: 401 }), {
          status: 401,
          headers: { "Content-Type": "application/problem+json" },
        }),
      ),
    );
    renderRu(<LoginPage />, (qc) => qc.setQueryData(["config"], configBody()));
    fireEvent.change(screen.getByLabelText(loginDict.ru["field.username"]), { target: { value: "ada" } });
    fireEvent.change(screen.getByLabelText(loginDict.ru["field.password"]), { target: { value: "wrong" } });
    fireEvent.click(screen.getByRole("button", { name: loginDict.ru["submit"] }));

    expect(await screen.findByRole("alert")).toHaveTextContent("invalid credentials");
  });

  it("keeps the English page byte-for-byte with the provider mounted", () => {
    renderWithProvider(<LoginPage />, (qc) => qc.setQueryData(["config"], configBody({ authMode: "oidc" })));
    expect(screen.getByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(screen.getByText("Authenticate through your identity provider.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Sign in with SSO" })).toBeInTheDocument();
  });
});

/* ── the user menu ───────────────────────────────────────────────────────── */

const ada: Me = {
  subject: { kind: "user", id: "u1", displayName: "Ada Lovelace", groups: [], roles: ["viewer", "operator"] },
  permissions: ["tokens:manage"],
};

const roleless: Me = {
  subject: { kind: "user", id: "u2", displayName: "Grace", groups: [], roles: [] },
  permissions: [],
};

function openMenu(me: Me, ru: boolean) {
  const can = (p: string) => me.permissions.includes(p);
  const utils = (ru ? renderRu : renderWithProvider)(<UserMenu me={me} can={can} />);
  fireEvent.click(screen.getByRole("button", { name: new RegExp(me.subject.displayName, "i") }));
  return utils;
}

describe("the user menu in Russian", () => {
  it("translates both menu items and leaves the ROLE names as the server resolved them", () => {
    openMenu(ada, true);
    expect(screen.getByRole("menuitem", { name: userMenuDict.ru["tokens"] })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: userMenuDict.ru["signOut"] })).toBeInTheDocument();
    expect(screen.getByText("viewer, operator")).toBeInTheDocument();
  });

  it("says «ролей не назначено» where there are none, in lower case and with no full stop", () => {
    openMenu(roleless, true);
    expect(screen.getByText(userMenuDict.ru["roles.none"])).toBeInTheDocument();
    expect(userMenuDict.ru["roles.none"]).toBe(userMenuDict.ru["roles.none"].toLocaleLowerCase("ru"));
    expect(userMenuDict.ru["roles.none"].endsWith(".")).toBe(false);
  });

  it("swaps the sign-out verb for its in-flight form while the request is out", async () => {
    let release = () => {};
    const held = new Promise<Response>((resolve) => {
      release = () => resolve(new Response(null, { status: 204 }));
    });
    vi.stubGlobal("fetch", vi.fn().mockReturnValue(held));
    openMenu(ada, true);
    fireEvent.click(screen.getByRole("menuitem", { name: userMenuDict.ru["signOut"] }));

    expect(await screen.findByRole("menuitem", { name: userMenuDict.ru["signOut.pending"] })).toBeInTheDocument();
    release();
  });

  it("keeps the English menu byte-for-byte with the provider mounted", () => {
    openMenu(ada, false);
    expect(screen.getByRole("menuitem", { name: "Token management" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Sign out" })).toBeInTheDocument();
  });
});

/* ── the Recent changes rail ─────────────────────────────────────────────── */

function stubRail(opts: { database?: boolean; events?: unknown[]; hangEvents?: boolean } = {}) {
  const { database = true, events = [], hangEvents = false } = opts;
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody({ database })));
      if (href.startsWith("/api/v1/events")) {
        return hangEvents ? new Promise<Response>(() => {}) : Promise.resolve(json({ events, nextCursor: "" }));
      }
      return Promise.resolve(json({}));
    }),
  );
}

describe("the Recent changes rail in Russian", () => {
  it("names the landmark and the heading with the same words", async () => {
    stubRail();
    renderRu(<RecentChanges scope="node-a" />);
    const rail = await screen.findByRole("complementary", { name: recentChangesDict.ru["aria"] });
    expect(within(rail).getByRole("heading", { name: recentChangesDict.ru["title"] })).toBeInTheDocument();
  });

  it("says the honest thing about a console with no database, without calling it broken", async () => {
    stubRail({ database: false });
    renderRu(<RecentChanges scope="node-a" />);
    expect(await screen.findByText(recentChangesDict.ru["db.note"])).toBeInTheDocument();
  });

  it("translates the empty state", async () => {
    stubRail();
    renderRu(<RecentChanges scope="node-a" />);
    expect(await screen.findByText(recentChangesDict.ru["empty"])).toBeInTheDocument();
  });

  it("translates the loading line a screen reader is the only one to hear", async () => {
    stubRail({ hangEvents: true });
    renderRu(<RecentChanges scope="node-a" />);
    expect(await screen.findByText(recentChangesDict.ru["loading"])).toBeInTheDocument();
  });

  /* The stamp is INTERPOLATED, never translated — but it lands inside a Russian
     sentence, so it takes that sentence's tag rather than the runtime default.
     This case used to pin the bare toLocaleString(), which is the policy
     lib/i18n/index.tsx documents for a stamp standing ON ITS OWN and not for
     one wedged into a translated line (QA scope 2, finding #8). */
  it("interpolates the Time Machine's instant, in the sentence's own language", async () => {
    const at = "2026-08-01T12:00:00Z";
    const stamp = new Date(at).toLocaleString("ru-RU");
    window.history.pushState({}, "", `/nodes/node-a?at=${at}`);
    stubRail();
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <LocaleProvider>
          <TimeMachineProvider>
            <RecentChanges scope="node-a" />
          </TimeMachineProvider>
        </LocaleProvider>
      </QueryClientProvider>,
    );
    expect(await screen.findByText(`${recentChangesDict.ru["upTo"].replace("{at}", stamp)}`)).toBeInTheDocument();
    // Still the runtime's own formatting of a real Date — nothing about the
    // stamp is a dictionary string.
    expect(screen.getByText(new RegExp(stamp.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")))).toBeInTheDocument();
  });

  it("keeps the English rail byte-for-byte with the provider mounted", async () => {
    stubRail({ database: false });
    renderWithProvider(<RecentChanges scope="node-a" />);
    await screen.findByRole("complementary", { name: "Recent changes" });
    expect(
      await screen.findByText("History requires a database — showing live events only."),
    ).toBeInTheDocument();
    expect(screen.getByText("No recent changes.")).toBeInTheDocument();
  });
});

/* ── the Open incidents rail + the way in ────────────────────────────────── */

const NODE_SCOPE: InvestigationScope = { kind: "node", a: "node-a", b: "" };

function stubIncidents(opts: { permissions?: string[]; database?: boolean; incidents?: unknown[] } = {}) {
  const { permissions = ["incidents:read"], database = true, incidents = [] } = opts;
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/auth/me")) {
        return Promise.resolve(
          json({ subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["operator"] }, permissions }),
        );
      }
      if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody({ database })));
      if (href.startsWith("/api/v1/incidents")) return Promise.resolve(json({ incidents, nextCursor: "" }));
      return Promise.resolve(json({}));
    }),
  );
}

describe("the Open incidents rail in Russian", () => {
  it("names the landmark and the heading, and calls the way in what the matrix calls it", async () => {
    stubIncidents();
    renderRu(
      <>
        <InvestigateLink scope={NODE_SCOPE} />
        <RelatedIncidents scope={NODE_SCOPE} />
      </>,
    );
    const rail = await screen.findByRole("complementary", { name: investigateEntryDict.ru["aria"] });
    expect(within(rail).getByRole("heading", { name: investigateEntryDict.ru["title"] })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: investigateEntryDict.ru["investigate"] })).toBeInTheDocument();
  });

  it("names the missing PERMISSION, and never asks", async () => {
    stubIncidents({ permissions: [] });
    renderRu(<RelatedIncidents scope={NODE_SCOPE} />);
    expect(await screen.findByText(investigateEntryDict.ru["denied"])).toBeInTheDocument();
    expect(investigateEntryDict.ru["denied"]).toContain("incidents:read");
    expect(vi.mocked(fetch).mock.calls.some((c) => String(c[0]).startsWith("/api/v1/incidents"))).toBe(false);
  });

  it("names the missing CONFIG KEY, in the config key's own bytes", async () => {
    stubIncidents({ database: false });
    renderRu(<RelatedIncidents scope={NODE_SCOPE} />);
    expect(await screen.findByText(investigateEntryDict.ru["noDatabase"])).toBeInTheDocument();
    expect(investigateEntryDict.ru["noDatabase"]).toContain("console.database.mode");
  });

  it("translates the empty state and the row badge, keeping the incident's own title", async () => {
    stubIncidents({
      incidents: [{ id: "inc-1", title: "packet loss on node-a", scope: "node-a", status: "open" }],
    });
    renderRu(<RelatedIncidents scope={NODE_SCOPE} />);
    expect(await screen.findByRole("link", { name: "packet loss on node-a" })).toBeInTheDocument();
    expect(screen.getByText(investigateEntryDict.ru["open"])).toBeInTheDocument();
  });

  it("keeps the English rail byte-for-byte with the provider mounted", async () => {
    stubIncidents({ permissions: [] });
    renderWithProvider(<RelatedIncidents scope={NODE_SCOPE} />);
    await screen.findByRole("complementary", { name: "Open incidents" });
    expect(await screen.findByText("Incidents need incidents:read — none was requested.")).toBeInTheDocument();
  });
});

/* ── the stub page ───────────────────────────────────────────────────────── */

describe("the stub page in Russian", () => {
  it("states the roadmap fact in both sentences, and leaves the NAV's own words to the nav", () => {
    renderRu(<StubPage title="Reports" description="Nothing here yet." />);
    expect(screen.getByText(stubPageDict.ru["title"])).toBeInTheDocument();
    expect(screen.getByText(stubPageDict.ru["body"])).toBeInTheDocument();
    // title/description are props from nav.ts, translated where the nav is.
    expect(screen.getByRole("heading", { name: "Reports" })).toBeInTheDocument();
  });

  it("keeps the English slate byte-for-byte with the provider mounted", () => {
    renderWithProvider(<StubPage title="Reports" description="Nothing here yet." />);
    expect(screen.getByText("Not built yet — on the roadmap")).toBeInTheDocument();
    expect(
      screen.getByText(
        "This view is delivered in a later milestone. The navigation shows the full product so " +
          "the information architecture stays honest about what is coming.",
      ),
    ).toBeInTheDocument();
  });
});

/* ── the borrowed vocabulary ─────────────────────────────────────────────── */

describe("the words these five borrowed", () => {
  it("calls an open incident what /overview calls it, on the card as on the dashboard", () => {
    expect(investigateEntryDict.ru["title"]).toBe(overviewDict.ru["incidents.title"]);
  });

  it("badges a row with the ONE word /investigate's own incident header uses", () => {
    expect(investigateEntryDict.ru["open"]).toBe(investigateDict.ru["incident.open"]);
  });

  it("labels the way in with the verb the investigate form's submit button uses", () => {
    expect(investigateEntryDict.ru["investigate"]).toBe(investigateDict.ru["form.submit"]);
  });

  it("opens both database-gate sentences with the same four words /overview opens its own with", () => {
    const lead = "Истории нужна база";
    expect(overviewDict.ru["db.note"].startsWith(lead)).toBe(true);
    expect(recentChangesDict.ru["db.note"].startsWith(lead)).toBe(true);
  });

  it("ends both permission refusals with the console's ONE «no request was made» clause", () => {
    const clause = "запрос не отправлялся.";
    expect(overviewDict.ru["incidents.denied"].endsWith(clause)).toBe(true);
    expect(investigateEntryDict.ru["denied"].endsWith(clause)).toBe(true);
    expect(investigateEntryDict.ru["noDatabase"].endsWith("Запрос не отправлялся.")).toBe(true);
  });

  it("keeps every English half byte-for-byte identical to what the five files rendered before", () => {
    expect(loginDict.en["oidc.lead"]).toBe("Authenticate through your identity provider.");
    expect(userMenuDict.en["roles.none"]).toBe("no roles bound");
    expect(recentChangesDict.en["db.note"]).toBe("History requires a database — showing live events only.");
    expect(investigateEntryDict.en["noDatabase"]).toBe(
      "Incidents are stored — set console.database.mode. Nothing was requested.",
    );
    expect(stubPageDict.en["title"]).toBe("Not built yet — on the roadmap");
  });
});

/* Kept honest: the rail's socket half is not exercised here — that is
   components/recent-changes.test.tsx's job, and the language switch does not
   touch it. This file only asserts what the five surfaces SAY. */
