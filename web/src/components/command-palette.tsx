import * as React from "react";
import { useNavigate } from "@tanstack/react-router";
import { Search } from "lucide-react";
import { useTheme } from "@/components/theme-provider";
import { TIME_MACHINE_TRIGGER_SELECTOR } from "@/components/timemachine-control";
import { useAuth } from "@/hooks/use-auth";
import {
  buildRegistry,
  commandTitle,
  GROUP_KEYS,
  GROUP_ORDER,
  isCommandDisabled,
  PALETTE_OPEN_EVENT,
  searchCommands,
  type Command,
  type CommandContext,
} from "@/lib/commands";
import { useLocale, useT } from "@/lib/i18n";
import { paletteDict } from "@/lib/i18n/dict/palette";
import { useTimeMachine, useWritesDisabled } from "@/lib/timemachine";
import { cn } from "@/lib/utils";

/** This component owns keys, focus and paint only. */

const LIST_ID = "command-palette-list";
const optionId = (i: number) => `command-palette-option-${i}`;

/** isTextEntry answers "would this hotkey be stealing a keystroke?". */
function isTextEntry(el: Element | null): boolean {
  if (!(el instanceof HTMLElement)) return false;
  if (el.isContentEditable) return true;
  return el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.tagName === "SELECT";
}

