import { Component, type ReactNode } from "react";
import { useT } from "@/lib/i18n";
import { errorBoundaryDict } from "@/lib/i18n/dict/error-boundary";
import { Card } from "@/components/ui/card";

/** RouteErrorFallback is the panel a caught render throw shows; a function so it can translate. */
function RouteErrorFallback({ error }: { error: Error }) {
  const t = useT(errorBoundaryDict);
  return (
    <div className="mx-auto w-full max-w-[1440px] px-4 py-8 sm:px-8 lg:px-10">
      <Card className="p-6" role="alert">
        <h1 className="text-2xl font-semibold tracking-tight">{t("title")}</h1>
        <p className="mt-2 max-w-prose text-sm text-muted-foreground">{t("body")}</p>
        {/* The thrown Error's own name/message, verbatim, so a report can name it. */}
        <p className="mt-3 break-all font-mono text-[12px] text-muted-foreground">
          {error.name}: {error.message}
        </p>
        <button
          type="button"
          onClick={() => window.location.reload()}
          className="mt-4 inline-block text-sm text-primary hover:underline"
        >
          {t("reload")}
        </button>
      </Card>
    </div>
  );
}

/**
 * RouteErrorBoundary catches a render throw inside one routed page so the crash
 * stays in the content area and the shell chrome survives. `resetKey` (the route
 * path) clears the caught error on navigation, so leaving a broken page recovers
 * without a full reload.
 */
export class RouteErrorBoundary extends Component<
  { resetKey: string; children: ReactNode },
  { error: Error | null }
> {
  state: { error: Error | null } = { error: null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidUpdate(prev: { resetKey: string }) {
    if (prev.resetKey !== this.props.resetKey && this.state.error) {
      this.setState({ error: null });
    }
  }

  render() {
    if (this.state.error) return <RouteErrorFallback error={this.state.error} />;
    return this.props.children;
  }
}
