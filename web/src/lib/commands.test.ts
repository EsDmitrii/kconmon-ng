import { describe, expect, it, vi } from "vitest";
import {
  buildRegistry,
  commandTitle,
  GROUP_KEYS,
  isCommandDisabled,
  scoreCommand,
  searchCommands,
  type Command,
  type CommandContext,
} from "@/lib/commands";
import { chromeDict, NAV_KEYS } from "@/lib/i18n/dict/chrome";
import { paletteDict } from "@/lib/i18n/dict/palette";
import { NAV_ITEMS } from "@/nav";

function ctx(over: Partial<CommandContext> = {}): CommandContext {
  return {
    can: () => true,
    writesDisabled: false,
    navigate: () => {},
    theme: "dark",
    toggleTheme: () => {},
    isLive: true,
    returnToLive: () => {},
    openTimeMachinePicker: () => {},
    hasTimeMachinePicker: true,
    ...over,
  };
}

function cmd(over: Partial<Command> & { id: string; title: string }): Command {
  return { group: "Actions", perform: () => {}, ...over };
}

describe("scoreCommand", () => {
  it("returns 1 for an empty query so the palette shows the whole registry", () => {
    expect(scoreCommand("", cmd({ id: "a", title: "Overview" }))).toBe(1);
    expect(scoreCommand("   ", cmd({ id: "a", title: "Overview" }))).toBe(1);
  });

  it("returns 0 when nothing matches", () => {
    expect(scoreCommand("zzz", cmd({ id: "a", title: "Overview", keywords: ["health"] }))).toBe(0);
  });

  it("is case-insensitive in both directions", () => {
    const c = cmd({ id: "a", title: "Overview" });
    expect(scoreCommand("OVER", c)).toBe(scoreCommand("over", c));
    expect(scoreCommand("over", c)).toBe(scoreCommand("over", cmd({ id: "a", title: "OVERVIEW" })));
  });

  it("ranks title-start prefix over a mid-title word boundary", () => {
    const start = cmd({ id: "a", title: "Machine time" });
    const mid = cmd({ id: "b", title: "Time machine" });
    expect(scoreCommand("machine", start)).toBeGreaterThan(scoreCommand("machine", mid));
  });

  it("ranks a word-boundary hit over a mid-word substring", () => {
    const boundary = cmd({ id: "a", title: "Go to alerting" });
    const substring = cmd({ id: "b", title: "Realerting" });
    expect(scoreCommand("alert", boundary)).toBeGreaterThan(scoreCommand("alert", substring));
  });

  it("ranks any title hit over a keyword hit", () => {
    const title = cmd({ id: "a", title: "Something dns" });
    const keyword = cmd({ id: "b", title: "Something else", keywords: ["dns"] });
    expect(scoreCommand("dns", title)).toBeGreaterThan(scoreCommand("dns", keyword));
    expect(scoreCommand("dns", keyword)).toBeGreaterThan(0);
  });

  it("ranks a keyword word-boundary hit over a keyword substring", () => {
    const boundary = cmd({ id: "a", title: "X", keywords: ["run a check"] });
    const substring = cmd({ id: "b", title: "X", keywords: ["prerun"] });
    expect(scoreCommand("run", boundary)).toBeGreaterThan(scoreCommand("run", substring));
  });

  it("requires EVERY word of a multi-word query to match somewhere", () => {
    const c = cmd({ id: "a", title: "Create an alert rule", keywords: ["prometheus"] });
    expect(scoreCommand("alert rule", c)).toBeGreaterThan(0);
    expect(scoreCommand("alert prometheus", c)).toBeGreaterThan(0);
    expect(scoreCommand("alert nothinghere", c)).toBe(0);
  });

  it("sums the per-word scores, so a two-word match outranks a one-word match", () => {
    const c = cmd({ id: "a", title: "Create an alert rule" });
    expect(scoreCommand("alert rule", c)).toBeGreaterThan(scoreCommand("alert", c));
  });
});

