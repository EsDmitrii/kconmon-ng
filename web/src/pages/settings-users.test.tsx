import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { LocaleProvider } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import { UsersSection } from "./settings-users";

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

/** A refusal as the server writes it: RFC 7807, which is what lib/api reads the detail out of. */
const problem = (status: number, title: string, detail: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

type Call = { url: string; init?: RequestInit };

/** Answers the reads, and hands every write to onWrite (204 when there is none). */
function setup(users: unknown[], onWrite?: (url: string, init: RequestInit) => Response): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init?: RequestInit) => {
      calls.push({ url, init });
      if (init?.method && init.method !== "GET") {
        return Promise.resolve(onWrite ? onWrite(url, init) : new Response(null, { status: 204 }));
      }
      if (url.startsWith("/api/v1/users")) return Promise.resolve(json({ users }));
      if (url.startsWith("/api/v1/rbac/roles")) return Promise.resolve(json({ roles: [{ name: "db-ops", permissions: [] }] }));
      if (url.startsWith("/api/v1/auth/me")) {
        return Promise.resolve(json({ subject: { kind: "user", id: "u-1", displayName: "Root", groups: [], roles: ["admin"] }, permissions: ["users:manage"] }));
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
