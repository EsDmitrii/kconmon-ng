import { useLayoutEffect, useState, type RefObject } from "react";

/**
 * useFillHeight sizes the feed to end where <main>'s visible area does. The chrome above it is one
 * line on a wide screen and four on a phone, so no fixed offset fits both: too small and <main>
 * scrolls around a scrolling feed, too large and a blank band sits under the last row. Undefined
 * until measured (and outside the app shell), when the class-based height stands; the class's
 * min-height floors both.
 */
export function useFillHeight(ref: RefObject<HTMLElement | null>, active: boolean): number | undefined {
  const [height, setHeight] = useState<number>();
  useLayoutEffect(() => {
    const el = ref.current;
    const main = el?.closest("main");
    if (!active || !el || !main) return;
    let shell: HTMLElement = el;
    while (shell.parentElement && shell.parentElement !== main) shell = shell.parentElement;
    const measure = () => {
      const box = el.getBoundingClientRect();
      const top = box.top - main.getBoundingClientRect().top + main.scrollTop;
      const below = shell.getBoundingClientRect().bottom - box.bottom;
      setHeight(Math.floor(main.clientHeight - top - below));
    };
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(measure);
    ro.observe(main);
    ro.observe(shell);
    return () => ro.disconnect();
  }, [ref, active]);
  return height;
}
