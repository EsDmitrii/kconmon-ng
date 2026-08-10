import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { RealtimeBadge } from "./realtime-badge";

afterEach(() => {
  cleanup();
  // vitest.setup.ts backs localStorage with ONE Map per test FILE.
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

describe("RealtimeBadge", () => {
  it("shows Live on the ok health token when realtime is up", () => {
    render(<RealtimeBadge realtime />);
    const badge = screen.getByText("Live");
    expect(badge.querySelector("span")?.className).toContain("bg-health-ok");
    expect(badge.getAttribute("title")).toMatch(/pushed/i);
  });

  it("shows Delayed data on the warn health token and explains the fallback", () => {
    render(<RealtimeBadge realtime={false} />);
    const badge = screen.getByText("Delayed data");
    expect(badge.querySelector("span")?.className).toContain("bg-health-warn");
    expect(badge.getAttribute("title")).toMatch(/polling/i);
  });
});

/* Russian The two cases above render with no <LocaleProvider>, which lib/i18n defines as English. */

describe("RealtimeBadge — Russian", () => {
  it("says the feed is online, and keeps the ok token", () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    render(
      <LocaleProvider>
        <RealtimeBadge realtime />
      </LocaleProvider>,
    );
    const badge = screen.getByText("Онлайн");
    expect(badge.querySelector("span")?.className).toContain("bg-health-ok");
    expect(badge.getAttribute("title")).toContain("WebSocket");
  });

  it("names the fallback and its 15s bound, on the warn token", () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    render(
      <LocaleProvider>
        <RealtimeBadge realtime={false} />
      </LocaleProvider>,
    );
    const badge = screen.getByText("Данные с задержкой");
    expect(badge.querySelector("span")?.className).toContain("bg-health-warn");
    // The bound is the load-bearing half: a softer Russian would be a lie the
    // English does not tell.
    expect(badge.getAttribute("title")).toMatch(/REST-опрос раз в 15 с/);
    expect(badge.getAttribute("title")).toMatch(/до 15 с/);
  });
});
