import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { StrictMode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { TimeMachineProvider } from "@/lib/timemachine";
import { AlertingPage } from "@/pages/alerting";

/* The paused-retry trap, found LIVE at the M7 final gate.
 *
 * react-query pauses retries while onlineManager reports offline. A query
 * whose first attempt failed and whose retry is paused sits at
 * status:"pending" / fetchStatus:"paused" — isLoading (isPending &&
 * isFetching) is FALSE there, so the old empty-state guard of
 * `!isLoading && !isError && list.length === 0` rendered "No foreign
 * PrometheusRule objects in this namespace" while the API was answering 409
 * problem+json on the wire. A browser that flickered offline once (laptop
 * sleep, flaky wifi, the embedded dev pane) presented a made-up answer as a
 * settled one.
 *
 * The fix across alerting/mtr/settings: skeleton on isPending (covers
 * loading AND paused), empty state ONLY on isSuccess. This file pins the
 * alerting foreign section under the app's REAL mounting conditions
 * (StrictMode + retry:1, main.tsx's own client options) in both worlds:
 * online-error and offline-paused. */

const DETAIL =
  "prometheus rule sync is not running on this console: the alert rules themselves are unaffected and stay readable";

const problem = (status: number, title: string, detail: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function stubTransport() {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const href = String(input);
      if (href.includes("/api/v1/auth/me")) {
        return Promise.resolve(
          json({
            subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["admin"] },
            permissions: ["alerts:read", "alerts:manage"],
          }),
        );
      }
      if (href.includes("/api/v1/config")) {
        return Promise.resolve(
          json({
            auth: { mode: "local", role: "viewer", loginPath: "/api/v1/auth/login" },
            anonymousBanner: false,
            controller: { configured: true },
            prometheus: { configured: true },
            database: { configured: true },
          }),
        );
      }
      if (href.startsWith("/api/v1/alert-rules/foreign")) {
        return Promise.resolve(problem(409, "prometheus rule sync is disabled", DETAIL));
      }
      if (href.startsWith("/api/v1/alert-rules")) return Promise.resolve(json({ rules: [] }));
      return Promise.resolve(json({}));
    }),
  );
}

function mountLikeMainTsx() {
  window.history.pushState({}, "", "/alerting");
  const qc = new QueryClient({
    defaultOptions: { queries: { staleTime: 10_000, refetchOnWindowFocus: false, retry: 1 } },
  });
  return render(
    <StrictMode>
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>
          <AlertingPage />
        </TimeMachineProvider>
      </QueryClientProvider>
    </StrictMode>,
  );
}

afterEach(() => {
  onlineManager.setOnline(true);
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  window.history.pushState({}, "", "/");
});

describe("the foreign section under the app's own mounting conditions", () => {
  it("online: the 409 detail renders, never the empty-state line", { timeout: 15000 }, async () => {
    stubTransport();
    mountLikeMainTsx();
    await waitFor(() => expect(screen.getByText(DETAIL)).toBeTruthy(), { timeout: 10000 });
    expect(screen.queryByText(/No foreign PrometheusRule objects/)).toBeNull();
  });

  it("offline-paused retry: the section stays UNSETTLED — no empty line, no fake answer", { timeout: 15000 }, async () => {
    stubTransport();
    onlineManager.setOnline(false);
    mountLikeMainTsx();

    // Let the first attempt fail and the retry pause: the rules list settles
    // from cache-independent 200s? No — offline pauses those too, so anchor
    // on time: give react-query ample room to have rendered the trap.
    await new Promise((r) => setTimeout(r, 1500));
    expect(
      screen.queryByText(/No foreign PrometheusRule objects/),
      "a paused retry must not present 'no foreign objects' as a settled answer",
    ).toBeNull();

    // Back online: the retry resumes, fails for real, and the honest 409 lands.
    onlineManager.setOnline(true);
    await waitFor(() => expect(screen.getByText(DETAIL)).toBeTruthy(), { timeout: 10000 });
  });
});
