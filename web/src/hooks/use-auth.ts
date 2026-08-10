import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { getMe } from "@/lib/api";
import type { Me } from "@/lib/types";

/**
 * useAuth exposes the current subject (GET /api/v1/auth/me, cached under the shared ["me"] query
 * key — login/logout invalidate exactly this key so every mounted consumer, e.g. the sidebar's user
 * menu, refreshes together) plus a `can(permission)` predicate; UI affordances are HIDDEN when the
 * permission is absent.
 */
export function useAuth(): { me: Me | undefined; can: (p: string) => boolean; isAnonymous: boolean } {
  const { data } = useQuery({
    queryKey: ["me"],
    queryFn: getMe,
    retry: false,
    staleTime: Infinity,
  });
  const permissions = data?.permissions;
  const can = useMemo(() => (p: string) => permissions?.includes(p) ?? false, [permissions]);
  // Optional all the way down; `me` declares `subject` required because the Go handler always
  // emits.
  const isAnonymous = data?.subject?.kind === "anonymous";
  return { me: data, can, isAnonymous };
}
