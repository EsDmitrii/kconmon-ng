import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { PasswordDialogHost } from "./change-password";
import { UserMenu } from "./user-menu";
import { TOKENS_ANCHOR } from "@/pages/settings";
import type { Me } from "@/lib/types";

const me: Me = {
  subject: { kind: "user", id: "u1", displayName: "Ada Lovelace", groups: [], roles: ["viewer", "operator"] },
  permissions: ["mtr:run"],
};

function renderMenu(canManageTokens: boolean, authMode?: "local" | "oidc") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (authMode) qc.setQueryData(["config"], { auth: { mode: authMode } });
  const can = (p: string) => (canManageTokens ? p === "tokens:manage" : false);
  return {
    qc,
    ...render(
      <QueryClientProvider client={qc}>
        <PasswordDialogHost>
          <UserMenu me={me} can={can} />
        </PasswordDialogHost>
      </QueryClientProvider>,
    ),
  };
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

  /* The popup is a plain group of controls, not a menu: announcing a menu button promised arrow-key
     roving it does not have. */
  it("does not announce a menu, and points at the group it opens", () => {
    renderMenu(false);
    const trigger = screen.getByRole("button", { name: /ada lovelace/i });
    expect(trigger).not.toHaveAttribute("aria-haspopup");
    fireEvent.click(trigger);
    expect(trigger).toHaveAttribute("aria-controls", screen.getByRole("group").id);
  });

  it("opening it shows roles and a sign-out action", () => {
    renderMenu(false);
    fireEvent.click(screen.getByRole("button", { name: /ada lovelace/i }));
    expect(screen.getByRole("group", { name: /alice|dmitrii|admin|.+/ })).toBeInTheDocument();
    expect(screen.getByText("viewer, operator")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /sign out/i })).toBeInTheDocument();
  });

  it("shows the token management link only with tokens:manage", () => {
    renderMenu(false);
    fireEvent.click(screen.getByRole("button", { name: /ada lovelace/i }));
    expect(screen.queryByRole("link", { name: /token management/i })).not.toBeInTheDocument();

    cleanup();
    renderMenu(true);
    fireEvent.click(screen.getByRole("button", { name: /ada lovelace/i }));
    expect(screen.getByRole("link", { name: /token management/i })).toBeInTheDocument();
  });

  /* The link pointed at /settings while that page had no tokens section on it
     at all (QA round 6, finding #14); it now names the section's own anchor. */
  it("lands on the Settings tokens section, not the top of the page", () => {
    renderMenu(true);
    fireEvent.click(screen.getByRole("button", { name: /ada lovelace/i }));
    expect(screen.getByRole("link", { name: /token management/i })).toHaveAttribute(
      "href",
      `/settings#${TOKENS_ANCHOR}`,
    );
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
    fireEvent.click(screen.getByRole("button", { name: /sign out/i }));

    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalledWith("/api/v1/auth/logout", expect.anything()));
    const [, init] = vi.mocked(fetch).mock.calls.find(([url]) => url === "/api/v1/auth/logout") ?? [];
    expect(new Headers(init?.headers).get("X-CSRF-Token")).toBe("tok-abc");
    await waitFor(() => expect(qc.getQueryState(["me"])?.isInvalidated).toBe(true));

    document.cookie = "csrf=; path=/; expires=Thu, 01 Jan 1970 00:00:00 GMT";
  });
});

describe("UserMenu password change", () => {
  it("offers it in auth.mode=local and opens the dialog", async () => {
    renderMenu(false, "local");
    fireEvent.click(screen.getByRole("button", { name: /ada lovelace/i }));
    fireEvent.click(screen.getByRole("button", { name: "Change password" }));
    expect(await screen.findByRole("dialog", { name: "Change password" })).toBeInTheDocument();
    expect(screen.getByLabelText("Current password")).toBeInTheDocument();
  });

  it("does not offer it where an identity provider owns the password", () => {
    renderMenu(false, "oidc");
    fireEvent.click(screen.getByRole("button", { name: /ada lovelace/i }));
    expect(screen.queryByRole("button", { name: "Change password" })).toBeNull();
  });
});

/* ── where focus goes when the menu closes ───────────────────────────────── */

/*
 * The menu used to unmount with focus still inside it: document.activeElement fell back to <body>,
 * and the next Tab restarted at the skip link — a keyboard user at the BOTTOM of the sidebar was
 * thrown to the TOP of the document, twice (Escape, and again after signing out).
 */
describe("UserMenu hands focus back", () => {
  it("returns focus to the trigger on Escape", async () => {
    renderMenu(false);
    const trigger = screen.getByRole("button", { name: /ada lovelace/i });
    trigger.focus();
    fireEvent.click(trigger);

    const signOut = await screen.findByRole("button", { name: /sign out/i });
    signOut.focus();
    expect(document.activeElement).toBe(signOut);

    fireEvent.keyDown(document, { key: "Escape" });

    expect(screen.queryByRole("button", { name: /sign out/i })).toBeNull();
    expect(document.activeElement).toBe(trigger);
    expect(document.activeElement).not.toBe(document.body);
  });
});
