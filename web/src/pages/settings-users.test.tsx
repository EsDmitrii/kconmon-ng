import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { LocaleProvider } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import { UsersSection } from "./settings-users";
import { emulatePhone, lightThemeHazards, phoneOverflowHazards, resetTheme, restoreViewport, startInLight } from "@/lib/phone-and-light";

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

/** A refusal as the server writes it: RFC 7807, which is what lib/api reads the detail out of. */
const problem = (status: number, title: string, detail: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

type Call = { url: string; init?: RequestInit };

/** Answers the reads, and hands every write to onWrite (204 when there is none). The signed-in
 *  subject holds users:manage and rbac:manage unless `permissions` says otherwise; GET
 *  /api/v1/rbac/roles is gated on rbac:manage on the server. */
function setup(
  users: unknown[],
  onWrite?: (url: string, init: RequestInit) => Response,
  opts: { permissions?: string[]; roles?: () => Response } = {},
): Call[] {
  const { permissions = ["users:manage", "rbac:manage"], roles } = opts;
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init?: RequestInit) => {
      calls.push({ url, init });
      if (init?.method && init.method !== "GET") {
        return Promise.resolve(onWrite ? onWrite(url, init) : new Response(null, { status: 204 }));
      }
      if (url.startsWith("/api/v1/users")) return Promise.resolve(json({ users }));
      if (url.startsWith("/api/v1/rbac/roles")) {
        if (!permissions.includes("rbac:manage")) return Promise.resolve(problem(403, "forbidden", "missing permission rbac:manage"));
        return Promise.resolve(roles ? roles() : json({ roles: [{ name: "db-ops", permissions: [] }] }));
      }
      if (url.startsWith("/api/v1/auth/me")) {
        return Promise.resolve(json({ subject: { kind: "user", id: "u-1", displayName: "Root", groups: [], roles: ["admin"] }, permissions }));
      }
      return Promise.resolve(json({}));
    }),
  );
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <LocaleProvider>
        <TimeMachineProvider>
          <UsersSection />
        </TimeMachineProvider>
      </LocaleProvider>
    </QueryClientProvider>,
  );
  return calls;
}

const writes = (calls: Call[]) => calls.filter((c) => c.init?.method && c.init.method !== "GET");

