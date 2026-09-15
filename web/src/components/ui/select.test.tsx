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

  /* A <select>'s intrinsic width is its widest option, and one long option
     pushed a phone-width form past the viewport. The field base caps it. */
  it("can never be wider than its box", () => {
    renderSelect();
    expect(screen.getByRole("combobox")).toHaveClass("min-w-0", "max-w-full");
  });

  it("field variant: no wrapper, the select is the element rendered", () => {
    renderSelect();
    expect(screen.getByRole("combobox").parentElement?.tagName).toBe("DIV");
  });
});

/* The toolbar idiom the Live feed hand-rolled, now one variant: the platform
   picker stays native, only the closed face matches the 40px Segmented track. */
describe("Select — filter variant", () => {
  function renderFilter(props: React.ComponentProps<typeof Select> = {}) {
    render(
      <Select aria-label="type" variant="filter" value="all" onChange={() => {}} {...props}>
        <option value="all">All types</option>
        <option value="pair">pair</option>
      </Select>,
    );
    return screen.getByRole("combobox");
  }

  it("dresses the closed face like the Segmented track and keeps the native control", () => {
    const select = renderFilter();
    expect(select.tagName).toBe("SELECT");
    expect(select).toHaveClass("h-10", "appearance-none", "rounded-md", "border-0", "bg-surface-2", "pl-3.5", "pr-8", "text-sm");
    expect(select).toHaveClass("min-w-0", "max-w-full", "focus-visible:ring-2", "focus-visible:ring-ring");
    expect(select).not.toHaveClass("h-9", "border", "border-border-strong", "bg-transparent");
  });

  it("wraps the control so a real chevron can sit over the UA arrow", () => {
    const select = renderFilter();
    const wrapper = select.parentElement!;
    expect(wrapper.tagName).toBe("SPAN");
    expect(wrapper).toHaveClass("relative", "inline-flex");
    const chevron = wrapper.querySelector("svg");
    expect(chevron).not.toBeNull();
    expect(chevron).toHaveAttribute("aria-hidden", "true");
    expect(chevron).toHaveClass("pointer-events-none", "absolute");
  });

  it("merges a call-site className onto the select itself", () => {
    expect(renderFilter({ className: "w-40" })).toHaveClass("w-40", "h-10");
  });
});
