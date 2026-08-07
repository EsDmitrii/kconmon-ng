import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { getMe } from "@/lib/api";
import type { Me } from "@/lib/types";

/**
 * useAuth exposes the current subject (GET /api/v1/auth/me, cached under the
 * shared ["me"] query key — login/logout invalidate exactly this key so
 * every mounted consumer, e.g. the sidebar's user menu, refreshes together)
 * plus a `can(permission)` predicate.
 *
 * UI affordances are HIDDEN when the permission is absent — but the server
 * is the only enforcement point; every mutation is re-checked there
 * regardless of what this hook says. This is cosmetics only, so nobody
 * later "optimises" a server check away on the strength of `can()` having
 * already said yes: a stale/pending `me` still reads `can() === false`
 * (fail-closed on the UI, not evidence either way about the server).
 *
 * `retry: false` because retrying a 401 three times just triples the
 * apiFetch redirect-to-/login side effect for no benefit — the answer does
 * not change on retry, unlike a transient network blip on other queries.
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
  const isAnonymous = data?.subject.kind === "anonymous";
  return { me: data, can, isAnonymous };
}
