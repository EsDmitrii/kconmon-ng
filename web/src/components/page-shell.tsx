import type { ReactNode } from "react";

/* PageShell: a centred, breathing column. The key prop on the inner div is the
   page title so route changes re-run the entrance animation — pure
   transform/opacity, collapses under reduced motion. */
export function PageShell({ title, description, actions, children }: {
  title: string; description?: string; actions?: ReactNode; children: ReactNode;
}) {
  return (
    <div className="mx-auto w-full max-w-[1440px] px-8 py-8 lg:px-10">
      <div key={title} className="page-enter flex flex-col gap-7">
        <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
          <div className="min-w-0">
            <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
            {description ? (
              <p className="mt-1 max-w-prose text-sm text-muted-foreground">{description}</p>
            ) : null}
          </div>
          {actions ? <div className="flex flex-wrap items-center gap-2">{actions}</div> : null}
        </div>
        {children}
      </div>
    </div>
  );
}
