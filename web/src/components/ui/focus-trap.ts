/* The Tab trap of the kit's two modal surfaces, ui/modal and the navigation drawer. */

const TABBABLE_SELECTOR =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

/** The controls inside `root` that Tab can land on, in document order. A hidden one is skipped
 *  unless it holds focus, so the cycle never parks on something the reader cannot see. */
function tabbables(root: HTMLElement): HTMLElement[] {
  return [...root.querySelectorAll<HTMLElement>(TABBABLE_SELECTOR)].filter(
    (el) => el.checkVisibility?.({ visibilityProperty: true }) !== false || el === document.activeElement,
  );
}

/** cycleTab keeps a Tab keypress inside `panel`: past the last control it wraps to the first, and
 *  Shift+Tab from the first control or from the panel itself wraps to the last. */
export function cycleTab(panel: HTMLElement, event: { key: string; shiftKey: boolean; preventDefault(): void }) {
  if (event.key !== "Tab") return;
  const items = tabbables(panel);
  if (items.length === 0) {
    event.preventDefault();
    panel.focus();
    return;
  }
  const first = items[0];
  const last = items[items.length - 1];
  const active = document.activeElement;
  if (event.shiftKey && (active === first || active === panel)) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && active === last) {
    event.preventDefault();
    first.focus();
  }
}
