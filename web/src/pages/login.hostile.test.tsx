import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { resetNavigateForTest, setNavigateForTest } from "@/lib/api";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { LoginPage } from "./login";

/**
 * THE HOSTILE READER'S LOGIN PAGE.
 *
 * pages/login.test.tsx pins what the page does for somebody who signs in. This
 * file is the other half: what it does for somebody who is TRYING TO BREAK IT,
 * and the one property here that is a security boundary rather than a nicety —
 * ?returnTo= must not be able to send an operator off this origin.
 *
 * ── why the "starts with /" test was not enough ───────────────────────────
 * The URL parser STRIPS tab, LF and CR from a URL before it resolves anything
 * (WHATWG URL, "URL parsing" step 2). So "/\n/evil.com" starts with "/", is
 * not "//…", carries no backslash — and reaches the browser as "//evil.com",
 * which is a protocol-relative URL to somebody else's host. A phishing link of
 * the form
 *
 *     https://console.example/login?returnTo=%2F%0A%2Fevil.com
 *
 * therefore showed the console's own sign-in form and then handed the freshly
 * authenticated operator to evil.com. The guard below no longer reasons about
 * the STRING at all: it resolves the candidate against this origin and refuses
 * anything that did not stay on it, which is the only check that cannot be
 * out-spelled.
 */

function configBody(mode: string) {
  return {
    auth: {
      mode,
      role: "",
      loginPath: mode === "local" ? "/api/v1/auth/login" : mode === "oidc" ? "/api/v1/auth/oidc/start" : "",
    },
    anonymousBanner: mode === "anonymous",
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: false },
  };
}

function renderPage(mode: string, locale?: "en" | "ru") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["config"], configBody(mode));
  if (locale) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  return {
    qc,
    ...render(
      <QueryClientProvider client={qc}>
        <LocaleProvider>
          <LoginPage />
        </LocaleProvider>
      </QueryClientProvider>,
    ),
  };
}

/** signIn types a pair of credentials and submits once. */
function signIn(username = "ada", password = "secret") {
  fireEvent.change(screen.getByLabelText(/username|Имя пользователя/i), { target: { value: username } });
  fireEvent.change(screen.getByLabelText(/password|Пароль/i), { target: { value: password } });
  fireEvent.click(screen.getByRole("button", { name: /sign in|Войти/i }));
}

/** ok204 is the local login endpoint's success: no body at all. */
const ok204 = () => new Response(null, { status: 204 });

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
  localStorage.removeItem(LOCALE_STORAGE_KEY);
  resetNavigateForTest();
});

/* ── the open redirect ──────────────────────────────────────────────────── */

/** Every shape of "take this operator somewhere else" the parameter can carry. */
const OFF_ORIGIN: readonly (readonly [string, string])[] = [
  ["an absolute URL", "https://evil.com/steal"],
  ["a protocol-relative URL", "//evil.com/steal"],
  ["a backslash-relative URL", "/\\evil.com"],
  ["a double backslash", "\\\\evil.com"],
  ["a javascript: URI", "javascript:alert(document.cookie)"],
  ["a data: URI", "data:text/html,<script>alert(1)</script>"],
  /* The three the string rules could not see: the URL parser deletes these
     bytes, and what is left begins with "//". */
  ["a smuggled LF", "/\n/evil.com"],
  ["a smuggled CR", "/\r/evil.com"],
  ["a smuggled TAB", "/\t/evil.com"],
  ["a smuggled LF before a backslash", "/\n\\evil.com"],
  /* Whitespace is trimmed off the ends of a URL, so a leading space hides a
     scheme just as well as a leading slash hides a host. */
  ["a leading space before a scheme", " https://evil.com"],
  ["a leading newline before a scheme", "\nhttps://evil.com"],
];

