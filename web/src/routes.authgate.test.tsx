import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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

/* The console's session store blinked (a Postgres restart): the credential is not known to be bad,
   so the server answers 503 "authentication unavailable" instead of 401. That is not a sign-out:
   the browser stays here, says so, and asks again until the store answers. */
describe("the session store is down (503 authentication unavailable)", () => {
  const unavailable = () =>
    Promise.resolve(
      new Response(
        JSON.stringify({
          type: "about:blank",
          title: "authentication unavailable",
          status: 503,
          detail: "the session or user store did not answer; retry shortly, the session is still valid",
        }),
        { status: 503, headers: { "Content-Type": "application/problem+json", "Retry-After": "1" } },
      ),
    );
  const signedIn = () =>
    Promise.resolve(
      json({
        subject: { kind: "user", id: "user:ada", displayName: "Ada", groups: [], roles: ["admin"] },
        permissions: [],
      }),
    );

  it("stays signed in and says it will retry, without leaving for /login", async () => {
    const gone: string[] = [];
    setNavigateForTest((p) => gone.push(p));
    const { calls } = renderAt("/", unavailable);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Cannot check your sign-in right now");
    expect(alert).toHaveTextContent(/not been signed out/);
    expect(screen.getByRole("button", { name: "Retry now" })).toBeInTheDocument();
    expect(gone).toEqual([]);
    expect(screen.queryByRole("navigation", { name: "Main" })).not.toBeInTheDocument();
    expect(calls.filter((c) => !c.includes("/api/v1/auth/me"))).toEqual([]);
  });

  it("opens the console once the store answers again, on its own", async () => {
    let n = 0;
    renderAt("/", () => (n++ === 0 ? unavailable() : signedIn()));
    await screen.findByRole("alert");
    expect(await screen.findByRole("navigation", { name: "Main" }, { timeout: 5000 })).toBeInTheDocument();
    expect(screen.queryByText("Cannot check your sign-in right now")).not.toBeInTheDocument();
  });

  it("asks again at once on Retry now", async () => {
    let n = 0;
    renderAt("/", () => (n++ === 0 ? unavailable() : signedIn()));
    fireEvent.click(await screen.findByRole("button", { name: "Retry now" }));
    expect(await screen.findByRole("navigation", { name: "Main" }, { timeout: 500 })).toBeInTheDocument();
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
