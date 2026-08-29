import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Select } from "@/components/ui/select";

afterEach(cleanup);

/** select.test.tsx — the native select in the shared field look (input.tsx). */

describe("Select", () => {
  function renderSelect(props: React.ComponentProps<typeof Select> = {}) {
    const onChange = vi.fn();
    render(
      <Select aria-label="protocol" value="tcp" onChange={onChange} {...props}>
        <option value="tcp">TCP</option>
        <option value="udp">UDP</option>
      </Select>,
    );
    return onChange;
  }

  it("renders a native select wearing the field base", () => {
    renderSelect();
    const select = screen.getByRole("combobox");
    expect(select.tagName).toBe("SELECT");
    expect(select).toHaveClass("h-9", "rounded-md", "border", "bg-transparent", "px-3", "text-[13px]");
    expect(select).toHaveClass("border-border-strong", "focus-visible:ring-2", "focus-visible:ring-ring");
  });

  it("keeps a disabled picker readable, not invisible", () => {
    renderSelect({ disabled: true });
    const select = screen.getByRole("combobox");
    expect(select).toBeDisabled();
    expect(select).toHaveClass("disabled:opacity-70", "disabled:cursor-not-allowed");
  });

  it("stays a drop-in for native select props", () => {
    const onChange = renderSelect();
    const select = screen.getByRole("combobox");
    expect(select).toHaveValue("tcp");
    fireEvent.change(select, { target: { value: "udp" } });
    expect(onChange).toHaveBeenCalledTimes(1);
  });

  it("flags invalid with the health-bad border and aria-invalid", () => {
    renderSelect({ invalid: true });
    const select = screen.getByRole("combobox");
    expect(select).toHaveClass("border-health-bad");
    expect(select).not.toHaveClass("border-border-strong");
    expect(select).toHaveAttribute("aria-invalid", "true");
  });

  it("merges a call-site className", () => {
    renderSelect({ className: "w-40" });
    expect(screen.getByRole("combobox")).toHaveClass("w-40", "h-9");
  });
});