describe("searchCommands", () => {
  it("drops non-matches and orders by score", () => {
    const list = [
      cmd({ id: "a", title: "Realerting" }),
      cmd({ id: "b", title: "Alerting" }),
      cmd({ id: "c", title: "Topology" }),
    ];
    expect(searchCommands("alert", list).map((c) => c.id)).toEqual(["b", "a"]);
  });

  it("breaks ties by title, deterministically and independent of input order", () => {
    const a = cmd({ id: "z", title: "Alpha thing" });
    const b = cmd({ id: "y", title: "Bravo thing" });
    expect(searchCommands("thing", [b, a]).map((c) => c.title)).toEqual(["Alpha thing", "Bravo thing"]);
    expect(searchCommands("thing", [a, b]).map((c) => c.title)).toEqual(["Alpha thing", "Bravo thing"]);
  });

  it("keeps registry order for an empty query (every score is 1, titles break the tie only within it)", () => {
    const list = [cmd({ id: "a", title: "Overview" }), cmd({ id: "b", title: "Live" })];
    expect(searchCommands("", list).length).toBe(2);
  });
});

describe("buildRegistry navigation parity", () => {
  it("carries every nav.ts entry exactly once, with nav.ts's own label and description", () => {
    const registry = buildRegistry(ctx());
    for (const item of NAV_ITEMS) {
      const hits = registry.filter((c) => c.id === `nav:${item.path}`);
      expect(hits, `nav entry ${item.path}`).toHaveLength(1);
      expect(hits[0].title).toBe(item.label);
      expect(hits[0].group).toBe("Navigation");
      expect(hits[0].keywords).toContain(item.description);
    }
    expect(registry.filter((c) => c.group === "Navigation")).toHaveLength(NAV_ITEMS.length);
  });

  it("navigates to the nav entry's own path", () => {
    const navigate = vi.fn();
    const registry = buildRegistry(ctx({ navigate }));
    const c = registry.find((x) => x.id === "nav:/matrix")!;
    c.perform(ctx({ navigate }));
    expect(navigate).toHaveBeenCalledWith("/matrix");
  });

  it("has unique ids across the whole registry", () => {
    const ids = buildRegistry(ctx()).map((c) => c.id);
    expect(new Set(ids).size).toBe(ids.length);
  });
});

describe("buildRegistry permission gating (HIDE)", () => {
  it("hides an action whose permission the subject lacks", () => {
    const none = buildRegistry(ctx({ can: () => false }));
    expect(none.find((c) => c.id === "action:alert-rule")).toBeUndefined();
    expect(none.find((c) => c.id === "action:annotation")).toBeUndefined();
    expect(none.find((c) => c.id === "action:maintenance")).toBeUndefined();
    expect(none.find((c) => c.id === "action:run-check")).toBeUndefined();
    // Navigation is not permission-gated in nav.ts, so it must survive.
    expect(none.filter((c) => c.group === "Navigation")).toHaveLength(NAV_ITEMS.length);
  });

  it("shows exactly the actions the subject's permissions allow", () => {
    const only = buildRegistry(ctx({ can: (p) => p === "alerts:manage" }));
    expect(only.find((c) => c.id === "action:alert-rule")).toBeDefined();
    expect(only.find((c) => c.id === "action:annotation")).toBeUndefined();
  });

  it("gates each action on the permission its destination actually requires", () => {
    const perms: Record<string, string> = {
      "action:run-check": "runs:create",
      "action:alert-rule": "alerts:manage",
      "action:maintenance": "maintenance:write",
      "action:annotation": "annotations:write",
    };
    const registry = buildRegistry(ctx());
    for (const [id, permission] of Object.entries(perms)) {
      expect(registry.find((c) => c.id === id)?.permission, id).toBe(permission);
    }
  });
});

