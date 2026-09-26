import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { AnonymousBanner } from "@/components/anonymous-banner";

test("shows the anonymous-mode warning", () => {
  render(<AnonymousBanner mode="anonymous" />);
  expect(screen.getByRole("status")).toHaveTextContent(/anonymous mode/i);
  expect(screen.getByRole("status")).toHaveTextContent(/do not use in production/i);
});

describe("AnonymousBanner mode prop", () => {
  it("hides for a non-anonymous mode", () => {
    render(<AnonymousBanner mode="local" />);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("shows for mode=anonymous", () => {
    render(<AnonymousBanner mode="anonymous" />);
    expect(screen.getByRole("status")).toHaveTextContent(/anonymous mode/i);
  });
});

/* "the fixed role" tells an operator nothing they can act on; the role's NAME
   is on GET /api/v1/config. */
describe("AnonymousBanner role prop", () => {
  it("names the role everyone has", () => {
    render(<AnonymousBanner mode="anonymous" role="admin" />);
    expect(screen.getByRole("status")).toHaveTextContent(
      "everyone has the admin role (console.auth.anonymous.role)",
    );
  });

  it("falls back to the unnamed wording rather than printing a gap", () => {
    render(<AnonymousBanner mode="anonymous" role="" />);
    expect(screen.getByRole("status")).toHaveTextContent("everyone has the fixed role");
    expect(screen.getByRole("status")).not.toHaveTextContent("console.auth.anonymous.role");
  });
});

/* Below sm the strip shows one clause instead of the sentence. Both forms are in the DOM and CSS
   picks one, so this pins what jsdom can see: the clause, and the full sentence riding on title. */
describe("AnonymousBanner narrow-width clause", () => {
  it("carries the short clause next to the sentence, and the sentence on title", () => {
    render(<AnonymousBanner mode="anonymous" role="admin" />);
    const banner = screen.getByRole("status");
    expect(banner).toHaveTextContent("Authentication is disabled; everyone is admin.");
    expect(banner).toHaveAttribute(
      "title",
      "Anonymous mode. Authentication is disabled — everyone has the admin role (console.auth.anonymous.role). Do not use in production.",
    );
    expect(screen.getByText("Authentication is disabled; everyone is admin.")).toHaveClass("sm:hidden");
  });

  it("keeps the role out of the clause when the config carried none", () => {
    render(<AnonymousBanner mode="anonymous" role="" />);
    expect(screen.getByRole("status")).toHaveTextContent("Authentication is disabled; everyone has one fixed role.");
  });
});
