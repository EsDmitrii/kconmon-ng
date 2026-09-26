import { readFileSync } from "node:fs";
import type { ComponentType } from "react";
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { PAGE_CONTAINER_CLASS } from "@/components/page-shell";
import { router } from "@/routes";

/* The entry chunk carries only the landing page and the login; a static page import in routes.tsx
   would pull ECharts, CodeMirror or React Flow back into index.html without failing anything else. */
describe("routes.tsx keeps the heavy pages out of the entry", () => {
  const source = readFileSync("src/routes.tsx", "utf8");

  it("imports only the Overview and the Login page statically", () => {
    const staticPages = [...source.matchAll(/^import [^;]* from "@\/pages\/([^"]+)";$/gm)].map((m) => m[1]).sort();
    expect(staticPages).toEqual(["login", "overview"]);
  });

  it.each(["matrix", "topology", "explore", "promql-console", "investigate"])(
    "loads %s through lazyRouteComponent",
    (page) => {
      expect(source).toMatch(new RegExp(`lazyRouteComponent\\(\\(\\) => import\\("@/pages/${page}"\\)`));
    },
  );
});

describe("the page pending frame", () => {
  it("sits in the same padded container as PageShell, not flush against <main>", () => {
    const Pending = router.options.defaultPendingComponent as ComponentType;
    render(<Pending />);
    const status = screen.getByRole("status");
    expect(status.parentElement).toHaveClass(...PAGE_CONTAINER_CLASS.split(" "));
  });
});