describe("buildRegistry Time Machine treatment (DISABLE=time)", () => {
  it("disables WRITE actions while engaged, and hides nothing", () => {
    const engaged = ctx({ writesDisabled: true, isLive: false });
    const registry = buildRegistry(engaged);
    for (const id of ["action:run-check", "action:alert-rule", "action:maintenance", "action:annotation"]) {
      const c = registry.find((x) => x.id === id);
      expect(c, id).toBeDefined();
      expect(isCommandDisabled(c!, engaged), id).toBe(true);
    }
  });

  it("leaves navigation and read-only actions enabled while engaged", () => {
    const engaged = ctx({ writesDisabled: true, isLive: false });
    const registry = buildRegistry(engaged);
    expect(isCommandDisabled(registry.find((c) => c.id === "nav:/matrix")!, engaged)).toBe(false);
    expect(isCommandDisabled(registry.find((c) => c.id === "action:investigate")!, engaged)).toBe(false);
    expect(isCommandDisabled(registry.find((c) => c.id === "view:timemachine-live")!, engaged)).toBe(false);
  });

  it("enables the same writes again once Live", () => {
    const live = ctx();
    const registry = buildRegistry(live);
    expect(isCommandDisabled(registry.find((c) => c.id === "action:alert-rule")!, live)).toBe(false);
  });
});

describe("buildRegistry Time Machine and theme entries", () => {
  it("offers the picker while Live and Return to Live while engaged, never both", () => {
    const live = buildRegistry(ctx({ isLive: true }));
    expect(live.find((c) => c.id === "view:timemachine-pick")).toBeDefined();
    expect(live.find((c) => c.id === "view:timemachine-live")).toBeUndefined();

    const engaged = buildRegistry(ctx({ isLive: false }));
    expect(engaged.find((c) => c.id === "view:timemachine-pick")).toBeUndefined();
    expect(engaged.find((c) => c.id === "view:timemachine-live")).toBeDefined();
  });

  it("opens the Time Machine picker rather than jumping to an arbitrary instant", () => {
    const openTimeMachinePicker = vi.fn();
    const c = ctx({ openTimeMachinePicker });
    buildRegistry(c).find((x) => x.id === "view:timemachine-pick")!.perform(c);
    expect(openTimeMachinePicker).toHaveBeenCalledTimes(1);
  });

  it("returns to Live through the Time Machine context", () => {
    const returnToLive = vi.fn();
    const c = ctx({ isLive: false, returnToLive });
    buildRegistry(c).find((x) => x.id === "view:timemachine-live")!.perform(c);
    expect(returnToLive).toHaveBeenCalledTimes(1);
  });

  it("names the theme it switches TO, exactly as ThemeToggle's label does", () => {
    expect(buildRegistry(ctx({ theme: "dark" })).find((c) => c.id === "view:theme")!.title).toBe(
      "Switch to light theme",
    );
    expect(buildRegistry(ctx({ theme: "light" })).find((c) => c.id === "view:theme")!.title).toBe(
      "Switch to dark theme",
    );
  });

  it("toggles the theme through the provider", () => {
    const toggleTheme = vi.fn();
    const c = ctx({ toggleTheme });
    buildRegistry(c).find((x) => x.id === "view:theme")!.perform(c);
    expect(toggleTheme).toHaveBeenCalledTimes(1);
  });
});

describe("buildRegistry action destinations", () => {
  it("deep-links each action to the surface that actually owns it", () => {
    const destinations: Record<string, string> = {
      "action:run-check": "/diagnostics",
      "action:investigate": "/investigate",
      "action:alert-rule": "/alerting",
      "action:maintenance": "/explore",
      "action:annotation": "/explore",
    };
    for (const [id, path] of Object.entries(destinations)) {
      const navigate = vi.fn();
      const c = ctx({ navigate });
      buildRegistry(c).find((x) => x.id === id)!.perform(c);
      expect(navigate, id).toHaveBeenCalledWith(path);
    }
  });
});

/* the bilingual palette The English fixtures above are unchanged and stay the ranking contract. */

/** A two-language entry, the shape every registry row now has. */
function bi(over: Partial<Command> & { id: string; title: string; titleRu: string }): Command {
  return { group: "Actions", perform: () => {}, ...over };
}

describe("commandTitle", () => {
  it("shows the English title in en and the Russian one in ru", () => {
    const c = bi({ id: "a", title: "Matrix", titleRu: "Матрица" });
    expect(commandTitle(c, "en")).toBe("Matrix");
    expect(commandTitle(c, "ru")).toBe("Матрица");
  });

  it("falls back to English for an entry with no Russian title — never to the id", () => {
    const c = cmd({ id: "a", title: "MTR" });
    expect(commandTitle(c, "ru")).toBe("MTR");
    expect(commandTitle(c, "en")).toBe("MTR");
  });
});

