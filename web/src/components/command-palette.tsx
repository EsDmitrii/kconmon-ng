import * as React from "react";
import { useNavigate } from "@tanstack/react-router";
import { Search } from "lucide-react";
import { useTheme } from "@/components/theme-provider";
import { TIME_MACHINE_TRIGGER_LABEL } from "@/components/timemachine-bar";
import { useAuth } from "@/hooks/use-auth";
import {
  buildRegistry,
  GROUP_ORDER,
  isCommandDisabled,
  PALETTE_OPEN_EVENT,
  searchCommands,
  type Command,
  type CommandContext,
} from "@/lib/commands";
import { useTimeMachine, useWritesDisabled } from "@/lib/timemachine";
import { cn } from "@/lib/utils";

/**
 * CommandPalette — ⌘K / Ctrl-K overlay over the registry in lib/commands.ts
 * (plan Decision 8: hand-rolled, no cmdk, no kbar, no fuzzy library, zero new
 * dependencies).
 *
 * This component owns keys, focus and paint only. What can be invoked, who may
 * see it and how a query ranks it all live in lib/commands.ts, which is pure
 * and unit-tested without a DOM — so a scoring change is never debugged
 * through a jsdom render.
 *
 * A11Y — this is the reference implementation the M7 a11y sweep (Task 12) will
 * hold the other overlays to:
 *   - the combobox/listbox pattern: a text input with role="combobox",
 *     aria-expanded, aria-controls pointing at the list, and
 *     aria-activedescendant naming the highlighted option. The options are
 *     NOT tab stops (tabIndex -1, the wheel-column idiom from
 *     ui/datetime-picker.tsx) — focus stays in the input, which is what makes
 *     type-and-arrow work at all;
 *   - groups are real role="group" nodes with an accessible name, not bare
 *     styled headers, so the section a row belongs to is announced;
 *   - a disabled entry keeps aria-disabled rather than the `disabled`
 *     attribute: it must remain a legal aria-activedescendant target, and a
 *     user arrowing onto it should hear WHY it will not fire;
 *   - Escape closes and returns focus to whatever had it. Performing a command
 *     deliberately does NOT restore focus — the command owns where focus goes
 *     next (a route change, or the Time Machine picker it just opened).
 *
 * TIME MACHINE COPY — a deviation worth naming. lib/timemachine.tsx's
 * useWritesDisabled says a time-disabled control carries NO per-button
 * tooltip, because the top bar's amber banner is the whole explanation. That
 * reasoning does not survive here: the palette is an overlay with a scrim over
 * that banner. So a disabled row carries a short visible "Live only" tag —
 * visible text rather than a title attribute, so it is part of the option's
 * accessible name instead of a hover-only secret.
 */

