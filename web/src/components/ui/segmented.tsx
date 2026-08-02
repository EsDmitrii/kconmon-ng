import * as React from "react";
import { cn } from "@/lib/utils";

/* Segmented: a real radiogroup with a physically sliding thumb — the active
   background is one absolutely-positioned element translated under the active
   option (compositor-only transform, no layout animation). The track is a
   recessed surface-2 pill; the thumb raises on bg-card + shadow-card. Arrow
   keys move selection and focus together (roving tabindex); options are
   ≥32px tall hit targets. Under jsdom the thumb measures 0 and simply stays
   invisible — the semantics don't depend on it. */
export interface SegmentedOption<T extends string> {
  value: T;
  label: React.ReactNode;
}

export interface SegmentedProps<T extends string> {
  options: readonly SegmentedOption<T>[];
  value: T;
  onChange: (value: T) => void;
  "aria-label": string;
  className?: string;
}

export function Segmented<T extends string>({
  options,
  value,
  onChange,
  className,
  "aria-label": ariaLabel,
}: SegmentedProps<T>) {
  const refs = React.useRef<(HTMLButtonElement | null)[]>([]);
  const trackRef = React.useRef<HTMLDivElement | null>(null);
  const [thumb, setThumb] = React.useState<{ x: number; w: number } | null>(null);

  const activeIndex = options.findIndex((o) => o.value === value);

  const measure = React.useCallback(() => {
    const el = refs.current[activeIndex];
    if (el && el.offsetWidth > 0) setThumb({ x: el.offsetLeft, w: el.offsetWidth });
  }, [activeIndex]);

  React.useLayoutEffect(() => {
    measure();
    const track = trackRef.current;
    if (!track || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(measure);
    ro.observe(track);
    return () => ro.disconnect();
  }, [measure]);

  const move = (from: number, delta: number) => {
    const next = (from + delta + options.length) % options.length;
    onChange(options[next].value);
    refs.current[next]?.focus();
  };

  const onKeyDown = (e: React.KeyboardEvent, i: number) => {
    if (e.key === "ArrowRight" || e.key === "ArrowDown") {
      e.preventDefault();
      move(i, 1);
    } else if (e.key === "ArrowLeft" || e.key === "ArrowUp") {
      e.preventDefault();
      move(i, -1);
    }
  };

  return (
    <div
      ref={trackRef}
      role="radiogroup"
      aria-label={ariaLabel}
      className={cn("relative inline-flex items-center rounded-md bg-surface-2 p-1", className)}
    >
      {thumb ? (
        <span
          aria-hidden="true"
          className="absolute inset-y-1 left-0 rounded-sm bg-card shadow-card transition-[transform,width] duration-(--dur) ease-(--ease)"
          style={{ transform: `translateX(${thumb.x}px)`, width: thumb.w }}
        />
      ) : null}
      {options.map((opt, i) => {
        const active = opt.value === value;
        return (
          <button
            key={opt.value}
            ref={(el) => {
              refs.current[i] = el;
            }}
            type="button"
            role="radio"
            aria-checked={active}
            tabIndex={active ? 0 : -1}
            onClick={() => onChange(opt.value)}
            onKeyDown={(e) => onKeyDown(e, i)}
            className={cn(
              "relative z-[1] h-8 rounded-sm px-3.5 text-sm transition-colors duration-(--dur) ease-(--ease)",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
              active ? "font-medium text-foreground" : "text-muted-foreground hover:text-foreground",
            )}
          >
            {opt.label}
          </button>
        );
      })}
    </div>
  );
}
