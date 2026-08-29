import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { EmptyState } from "@/components/ui/empty-state";

afterEach(cleanup);

/**
 * empty-state.test.tsx — the BlankSlate pattern lifted from overview.tsx.
 * The default rendering must stay pixel-identical to what overview draws
 * today, so those classes are pinned literally.
 */

describe("EmptyState — the BlankSlate look", () => {
  it("renders the centered slate with title and body in today's classes", () => {
    const { container } = render(<EmptyState title="No pairs measured" body="Agents have not reported yet." />);
    const root = container.firstElementChild;
    expect(root).toHaveClass("flex", "flex-col", "items-center", "gap-2", "px-6", "py-10", "text-center");

    const title = screen.getByText("No pairs measured");
    expect(title).toHaveClass("text-sm", "font-medium");

    const body = screen.getByText("Agents have not reported yet.");
    expect(body).toHaveClass("max-w-sm", "text-xs", "leading-relaxed", "text-muted-foreground");
  });

  it("draws the icon circle as decoration, invisible to screen readers", () => {
    const { container } = render(<EmptyState title="t" body="b" />);
    const circle = container.querySelector('span[aria-hidden="true"]');
    expect(circle).not.toBeNull();
    expect(circle).toHaveClass(
      "mb-1",
      "flex",
      "size-10",
      "items-center",
      "justify-center",
      "rounded-full",
      "bg-surface-2",
      "text-muted-foreground",
    );
    // The default glyph: circle + dash, exactly what BlankSlate drew.
    expect(circle!.querySelector("svg circle")).not.toBeNull();
  });

  it("accepts a custom icon in place of the default glyph", () => {
    const { container } = render(<EmptyState title="t" icon={<span data-testid="my-icon" />} />);
    expect(screen.getByTestId("my-icon")).toBeInTheDocument();
    expect(container.querySelector("svg circle")).toBeNull();
  });

  it("renders the CTA slot only when given", () => {
    render(<EmptyState title="t" body="b" action={<button>Run a check</button>} />);
    expect(screen.getByRole("button", { name: "Run a check" })).toBeInTheDocument();
    cleanup();

    const { container } = render(<EmptyState title="t" body="b" />);
    expect(container.querySelector("button")).toBeNull();
    // No stray action wrapper changing the slate's rhythm.
    expect(container.querySelectorAll("div.mt-2")).toHaveLength(0);
  });

  it("omits the body paragraph entirely when body is not given", () => {
    const { container } = render(<EmptyState title="only a title" />);
    expect(container.querySelectorAll("p")).toHaveLength(1);
  });

  it("passes through className and native div props", () => {
    render(<EmptyState title="t" data-testid="slate" className="py-6" />);
    const slate = screen.getByTestId("slate");
    expect(slate).toHaveClass("py-6");
    expect(slate).not.toHaveClass("py-10");
  });
});
