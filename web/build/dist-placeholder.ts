import fs from "node:fs";
import path from "node:path";
import type { Plugin, ResolvedConfig } from "vite";

// The tracked dist/index.html, byte for byte: what `go build` embeds without a node build.
export const placeholderIndex = [
  "<!doctype html>",
  '<html lang="en">',
  "  <head>",
  '    <meta charset="UTF-8" />',
  "    <title>kconmon-ng Console</title>",
  "  </head>",
  "  <body>",
  '    <div id="root"></div>',
  "    <!-- Placeholder. Replaced by the Vite build (web/) at image build time. -->",
  "  </body>",
  "</html>",
  "",
].join("\n");

export const distGitignore = [
  "# Vite build output is generated; only the placeholder index.html is tracked so",
  "# `go build`/`go test` compile the embed without requiring a node build.",
  "*",
  "!.gitignore",
  "!index.html",
  "",
].join("\n");

// emptyOutDir wipes the tracked .gitignore whitelist that keeps generated
// assets out of git, and the build overwrites the tracked placeholder
// index.html; restore both after a build into dist so a local build leaves git clean.
// apply: "build" matters: a dev server or vitest calls closeBundle on close too.
export function restoreDistPlaceholder(distDir: string): Plugin {
  let outDir = "";
  let logger: ResolvedConfig["logger"] | undefined;
  return {
    name: "restore-dist-placeholder",
    apply: "build",
    configResolved(config) {
      outDir = path.resolve(config.root, config.build.outDir);
      logger = config.logger;
    },
    closeBundle() {
      if (outDir !== path.resolve(distDir)) return;
      fs.writeFileSync(path.join(distDir, ".gitignore"), distGitignore);
      // CI keeps the build: its "Heavy chunks stay out of the entry page" step reads this index.html.
      // Dockerfile.console builds to --outDir /out, so the image never sees the placeholder.
      if (process.env.CI || process.env.KCONMON_EMBED_BUILD === "1") return;
      fs.writeFileSync(path.join(distDir, "index.html"), placeholderIndex);
      logger?.info(
        "dist/index.html reset to the tracked placeholder; a go build now embeds no UI. " +
          "Rebuild with KCONMON_EMBED_BUILD=1 to keep the built page for a local console binary.",
      );
    },
  };
}
