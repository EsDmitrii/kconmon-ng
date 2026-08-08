import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it } from "vitest";
import { ResultTabs } from "./promql-console";

/**
 * M7 Task 12b (plan Decision 12). The Console's result switcher is the one
 * place in this codebase that declared role="tablist" without honouring the
 * pattern: three separate tab stops, no arrow keys, and three panels with no
 * role. Every other switcher on this very page is a Segmented radiogroup with
 * a roving tabindex, so the bar these cases hold the strip to is the repo's
 * own (components/ui/segmented.tsx), not an abstract checklist.
 *
 * ResultTabs is driven directly rather than through PromQLConsolePage: the
 * page mounts CodeMirror and ECharts, neither of which renders comfortably in
 * jsdom — the same reason pages/topology.tsx exports nodeNavigationPath.
 */

type Tab = "table" | "chart" | "json";

/** The strip is controlled, so a pin needs the state its page holds. */
function Harness({ chartDisabled = false }: { chartDisabled?: boolean }) {
  const [active, setActive] = useState<Tab>("table");
  return (
    <ResultTabs
      active={active}
      onChange={setActive}
      isDisabled={(id) => chartDisabled && id === "chart"}
    />
  );
}

const tab = (name: string) => screen.getByRole("tab", { name });

afterEach(cleanup);

describe("Console result tabs", () => {
  it("is ONE tab stop: only the selected tab is reachable by Tab", () => {
    render(<Harness />);
    expect(tab("Table")).toHaveAttribute("tabindex", "0");
    expect(tab("Chart")).toHaveAttribute("tabindex", "-1");
    expect(tab("JSON")).toHaveAttribute("tabindex", "-1");
  });

  it("names the panel each tab reveals", () => {
    render(<Harness />);
    expect(tab("Table")).toHaveAttribute("aria-controls", "promql-result-panel-table");
    expect(tab("Chart")).toHaveAttribute("aria-controls", "promql-result-panel-chart");
    expect(tab("JSON")).toHaveAttribute("aria-controls", "promql-result-panel-json");
    // The other half of the wiring: the panel points back at its own tab, so
    // the id the tab claims has to be the id the tab carries.
    expect(tab("Table")).toHaveAttribute("id", "promql-result-tab-table");
  });

  it("moves selection AND focus with the arrow keys", () => {
    render(<Harness />);
    fireEvent.keyDown(tab("Table"), { key: "ArrowRight" });
    expect(tab("Chart")).toHaveAttribute("aria-selected", "true");
    expect(tab("Chart")).toHaveFocus();
    expect(tab("Table")).toHaveAttribute("tabindex", "-1");
  });

  it("wraps at both ends", () => {
    render(<Harness />);
    fireEvent.keyDown(tab("Table"), { key: "ArrowLeft" });
    expect(tab("JSON")).toHaveAttribute("aria-selected", "true");
    fireEvent.keyDown(tab("JSON"), { key: "ArrowRight" });
    expect(tab("Table")).toHaveAttribute("aria-selected", "true");
  });

  it("honours Home and End", () => {
    render(<Harness />);
    fireEvent.keyDown(tab("Table"), { key: "End" });
    expect(tab("JSON")).toHaveAttribute("aria-selected", "true");
    fireEvent.keyDown(tab("JSON"), { key: "Home" });
    expect(tab("Table")).toHaveAttribute("aria-selected", "true");
  });

  /* Chart is disabled for instant queries. A disabled button cannot take
     focus, so an arrow key that selected it would move selection to an element
     the keyboard can never reach — the strip steps over it instead. */
  it("steps over a disabled tab instead of stranding focus on it", () => {
    render(<Harness chartDisabled />);
    expect(tab("Chart")).toBeDisabled();
    fireEvent.keyDown(tab("Table"), { key: "ArrowRight" });
    expect(tab("JSON")).toHaveAttribute("aria-selected", "true");
    expect(tab("JSON")).toHaveFocus();
    expect(tab("Chart")).toHaveAttribute("aria-selected", "false");
  });

  it("still switches on click", () => {
    render(<Harness />);
    fireEvent.click(tab("JSON"));
    expect(tab("JSON")).toHaveAttribute("aria-selected", "true");
  });
});
