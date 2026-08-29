import { existsSync, readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { DOCS_BASE_URL, PageHelp, docsConsoleUrl } from "@/components/page-help";

afterEach(cleanup);

/**
 * page-help.test.tsx — the "?" affordance (M7-5) in three layers:
 *
 *   1. the component's own contract: a labelled button, a labelled dialog, the
 *      body and the docs link inside it, Escape out with focus returned;
 *   2. the URL constant: one base, one shape, so the docs site can move by
 *      editing one line;
 *   3. a source sweep: every slug a page wires must name a REAL file under the
 *      repo's docs/console/ — the link is only worth its pixels while it lands
 *      on a page that exists — and exactly the 12 nav pages wire one (detail
 *      routes carry data titles, not page names, and stay bare).
 *
 * Rendered with no LocaleProvider on purpose: English is the default, which is
 * the property every other component test here leans on too.
 */

describe("PageHelp — the '?' after a page title", () => {
  it("renders a small labelled button, and no dialog until it is pressed", () => {
    render(<PageHelp body="Three sentences about the page." slug="matrix" />);
    const button = screen.getByRole("button", { name: "About this page" });
    expect(button).toHaveAttribute("aria-haspopup", "dialog");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("opens a labelled dialog carrying the page's own body", () => {
    render(<PageHelp body="Three sentences about the page." slug="matrix" />);
    fireEvent.click(screen.getByRole("button", { name: "About this page" }));
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveAccessibleName("About this page");
    expect(screen.getByText("Three sentences about the page.")).toBeInTheDocument();
  });

  it("points 'Learn more' at THIS page's docs chapter, opening in a new tab", () => {
    render(<PageHelp body="B." slug="routes-mtr" />);
    fireEvent.click(screen.getByRole("button", { name: "About this page" }));
    const link = screen.getByRole("link", { name: "Learn more in the docs" });
    expect(link).toHaveAttribute("href", "https://esdmitrii.github.io/kconmon-ng/console/routes-mtr/");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noreferrer");
  });

  it("closes on Escape and hands focus back to the '?' that opened it", () => {
    render(<PageHelp body="B." slug="matrix" />);
    const button = screen.getByRole("button", { name: "About this page" });
    /* jsdom's click does not move focus on its own; a keyboard user's focus IS
       on the button when the dialog opens, which is what the return asserts. */
    button.focus();
    fireEvent.click(button);
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(button).toHaveFocus();
  });
});

describe("the docs URL — one constant, one shape", () => {
  it("DOCS_BASE_URL ends with a slash, so joining neither doubles nor drops one", () => {
    expect(DOCS_BASE_URL.endsWith("/")).toBe(true);
  });

  it("docsConsoleUrl lands under console/ with a trailing slash (MkDocs directory URLs)", () => {
    expect(docsConsoleUrl("overview")).toBe(`${DOCS_BASE_URL}console/overview/`);
  });
});

/* ── the sweep: wired slugs vs the real docs tree ─────────────────────────── */

const PAGES = join(__dirname, "..", "pages");
const DOCS_CONSOLE = join(__dirname, "..", "..", "..", "docs", "console");

/** The intended wiring, page file → docs/console slug. A page added to the nav
 *  without a row here (or vice versa) fails loudly with both names in hand. */
const WIRED: Record<string, string> = {
  "overview.tsx": "overview",
  "live.tsx": "events",
  "matrix.tsx": "matrix",
  "topology.tsx": "topology",
  "investigate.tsx": "incidents",
  "mtr.tsx": "routes-mtr",
  "diagnostics.tsx": "run-checks",
  "explore.tsx": "metrics",
  "promql-console.tsx": "promql",
  "targets.tsx": "scheduled-checks",
  "alerting.tsx": "alerting",
  "settings.tsx": "settings",
};

/** Matches the one wiring shape the pages use; source text rather than a
 *  render because the claim is about every page at once, routers and all
 *  (the timemachine-surfaces.test.ts precedent). */
function wiredSlugs(): Record<string, string> {
  const out: Record<string, string> = {};
  for (const name of readdirSync(PAGES).filter((f) => f.endsWith(".tsx") && !f.includes(".test."))) {
    const match = /help=\{\{ body: t\("help\.body"\), slug: "([a-z0-9-]+)" \}\}/.exec(
      readFileSync(join(PAGES, name), "utf8"),
    );
    if (match) out[name] = match[1];
  }
  return out;
}

describe("the wired slugs", () => {
  it("cover exactly the 12 nav pages — detail routes stay bare", () => {
    expect(wiredSlugs()).toEqual(WIRED);
  });

  it("each name a real file under docs/console/", () => {
    const missing = Object.entries(wiredSlugs())
      .filter(([, slug]) => !existsSync(join(DOCS_CONSOLE, `${slug}.md`)))
      .map(([file, slug]) => `${file} → docs/console/${slug}.md`);
    expect(missing).toEqual([]);
  });
});
