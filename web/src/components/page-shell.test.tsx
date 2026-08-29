import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { PageShell } from "@/components/page-shell";

afterEach(cleanup);

const outerOf = (container: HTMLElement) => container.firstElementChild as HTMLElement;

describe("PageShell variant='page' (the default)", () => {
  /* Pins the pre-variant look class-for-class: M4-5a's contract is that adding
     the variant prop changes NOTHING for the pages that don't pass it. */
  it("keeps the centred max-width column exactly as before the variant existed", () => {
    const { container } = render(
      <PageShell title="Overview" description="All pairs at a glance">
        body
      </PageShell>,
    );
    const outer = outerOf(container);
    expect(outer.className).toBe("mx-auto w-full max-w-[1440px] px-4 py-8 sm:px-8 lg:px-10");
    expect((outer.firstElementChild as HTMLElement).className).toBe("page-enter flex flex-col gap-7");
    const h1 = screen.getByRole("heading", { level: 1, name: "Overview" });
    expect(h1.className).toBe("text-2xl font-semibold tracking-tight");
    // Stacked header: the description sits UNDER the title, not beside it.
    const desc = screen.getByText("All pairs at a glance");
    expect(desc.className).toBe("mt-1 max-w-prose text-sm text-muted-foreground");
    expect(desc.previousElementSibling).toBe(h1);
  });

  it("renders explicit variant='page' byte-identical to omitting the prop", () => {
    const first = render(
      <PageShell title="T" description="D" actions={<button type="button">Act</button>}>
        body
      </PageShell>,
    );
    const defaultHtml = first.container.innerHTML;
    first.unmount();
    const second = render(
      <PageShell variant="page" title="T" description="D" actions={<button type="button">Act</button>}>
        body
      </PageShell>,
    );
    expect(second.container.innerHTML).toBe(defaultHtml);
  });
});

describe("PageShell variant='tool'", () => {
  it("drops the max-width and the centred column so the surface runs edge-to-edge", () => {
    const { container } = render(
      <PageShell variant="tool" title="Matrix">
        grid
      </PageShell>,
    );
    const outer = outerOf(container);
    expect(outer).toHaveClass("w-full");
    expect(outer.className).not.toMatch(/max-w-/);
    expect(outer.className).not.toMatch(/mx-auto/);
  });

  it("puts title and description inline in one slim header row", () => {
    render(
      <PageShell variant="tool" title="Matrix" description="Full mesh">
        grid
      </PageShell>,
    );
    const h1 = screen.getByRole("heading", { level: 1, name: "Matrix" });
    const desc = screen.getByText("Full mesh");
    // Same flex row as the title — not stacked below it like the reading column.
    expect(desc.parentElement).toBe(h1.parentElement);
    expect(h1.parentElement?.className).toMatch(/items-baseline/);
  });

  it("preserves the action slot", () => {
    render(
      <PageShell variant="tool" title="Live" actions={<button type="button">Pause</button>}>
        feed
      </PageShell>,
    );
    expect(screen.getByRole("button", { name: "Pause" })).toBeInTheDocument();
  });

  it("keeps the tool header byte-identical when no help is passed", () => {
    /* Same contract as the variant prop itself carried at birth: an optional
       prop nobody passes changes NOTHING. */
    const { container } = render(
      <PageShell variant="tool" title="Matrix" description="Full mesh">
        grid
      </PageShell>,
    );
    const h1 = screen.getByRole("heading", { level: 1, name: "Matrix" });
    expect(h1.nextElementSibling).toBe(screen.getByText("Full mesh"));
    expect(container.querySelector("button")).toBeNull();
  });

  it("imposes no wrapper of its own around the content — no Card, nothing", () => {
    /* ChartCursorProvider is context-only, so the page's surface must land as a
       DIRECT DOM child of the shell's column. The shell never supplied a Card;
       a page adopting "tool" drops its OWN outer Card to actually go full-bleed. */
    const { container } = render(
      <PageShell variant="tool" title="Topology">
        <section data-testid="surface">map</section>
      </PageShell>,
    );
    const surface = screen.getByTestId("surface");
    expect(surface.parentElement).toBe(outerOf(container).firstElementChild);
  });
});

describe("PageShell help (M7-5) — the '?' after the title, in BOTH variants", () => {
  /* The affordance itself (dialog contract, docs link, Escape/focus) is
     page-help.test.tsx's subject. Here: the shell mounts it next to the title
     in each variant, and mounts nothing without the prop — that last half is
     also pinned by the byte-identical tests above, which render help-less. */
  const help = { body: "What this page is for.", slug: "overview" };

  it("page variant: the '?' sits in one row with the title", () => {
    render(
      <PageShell title="Overview" description="All pairs at a glance" help={help}>
        body
      </PageShell>,
    );
    const h1 = screen.getByRole("heading", { level: 1, name: "Overview" });
    const button = screen.getByRole("button", { name: "About this page" });
    expect(button.parentElement).toBe(h1.parentElement);
    // The description still reads as the stacked line UNDER that row.
    expect(screen.getByText("All pairs at a glance").previousElementSibling).toBe(h1.parentElement);
  });

  it("tool variant: the '?' sits in the slim header row too", () => {
    render(
      <PageShell variant="tool" title="Matrix" description="Full mesh" help={help}>
        grid
      </PageShell>,
    );
    const h1 = screen.getByRole("heading", { level: 1, name: "Matrix" });
    const button = screen.getByRole("button", { name: "About this page" });
    // Between the title and the inline description, inside the same flex row.
    expect(button.closest("span")?.parentElement).toBe(h1.parentElement);
  });

  it("opens the page's own body and docs link from the shell mount", () => {
    render(
      <PageShell title="Overview" help={help}>
        body
      </PageShell>,
    );
    fireEvent.click(screen.getByRole("button", { name: "About this page" }));
    expect(screen.getByText("What this page is for.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Learn more in the docs" })).toHaveAttribute(
      "href",
      "https://esdmitrii.github.io/kconmon-ng/console/overview/",
    );
  });
});