const root = { id: "u-1", username: "root", displayName: "Root", roles: ["admin"], disabled: false, createdAt: "2026-09-24T00:00:00Z" };
const ops = { id: "u-2", username: "ops", displayName: "Ops", roles: ["viewer"], disabled: false, createdAt: "2026-09-24T00:00:00Z" };

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("UsersSection", () => {
  it("lists users with their roles and marks the signed-in one", async () => {
    setup([root, ops]);
    const table = await screen.findByRole("table", { name: "Local users" });
    expect(within(table).getByText("root")).toBeInTheDocument();
    expect(within(table).getByText("ops")).toBeInTheDocument();
    expect(within(table).getByLabelText("Role of ops")).toHaveValue("viewer");
    expect(await within(table).findByText("you")).toBeInTheDocument();
  });

  it("offers the built-in roles and the custom ones", async () => {
    setup([root]);
    const select = await screen.findByLabelText("Role of root");
    await waitFor(() => expect(within(select).getAllByRole("option").map((o) => o.textContent)).toContain("db-ops"));
    expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual(
      expect.arrayContaining(["viewer", "operator", "alert-editor", "admin"]),
    );
  });

  it("says why only the built-in roles are offered to a users:manage holder without rbac:manage", async () => {
    const calls = setup([root], undefined, { permissions: ["users:manage"] });
    expect(await screen.findByText(/Custom roles are not listed: reading them needs rbac:manage/)).toBeInTheDocument();
    const select = await screen.findByLabelText("Role of root");
    expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual(["viewer", "operator", "alert-editor", "admin"]);
    expect(calls.some((c) => c.url.startsWith("/api/v1/rbac/roles"))).toBe(false);
  });

  it("says so, with a retry, when the custom roles cannot be read", async () => {
    let fail = true;
    setup([root], undefined, {
      roles: () => (fail ? problem(502, "bad gateway", "store unavailable") : json({ roles: [{ name: "db-ops", permissions: [] }] })),
    });
    expect(await screen.findByText(/store unavailable/)).toBeInTheDocument();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    const select = await screen.findByLabelText("Role of root");
    await waitFor(() => expect(within(select).getAllByRole("option").map((o) => o.textContent)).toContain("db-ops"));
    expect(screen.queryByText(/store unavailable/)).toBeNull();
  });

  it("creates a user, and refuses a short password before anything is sent", async () => {
    const calls = setup([root], () => json({ ...ops, username: "bob", id: "u-3" }, 201));
    fireEvent.click(await screen.findByRole("button", { name: "Add user" }));
    fireEvent.change(screen.getByLabelText("Username"), { target: { value: "bob" } });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "short" } });
    fireEvent.click(screen.getByRole("button", { name: "Create user" }));
    expect(await screen.findByText("The password needs at least 12 characters.")).toBeInTheDocument();
    expect(writes(calls)).toHaveLength(0);

    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "a long enough secret" } });
    fireEvent.click(screen.getByRole("button", { name: "Create user" }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    const post = writes(calls)[0];
    expect(post.url).toBe("/api/v1/users");
    expect(JSON.parse(String(post.init?.body))).toEqual({ username: "bob", password: "a long enough secret", role: "viewer" });
  });

  it("refuses a username the server would refuse", async () => {
    const calls = setup([root]);
    fireEvent.click(await screen.findByRole("button", { name: "Add user" }));
    fireEvent.change(screen.getByLabelText("Username"), { target: { value: "bob smith" } });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "a long enough secret" } });
    fireEvent.click(screen.getByRole("button", { name: "Create user" }));
    expect(await screen.findByText(/letters, digits and \. _ @ - only/)).toBeInTheDocument();
    expect(writes(calls)).toHaveLength(0);
  });

  it("asks before disabling, and shows the server's reason when it is the last admin", async () => {
    const calls = setup([root], () =>
      problem(409, "last administrator", "this is the last enabled user who can manage users; grant users:manage to someone else first"),
    );
    fireEvent.click(await screen.findByRole("button", { name: "Disable root" }));
    expect(writes(calls)).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: /Confirm disabling root/ }));
    expect(await screen.findByText(/last enabled user who can manage users/)).toBeInTheDocument();
    expect(JSON.parse(String(writes(calls)[0].init?.body))).toEqual({ disabled: true });
  });

  /* Disabled test accounts piled up for good: there was no way to remove one. */
  it("deletes a user after a confirm, and re-reads the list", async () => {
    const gone = { ...ops, disabled: true };
    const calls = setup([root, gone]);
    fireEvent.click(await screen.findByRole("button", { name: "Delete ops" }));
    expect(writes(calls)).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: /Confirm deleting ops/ }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect(writes(calls)[0].url).toBe("/api/v1/users/u-2");
    expect(writes(calls)[0].init?.method).toBe("DELETE");
    await waitFor(() => expect(calls.filter((c) => c.url === "/api/v1/users" && !c.init?.method).length).toBeGreaterThan(1));
  });

  it("shows the server's reason when the user to delete is the last admin", async () => {
    setup([root], () =>
      problem(409, "last administrator", "this is the last enabled user who can manage users; grant users:manage to someone else first"),
    );
    fireEvent.click(await screen.findByRole("button", { name: "Delete root" }));
    fireEvent.click(screen.getByRole("button", { name: /Confirm deleting root/ }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/last enabled user who can manage users/);
    expect(screen.getByText("root")).toBeInTheDocument();
  });

  it("keeps the user and says why when their tokens could not be revoked (502)", async () => {
    const calls = setup([root, ops], () =>
      problem(502, "tokens unavailable", "failed to revoke the user's API tokens; the user was not deleted"),
    );
    fireEvent.click(await screen.findByRole("button", { name: "Delete ops" }));
    fireEvent.click(screen.getByRole("button", { name: /Confirm deleting ops/ }));
    expect(await screen.findByRole("alert")).toHaveTextContent("the user was not deleted");
    expect(writes(calls)).toHaveLength(1);
    expect(screen.getByText("ops")).toBeInTheDocument();
  });

  it("changes a role in place", async () => {
    const calls = setup([root, ops], () => json({ ...ops, roles: ["operator"] }));
    fireEvent.change(await screen.findByLabelText("Role of ops"), { target: { value: "operator" } });
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect(writes(calls)[0].url).toBe("/api/v1/users/u-2");
    expect(JSON.parse(String(writes(calls)[0].init?.body))).toEqual({ role: "operator" });
  });

  it("resets a password and says the user is signed out", async () => {
    const calls = setup([root, ops]);
    fireEvent.click(await screen.findByRole("button", { name: "Reset the password of ops" }));
    const input = screen.getByLabelText("New password for ops");
    fireEvent.change(input, { target: { value: "short" } });
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));
    expect(await screen.findByText("The password needs at least 12 characters.")).toBeInTheDocument();
    expect(writes(calls)).toHaveLength(0);

    fireEvent.change(input, { target: { value: "a brand new secret" } });
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));
    expect(await screen.findByText("Password set. ops is signed out of every session.")).toBeInTheDocument();
    expect(writes(calls)[0].url).toBe("/api/v1/users/u-2/password");
    expect(screen.queryByLabelText("New password for ops")).toBeNull();
  });
});

