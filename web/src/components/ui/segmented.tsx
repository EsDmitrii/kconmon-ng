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
  /* y/h ride along with x/w (QA round 4, finding #17). A track that WRAPS —
     the six-option check-type picker on /diagnostics is the first one to — has
     its options on more than one row, and a thumb positioned by translateX
     alone under a fixed inset-y sat on row one no matter which option was
     selected. Measuring the active option's own box in both axes costs
     nothing on a single-row track (where offsetTop is simply the track's
     padding) and is the only thing that makes a wrapped one honest. */
  const [thumb, setThumb] = React.useState<{ x: number; y: number; w: number; h: number } | null>(null);

  const activeIndex = options.findIndex((o) => o.value === value);

  const measure = React.useCallback(() => {
    const el = refs.current[activeIndex];
    if (el && el.offsetWidth > 0) {
      setThumb({ x: el.offsetLeft, y: el.offsetTop, w: el.offsetWidth, h: el.offsetHeight });
    }
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
      /* shrink-0 (QA round 3, finding #20): the track is one control and wraps
         AS A WHOLE inside its parent's flex-wrap row. Without it a narrow
         viewport compressed the track instead of wrapping it, and the flex
         children absorbed that by breaking their own labels — "Zone pair"
         became two lines inside a 32px-tall pill, which pushed the thumb out
         of alignment with the text it is supposed to sit under. */
      className={cn("relative inline-flex shrink-0 items-center rounded-md bg-surface-2 p-1", className)}
    >
      {thumb ? (
        <span
          aria-hidden="true"
          className="absolute left-0 top-0 rounded-sm bg-card shadow-card transition-[transform,width,height] duration-(--dur) ease-(--ease)"
          style={{ transform: `translate(${thumb.x}px, ${thumb.y}px)`, width: thumb.w, height: thumb.h }}
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
              /* whitespace-nowrap: a two-word option ("Zone pair") is ONE
                 label and never breaks (finding #20). */
              "relative z-[1] h-8 whitespace-nowrap rounded-sm px-3.5 text-sm transition-colors duration-(--dur) ease-(--ease)",
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
