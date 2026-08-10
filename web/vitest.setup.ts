import "@testing-library/jest-dom/vitest";

// Node 22's experimental `localStorage` global (undefined without
// --localstorage-file) shadows jsdom's, and jsdom does not implement
// matchMedia. Provide minimal test doubles.
const store = new Map<string, string>();
Object.defineProperty(globalThis, "localStorage", {
  configurable: true,
  value: {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
    key: (i: number) => [...store.keys()][i] ?? null,
    get length() {
      return store.size;
    },
  },
});

// jsdom implements no ResizeObserver, and React Flow constructs one the moment
// its pane mounts. The topology map reaches that point in tests now that it
// draws from agents as well as from Kubernetes nodes, so the no-op double is
// what keeps a rendered map from throwing on mount.
Object.defineProperty(globalThis, "ResizeObserver", {
  configurable: true,
  value: class {
    observe() {}
    unobserve() {}
    disconnect() {}
  },
});

Object.defineProperty(globalThis, "matchMedia", {
  configurable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  }),
});
