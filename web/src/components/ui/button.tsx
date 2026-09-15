import * as React from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { Loader2 } from "lucide-react";
import { cn } from "@/lib/utils";

/* disabled:opacity-65, not 50: at half opacity a disabled label measured 3.18:1
   against the light theme's page background, under WCAG AA's 4.5:1 for 14px
   text, and a disabled control still has to be READ — the Sync button on
   /alerting carries the reason it is off in its own title. 65 lands at 5.03:1
   on light and 7.44:1 on dark, so one value covers both themes.

   A disabled PRIMARY also stops looking primary: muted fill, no shadow, the
   plain foreground for its label. Opacity alone left a live-looking blue
   button in dark mode, and a disabled primary is a real state on Run checks,
   Settings import and every mutation the Time Machine locks. The label token
   was measured, not picked: muted-foreground composited at .65 over the page
   is 2.44:1 on light and 3.28:1 on dark, under AA; foreground is 4.77:1 and
   6.89:1 (4.71:1 / 6.90:1 on a card), so that is the one that ships.

   destructive is the confirm step of every delete: the primary treatment in
   the destructive hue, so the second click reads as the irreversible one and
   the page keeps its single blue action. */
const buttonVariants = cva(
  "inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-medium transition-[background-color,box-shadow,color] duration-(--dur) ease-(--ease) focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background disabled:pointer-events-none disabled:opacity-65",
  {
    variants: {
      variant: {
        default:
          "bg-primary text-primary-foreground hover:bg-primary/90 disabled:bg-muted disabled:text-foreground disabled:shadow-none",
        destructive: "bg-destructive text-destructive-foreground hover:bg-destructive/90",
        secondary: "bg-secondary text-secondary-foreground hover:bg-secondary/80",
        outline: "border border-border-strong bg-transparent hover:bg-accent hover:text-accent-foreground",
        ghost: "hover:bg-accent hover:text-accent-foreground",
      },
      size: {
        default: "h-9 px-4 py-2",
        sm: "h-8 rounded-md px-3",
        icon: "h-9 w-9",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);

export interface ButtonProps
  extends React.ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof buttonVariants> {
  /* Shows a spinner, sets aria-busy and disables the button — replaces the
     "swap the label to Running…" pattern so the width never jumps. */
  loading?: boolean;
}

export const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(
  ({ className, variant, size, loading = false, disabled, children, ...props }, ref) => (
    <button
      ref={ref}
      className={cn(buttonVariants({ variant, size, className }))}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      {...props}
    >
      {loading ? <Loader2 aria-hidden="true" className="size-4 animate-spin" /> : null}
      {children}
    </button>
  ),
);
Button.displayName = "Button";
