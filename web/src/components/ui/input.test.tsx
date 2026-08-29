import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Input, Textarea, fieldClasses } from "@/components/ui/input";

afterEach(cleanup);

/**
 * input.test.tsx — the shared field look that replaces the three verbatim
 * fieldClasses copies (targets/alerting/settings), plus the ring treatment
 * borrowed from button.tsx. Pinned through class names: jsdom cannot draw a
 * ring, but it can catch the ring CLASSES disappearing.
 */

describe("Input — the field look", () => {
  it("carries the field base: h-9, rounded, transparent, 13px", () => {
    render(<Input aria-label="name" />);
    expect(screen.getByRole("textbox")).toHaveClass(
      "h-9",
      "rounded-md",
      "border",
      "bg-transparent",
      "px-3",
      "text-[13px]",
      "border-border-strong",
    );
  });

  it("mirrors button.tsx's focus ring and adds a hover border step", () => {
    render(<Input aria-label="name" />);
    const input = screen.getByRole("textbox");
    expect(input).toHaveClass(
      "focus-visible:outline-none",
      "focus-visible:ring-2",
      "focus-visible:ring-ring",
      "focus-visible:ring-offset-2",
      "focus-visible:ring-offset-background",
    );
    expect(input).toHaveClass("hover:border-muted-foreground/50");
    expect(input.className).toContain("transition-[border-color,box-shadow]");
  });

  it("stays a drop-in for native input props", () => {
    const onChange = vi.fn();
    render(<Input aria-label="name" value="node-a" onChange={onChange} placeholder="node" />);
    const input = screen.getByRole("textbox");
    expect(input).toHaveValue("node-a");
    expect(input).toHaveAttribute("placeholder", "node");
    fireEvent.change(input, { target: { value: "node-b" } });
    expect(onChange).toHaveBeenCalledTimes(1);
  });

  it("merges a call-site className on top of the base", () => {
    render(<Input aria-label="q" className="font-mono w-64" />);
    expect(screen.getByRole("textbox")).toHaveClass("font-mono", "w-64", "h-9");
  });
});

describe("Input — invalid state", () => {
  it("swaps the border to health-bad and announces aria-invalid", () => {
    render(<Input aria-label="name" invalid />);
    const input = screen.getByRole("textbox");
    expect(input).toHaveClass("border-health-bad");
    expect(input).not.toHaveClass("border-border-strong");
    expect(input).toHaveAttribute("aria-invalid", "true");
  });

  it("does not fade the error border under the pointer", () => {
    render(<Input aria-label="name" invalid />);
    expect(screen.getByRole("textbox")).not.toHaveClass("hover:border-muted-foreground/50");
  });

  it("treats a caller's own aria-invalid as invalid too", () => {
    render(<Input aria-label="name" aria-invalid />);
    expect(screen.getByRole("textbox")).toHaveClass("border-health-bad");
  });

  it("emits no aria-invalid when valid", () => {
    render(<Input aria-label="name" />);
    expect(screen.getByRole("textbox")).not.toHaveAttribute("aria-invalid");
  });
});

describe("Textarea", () => {
  it("shares the field look but takes its height from rows", () => {
    render(<Textarea aria-label="body" rows={3} />);
    const area = screen.getByRole("textbox");
    expect(area.tagName).toBe("TEXTAREA");
    expect(area).toHaveClass("h-auto", "py-2", "rounded-md", "border-border-strong");
    expect(area).not.toHaveClass("h-9");
  });

  it("flags invalid the same way as Input", () => {
    render(<Textarea aria-label="body" invalid />);
    const area = screen.getByRole("textbox");
    expect(area).toHaveClass("border-health-bad");
    expect(area).toHaveAttribute("aria-invalid", "true");
  });
});

describe("fieldClasses — the exported builder", () => {
  it("keeps the invalid/valid border switch for odd elements", () => {
    expect(fieldClasses(false)).toContain("border-border-strong");
    expect(fieldClasses(true)).toContain("border-health-bad");
    expect(fieldClasses(true)).not.toContain("border-border-strong");
  });
});
