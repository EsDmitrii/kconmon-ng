import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "@/components/theme-provider";
import { retryUnlessClientError } from "@/lib/api";
import { router } from "@/routes";
import "./index.css";

const queryClient = new QueryClient({
  // retryUnlessClientError, not a count: a 4xx problem is a settled answer —
  // retrying a 409/422 can only succeed by the SERVER changing its mind, and
  // a retry that react-query PAUSES (offline flicker) leaves the query
  // pending forever: QA scope 6 #2 found the foreign-rules section stuck on
  // a skeleton over a 3ms 409 exactly this way. Server errors keep one retry.
  defaultOptions: { queries: { staleTime: 10_000, refetchOnWindowFocus: false, retry: retryUnlessClientError } },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemeProvider>
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>
  </StrictMode>,
);