const PLACEHOLDER = "Type a command or search…";
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
  const writesDisabled = useWritesDisabled();
  // useTimeMachine, not useTimeContext: the palette is a CONTROL (it can
  // return the console to Live), and a control mounted outside the provider is
  // a wiring bug that should say so — the same line ui/timemachine-bar.tsx
  // takes. AppShell mounts this inside TimeMachineProvider.
  const { isLive, returnToLive } = useTimeMachine();
  const { theme, toggle } = useTheme();
  const navigate = useNavigate();

  const [open, setOpen] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const [activeIndex, setActiveIndex] = React.useState(0);
  const inputRef = React.useRef<HTMLInputElement>(null);
  const listRef = React.useRef<HTMLDivElement>(null);
  /* The dialog itself, so the Tab trap can ask what is focusable inside it. */
  const panelRef = React.useRef<HTMLDivElement>(null);
  /* Where focus was when the palette opened, so Escape can put it back. */
  const restoreRef = React.useRef<HTMLElement | null>(null);

  /**
   * openTimeMachinePicker clicks the Time Machine bar's own trigger.
   *
   * The alternative — engaging at some instant the palette chose — would be
   * the console inventing the answer to the only question that matters, so
   * the ON direction hands the choice back to the picker that already exists.
   * The selector is built from the constant timemachine-bar.tsx exports and
   * renders, so the two cannot drift; nothing in lib/timemachine.tsx changed.
   */
  const openTimeMachinePicker = React.useCallback(() => {
    const el = document.querySelector<HTMLElement>(`[aria-label="${TIME_MACHINE_TRIGGER_LABEL}"]`);
    el?.focus();
    el?.click();
  }, []);

  const ctx = React.useMemo<CommandContext>(
    () => ({
      can,
      writesDisabled,
      // TanStack owns navigation, and `navigate` drops `?at=` exactly the way
      // a <Link> does — the limitation lib/timemachine.tsx already documents
      // ("the shareable-link guarantee holds for the URL you are ON, not
      // across in-app navigations"). The Time Machine CONTEXT survives the
      // move, so the destination still renders at `t`; only the URL forgets,
      // until the next engage/returnToLive rewrites it. Teaching the router
      // about search params is the fix, and it is out of scope here — as it
      // was in M5.
      navigate: (path: string) => void navigate({ to: path }),
      theme,
      toggleTheme: toggle,
      isLive,
      returnToLive,
      openTimeMachinePicker,
    }),
    [can, writesDisabled, navigate, theme, toggle, isLive, returnToLive, openTimeMachinePicker],
  );

  const results = React.useMemo(() => searchCommands(query, buildRegistry(ctx)), [query, ctx]);

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

  /* The explicit ask (QA round 4, finding #18). A surface that owns its own
     keymap — the PromQL editor, whose CodeMirror keymap eats Mod-k before the
     document ever sees it — dispatches lib/commands' PALETTE_OPEN_EVENT
     instead of faking a keystroke. There is no text-entry guard on this path
     on purpose: the sender IS the text entry, and it has already decided. */
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
   * trapTab keeps Tab inside the dialog (QA round 1, finding #8).
   *
   * aria-modal="true" tells assistive tech that everything behind this panel
   * is inert; without a trap the very next Tab put focus on a page the same
   * attribute has just declared unreachable, which is the one combination the
   * ARIA practices call out as broken. The scrim already blocks the pointer,
   * so this closes the keyboard's half of the same door.
   *
   * The cycle is ui/datetime-picker.tsx's onDialogKeyDown, verbatim in
   * approach: query what is focusable NOW rather than caching a list, because
   * typing rebuilds the result rows under it. In practice the palette's own
   * options are tabIndex -1 (focus stays in the combobox input, which is what
   * makes type-and-arrow work), so the cycle usually has ONE stop and Tab is
   * a no-op that goes nowhere instead of leaving.
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
        aria-label="Command palette"
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
            aria-label="Type a command or search"
            aria-expanded="true"
            aria-controls={LIST_ID}
            aria-activedescendant={active >= 0 ? optionId(active) : undefined}
            autoComplete="off"
            spellCheck={false}
            placeholder={PLACEHOLDER}
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setActiveIndex(0);
            }}
            className="h-11 w-full bg-transparent text-[14px] text-foreground outline-none placeholder:text-muted-foreground"
          />
        </div>

        <div ref={listRef} id={LIST_ID} role="listbox" aria-label="Commands" className="max-h-80 overflow-y-auto p-1.5">
          {sections.map((section) => (
            <div key={section.group} role="group" aria-label={section.group} className="mb-1 last:mb-0">
              <div
                aria-hidden="true"
                className="px-2 pb-1 pt-1.5 text-[10.5px] font-semibold uppercase tracking-[0.1em] text-muted-foreground/70"
              >
                {section.group}
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
                    <span className="truncate">{cmd.title}</span>
                    {disabled ? (
                      <span className="shrink-0 text-[11px] text-muted-foreground">Live only</span>
                    ) : null}
                  </button>
                );
              })}
            </div>
          ))}
          {flat.length === 0 ? (
            <p className="px-2 py-6 text-center text-[13px] text-muted-foreground">Nothing matches</p>
          ) : null}
        </div>
      </div>
    </div>
  );
}