export function CommandPalette() {
  const { can } = useAuth();
  const t = useT(paletteDict);
  const { locale } = useLocale();
  const writesDisabled = useWritesDisabled();
  // useTimeMachine, not useTimeContext: the palette is a CONTROL (it can return the console to
  // Live).
  const { isLive, returnToLive } = useTimeMachine();
  const { theme, toggle } = useTheme();
  const navigate = useNavigate();

  const [open, setOpen] = React.useState(false);
  /* Set when the palette opens; see togglePalette. */
  const [hasPicker, setHasPicker] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const [activeIndex, setActiveIndex] = React.useState(0);
  const inputRef = React.useRef<HTMLInputElement>(null);
  const listRef = React.useRef<HTMLDivElement>(null);
  /* The dialog itself, so the Tab trap can ask what is focusable inside it. */
  const panelRef = React.useRef<HTMLDivElement>(null);
  /* Where focus was when the palette opened, so Escape can put it back. */
  const restoreRef = React.useRef<HTMLElement | null>(null);

  /**
   * openTimeMachinePicker clicks the Time Machine's own trigger, now in the page header; the
   * alternative — engaging at some instant the palette chose.
   */
  const openTimeMachinePicker = React.useCallback(() => {
    const el = document.querySelector<HTMLElement>(TIME_MACHINE_TRIGGER_SELECTOR);
    el?.focus();
    el?.click();
  }, []);

  const ctx = React.useMemo<CommandContext>(
    () => ({
      can,
      writesDisabled,
      // TanStack owns navigation, and `navigate` drops `?at=` exactly the way a <Link> does; the
      // Time Machine CONTEXT survives the move, so the destination still renders at `t`.
      navigate: (path: string) => void navigate({ to: path }),
      theme,
      toggleTheme: toggle,
      isLive,
      returnToLive,
      openTimeMachinePicker,
      hasTimeMachinePicker: hasPicker,
    }),
    [can, writesDisabled, navigate, theme, toggle, isLive, returnToLive, openTimeMachinePicker, hasPicker],
  );

  const results = React.useMemo(
    () => searchCommands(query, buildRegistry(ctx), locale),
    [query, ctx, locale],
  );

  /* One pass builds both the rendered sections and the FLAT order the arrow
     keys walk, so the highlight can never point at a different row than the
     one the eye is on. */
  const { sections, flat } = React.useMemo(() => {
    const flatList: Command[] = [];
    const secs = GROUP_ORDER.map((group) => {
      const items = results
        .filter((c) => c.group === group)
        .map((cmd) => {
          flatList.push(cmd);
          return { cmd, index: flatList.length - 1 };
        });
      return { group, items };
    }).filter((s) => s.items.length > 0);
    return { sections: secs, flat: flatList };
  }, [results]);

  const active = flat.length === 0 ? -1 : Math.min(activeIndex, flat.length - 1);

  const close = React.useCallback((restoreFocus: boolean) => {
    setOpen(false);
    if (restoreFocus) restoreRef.current?.focus();
    restoreRef.current = null;
  }, []);

  /* toggle is what BOTH entry points end in, so the hotkey and the explicit
     PALETTE_OPEN_EVENT can never drift on what "opening" means (focus
     restore, a cleared query, the highlight back at the top). */
  const togglePalette = React.useCallback(
    (from: Element | null) => {
      if (open) {
        close(true);
        return;
      }
      restoreRef.current = from instanceof HTMLElement ? from : null;
      setQuery("");
      setActiveIndex(0);
      /* Whether THIS page has a Time Machine trigger to click, asked once at
         open time. The control is opt-in per page (components/page-shell.tsx),
         so on Targets, Alerting and Settings the picker command had nothing to
         click and answered a keystroke with nothing at all (owner report). */
      setHasPicker(document.querySelector(TIME_MACHINE_TRIGGER_SELECTOR) !== null);
      setOpen(true);
    },
    [open, close],
  );

  /* The global hotkey. The guard is the standard one: a hotkey must not steal
     a keystroke from a field the user is typing in — with the palette's OWN
     input as the deliberate exception, so ⌘K closes what ⌘K opened. */
  React.useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key !== "k" && e.key !== "K") return;
      if (!e.metaKey && !e.ctrlKey) return;
      const el = document.activeElement;
      if (isTextEntry(el) && el !== inputRef.current) return;
      e.preventDefault();
      togglePalette(el);
    }
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [togglePalette]);

  /*
   * A surface that owns its own keymap — the PromQL editor, whose CodeMirror keymap eats Mod-k
   * before the document ever sees it.
   */
  React.useEffect(() => {
    const onOpenRequest = () => togglePalette(document.activeElement);
    window.addEventListener(PALETTE_OPEN_EVENT, onOpenRequest);
    return () => window.removeEventListener(PALETTE_OPEN_EVENT, onOpenRequest);
  }, [togglePalette]);

  React.useEffect(() => {
    if (open) inputRef.current?.focus();
  }, [open]);

  /* Keep the highlighted row in view. Guarded: jsdom has no scrollIntoView,
     the same guard ui/datetime-picker.tsx's wheels carry. */
  React.useEffect(() => {
    if (!open || active < 0) return;
    listRef.current?.querySelector<HTMLElement>(`#${CSS.escape(optionId(active))}`)?.scrollIntoView?.({
      block: "nearest",
    });
  }, [open, active]);

  /**
   * trapTab keeps Tab inside the dialog; the cycle is ui/datetime-picker.tsx's onDialogKeyDown,
   * verbatim in approach.
   */
  function trapTab(e: React.KeyboardEvent) {
    const panel = panelRef.current;
    if (!panel) return;
    const nodes = Array.from(
      panel.querySelectorAll<HTMLElement>("button:not([disabled]), input:not([disabled]), a[href]"),
    ).filter((n) => n.tabIndex >= 0);
    if (nodes.length === 0) return;
    const first = nodes[0];
    const last = nodes[nodes.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }

  function run(cmd: Command) {
    if (isCommandDisabled(cmd, ctx)) return;
    // No focus restore: the command owns focus from here (a route change, or
    // the Time Machine picker it is about to open).
    close(false);
    cmd.perform(ctx);
  }

  function onPanelKeyDown(e: React.KeyboardEvent) {
    if (e.key === "Tab") {
      trapTab(e);
      return;
    }
    if (e.key === "Escape") {
      e.preventDefault();
      close(true);
      return;
    }
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      if (flat.length === 0) return;
      const step = e.key === "ArrowDown" ? 1 : -1;
      setActiveIndex(((active + step) % flat.length + flat.length) % flat.length);
      return;
    }
    if (e.key === "Enter") {
      e.preventDefault();
      if (active >= 0) run(flat[active]);
    }
  }

  if (!open) return null;

  return (
    <div
      data-testid="command-palette-backdrop"
      className="fixed inset-0 z-[100] flex items-start justify-center bg-background/70 p-4 pt-[14vh] backdrop-blur-[2px]"
      onMouseDown={() => close(true)}
    >
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-label={t("dialog")}
        onMouseDown={(e) => e.stopPropagation()}
        onKeyDown={onPanelKeyDown}
        className="pop-enter flex w-full max-w-xl flex-col overflow-hidden rounded-lg border border-border bg-popover text-popover-foreground shadow-pop"
      >
        <div className="flex items-center gap-2 border-b border-border px-3.5">
          <Search aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />
          <input
            ref={inputRef}
            type="text"
            role="combobox"
            aria-label={t("input")}
            aria-expanded="true"
            aria-controls={LIST_ID}
            aria-activedescendant={active >= 0 ? optionId(active) : undefined}
            autoComplete="off"
            spellCheck={false}
            placeholder={t("placeholder")}
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setActiveIndex(0);
            }}
            className="h-11 w-full bg-transparent text-[14px] text-foreground outline-none placeholder:text-muted-foreground"
          />
        </div>

        <div ref={listRef} id={LIST_ID} role="listbox" aria-label={t("list")} className="max-h-80 overflow-y-auto p-1.5">
          {sections.map((section) => (
            /* One translated string for both the accessible name and the
               visible header — the header is aria-hidden precisely because the
               group already carries the name, and the two must not diverge. */
            <div
              key={section.group}
              role="group"
              aria-label={t(GROUP_KEYS[section.group])}
              className="mb-1 last:mb-0"
            >
              <div
                aria-hidden="true"
                className="px-2 pb-1 pt-1.5 text-[10.5px] font-semibold uppercase tracking-[0.1em] text-muted-foreground"
              >
                {t(GROUP_KEYS[section.group])}
              </div>
              {section.items.map(({ cmd, index }) => {
                const disabled = isCommandDisabled(cmd, ctx);
                return (
                  <button
                    key={cmd.id}
                    id={optionId(index)}
                    type="button"
                    role="option"
                    tabIndex={-1}
                    aria-selected={index === active}
                    aria-disabled={disabled || undefined}
                    onMouseMove={() => setActiveIndex(index)}
                    onClick={() => run(cmd)}
                    className={cn(
                      "flex w-full items-center justify-between gap-3 rounded-md px-2 py-1.5 text-left text-[13.5px]",
                      "transition-colors duration-(--dur-fast) ease-(--ease)",
                      index === active ? "bg-accent text-accent-foreground" : "text-foreground",
                      disabled && "opacity-50",
                    )}
                  >
                    <span className="truncate">{commandTitle(cmd, locale)}</span>
                    {disabled ? (
                      <span className="shrink-0 text-[11px] text-muted-foreground">{t("liveOnly")}</span>
                    ) : null}
                  </button>
                );
              })}
            </div>
          ))}
          {flat.length === 0 ? (
            <p className="px-2 py-6 text-center text-[13px] text-muted-foreground">{t("empty")}</p>
          ) : null}
        </div>
      </div>
    </div>
  );
}
