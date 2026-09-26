import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";
import { restoreDistPlaceholder } from "./build/dist-placeholder";

const distDir = path.resolve(import.meta.dirname, "../internal/console/ui/dist");

// The Go binary embeds internal/console/ui/dist, so build there directly.
export default defineConfig({
  plugins: [react(), tailwindcss(), restoreDistPlaceholder(distDir)],
  resolve: {
    alias: { "@": path.resolve(import.meta.dirname, "./src") },
  },
  base: "/",
  build: {
    outDir: distDir,
    emptyOutDir: true,
    // The ECharts vendor chunk is ~535 kB even with only the parts the console registers
    // (components/echart.tsx), and only the chart pages load it; the default 500 would warn on
    // every build and teach everyone to ignore the warning.
    chunkSizeWarningLimit: 600,
    rolldownOptions: {
      output: {
        // The heaviest vendor libs get chunks of their own, so the lazily
        // loaded pages that use them (routes.tsx) share one copy each and an
        // Overview visit downloads none of ECharts, CodeMirror or React Flow.
        // React (and the store shim the router shares with React Flow) goes
        // first: a group also captures its members' dependencies, and without
        // a higher-priority group of its own that code landed inside the
        // xyflow chunk, which the entry then had to download just for it.
        codeSplitting: {
          groups: [
            { name: "react", test: /[\\/]node_modules[\\/](react|react-dom|scheduler|use-sync-external-store)[\\/]/, priority: 20 },
            { name: "echarts", test: /[\\/]node_modules[\\/](echarts|zrender)[\\/]/ },
            {
              name: "codemirror",
              test: /[\\/]node_modules[\\/](codemirror|@codemirror|@lezer|@prometheus-io|style-mod|w3c-keyname|crelt)[\\/]/,
            },
            { name: "xyflow", test: /[\\/]node_modules[\\/](@xyflow|d3-[a-z-]+|classcat|zustand)[\\/]/ },
          ],
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