describe("?returnTo= cannot send the operator off this origin", () => {
  it.each(OFF_ORIGIN)("refuses %s after a successful sign-in", async (_name, hostile) => {
    window.history.pushState({}, "", `/login?returnTo=${encodeURIComponent(hostile)}`);
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(ok204()));
    renderPage("local");

    signIn();

    await waitFor(() => expect(navigateSpy).toHaveBeenCalled());
    const target = String(navigateSpy.mock.calls[0][0]);
    // The assertion is the RESOLUTION, not the spelling: whatever the page
    // decided to navigate to has to land on this origin once a browser has
    // parsed it.
    expect(new URL(target, window.location.origin).origin).toBe(window.location.origin);
    // ...and the console's answer to "I do not trust this" is the root.
    expect(target).toBe("/");
  });

  it.each(OFF_ORIGIN)("keeps %s out of the SSO start link too", (_name, hostile) => {
    window.history.pushState({}, "", `/login?returnTo=${encodeURIComponent(hostile)}`);
    renderPage("oidc");

    const href = screen.getByRole("link", { name: /sign in with sso/i }).getAttribute("href") ?? "";
    const carried = new URLSearchParams(href.slice(href.indexOf("?") + 1)).get("returnTo") ?? "";
    // The IdP is handed this string and redirects to it after the exchange; a
    // hostile one here is the same open redirect one hop further away.
    expect(new URL(carried, window.location.origin).origin).toBe(window.location.origin);
    expect(carried).toBe("/");
  });

  /** The parameter still WORKS — a guard that refuses everything is not a fix. */
  it.each([
    ["a plain path", "/matrix", "/matrix"],
    ["a path with a query", "/explore?protocol=tcp", "/explore?protocol=tcp"],
    ["a path with a fragment", "/settings#tokens", "/settings#tokens"],
    ["a deep detail route", "/pairs/node-a/node-b", "/pairs/node-a/node-b"],
    ["an encoded slash inside a segment", "/nodes/a%2Fb", "/nodes/a%2Fb"],
  ])("still returns to %s", async (_name, benign, expected) => {
    window.history.pushState({}, "", `/login?returnTo=${encodeURIComponent(benign)}`);
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(ok204()));
    renderPage("local");

    signIn();

    await waitFor(() => expect(navigateSpy).toHaveBeenCalledWith(expected));
  });

  /** /login → /login is a door that opens onto itself. */
  it("does not return to the sign-in page it is already on", async () => {
    window.history.pushState({}, "", "/login?returnTo=%2Flogin%3FreturnTo%3D%252Flogin");
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(ok204()));
    renderPage("local");

    signIn();

    await waitFor(() => expect(navigateSpy).toHaveBeenCalledWith("/"));
  });

  it("ignores a second returnTo appended after a good one", async () => {
    window.history.pushState({}, "", "/login?returnTo=%2Fmatrix&returnTo=https%3A%2F%2Fevil.com");
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(ok204()));
    renderPage("local");

    signIn();

    await waitFor(() => expect(navigateSpy).toHaveBeenCalledWith("/matrix"));
  });

  it("survives a returnTo of ten thousand characters without navigating off-origin", async () => {
    const huge = `/matrix?q=${"a".repeat(10_000)}`;
    window.history.pushState({}, "", `/login?returnTo=${encodeURIComponent(huge)}`);
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(ok204()));
    renderPage("local");

    signIn();

    await waitFor(() => expect(navigateSpy).toHaveBeenCalled());
    const target = String(navigateSpy.mock.calls[0][0]);
    expect(new URL(target, window.location.origin).origin).toBe(window.location.origin);
  });
});

/* ── hostile credentials ────────────────────────────────────────────────── */

