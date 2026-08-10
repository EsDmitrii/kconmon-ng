import { useEffect, useRef } from "react";
import { EditorView, keymap, tooltips, type KeyBinding } from "@codemirror/view";
import { Compartment, EditorState, Prec, type Extension } from "@codemirror/state";
import { defaultKeymap } from "@codemirror/commands";
import { basicSetup } from "codemirror";
import { PromQLExtension } from "@prometheus-io/codemirror-promql";
import { useTheme } from "@/components/theme-provider";
import { openCommandPalette } from "@/lib/commands";

/*
 * CodeMirror paints its own chrome, so the design-system tokens are bridged in as an
 * EditorView.theme keyed on our ThemeProvider.
 */
function editorTheme(dark: boolean) {
  return EditorView.theme(
    {
      "&": {
        backgroundColor: "transparent",
        color: "hsl(var(--foreground))",
        fontSize: "13.5px",
      },
      ".cm-content": { caretColor: "hsl(var(--primary))", fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace" },
      ".cm-cursor": { borderLeftColor: "hsl(var(--primary))" },
      "&.cm-focused": { outline: "none" },
      ".cm-gutters": {
        backgroundColor: "transparent",
        color: "hsl(var(--muted-foreground) / 0.6)",
        border: "none",
      },
      ".cm-activeLine": { backgroundColor: "hsl(var(--surface-2) / 0.5)" },
      ".cm-activeLineGutter": { backgroundColor: "transparent" },
      "&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection": {
        backgroundColor: "hsl(var(--primary) / 0.25)",
      },
      ".cm-tooltip": {
        backgroundColor: "hsl(var(--popover))",
        color: "hsl(var(--popover-foreground))",
        border: "1px solid hsl(var(--border))",
        borderRadius: "8px",
        boxShadow: "var(--shadow-pop)",
      },
      ".cm-tooltip-autocomplete ul li[aria-selected]": {
        backgroundColor: "hsl(var(--accent))",
        color: "hsl(var(--accent-foreground))",
      },
    },
    { dark },
  );
}

/**
 * tooltipConfig re-parents every CodeMirror tooltip — the autocomplete popup above all;
 * BODY-PARENTING RATHER THAN overflow-visible, and the reason is the theme.
 */
export function tooltipConfig(): { parent: HTMLElement } {
  return { parent: document.body };
}

/**
 * editorKeymap is the editor's OWN bindings, ahead of CodeMirror's defaults; mod-k is .
 * CodeMirror's defaultKeymap binds Mod-k to deleteLine and returns true.
 */
export const editorKeymap: readonly KeyBinding[] = [
  {
    key: "Mod-k",
    run: () => {
      openCommandPalette();
      return true;
    },
  },
];

/**
 * topKeymap is every binding that must beat basicSetup's own keymaps AND defaultKeymap. Mod-Enter
 * belongs here for the same reason Mod-k does: basicSetup binds Enter to insertNewlineAndIndent and
 * returns true, so a Mod-Enter registered at default precedence AFTER it inserted a newline instead
 * of running the query.
 */
export function topKeymap(opts: { onRun: () => void }): KeyBinding[] {
  return [
    { key: "Mod-Enter", run: () => { opts.onRun(); return true; } },
    ...editorKeymap,
  ];
}

/** promqlExtensions is the editor's whole configuration MINUS the theme. */
export function promqlExtensions(opts: { onRun: () => void; onChange: (v: string) => void }): Extension[] {
  const promql = new PromQLExtension();
  return [
    basicSetup,
    tooltips(tooltipConfig()),
    Prec.highest(keymap.of(topKeymap(opts))),
    keymap.of(defaultKeymap),
    promql.asExtension(),
    EditorView.updateListener.of((u) => {
      if (u.docChanged) opts.onChange(u.state.doc.toString());
    }),
  ];
}

export function PromQLEditor({ initial, onChange, onRun }: {
  initial: string; onChange: (v: string) => void; onRun: () => void;
}) {
  const host = useRef<HTMLDivElement>(null);
  const runRef = useRef(onRun);
  runRef.current = onRun;
  const changeRef = useRef(onChange);
  changeRef.current = onChange;
  const { theme } = useTheme();
  const themeCompartment = useRef(new Compartment());
  const viewRef = useRef<EditorView | null>(null);

  useEffect(() => {
    if (!host.current) return;
    const view = new EditorView({
      parent: host.current,
      state: EditorState.create({
        doc: initial,
        extensions: [
          ...promqlExtensions({
            onRun: () => runRef.current(),
            onChange: (v) => changeRef.current(v),
          }),
          themeCompartment.current.of(editorTheme(document.documentElement.classList.contains("dark"))),
        ],
      }),
    });
    viewRef.current = view;
    return () => {
      view.destroy();
      viewRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Re-point the editor theme when the app theme flips. The tooltips live on
  // document.body but carry this view's theme classes (see tooltipConfig), so
  // reconfiguring here re-styles them too.
  useEffect(() => {
    viewRef.current?.dispatch({
      effects: themeCompartment.current.reconfigure(editorTheme(theme === "dark")),
    });
  }, [theme]);

  return <div ref={host} className="rounded-md font-mono text-sm" />;
}
