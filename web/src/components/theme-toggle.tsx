import { Moon, Sun } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useTheme } from "@/components/theme-provider";

/* The icon swap gets a tiny rotation+fade so the toggle feels mechanical
   rather than a hard swap; aria-label names the theme it switches TO. */
export function ThemeToggle() {
  const { theme, toggle } = useTheme();
  const next = theme === "dark" ? "light" : "dark";
  return (
    <Button variant="ghost" size="icon" onClick={toggle} aria-label={`Switch to ${next} theme`}>
      <span
        key={theme}
        className="pop-enter inline-flex"
      >
        {theme === "dark" ? <Sun className="size-4" /> : <Moon className="size-4" />}
      </span>
    </Button>
  );
}
