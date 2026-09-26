import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { resetNavigateForTest, setNavigateForTest } from "@/lib/api";
import { LoginPage } from "./login";
import { emulatePhone, lightThemeHazards, phoneOverflowHazards, resetTheme, restoreViewport, startInLight } from "@/lib/phone-and-light";

function configBody(mode: string) {
  return { auth: { mode, role: "", loginPath: mode === "local" ? "/api/v1/auth/login" : mode === "oidc" ? "/api/v1/auth/oidc/start" : "" }, anonymousBanner: mode === "anonymous", controller: { configured: true }, prometheus: { configured: true }, database: { configured: false } };
}

function renderPage(mode: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["config"], configBody(mode));
  return { qc, ...render(<QueryClientProvider client={qc}><LoginPage /></QueryClientProvider>) };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
  resetNavigateForTest();
});

describe("LoginPage", () => {
  it("local mode renders a username/password form", () => {
    renderPage("local");
    expect(screen.getByLabelText(/username/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/password/i)).toBeInTheDocument();
  });

  it("oidc mode renders a single SSO link to the start endpoint, no form", () => {
    renderPage("oidc");
    expect(screen.queryByLabelText(/username/i)).not.toBeInTheDocument();
    const link = screen.getByRole("link", { name: /sign in with sso/i });
    expect(link).toHaveAttribute("href", "/api/v1/auth/oidc/start?returnTo=%2F");
  });

  it.each(["header", "anonymous"])("%s mode redirects home instead of rendering", async (mode) => {
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    renderPage(mode);
    await waitFor(() => expect(navigateSpy).toHaveBeenCalledWith("/"));
    expect(screen.queryByLabelText(/username/i)).not.toBeInTheDocument();
  });

  it("a successful submit posts, invalidates the me query, and navigates to returnTo", async () => {
    window.history.pushState({}, "", "/login?returnTo=%2Fmatrix");
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 204 })));
    const { qc } = renderPage("local");
    qc.setQueryData(["me"], { subject: { kind: "anonymous", id: "x", displayName: "x", groups: [], roles: [] }, permissions: [] });

    fireEvent.change(screen.getByLabelText(/username/i), { target: { value: "ada" } });
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: "secret" } });
    fireEvent.click(screen.getByRole("button", { name: /sign in/i }));

    await waitFor(() => expect(navigateSpy).toHaveBeenCalledWith("/matrix"));
    expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      "/api/v1/auth/login",
      expect.objectContaining({ method: "POST", body: JSON.stringify({ username: "ada", password: "secret" }) }),
    );
    expect(qc.getQueryState(["me"])?.isInvalidated).toBe(true);
  });

  /* The other half of lib/api.ts's "no ?returnTo= for the root": with no
     parameter to read, the sign-in must land on / rather than nowhere. */
  it("lands on / after a sign-in reached with no returnTo at all", async () => {
    window.history.pushState({}, "", "/login");
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 204 })));
    renderPage("local");

    fireEvent.change(screen.getByLabelText(/username/i), { target: { value: "ada" } });
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: "secret" } });
    fireEvent.click(screen.getByRole("button", { name: /sign in/i }));

    await waitFor(() => expect(navigateSpy).toHaveBeenCalledWith("/"));
  });

  it("sends the SSO link home too when nothing asked for anywhere else", () => {
    window.history.pushState({}, "", "/login");
    renderPage("oidc");
    expect(screen.getByRole("link", { name: /sign in with sso/i })).toHaveAttribute(
      "href",
      "/api/v1/auth/oidc/start?returnTo=%2F",
    );
  });

  it("asks for both fields before sending anything, instead of showing the API's JSON contract", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    renderPage("local");

    expect(screen.getByLabelText(/username/i)).toBeRequired();
    expect(screen.getByLabelText(/password/i)).toBeRequired();
    fireEvent.submit(screen.getByRole("button", { name: /sign in/i }).closest("form")!);

    await waitFor(() => expect(document.activeElement).toBe(screen.getByLabelText(/username/i)));
    expect(fetchMock).not.toHaveBeenCalled();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("a failed submit shows an inline error and does not navigate", async () => {
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ type: "about:blank", title: "invalid credentials", status: 401 }), {
          status: 401,
          headers: { "Content-Type": "application/problem+json" },
        }),
      ),
    );
    renderPage("local");

    fireEvent.change(screen.getByLabelText(/username/i), { target: { value: "ada" } });
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: "wrong" } });
    fireEvent.click(screen.getByRole("button", { name: /sign in/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/invalid credentials/i);
    expect(navigateSpy).not.toHaveBeenCalled();
  });
});

/* ── WB13: the page on a 375px phone and in the light theme ──────────────── */
describe("LoginPage — on a phone and in the light theme", () => {
  afterEach(() => {
    restoreViewport();
    resetTheme();
  });

  it("keeps everything wider than a 375px phone inside a scroller of its own", async () => {
    emulatePhone();
    renderPage("local");
    await screen.findByLabelText(/username/i);
    expect(phoneOverflowHazards(document.body)).toEqual([]);
  });

  it("draws every colour from a token the light theme restyles", async () => {
    startInLight();
    renderPage("local");
    await screen.findByLabelText(/username/i);
    expect(lightThemeHazards(document.body)).toEqual([]);
  });
});
