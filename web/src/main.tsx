import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "@/components/theme-provider";
import { retryUnlessClientError } from "@/lib/api";
import { LocaleProvider } from "@/lib/i18n";
import { router } from "@/routes";
/* Self-hosted fonts (no external hosts — CSP/air-gap): vite bundles the woff2. */
import "@fontsource-variable/inter";
import "@fontsource-variable/jetbrains-mono";
import "./index.css";

const queryClient = new QueryClient({
  // retryUnlessClientError, not a count: a 4xx problem is a settled answer.
  defaultOptions: { queries: { staleTime: 10_000, refetchOnWindowFocus: false, retry: retryUnlessClientError } },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemeProvider>
        {/* Above the router, next to the theme: both are console-wide display
            preferences owned by the browser, and the language of the chrome
            must not depend on which route is on screen. Reading useT below
            this point needs no wiring — a component outside the provider gets
            English, which is the default anyway (lib/i18n's module doc). */}
        <LocaleProvider>
          <RouterProvider router={router} />
        </LocaleProvider>
      </ThemeProvider>
    </QueryClientProvider>
  </StrictMode>,
);
