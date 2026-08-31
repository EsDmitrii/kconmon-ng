import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { resetNavigateForTest, setNavigateForTest } from "@/lib/api";
import { routeTree } from "@/routes";

/**
 * THE FLASH BEFORE /login.
 *
 * The owner's screen recording: a signed-out visitor opening the console saw
 * the WHOLE product for a fraction of a second — sidebar, Overview, panels
 * reading "authentication required" — before the browser left for /login. The
 * shell rendered ahead of the session check, its queries all 401ed and painted
 * their error states, and only then the first 401's redirect landed.
 *
 * This file pins the gate that removes it: until /auth/me answers, and while a
 * 401's redirect is in flight, nothing of the console renders — no sidebar, no
 * page, no data requests. Only a real answer (any subject) or a NON-401
 * failure opens the shell; the latter deliberately, so a network hiccup
 * degrades to the pages' own inline errors instead of a dead splash.
 */

const json = (body: unknown, status = 200, contentType = "application/json") =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": contentType } });

const CONFIG = {
  auth: { mode: "oidc", role: "", loginPath: "/api/v1/auth/oidc/start" },
  anonymousBanner: false,
  controller: { configured: true },
  prometheus: { configured: true },
  database: { configured: false },
};

function renderAt(path: string, me: () => Promise<Response>) {
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const href = String(url);
      calls.push(href);
      if (href.includes("/api/v1/auth/me")) return me();
      if (href.includes("/api/v1/config")) return Promise.resolve(json(CONFIG));
      return Promise.resolve(json({}));
    }),
  );
  const testRouter = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: [path] }) });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return {
    calls,
    ...render(
      <QueryClientProvider client={qc}>
        <ThemeProvider>
          <RouterProvider router={testRouter} />
        </ThemeProvider>
      </QueryClientProvider>,
    ),
  };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  resetNavigateForTest();
});

describe("while the session check is still in flight", () => {
  it("shows the splash and none of the console", async () => {
    renderAt("/", () => new Promise<Response>(() => {}));
    expect(await screen.findByTestId("auth-gate-splash")).toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "Main" })).not.toBeInTheDocument();
    expect(screen.queryByRole("main")).not.toBeInTheDocument();
  });

  it("has asked the API for nothing but the subject", async () => {
    const { calls } = renderAt("/", () => new Promise<Response>(() => {}));
    await screen.findByTestId("auth-gate-splash");
    expect(calls.filter((c) => !c.includes("/api/v1/auth/me"))).toEqual([]);
  });
});

describe("a signed-out visitor (401)", () => {
  const unauthorized = () =>
    Promise.resolve(json({ type: "about:blank", title: "Unauthorized", status: 401 }, 401, "application/problem+json"));

  it("keeps the console dark while the browser leaves for /login", async () => {
    const gone: string[] = [];
    setNavigateForTest((p) => gone.push(p));
    const { calls } = renderAt("/", unauthorized);
    await waitFor(() => expect(gone).toEqual(["/login"]));
    // The redirect is a full browser navigation; until it lands the splash
    // stays, and the product's layout never existed for this visitor.
    expect(screen.getByTestId("auth-gate-splash")).toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "Main" })).not.toBeInTheDocument();
    expect(calls.filter((c) => !c.includes("/api/v1/auth/me"))).toEqual([]);
  });
});

describe("the gate fails open", () => {
  it("renders the console when /auth/me breaks in any non-401 way", async () => {
    renderAt("/", () => Promise.resolve(json({ type: "about:blank", title: "boom", status: 500 }, 500)));
    expect(await screen.findByRole("navigation", { name: "Main" })).toBeInTheDocument();
    expect(screen.queryByTestId("auth-gate-splash")).not.toBeInTheDocument();
  });

  it("renders the console for any answered subject, anonymous included", async () => {
    renderAt("/", () =>
      Promise.resolve(
        json({
          subject: { kind: "anonymous", id: "anonymous", displayName: "Anonymous", groups: [], roles: ["viewer"] },
          permissions: [],
        }),
      ),
    );
    expect(await screen.findByRole("navigation", { name: "Main" })).toBeInTheDocument();
    expect(screen.queryByTestId("auth-gate-splash")).not.toBeInTheDocument();
  });
});
