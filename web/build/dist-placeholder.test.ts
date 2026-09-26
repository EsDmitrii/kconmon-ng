// @vitest-environment node
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { build, createServer, type InlineConfig } from "vite";
import { distGitignore, placeholderIndex, restoreDistPlaceholder } from "./dist-placeholder";

const builtIndex = '<!doctype html><html><body><script type="module" src="/assets/index-abc.js"></script></body></html>\n';

let root: string;
let distDir: string;

function config(extra: InlineConfig = {}): InlineConfig {
  return {
    configFile: false,
    root,
    logLevel: "silent",
    plugins: [restoreDistPlaceholder(distDir)],
    build: { outDir: distDir, emptyOutDir: true },
    ...extra,
  };
}

beforeEach(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "kconmon-dist-"));
  distDir = path.join(root, "dist");
  fs.mkdirSync(distDir);
  fs.writeFileSync(path.join(root, "index.html"), '<!doctype html><html><body><script type="module" src="/main.js"></script></body></html>\n');
  fs.writeFileSync(path.join(root, "main.js"), "document.title = 'x';\n");
  vi.stubEnv("CI", undefined);
  vi.stubEnv("KCONMON_EMBED_BUILD", undefined);
});

afterEach(() => {
  vi.unstubAllEnvs();
  fs.rmSync(root, { recursive: true, force: true });
});

const read = (name: string) => fs.readFileSync(path.join(distDir, name), "utf8");

describe("restoreDistPlaceholder", () => {
  it("leaves dist alone when a dev server (or vitest) closes", async () => {
    fs.writeFileSync(path.join(distDir, "index.html"), builtIndex);
    const server = await createServer(config({ server: { middlewareMode: true, hmr: false, ws: false } }));
    await server.close();
    expect(read("index.html")).toBe(builtIndex);
    expect(fs.existsSync(path.join(distDir, ".gitignore"))).toBe(false);
  });

  it("restores the tracked placeholder and .gitignore after a local build into dist", async () => {
    await build(config());
    expect(read("index.html")).toBe(placeholderIndex);
    expect(read(".gitignore")).toBe(distGitignore);
    expect(fs.readdirSync(path.join(distDir, "assets")).length).toBeGreaterThan(0);
  });

  it("keeps the built index under CI, whose entry-page check reads it", async () => {
    vi.stubEnv("CI", "true");
    await build(config());
    expect(read("index.html")).toMatch(/src="\/assets\//);
    expect(read(".gitignore")).toBe(distGitignore);
  });

  it("keeps the built index when KCONMON_EMBED_BUILD=1 asks for a local go build to embed it", async () => {
    vi.stubEnv("KCONMON_EMBED_BUILD", "1");
    await build(config());
    expect(read("index.html")).toMatch(/src="\/assets\//);
    expect(read(".gitignore")).toBe(distGitignore);
  });

  it("does not touch dist when the build goes to another outDir", async () => {
    fs.writeFileSync(path.join(distDir, "index.html"), builtIndex);
    const other = path.join(root, "out");
    await build(config({ build: { outDir: other, emptyOutDir: true } }));
    expect(read("index.html")).toBe(builtIndex);
    expect(fs.existsSync(path.join(distDir, ".gitignore"))).toBe(false);
    expect(fs.readFileSync(path.join(other, "index.html"), "utf8")).toMatch(/src="\/assets\//);
  });
});
