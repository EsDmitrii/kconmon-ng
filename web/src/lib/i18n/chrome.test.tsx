import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { TimeMachineBar, TIME_MACHINE_TRIGGER_SELECTOR } from "@/components/timemachine-bar";
import { LOCALE_STORAGE_KEY, LocaleProvider, useLocale } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import { NAV_ITEMS } from "@/nav";
import { AppShell } from "@/routes";

/**
 * The chrome in both languages — the reference the page agents copy.
 *
 * The English cases here are NOT duplicates of routes.test.tsx or
 * anonymous-banner.test.tsx: those render the shell with no LocaleProvider at
 * all and pass, which is the property being pinned (no provider ⇒ English).
 * These render the same shell WITH the provider and assert the strings did not
 * move — a t() call that silently reworded a string would be caught here.
 */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function stubFetch() {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      if (String(url).includes("/api/v1/auth/me")) {
        return Promise.resolve(
          json({
            subject: { kind: "anonymous", id: "anonymous", displayName: "Anonymous", groups: [], roles: ["viewer"] },
            permissions: [],
          }),
        );
      }
      return Promise.resolve(
        json({
          auth: { mode: "anonymous", role: "viewer", loginPath: "" },
          anonymousBanner: true,
          controller: { configured: true },
          prometheus: { configured: true },
          database: { configured: false },
        }),
      );
    }),
  );
}

/** A switcher standing in for the Settings one, so "applies instantly" can be
 *  observed on the CHROME rather than only on the page that owns the control. */
function LocaleSwitch() {
  const { setLocale } = useLocale();
  return <button onClick={() => setLocale("ru")}>switch to ru</button>;
}

/** The shell, on a memory history, exactly as routes.test.tsx builds it — the
 *  <Link>s in the sidebar need a real RouterProvider and there is no
 *  context-free render mode. */