describe("scoreCommand indexes BOTH languages", () => {
  const matrix = bi({ id: "a", title: "Matrix", titleRu: "Матрица" });

  it("finds a Russian-titled entry by its English name", () => {
    expect(scoreCommand("matrix", matrix)).toBeGreaterThan(0);
  });

  it("finds it by its Russian name too — the operator types either", () => {
    expect(scoreCommand("матрица", matrix)).toBeGreaterThan(0);
  });

  it("scores a title-start hit the same in both languages: neither is the second-class one", () => {
    expect(scoreCommand("матрица", matrix)).toBe(scoreCommand("matrix", matrix));
  });

  it("still refuses a word that is in neither", () => {
    expect(scoreCommand("топология", matrix)).toBe(0);
    expect(scoreCommand("topology", matrix)).toBe(0);
  });

  it("matches a Russian KEYWORD blob when neither title carries the word", () => {
    const c = bi({
      id: "a",
      title: "Declare a maintenance window…",
      titleRu: "Объявить окно работ…",
      keywords: ["downtime planned", "простой регламент"],
    });
    expect(scoreCommand("простой", c)).toBeGreaterThan(0);
    expect(scoreCommand("downtime", c)).toBeGreaterThan(0);
    // A title hit still outranks a keyword hit, in Russian as in English.
    expect(scoreCommand("окно", c)).toBeGreaterThan(scoreCommand("регламент", c));
  });

  /* The reason WORD_CHAR had to learn Cyrillic. */
  it("treats Cyrillic as inside a word, so a mid-word fragment ranks BELOW a real prefix", () => {
    const prefix = bi({ id: "a", title: "x", titleRu: "матрица связности" });
    const midWord = bi({ id: "b", title: "y", titleRu: "телематрица" });
    expect(scoreCommand("матри", prefix)).toBeGreaterThan(scoreCommand("матри", midWord));
  });

  it("ranks a Russian word-boundary hit above a mid-word one", () => {
    const boundary = bi({ id: "a", title: "x", titleRu: "Перейти в матрицу" });
    const substring = bi({ id: "b", title: "y", titleRu: "телематрицу" });
    expect(scoreCommand("матриц", boundary)).toBeGreaterThan(scoreCommand("матриц", substring));
  });

  it("sums a MIXED query: an operator halfway through switching languages still lands", () => {
    const c = bi({ id: "a", title: "Create an alert rule…", titleRu: "Создать правило оповещения…" });
    expect(scoreCommand("alert правило", c)).toBeGreaterThan(0);
    expect(scoreCommand("alert nothinghere", c)).toBe(0);
  });
});

describe("searchCommands tie-break follows the DISPLAYED title", () => {
  const alpha = bi({ id: "z", title: "Alpha thing", titleRu: "Ярлык вещь" });
  const bravo = bi({ id: "y", title: "Bravo thing", titleRu: "Автор вещь" });

  it("orders by the English title in en", () => {
    expect(searchCommands("thing", [bravo, alpha], "en").map((c) => c.id)).toEqual(["z", "y"]);
  });

  it("orders by the RUSSIAN title in ru — the alphabet the reader is actually reading", () => {
    expect(searchCommands("вещь", [alpha, bravo], "ru").map((c) => c.id)).toEqual(["y", "z"]);
  });

  it("defaults to en, so every existing two-argument call is unchanged", () => {
    expect(searchCommands("thing", [bravo, alpha]).map((c) => c.id)).toEqual(["z", "y"]);
  });
});

