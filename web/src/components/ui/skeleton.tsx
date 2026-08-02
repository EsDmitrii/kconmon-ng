import { cn } from "@/lib/utils";

/* Skeleton: a dim shimmer placeholder (see .skeleton in index.css). Size it
   with width/height utilities; it is aria-hidden — wrap the loading region
   in role="status" with an sr-only label instead. */
export function Skeleton({ className }: { className?: string }) {
  return <span aria-hidden="true" className={cn("skeleton block", className)} />;
}
