import { useEffect, useRef } from "react";
import { EditorView, keymap, tooltips, type KeyBinding } from "@codemirror/view";
import { Compartment, EditorState, Prec, type Extension } from "@codemirror/state";
import { defaultKeymap } from "@codemirror/commands";
import { basicSetup } from "codemirror";
import { PromQLExtension } from "@prometheus-io/codemirror-promql";
import { useTheme } from "@/components/theme-provider";
import { openCommandPalette } from "@/lib/commands";

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

/**
 * tooltipConfig re-parents every CodeMirror tooltip — the autocomplete popup
 * above all — onto document.body (QA round 4, finding #5).
 *
 * The popup was rendered inside the editor, which lives inside a Card that is
 * `overflow-hidden` (pages/promql-console.tsx wraps it in one to keep the
 * editor's own corners rounded). A completion list longer than the two-line
 * editor was therefore clipped to the card: an operator typing `kconmon_` saw
 * one and a half suggestions and no way to scroll to the rest.
 *
 * BODY-PARENTING RATHER THAN overflow-visible, and the reason is the theme.
 * The alternative — dropping overflow-hidden on the card and racing z-index
 * against the sticky page header — leaves the popup inside a layout whose
 * every ancestor can clip it again the next time someone adds a container.
 * Parenting to body is CodeMirror's own answer, and it does NOT cost the dark
 * theme: `tooltips({parent})` renders into a container whose className is
 * `view.themeClasses` (@codemirror/view's TooltipViewManager), which carries
 * BOTH the base `cm-` classes and the generated class of every active
 * EditorView.theme — including editorTheme's own `{dark}` flag. So
 * `.cm-tooltip` and `.cm-tooltip-autocomplete ul li[aria-selected]` above keep
 * matching, in both themes, from the body.
 *
 * Exported as a function, not a constant: `document.body` is read when the
 * editor mounts, and a module-level capture would pin whatever body existed at
 * import time. It is also the seam its own test pins — a real CodeMirror mount
 * is not something jsdom renders honestly, so the CONFIG is what gets
 * asserted.
 */
export function tooltipConfig(): { parent: HTMLElement } {
  return { parent: document.body };
}

/**
 * editorKeymap is the editor's OWN bindings, ahead of CodeMirror's defaults.
 *
 * Mod-k is finding #18. CodeMirror's defaultKeymap binds Mod-k to deleteLine
 * and returns true from it, which calls preventDefault and stops the event —
 * so the palette's document-level ⌘K listener never fired while the editor had
 * focus, and the console's one global hotkey silently deleted a line of PromQL
 * instead. The binding here answers first and asks the palette to open through
 * lib/commands' PALETTE_OPEN_EVENT (see openCommandPalette for why an event
 * rather than a synthetic keystroke).
 *
 * Prec.highest at the call site is what makes "first" true regardless of where
 * this lands in the extension array; returning true keeps deleteLine from
 * running afterwards.
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
 * promqlExtensions is the editor's whole configuration MINUS the theme, in one
 * exported function so the two things jsdom cannot observe through a real
 * mount — the tooltip parent and the Mod-k binding — are still pinnable as
 * data. The theme stays with the component because it lives in a Compartment
 * that gets reconfigured on every theme flip.
 */
export function promqlExtensions(opts: { onRun: () => void; onChange: (v: string) => void }): Extension[] {
  const promql = new PromQLExtension();
  return [
    basicSetup,
    tooltips(tooltipConfig()),
    /* Highest precedence: this must beat basicSetup's own keymaps AND
       defaultKeymap below, both of which claim Mod-k. */
    Prec.highest(keymap.of([...editorKeymap])),
    keymap.of([
      { key: "Mod-Enter", run: () => { opts.onRun(); return true; } },
      ...defaultKeymap,
    ]),
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
