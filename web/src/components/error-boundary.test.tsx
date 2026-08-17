import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useRef } from "react";
import { ThemeProvider } from "@/components/theme-provider";
import { RouteErrorBoundary } from "@/components/error-boundary";
import { PageShell } from "@/components/page-shell";
import { LocaleProvider, useLocale } from "@/lib/i18n";

function Boom() {
  throw new Error("render exploded");
  return null;
}

afterEach(cleanup);

describe("RouteErrorBoundary", () => {
  it("renders a fallback with the error name, not a blank, when a child throws", () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    render(
      <ThemeProvider>
        <LocaleProvider>
          <RouteErrorBoundary resetKey="/matrix">
            <Boom />
          </RouteErrorBoundary>
        </LocaleProvider>
      </ThemeProvider>,
    );
    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(screen.getByText(/render exploded/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /reload/i })).toBeInTheDocument();
    spy.mockRestore();
  });
});

describe("PageShell does not remount its subtree on a language switch", () => {
  function Counter() {
    const renders = useRef(0);
    renders.current += 1;
    const { locale, setLocale } = useLocale();
    return (
      <div>
        <span data-testid="mounts">{renders.current}</span>
        <button onClick={() => setLocale(locale === "en" ? "ru" : "en")}>flip</button>
      </div>
    );
  }

  it("keeps a child mounted (its render ref survives) when the locale flips", async () => {
    const root = createRootRoute({
      component: () => (
        <PageShell title="Matrix">
          <Counter />
        </PageShell>
      ),
    });
    const router = createRouter({
      routeTree: root.addChildren([createRoute({ getParentRoute: () => root, path: "/", component: () => null })]),
      history: createMemoryHistory({ initialEntries: ["/"] }),
    });
    render(
      <ThemeProvider>
        <LocaleProvider>
          <RouterProvider router={router} />
        </LocaleProvider>
      </ThemeProvider>,
    );
    const counter = await screen.findByTestId("mounts");
    const before = Number(counter.textContent);
    fireEvent.click(screen.getByText("flip"));
    // A remount would reset the render ref to 1; a re-render only increments it.
    expect(Number(screen.getByTestId("mounts").textContent)).toBeGreaterThan(before);
  });
});
