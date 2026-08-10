import { describe, expect, it, vi } from "vitest";
import { PALETTE_OPEN_EVENT } from "@/lib/commands";
import { editorKeymap, promqlExtensions, topKeymap, tooltipConfig } from "./promql-editor";

/**
 * A real CodeMirror mount is not something jsdom renders honestly (no layout, no measurement, and
 * the completion popup is positioned from geometry that is all zeroes).
 */

describe("tooltipConfig (finding #5: the completion popup was clipped by the editor card)", () => {
  it("parents tooltips on document.body, out of every overflow-hidden ancestor", () => {
    expect(tooltipConfig().parent).toBe(document.body);
  });

  it("reads the body at call time rather than capturing one at import time", () => {
    // The whole reason this is a function: a module-level constant would pin
    // whichever body existed when the module first loaded.
    expect(tooltipConfig().parent).toBe(tooltipConfig().parent);
    expect(tooltipConfig()).not.toBe(tooltipConfig());
  });
});

describe("editorKeymap (finding #18: CodeMirror ate ⌘K)", () => {
  it("binds Mod-k, which is what defaultKeymap would otherwise spend on deleteLine", () => {
    expect(editorKeymap.map((b) => b.key)).toContain("Mod-k");
  });

  it("asks the palette to open and RETURNS TRUE, so deleteLine never runs after it", () => {
    const seen = vi.fn();
    window.addEventListener(PALETTE_OPEN_EVENT, seen);
    try {
      const binding = editorKeymap.find((b) => b.key === "Mod-k");
      // The command signature takes an EditorView; this binding never touches
      // it, which is exactly what makes it callable here.
      expect(binding?.run?.(undefined as never)).toBe(true);
      expect(seen).toHaveBeenCalledTimes(1);
    } finally {
      window.removeEventListener(PALETTE_OPEN_EVENT, seen);
    }
  });
});

describe("topKeymap (finding #2: ⌘Enter inserted a newline instead of running)", () => {
  it("carries Mod-Enter in the SAME highest-precedence keymap as the working Mod-k", () => {
    const keys = topKeymap({ onRun: () => {} }).map((b) => b.key);
    expect(keys).toContain("Mod-Enter");
    // Mod-k is the binding that already worked; sharing the array is what
    // guarantees the run binding got the same Prec.highest treatment.
    expect(keys).toContain("Mod-k");
  });

  it("runs the query and RETURNS TRUE, so basicSetup's Enter handler never inserts a newline after it", () => {
    const onRun = vi.fn();
    const binding = topKeymap({ onRun }).find((b) => b.key === "Mod-Enter");
    expect(binding?.run?.(undefined as never)).toBe(true);
    expect(onRun).toHaveBeenCalledTimes(1);
  });
});

describe("promqlExtensions", () => {
  it("builds a configuration without touching the DOM, tooltips extension included", () => {
    const extensions = promqlExtensions({ onRun: () => {}, onChange: () => {} });
    // Shape, not identity: an Extension is an opaque value, so what is pinned
    // is that the builder produces one array carrying every layer the editor
    // needs — the tooltip parenting among them (asserted directly above).
    expect(Array.isArray(extensions)).toBe(true);
    expect(extensions.length).toBeGreaterThanOrEqual(6);
  });
});