describe("the registry speaks both languages", () => {
  it("takes each nav entry's Russian label from the SIDEBAR's own table, never a second copy", () => {
    const registry = buildRegistry(ctx());
    for (const item of NAV_ITEMS) {
      const entry = registry.find((c) => c.id === `nav:${item.path}`)!;
      expect(entry.titleRu, item.path).toBe(chromeDict.ru[NAV_KEYS[item.path]]);
    }
  });

  it("carries nav.ts's English description AND its Russian twin as keywords", () => {
    const registry = buildRegistry(ctx());
    const matrix = registry.find((c) => c.id === "nav:/matrix")!;
    expect(matrix.keywords).toContain(NAV_ITEMS.find((i) => i.path === "/matrix")!.description);
    expect(matrix.keywords).toContain(paletteDict.ru["navDesc.matrix"]);
  });

  it("finds Matrix by «матрица» and by «тепловая» — the label and the tooltip, in Russian", () => {
    const registry = buildRegistry(ctx());
    expect(searchCommands("матрица", registry, "ru").map((c) => c.id)).toContain("nav:/matrix");
    expect(searchCommands("тепловая", registry, "ru").map((c) => c.id)).toContain("nav:/matrix");
  });

  it("finds the same entry by its English name while the console is in Russian", () => {
    const registry = buildRegistry(ctx());
    expect(searchCommands("heatmap", registry, "ru").map((c) => c.id)).toEqual(["nav:/matrix"]);
    expect(searchCommands("matrix", registry, "ru").map((c) => c.id)).toContain("nav:/matrix");
  });

  it("gives every action and view entry a Russian title", () => {
    for (const c of buildRegistry(ctx())) {
      expect(c.titleRu, c.id).toBeDefined();
      expect(c.titleRu, c.id).not.toBe("");
    }
  });

  it("names the theme it switches TO in Russian too", () => {
    const dark = buildRegistry(ctx({ theme: "dark" })).find((c) => c.id === "view:theme")!;
    expect(commandTitle(dark, "ru")).toBe("Переключить на светлую тему");
    const light = buildRegistry(ctx({ theme: "light" })).find((c) => c.id === "view:theme")!;
    expect(commandTitle(light, "ru")).toBe("Переключить на тёмную тему");
  });

  it("says «Вернуться в реальное время» — the Time Machine bar's own words, not a second wording", () => {
    const engaged = buildRegistry(ctx({ isLive: false })).find((c) => c.id === "view:timemachine-live")!;
    expect(commandTitle(engaged, "ru")).toBe(chromeDict.ru["timemachine.returnToLive"]);
  });

  it("finds the Time Machine entry by «машина времени», and Return to Live by «live»", () => {
    const live = buildRegistry(ctx({ isLive: true }));
    expect(searchCommands("машина времени", live, "ru").map((c) => c.id)).toContain("view:timemachine-pick");
    const engaged = buildRegistry(ctx({ isLive: false }));
    expect(searchCommands("live", engaged, "en").map((c) => c.id)).toContain("view:timemachine-live");
    expect(searchCommands("реальное", engaged, "ru").map((c) => c.id)).toContain("view:timemachine-live");
  });
});

describe("renamed nav entries still answer to their old names (M3-8)", () => {
  /* Operators' muscle memory keeps typing the pre-rename labels, so each old
     name stays in the entry's keyword corpus — in both languages. */
  const CASES: [query: string, id: string][] = [
    ["live", "nav:/live"],
    ["онлайн", "nav:/live"],
    ["investigate", "nav:/investigate"],
    ["расследование", "nav:/investigate"],
    ["explore", "nav:/explore"],
    ["console", "nav:/console"],
    ["консоль", "nav:/console"],
    ["diagnostics", "nav:/diagnostics"],
    ["диагностика", "nav:/diagnostics"],
    ["targets", "nav:/targets"],
    ["schedules", "nav:/targets"],
  ];

  it("finds every renamed page by the label it used to wear", () => {
    const registry = buildRegistry(ctx());
    for (const [query, id] of CASES) {
      expect(searchCommands(query, registry).map((c) => c.id), query).toContain(id);
    }
  });
});

describe("GROUP_KEYS", () => {
  it("names every group exactly once, so no section can render untranslated", () => {
    expect(Object.keys(GROUP_KEYS).sort()).toEqual(["Actions", "Navigation", "View"]);
    for (const key of Object.values(GROUP_KEYS)) {
      expect(paletteDict.ru[key]).toBeTruthy();
      expect(paletteDict.en[key]).toBeTruthy();
    }
  });
});