/* A reset on your own row would end the session you are using: the server re-issues a session only
   for the self-service change. And a role without users:manage takes this section away from you. */
describe("UsersSection, the signed-in user's own row", () => {
  const meCalls = (calls: Call[]) => calls.filter((c) => c.url.startsWith("/api/v1/auth/me"));

  it("offers Change password instead of a reset, and only on that row", async () => {
    const calls = setup([root, ops]);
    const table = await screen.findByRole("table", { name: "Local users" });
    await within(table).findByText("you");
    expect(screen.queryByRole("button", { name: "Reset the password of root" })).toBeNull();
    expect(screen.getByRole("button", { name: "Reset the password of ops" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /change your own password/i }));
    expect(await screen.findByRole("dialog", { name: "Change password" })).toBeInTheDocument();
    expect(screen.getByLabelText("Current password")).toBeInTheDocument();
    expect(writes(calls)).toHaveLength(0);
  });

  it("asks before changing your own role, then re-reads who you are", async () => {
    const calls = setup([root, ops], () => json({ ...root, roles: ["operator"] }));
    await within(await screen.findByRole("table", { name: "Local users" })).findByText("you");
    const before = meCalls(calls).length;

    fireEvent.change(screen.getByLabelText("Role of root"), { target: { value: "operator" } });
    expect(writes(calls)).toHaveLength(0);
    expect(screen.getByText(/This is your own account/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Confirm changing your own role to operator" }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect(writes(calls)[0].url).toBe("/api/v1/users/u-1");
    expect(JSON.parse(String(writes(calls)[0].init?.body))).toEqual({ role: "operator" });
    await waitFor(() => expect(meCalls(calls).length).toBeGreaterThan(before));
    expect(screen.queryByText(/This is your own account/)).toBeNull();
  });

  it("puts your role back and sends nothing when the change is cancelled", async () => {
    const calls = setup([root, ops]);
    await within(await screen.findByRole("table", { name: "Local users" })).findByText("you");
    const select = screen.getByLabelText("Role of root");

    fireEvent.change(select, { target: { value: "viewer" } });
    expect(select).toHaveValue("viewer");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(select).toHaveValue("admin");
    expect(screen.queryByRole("button", { name: /Confirm changing your own role/ })).toBeNull();
    expect(writes(calls)).toHaveLength(0);
  });
});

describe("UsersSection, a user without a role", () => {
  it("reads as roleless rather than as the first role, and any role can be picked", async () => {
    const bare = { id: "u-3", username: "bare", displayName: "", roles: [], disabled: false, createdAt: "2026-09-24T00:00:00Z" };
    const calls = setup([root, bare]);
    const select = (await screen.findByLabelText("Role of bare")) as HTMLSelectElement;
    expect(select.value).toBe("");
    fireEvent.change(select, { target: { value: "viewer" } });
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect(JSON.parse(String(writes(calls)[0]?.init?.body))).toEqual({ role: "viewer" });
  });
});

/* ── WB13: the page on a 375px phone and in the light theme ──────────────── */
describe("UsersSection — on a phone and in the light theme", () => {
  afterEach(() => {
    restoreViewport();
    resetTheme();
  });

  it("keeps everything wider than a 375px phone inside a scroller of its own", async () => {
    emulatePhone();
    setup([root, ops]);
    await screen.findByRole("table", { name: "Local users" });
    expect(phoneOverflowHazards(document.body)).toEqual([]);
  });

  it("draws every colour from a token the light theme restyles", async () => {
    startInLight();
    setup([root, { ...ops, disabled: true }]);
    await screen.findByRole("table", { name: "Local users" });
    expect(lightThemeHazards(document.body)).toEqual([]);
  });
});
