import { render, screen, fireEvent } from "@testing-library/react";
import { ThemeProvider } from "@/components/theme-provider";
import { ThemeToggle } from "@/components/theme-toggle";
import { vi } from "vitest";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";

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

test("names the target theme in Russian when the console is in Russian", () => {
  localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
  try {
    render(
      <LocaleProvider>
        <ThemeProvider>
          <ThemeToggle />
        </ThemeProvider>
      </LocaleProvider>,
    );
    expect(screen.getByLabelText(/Переключить на (светлую|тёмную) тему/)).toBeInTheDocument();
  } finally {
    localStorage.removeItem(LOCALE_STORAGE_KEY);
  }
});

/* A browser that blocks site data throws on any localStorage access; the theme is a convenience and
   must not take the whole console down with it. */
test("renders with the default theme when localStorage throws", () => {
  const denied = () => {
    throw new DOMException("Access is denied for this document.", "SecurityError");
  };
  const spyGet = vi.spyOn(localStorage, "getItem").mockImplementation(denied);
  const spySet = vi.spyOn(localStorage, "setItem").mockImplementation(denied);
  try {
    render(
      <ThemeProvider>
        <ThemeToggle />
      </ThemeProvider>,
    );
    expect(screen.getByLabelText(/switch to (light|dark) theme/i)).toBeInTheDocument();
    fireEvent.click(screen.getByLabelText(/switch to (light|dark) theme/i));
  } finally {
    spyGet.mockRestore();
    spySet.mockRestore();
  }
});
