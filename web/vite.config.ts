import { defineConfig, type Plugin } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";
import fs from "node:fs";

const distDir = path.resolve(import.meta.dirname, "../internal/console/ui/dist");

// emptyOutDir wipes the tracked .gitignore whitelist that keeps generated
// assets out of git (only the placeholder index.html is tracked); rewrite it
// after every build so the invariant survives rebuilds.
const restoreDistGitignore: Plugin = {
  name: "restore-dist-gitignore",
  closeBundle() {
    fs.writeFileSync(
      path.join(distDir, ".gitignore"),
      [
        "# Vite build output is generated; only the placeholder index.html is tracked so",
        "# `go build`/`go test` compile the embed without requiring a node build.",
        "*",
        "!.gitignore",
        "!index.html",
        "",
      ].join("\n"),
    );
  },
};

// The Go binary embeds internal/console/ui/dist, so build there directly.
export default defineConfig({
  plugins: [react(), tailwindcss(), restoreDistGitignore],
  resolve: {
    alias: { "@": path.resolve(import.meta.dirname, "./src") },
  },
  base: "/",
  build: {
    outDir: distDir,
    emptyOutDir: true,
    rollupOptions: {
      output: {
        // Split the heaviest, page-scoped vendor libs out of the main bundle
        // so an Overview/Matrix-only visit doesn't pay for ECharts/CodeMirror/
        // React Flow. Route-level lazy loading is a bigger change (the router
        // is code-based, not file-based); this is the low-risk first step.
        manualChunks: {
          echarts: ["echarts"],
          codemirror: [
            "codemirror",
            "@codemirror/autocomplete",
            "@codemirror/commands",
            "@codemirror/language",
            "@codemirror/lint",
            "@codemirror/state",
            "@codemirror/view",
            "@prometheus-io/codemirror-promql",
          ],
          xyflow: ["@xyflow/react"],
        },
      },
    },
  },
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./vitest.setup.ts"],
    css: true,
  },
});
