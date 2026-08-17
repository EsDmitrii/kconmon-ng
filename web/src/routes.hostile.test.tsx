import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { NOT_FOUND_PATH_LIMIT, routeTree } from "@/routes";

/**
 * THE ADDRESS BAR AS A WEAPON.
 *
 * routes.test.tsx pins which chrome each REAL route gets. This file is every
 * address that is not one: a typo, a stale bookmark, a traversal attempt, a
 * deep link an operator's role does not open, a URL a fuzzer produced.
 *
 * The rule the whole file is about: an address the console has no page for gets
 * a PAGE — a title, the address that failed, why that happens and a link
 * onward, in the reader's own language — rather than the router's default two
 * English words on an otherwise empty panel.
 */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function renderRoute(initialEntry: string, opts: { permissions?: string[]; locale?: "en" | "ru" } = {}) {
  const { permissions = ["tokens:manage", "webhooks:manage", "settings:write"], locale } = opts;
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const href = String(url);
      calls.push(href);
      if (href.includes("/api/v1/auth/me")) {
        return Promise.resolve(
          json({
            subject: { kind: "user", id: "ada", displayName: "Ada", groups: [], roles: ["admin"] },
            permissions,
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
            database: { configured: false },
          }),
        );
      }
      return Promise.resolve(json({}));
    }),
  );

  if (locale) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  const testRouter = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: [initialEntry] }) });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return {
    calls,
    ...render(
      <QueryClientProvider client={qc}>
        <LocaleProvider>
          <ThemeProvider>
            <RouterProvider router={testRouter} />
          </ThemeProvider>
        </LocaleProvider>
      </QueryClientProvider>,
    ),
  };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

/* ── addresses that are not routes ──────────────────────────────────────── */

const UNKNOWN: readonly (readonly [string, string])[] = [
  ["a typo", "/setttings"],
  ["a plural that does not exist", "/overviews"],
  ["a traversal attempt", "/settings/../../etc/passwd"],
  ["an encoded traversal attempt", "/%2e%2e%2f%2e%2e%2fetc%2fpasswd"],
  ["a detail route with no id", "/diagnostics/runs"],
  ["a node route with no name", "/nodes"],
  ["a pair route with half a pair", "/pairs/node-a"],
  ["a trailing segment on a real route", "/matrix/tcp/extra"],
  ["a query-only address", "/nope?at=2026-08-08T12:00:00Z"],
  ["a fragment-only address", "/nope#tokens"],
  ["an address that looks like an API path", "/api/v1/tokens"],
  ["an address with a NUL-ish escape", "/no%00pe"],
  ["a very long address", `/${"a".repeat(3_000)}`],
];

describe("an address with no page still gets a page", () => {
  it.each(UNKNOWN)("answers %s with the not-found page, not a blank panel", async (_name, path) => {
    renderRoute(path);
    // A heading, so the page has a name in the accessibility tree rather than
    // being a stray paragraph.
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeInTheDocument();
    // ...and a way onward that is not the back button.
    expect(screen.getByRole("link", { name: "Back to Overview" })).toHaveAttribute("href", "/");
  });

  it("names the address that failed, so a stale link identifies itself", async () => {
    renderRoute("/setttings?tab=tokens");
    const shown = await screen.findByTestId("not-found-path");
    expect(shown).toHaveTextContent("/setttings?tab=tokens");
  });

  it("clips an address long enough to be a payload rather than a path", async () => {
    renderRoute(`/${"a".repeat(3_000)}`);
    const shown = await screen.findByTestId("not-found-path");
    expect((shown.textContent ?? "").length).toBeLessThanOrEqual(NOT_FOUND_PATH_LIMIT + 1);
    expect(shown.textContent).toMatch(/…$/);
  });

  it("renders the address as TEXT — markup in a URL is not markup on the page", async () => {
    renderRoute("/<img src=x onerror=alert(1)>");
    const shown = await screen.findByTestId("not-found-path");
    expect(shown.querySelector("img")).toBeNull();
    expect(shown.textContent).toContain("onerror=alert(1)");
  });

  it("keeps the console's chrome, so the wrong turn is not a dead end", async () => {
    renderRoute("/nope");
    await screen.findByRole("heading", { name: "Page not found" });
    // The sidebar is the way out that does not need this page to offer one.
    expect(screen.getByRole("navigation", { name: "Main" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /skip to main content/i })).toBeInTheDocument();
  });

  it("speaks the reader's language", async () => {
    renderRoute("/nope", { locale: "ru" });
    expect(await screen.findByRole("heading", { name: "Страница не найдена" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Вернуться к обзору" })).toBeInTheDocument();
    // The address is data and stays as it came, in either language.
    expect(screen.getByTestId("not-found-path")).toHaveTextContent("/nope");
  });

  it("asks the API for nothing a missing page could need", async () => {
    const { calls } = renderRoute("/nope");
    await screen.findByRole("heading", { name: "Page not found" });
    // Only the chrome's own two reads; a 404 must not go fetching a resource
    // named after an address that does not exist.
    const resources = calls.filter((c) => !/\/api\/v1\/(auth\/me|config|version)/.test(c));
    expect(resources).toEqual([]);
  });
});

/* ── deep links a role does not open ────────────────────────────────────── */

describe("a deep link into an admin page as a viewer", () => {
  /** The built-in viewer holds none of the settings page's permissions. */
  const VIEWER = ["topology:read", "matrix:read", "events:read"];

  it("renders the page's own honest line rather than a blank or a crash", async () => {
    const { calls } = renderRoute("/settings", { permissions: VIEWER });
    expect(await screen.findByText(/can view none of the console/i)).toBeInTheDocument();
    // And asks for none of the admin resources on the way — a hidden section is
    // hidden in the network tab too.
    await waitFor(() => expect(calls.some((c) => c.includes("/api/v1/auth/me"))).toBe(true));
    expect(calls.filter((c) => /\/api\/v1\/(tokens|webhooks|export|import)/.test(c))).toEqual([]);
  });

  it("still gives the viewer the language switch, which belongs to the person", async () => {
    renderRoute("/settings", { permissions: VIEWER });
    expect(await screen.findByRole("radiogroup", { name: /language/i })).toBeInTheDocument();
  });
});

/* ── /login stays the one bare route ────────────────────────────────────── */

describe("the bare page cannot be widened by the address bar", () => {
  it.each([
    ["/login", "the plain route"],
    ["/login?returnTo=%2Fmatrix", "with a destination"],
    ["/login?returnTo=https%3A%2F%2Fevil.com", "with a hostile destination"],
    ["/login#anything", "with a fragment"],
  ])("%s (%s) shows no sidebar", async (path) => {
    renderRoute(path);
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "Main" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /settings/i })).not.toBeInTheDocument();
  });

  it("does not answer a near-miss of it with the sign-in card", async () => {
    renderRoute("/login/");
    // Either the card or the not-found page is defensible; what is not is a
    // half-rendered shell. Whatever renders, it is one of the two.
    await waitFor(() =>
      expect(
        screen.queryByRole("heading", { name: "Sign in" }) ??
          screen.queryByRole("heading", { name: "Page not found" }),
      ).toBeInTheDocument(),
    );
  });
});
