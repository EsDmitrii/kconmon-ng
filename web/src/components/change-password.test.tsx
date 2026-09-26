import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { resetNavigateForTest, setNavigateForTest } from "@/lib/api";
import { LocaleProvider } from "@/lib/i18n";
import { ChangePasswordDialog } from "./change-password";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  resetNavigateForTest();
});

const problem = (status: number, title: string, detail = "") =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

function renderDialog(onClose = () => {}) {
  render(
    <LocaleProvider>
      <ChangePasswordDialog open onClose={onClose} />
    </LocaleProvider>,
  );
}

function fill(current: string, next: string, repeat: string) {
  fireEvent.change(screen.getByLabelText("Current password"), { target: { value: current } });
  fireEvent.change(screen.getByLabelText("New password"), { target: { value: next } });
  fireEvent.change(screen.getByLabelText("Repeat the new password"), { target: { value: repeat } });
  fireEvent.click(screen.getByRole("button", { name: "Change password" }));
}

describe("ChangePasswordDialog", () => {
  it("checks length, the repeat and a no-op change before sending anything", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    renderDialog();
    fill("old secret here", "short", "short");
    expect(await screen.findByText("The new password needs at least 12 characters.")).toBeInTheDocument();
    fill("old secret here", "a long enough secret", "a different secret!");
    expect(await screen.findByText("The two new passwords differ.")).toBeInTheDocument();
    fill("a long enough secret", "a long enough secret", "a long enough secret");
    expect(await screen.findByText("The new password is the current one.")).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("sends both passwords and says so when the current one is wrong", async () => {
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    const fetchMock = vi.fn(() => Promise.resolve(problem(401, "invalid credentials", "the current password does not match")));
    vi.stubGlobal("fetch", fetchMock);
    renderDialog();
    fill("wrong password!!", "a long enough secret", "a long enough secret");
    expect(await screen.findByText("The current password is wrong.")).toBeInTheDocument();
    expect(navigateSpy).not.toHaveBeenCalled();
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("/api/v1/auth/password");
    expect(JSON.parse(String(init.body))).toEqual({ currentPassword: "wrong password!!", newPassword: "a long enough secret" });
  });

  it("sends a signed-out user to the login page instead of blaming the password", async () => {
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(problem(401, "not signed in"))));
    renderDialog();
    fill("old secret here", "a long enough secret", "a long enough secret");
    await waitFor(() => expect(navigateSpy).toHaveBeenCalledTimes(1));
    expect(String(navigateSpy.mock.calls[0]?.[0])).toMatch(/^\/login/);
    expect(screen.queryByText("The current password is wrong.")).toBeNull();
  });

  it("passes any other refusal through in the server's words", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.resolve(problem(429, "too many requests", "too many password attempts; retry shortly"))),
    );
    renderDialog();
    fill("old secret here", "a long enough secret", "a long enough secret");
    expect(await screen.findByText("too many password attempts; retry shortly")).toBeInTheDocument();
  });

  it("confirms success, clears the fields and offers only Done", async () => {
    const onClose = vi.fn();
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(new Response(null, { status: 204 }))));
    renderDialog(onClose);
    fill("old secret here", "a long enough secret", "a long enough secret");
    await waitFor(() =>
      expect(screen.getByText("Password changed. Your other sessions are signed out.")).toBeInTheDocument(),
    );
    expect(screen.queryByLabelText("Current password")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(onClose).toHaveBeenCalled();
  });

  /* The request is already on the server, so a dismissal mid-flight could not take it back: it only
     hid the answer, and the next open showed that answer as if it belonged to the new attempt. */
  it("cannot be dismissed while the change is in flight, and then says how it ended", async () => {
    const onClose = vi.fn();
    let answer: (r: Response) => void = () => {};
    vi.stubGlobal(
      "fetch",
      vi.fn(
        () =>
          new Promise<Response>((resolve) => {
            answer = resolve;
          }),
      ),
    );
    renderDialog(onClose);
    fill("old secret here", "a long enough secret", "a long enough secret");

    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Close" })).toBeDisabled();
    fireEvent.keyDown(screen.getByLabelText("Current password"), { key: "Escape" });
    fireEvent.click(screen.getByTestId("modal-backdrop"));
    expect(onClose).not.toHaveBeenCalled();

    answer(new Response(null, { status: 204 }));
    expect(await screen.findByText("Password changed. Your other sessions are signed out.")).toBeInTheDocument();
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