describe("the credential boxes take whatever is typed into them", () => {
  it.each([
    ["ten thousand characters", "a".repeat(10_000)],
    ["a SQL-looking string", "' OR 1=1 --"],
    ["an HTML-looking string", "<script>alert(1)</script>"],
    ["astral-plane unicode", "👩‍💻🇷🇺𝕒𝕕𝕒"],
    ["a right-to-left override", "ada‮moc.live"],
    /* A single-line <input> drops LF and CR of its own accord, so the byte
       worth asking about is the one it keeps. */
    ["a NUL byte", "ada\u0000root"],
    ["only whitespace", "   "],
    ["nothing at all", ""],
  ])("POSTs %s verbatim, once, and never throws", async (_name, hostile) => {
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    const fetchMock = vi.fn().mockResolvedValue(ok204());
    vi.stubGlobal("fetch", fetchMock);
    renderPage("local");

    signIn(hostile, hostile);

    await waitFor(() => expect(navigateSpy).toHaveBeenCalledWith("/"));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    // The browser is not a validator: what was typed is what the server judges.
    expect(JSON.parse(String(fetchMock.mock.calls[0][1].body))).toEqual({
      username: hostile,
      password: hostile,
    });
  });

  it("sends ONE request for a three-click storm on the submit button", async () => {
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    let release!: (r: Response) => void;
    const inFlight = new Promise<Response>((r) => (release = r));
    const fetchMock = vi.fn(() => inFlight);
    vi.stubGlobal("fetch", fetchMock);
    renderPage("local");

    fireEvent.change(screen.getByLabelText(/username/i), { target: { value: "ada" } });
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: "secret" } });
    const submit = screen.getByRole("button", { name: /sign in/i });
    fireEvent.click(submit);
    fireEvent.click(submit);
    fireEvent.click(submit);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(submit).toBeDisabled();
    release(ok204());
    await waitFor(() => expect(navigateSpy).toHaveBeenCalledWith("/"));
  });

  it("lets the operator try again after a refusal", async () => {
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ type: "about:blank", title: "invalid credentials", status: 401 }), {
          status: 401,
          headers: { "Content-Type": "application/problem+json" },
        }),
      )
      .mockResolvedValue(ok204());
    vi.stubGlobal("fetch", fetchMock);
    renderPage("local");

    signIn("ada", "wrong");
    expect(await screen.findByRole("alert")).toHaveTextContent(/invalid credentials/i);
    // The button has to come back, or one typo locks the console out.
    expect(screen.getByRole("button", { name: /sign in/i })).not.toBeDisabled();

    signIn("ada", "right");
    await waitFor(() => expect(navigateSpy).toHaveBeenCalledWith("/"));
  });
});

/* ── a refusal that says nothing ────────────────────────────────────────── */

describe("a failure always says something", () => {
  /**
   * The 401 that carries an EMPTY title and an EMPTY detail — a proxy's own
   * problem document, a gateway that answered for the console — used to render
   * as nothing at all: the button un-spun, the form sat there, and the operator
   * had no way to tell a rejected password from a click that did not register.
   */
  it.each([
    ["an empty title and detail", { type: "about:blank", title: "", status: 401, detail: "" }],
    ["a whitespace-only detail", { type: "about:blank", title: "", status: 401, detail: "   " }],
    ["no title or detail key at all", { type: "about:blank", status: 502 }],
  ])("falls back to its own sentence for %s", async (_name, body) => {
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify(body), {
          status: (body as { status: number }).status,
          headers: { "Content-Type": "application/problem+json" },
        }),
      ),
    );
    renderPage("local");

    signIn("ada", "whatever");

    expect(await screen.findByRole("alert")).toHaveTextContent("Sign in failed");
    expect(navigateSpy).not.toHaveBeenCalled();
  });

  it("says it in Russian for a Russian console", async () => {
    setNavigateForTest(vi.fn());
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ type: "about:blank", title: "", status: 401 }), {
          status: 401,
          headers: { "Content-Type": "application/problem+json" },
        }),
      ),
    );
    renderPage("local", "ru");

    signIn("ada", "whatever");

    expect(await screen.findByRole("alert")).toHaveTextContent("Войти не удалось");
  });

  it("keeps the server's own sentence whenever there is one", async () => {
    setNavigateForTest(vi.fn());
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({ type: "about:blank", title: "unauthorized", status: 401, detail: "account is locked" }),
          { status: 401, headers: { "Content-Type": "application/problem+json" } },
        ),
      ),
    );
    renderPage("local", "ru");

    signIn("ada", "whatever");

    // Server text is DATA (lib/i18n's module doc): it renders verbatim in both
    // languages rather than being replaced by the fallback.
    expect(await screen.findByRole("alert")).toHaveTextContent("account is locked");
  });

  it("renders a dropped connection rather than swallowing the rejection", async () => {
    setNavigateForTest(vi.fn());
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
    renderPage("local");

    signIn();

    expect(await screen.findByRole("alert")).toHaveTextContent("Sign in failed");
    expect(screen.getByRole("button", { name: /sign in/i })).not.toBeDisabled();
  });

  /** The login endpoint's OWN 401 means "wrong password", not "session gone",
   *  so it must not bounce the reader off the page they are typing into. */
  it("does not bounce off the login page when the login POST itself 401s", async () => {
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    window.history.pushState({}, "", "/login?returnTo=%2Fmatrix");
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

    signIn("ada", "wrong");
    await screen.findByRole("alert");

    expect(navigateSpy).not.toHaveBeenCalled();
    // The destination the operator was heading for is still in the URL, so the
    // retry that follows still lands there.
    expect(new URLSearchParams(window.location.search).get("returnTo")).toBe("/matrix");
  });
});
