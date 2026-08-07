import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useAuth } from "./use-auth";
import type { Me } from "@/lib/types";

function meBody(overrides: Partial<Me["subject"]> = {}, permissions: string[] = []): Me {
  return {
    subject: { kind: "user", id: "u1", displayName: "Ada Lovelace", groups: [], roles: ["viewer"], ...overrides },
    permissions,
  };
}

function harness(seed?: Me) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (seed) qc.setQueryData(["me"], seed);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );
  return { qc, wrapper };
}

afterEach(() => vi.unstubAllGlobals());

describe("useAuth", () => {
  it("can() reflects the subject's permission list", () => {
    const { wrapper } = harness(meBody({}, ["mtr:run", "tokens:manage"]));
    const { result } = renderHook(() => useAuth(), { wrapper });
    expect(result.current.can("mtr:run")).toBe(true);
    expect(result.current.can("tokens:manage")).toBe(true);
    expect(result.current.can("rbac:manage")).toBe(false);
  });

  it("isAnonymous is true for the anonymous subject", () => {
    const { wrapper } = harness(meBody({ kind: "anonymous", id: "anonymous", displayName: "Anonymous" }, ["mtr:run"]));
    const { result } = renderHook(() => useAuth(), { wrapper });
    expect(result.current.isAnonymous).toBe(true);
  });

  it("isAnonymous is false for a real user subject", () => {
    const { wrapper } = harness(meBody());
    const { result } = renderHook(() => useAuth(), { wrapper });
    expect(result.current.isAnonymous).toBe(false);
  });

  it("fetches GET /api/v1/auth/me when nothing is cached yet", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify(meBody()), { status: 200, headers: { "Content-Type": "application/json" } }),
      ),
    );
    const { wrapper } = harness();
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.me?.subject.displayName).toBe("Ada Lovelace"));
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe("/api/v1/auth/me");
  });
});