function renderShell({ locale }: { locale?: "en" | "ru" } = {}) {
  stubFetch();
  if (locale) localStorage.setItem(LOCALE_STORAGE_KEY, locale);

  const testRoot = createRootRoute({
    component: () => (
      <AppShell>
        <Outlet />
      </AppShell>
    ),
  });
  const testRouter = createRouter({
    routeTree: testRoot.addChildren(
      NAV_ITEMS.map((item) =>
        createRoute({ getParentRoute: () => testRoot, path: item.path, component: () => <div>page content</div> }),
      ),
    ),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <LocaleProvider>
          <LocaleSwitch />
          <RouterProvider router={testRouter} />
        </LocaleProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

/* By ROLE alone: the landmark's own name is now translated
   (chrome.ts "shell.nav.aria"), and a helper that hard-coded "Main" would find
   nothing the moment the console switched language — which is exactly the bug
   naming the landmark was meant to fix for a screen-reader user. There is one
   navigation landmark in this shell. */
const nav = () => screen.getByRole("navigation");
const navLink = (name: string) => within(nav()).getByRole("link", { name });

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  // One Map backs localStorage for this whole FILE (vitest.setup.ts): a locale
  // left behind would silently translate every case after it.
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

/* ── sidebar ────────────────────────────────────────────────────────────── */

describe("sidebar", () => {
  it("names every nav entry in English by default", async () => {
    renderShell();
    await screen.findByRole("navigation");
    for (const name of [
      "Overview",
      "Live",
      "Investigate",
      "Matrix",
      "Topology",
      "MTR",
      "Diagnostics",
      "Targets & Schedules",
      "Explore",
      "Alerting",
      "Console",
      "Settings",
    ]) {
      expect(navLink(name)).toBeInTheDocument();
    }
  });

  it("names every nav entry in Russian when Russian was chosen", async () => {
    renderShell({ locale: "ru" });
    await screen.findByRole("navigation");
    for (const name of [
      "Обзор",
      "Онлайн",
      "Расследование",
      "Матрица",
      "Топология",
      "Диагностика",
      "Цели и расписания",
      "Метрики",
      "Оповещения",
      "Консоль",
      "Настройки",
    ]) {
      expect(navLink(name)).toBeInTheDocument();
    }
    // MTR is a tool name, not prose: it reads the same in both languages.
    expect(navLink("MTR")).toBeInTheDocument();
  });

  it("translates the group headers", async () => {
    renderShell({ locale: "ru" });
    const sidebar = await screen.findByRole("navigation");
    expect(within(sidebar).getByText("Мониторинг")).toBeInTheDocument();
    expect(within(sidebar).getByText("Управление")).toBeInTheDocument();
    // "Расследование" is both a group header and a nav link, deliberately —
    // the English chrome says "Investigate" twice in the same two places.
    expect(within(sidebar).getAllByText("Расследование")).toHaveLength(2);
  });

  it("translates the footer line", async () => {
    renderShell({ locale: "ru" });
    expect(await screen.findByText("Консоль сетевой связности")).toBeInTheDocument();
    expect(screen.queryByText("Network connectivity console")).not.toBeInTheDocument();
  });

  it("switches language in place, with no reload and no refetch", async () => {
    const { container } = renderShell();
    await screen.findByRole("navigation");
    const fetchCalls = vi.mocked(fetch).mock.calls.length;

    fireEvent.click(screen.getByRole("button", { name: "switch to ru" }));

    expect(navLink("Обзор")).toBeInTheDocument();
    expect(within(nav()).queryByRole("link", { name: "Overview" })).not.toBeInTheDocument();
    // Same DOM, re-rendered: nothing remounted the app, nothing asked the
    // server for a translation.
    expect(container.querySelector("aside")).toBeInTheDocument();
    expect(vi.mocked(fetch).mock.calls.length).toBe(fetchCalls);
  });

  it("remembers the choice for the next load", async () => {
    renderShell();
    await screen.findByRole("navigation");
    fireEvent.click(screen.getByRole("button", { name: "switch to ru" }));
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBe("ru");

    cleanup();
    renderShell();
    expect(await screen.findByText("Консоль сетевой связности")).toBeInTheDocument();
  });
});

/* ── skip link and anonymous banner ─────────────────────────────────────── */

describe("shell chrome", () => {
  it("translates the skip link", async () => {
    renderShell({ locale: "ru" });
    const skip = await screen.findByRole("link", { name: "Перейти к основному содержимому" });
    expect(skip).toHaveAttribute("href", "#main-content");
  });

  it("translates the anonymous-mode banner without changing what it reports", async () => {
    renderShell({ locale: "ru" });
    const banner = await screen.findByRole("status");
    expect(banner).toHaveTextContent("Анонимный режим.");
    expect(banner).toHaveTextContent("Аутентификация выключена");
    expect(banner).toHaveTextContent("Не используйте в продакшене.");
  });

  /* The role name arrives with GET /api/v1/config, so these await the text
     rather than the banner — the banner itself is up before the config lands. */
  it("keeps the banner's English wording byte-for-byte", async () => {
    renderShell();
    expect(
      await screen.findByText(
        "Authentication is disabled — everyone has the viewer role (console.auth.anonymous.role). Do not use in production.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Anonymous mode.");
  });

  /* The role everyone actually HAS is the point of the warning, and
     GET /api/v1/config carries it (QA round 6, finding #12). */
  it("names the anonymous role in both languages", async () => {
    renderShell({ locale: "ru" });
    expect(
      await screen.findByText(
        "Аутентификация выключена, у всех роль viewer (console.auth.anonymous.role). Не используйте в продакшене.",
      ),
    ).toBeInTheDocument();
  });
});

/* ── Time Machine bar ───────────────────────────────────────────────────── */

describe("Time Machine bar", () => {
  const engagedAt = new Date(2026, 7, 7, 15, 34, 0);

  function renderBar({ locale, at }: { locale?: "en" | "ru"; at?: Date } = {}) {
    if (locale) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
    window.history.pushState({}, "", at ? `/matrix?at=${encodeURIComponent(at.toISOString())}` : "/");
    return render(
      <LocaleProvider>
        <TimeMachineProvider>
          <TimeMachineBar />
        </TimeMachineProvider>
      </LocaleProvider>,
    );
  }

  afterEach(() => window.history.pushState({}, "", "/"));

  it("translates the live trigger, label and accessible name together", () => {
    renderBar({ locale: "ru" });
    const trigger = screen.getByRole("button", { name: "Машина времени: посмотреть консоль на момент в прошлом" });
    expect(trigger).toHaveTextContent("Машина времени");
  });

  it("keeps the palette's seam locale-independent", () => {
    // The palette opens THIS control by selector. It used to match the
    // trigger's aria-label, which stopped being a seam the moment the label
    // became translatable — a Russian console would have found nothing.
    renderBar({ locale: "ru" });
    const el = document.querySelector<HTMLElement>(TIME_MACHINE_TRIGGER_SELECTOR);
    expect(el).not.toBeNull();
    expect(el).toHaveTextContent("Машина времени");
  });

  it("translates the engaged banner, its hint and its escape hatch", () => {
    renderBar({ locale: "ru", at: engagedAt });
    const banner = screen.getByRole("status");
    expect(banner).toHaveTextContent("Вы смотрите состояние на");
    expect(banner).toHaveTextContent("Чтобы что-то менять, вернитесь в реальное время");
    expect(screen.getByRole("button", { name: "Вернуться в реальное время" })).toBeInTheDocument();
  });

  /* A stamp inside a Russian sentence is formatted in Russian: the old bar
     wrote "8/7/2026, 3:34 PM" in the middle of «Вы смотрите состояние на …»
     (QA round 6, finding #13). Still interpolated, never translated. */
  it("formats the instant in the sentence's own language", () => {
    renderBar({ locale: "ru", at: engagedAt });
    const stamp = engagedAt.toLocaleString("ru-RU");
    expect(stamp).not.toBe(engagedAt.toLocaleString("en-US"));
    expect(screen.getByRole("status")).toHaveTextContent(stamp);
    const adjust = screen.getByRole("button", { name: /Изменить момент просмотра/ });
    expect(adjust.getAttribute("aria-label")).toContain(stamp);
    expect(adjust).toHaveTextContent(stamp);
  });

  it("keeps the English bar byte-for-byte", () => {
    renderBar({ at: engagedAt });
    const banner = screen.getByRole("status");
    expect(banner).toHaveTextContent(`You are viewing ${engagedAt.toLocaleString()}`);
    expect(banner).toHaveTextContent("— return to Live to act.");
    expect(screen.getByRole("button", { name: "Return to Live" })).toBeInTheDocument();
  });
});
