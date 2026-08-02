import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { RealtimeBadge } from "./realtime-badge";

describe("RealtimeBadge", () => {
  it("shows Live on the ok health token when realtime is up", () => {
    render(<RealtimeBadge realtime />);
    const badge = screen.getByText("Live");
    expect(badge.querySelector("span")?.className).toContain("bg-health-ok");
    expect(badge.getAttribute("title")).toMatch(/pushed/i);
  });

  it("shows Delayed data on the warn health token and explains the fallback", () => {
    render(<RealtimeBadge realtime={false} />);
    const badge = screen.getByText("Delayed data");
    expect(badge.querySelector("span")?.className).toContain("bg-health-warn");
    expect(badge.getAttribute("title")).toMatch(/polling/i);
  });
});
