import { render, screen, fireEvent } from "@testing-library/react";
import { ThemeProvider } from "@/components/theme-provider";
import { ThemeToggle } from "@/components/theme-toggle";

test("toggling flips the root theme class", () => {
  render(
    <ThemeProvider>
      <ThemeToggle />
    </ThemeProvider>,
  );
  const root = document.documentElement;
  const before = root.classList.contains("dark");
  // The label names the theme the click switches TO.
  fireEvent.click(screen.getByLabelText(new RegExp(`switch to ${before ? "light" : "dark"} theme`, "i")));
  expect(root.classList.contains("dark")).toBe(!before);
  expect(
    screen.getByLabelText(new RegExp(`switch to ${before ? "dark" : "light"} theme`, "i")),
  ).toBeInTheDocument();
});
