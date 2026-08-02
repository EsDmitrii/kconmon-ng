import { Hourglass } from "lucide-react";
import { PageShell } from "@/components/page-shell";
import { Card } from "@/components/ui/card";

/* A proper Blank Slate: what this view is, and the honest fact that it arrives
   with a later milestone — not a broken-looking placeholder. */
export function StubPage({ title, description }: { title: string; description: string }) {
  return (
    <PageShell title={title} description={description}>
      <Card className="flex flex-col items-center gap-3 px-8 py-16 text-center">
        <span
          aria-hidden="true"
          className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
        >
          <Hourglass className="size-5" />
        </span>
        <p className="text-sm font-medium">Not built yet — on the roadmap</p>
        <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">
          This view is delivered in a later milestone. The navigation shows the full product so
          the information architecture stays honest about what is coming.
        </p>
      </Card>
    </PageShell>
  );
}
