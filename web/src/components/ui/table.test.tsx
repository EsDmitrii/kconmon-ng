import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Table, TBody, Td, Th, THead, Tr } from "@/components/ui/table";

afterEach(cleanup);

/**
 * table.test.tsx — the density contract, pinned through class names. jsdom has
 * no layout engine, so "a dense row is ~30-32px" can only be asserted as the
 * structure that produces it: the paddings, the type size, and where the
 * numeric treatment lands. mono-data is defined in index.css by the type-scale
 * work; here only the class NAME is pinned, never its CSS.
 */

function renderTable(props: React.ComponentProps<typeof Table> = {}) {
  return render(
    <Table {...props}>
      <THead>
        <Tr>
          <Th>Pair</Th>
          <Th numeric>RTT p95</Th>
        </Tr>
      </THead>
      <TBody>
        <Tr>
          <Td>node-a → node-b</Td>
          <Td numeric>4.2ms</Td>
        </Tr>
      </TBody>
    </Table>,
  );
}

describe("Table — semantics", () => {
  it("renders a real table: rowgroups, column headers, cells", () => {
    renderTable();
    expect(screen.getByRole("table")).toBeInTheDocument();
    expect(screen.getAllByRole("rowgroup")).toHaveLength(2);
    expect(screen.getByRole("columnheader", { name: "Pair" })).toHaveAttribute("scope", "col");
    expect(screen.getByRole("cell", { name: "node-a → node-b" })).toBeInTheDocument();
  });

  it("wraps the table in an overflow-x-auto scroller by default, and not when bare", () => {
    renderTable();
    const table = screen.getByRole("table");
    expect(table.parentElement).toHaveClass("overflow-x-auto");
    cleanup();

    renderTable({ bare: true });
    expect(screen.getByRole("table").parentElement).not.toHaveClass("overflow-x-auto");
  });
});

describe("Table — default variant (the current page look)", () => {
  it("keeps the existing table typography and paddings", () => {
    renderTable();
    expect(screen.getByRole("table")).toHaveClass("w-full", "text-sm");

    const thead = screen.getAllByRole("rowgroup")[0];
    expect(thead).toHaveClass("text-left", "text-muted-foreground", "text-[11px]", "uppercase", "tracking-[0.07em]");
    expect(thead).toHaveClass("[&_tr]:border-b", "[&_tr]:border-border");

    expect(screen.getAllByRole("rowgroup")[1]).toHaveClass("divide-y", "divide-border");
    expect(screen.getByRole("columnheader", { name: "Pair" })).toHaveClass("py-3", "font-semibold", "text-left");
    expect(screen.getByRole("cell", { name: "node-a → node-b" })).toHaveClass("py-3");
  });
});

describe("Table — dense variant", () => {
  it("tightens rows to py-1.5 and 13px text, with a quieter header", () => {
    renderTable({ variant: "dense" });
    expect(screen.getByRole("table")).toHaveClass("text-[13px]");
    expect(screen.getByRole("table")).not.toHaveClass("text-sm");

    const thead = screen.getAllByRole("rowgroup")[0];
    expect(thead).toHaveClass("text-xs");
    expect(thead).not.toHaveClass("uppercase");

    expect(screen.getByRole("columnheader", { name: "Pair" })).toHaveClass("py-1.5", "font-medium");
    expect(screen.getByRole("cell", { name: "node-a → node-b" })).toHaveClass("py-1.5");
  });
});

describe("Table — numeric cells", () => {
  it("right-aligns numeric data in the mono figure treatment, opt-in only", () => {
    renderTable();
    const numericCell = screen.getByRole("cell", { name: "4.2ms" });
    expect(numericCell).toHaveClass("mono-data", "nums", "text-right");

    const textCell = screen.getByRole("cell", { name: "node-a → node-b" });
    expect(textCell).not.toHaveClass("mono-data", "nums", "text-right");
  });

  it("right-aligns the numeric column header without the mono treatment", () => {
    renderTable();
    const th = screen.getByRole("columnheader", { name: "RTT p95" });
    expect(th).toHaveClass("text-right");
    expect(th).not.toHaveClass("mono-data", "text-left");
  });
});

describe("Table — call-site overrides", () => {
  it("lets a caller's className win over the variant padding (tailwind-merge)", () => {
    render(
      <Table>
        <TBody>
          <Tr>
            <Td className="py-4 pr-6">wide</Td>
          </Tr>
        </TBody>
      </Table>,
    );
    const cell = screen.getByRole("cell", { name: "wide" });
    expect(cell).toHaveClass("py-4", "pr-6");
    expect(cell).not.toHaveClass("py-3");
  });

  it("supports row headers via scope override", () => {
    render(
      <Table>
        <TBody>
          <Tr>
            <Th scope="row" className="font-normal">
              created
            </Th>
          </Tr>
        </TBody>
      </Table>,
    );
    expect(screen.getByRole("rowheader", { name: "created" })).toHaveAttribute("scope", "row");
  });
});
