import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { ApiError } from "@/lib/api";
import { ChartCursorProvider } from "@/lib/chart-cursor";
import type { Problem } from "@/lib/types";
import { SignalPanels } from "./investigation-signals";

vi.mock("@/components/echart", () => ({ EChart: () => <div data-testid="echart" /> }));

/* A proxy may answer problem+json with neither detail nor title; the refusal line still says something. */
describe("SignalPanels refusals", () => {
  it("names a refusal whose problem document carries no words of its own", () => {
    render(
      <ThemeProvider>
        <ChartCursorProvider>
          <SignalPanels
            scopeLabel="node-a → node-b"
            loss={undefined}
            rtt={undefined}
            delta={{ before: null, after: null, delta: null }}
            deltaError={new ApiError({ type: "about:blank", status: 500 } as Problem)}
            windows={[]}
            annotations={[]}
            promConfigured
            gated={false}
            rangeTooWide={false}
          />
        </ChartCursorProvider>
      </ThemeProvider>,
    );
    expect(screen.getByTestId("matrix-delta")).toHaveTextContent("The query could not be run.");
  });
});
