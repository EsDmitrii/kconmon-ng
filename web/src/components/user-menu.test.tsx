import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { UserMenu } from "./user-menu";
import type { Me } from "@/lib/types";

const me: Me = {
  subject: { kind: "user", id: "u1", displayName: "Ada Lovelace", groups: [], roles: ["viewer", "operator"] },
  permissions: ["mtr:run"],
};

function renderMenu(canManageTokens: boolean) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const can = (p: string) => (canManageTokens ? p === "tokens:manage" : false);
  return { qc, ...render(<QueryClientProvider client={qc}><UserMenu me={me} can={can} /></QueryClientProvider>) };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("UserMenu", () => {
  it("shows the display name on the closed trigger", () => {
    renderMenu(false);
    expect(screen.getByText("Ada Lovelace")).toBeInTheDocument();
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("opening it shows roles and a sign-out action", () => {
    renderMenu(false);
    fireEvent.click(screen.getByRole("button", { name: /ada lovelace/i }));
    expect(screen.getByRole("menu")).toBeInTheDocument();
    expect(screen.getByText("viewer, operator")).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /sign out/i })).toBeInTheDocument();
  });

  it("shows the token management link only with tokens:manage", () => {
    renderMenu(false);
    fireEvent.click(screen.getByRole("button", { name: /ada lovelace/i }));
    expect(screen.queryByRole("menuitem", { name: /token management/i })).not.toBeInTheDocument();

    cleanup();
    renderMenu(true);
    fireEvent.click(screen.getByRole("button", { name: /ada lovelace/i }));
    expect(screen.getByRole("menuitem", { name: /token management/i })).toBeInTheDocument();
  });

  it("sign out calls logout, echoes CSRF, and invalidates the me query", async () => {
    document.cookie = "csrf=tok-abc; path=/";
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response(null, { status: 204 })),
    );
    const { qc } = renderMenu(false);
    qc.setQueryData(["me"], me);
    fireEvent.click(screen.getByRole("button", { name: /ada lovelace/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: /sign out/i }));

    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalledWith("/api/v1/auth/logout", expect.anything()));
    const [, init] = vi.mocked(fetch).mock.calls[0];
    expect(new Headers(init?.headers).get("X-CSRF-Token")).toBe("tok-abc");
    await waitFor(() => expect(qc.getQueryState(["me"])?.isInvalidated).toBe(true));

    document.cookie = "csrf=; path=/; expires=Thu, 01 Jan 1970 00:00:00 GMT";
  });
});
