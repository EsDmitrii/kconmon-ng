import { useEffect, useRef } from "react";
import { EditorView, keymap } from "@codemirror/view";
import { Compartment, EditorState } from "@codemirror/state";
import { defaultKeymap } from "@codemirror/commands";
import { basicSetup } from "codemirror";
import { PromQLExtension } from "@prometheus-io/codemirror-promql";
import { useTheme } from "@/components/theme-provider";

/* CodeMirror paints its own chrome, so the design-system tokens are bridged in
   as an EditorView.theme keyed on our ThemeProvider — otherwise the editor
   renders as a light strip on the dark page. Colours reference the CSS vars
   directly (CodeMirror styles are inline in the same document, so hsl(var())
   resolves fine). */
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

export function PromQLEditor({ initial, onChange, onRun }: {
  initial: string; onChange: (v: string) => void; onRun: () => void;
}) {
  const host = useRef<HTMLDivElement>(null);
  const runRef = useRef(onRun);
  runRef.current = onRun;
  const { theme } = useTheme();
  const themeCompartment = useRef(new Compartment());
  const viewRef = useRef<EditorView | null>(null);

  useEffect(() => {
    if (!host.current) return;
    const promql = new PromQLExtension();
    const view = new EditorView({
      parent: host.current,
      state: EditorState.create({
        doc: initial,
        extensions: [
          basicSetup,
          keymap.of([
            { key: "Mod-Enter", run: () => { runRef.current(); return true; } },
            ...defaultKeymap,
          ]),
          promql.asExtension(),
          themeCompartment.current.of(editorTheme(document.documentElement.classList.contains("dark"))),
          EditorView.updateListener.of((u) => {
            if (u.docChanged) onChange(u.state.doc.toString());
          }),
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

  // Re-point the editor theme when the app theme flips.
  useEffect(() => {
    viewRef.current?.dispatch({
      effects: themeCompartment.current.reconfigure(editorTheme(theme === "dark")),
    });
  }, [theme]);

  return <div ref={host} className="rounded-md font-mono text-sm" />;
}
