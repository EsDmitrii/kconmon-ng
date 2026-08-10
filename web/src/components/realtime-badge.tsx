import { Badge } from "@/components/ui/badge";
import { useT } from "@/lib/i18n";
import { realtimeDict } from "@/lib/i18n/dict/realtime";

/**
 * RealtimeBadge states the transport honestly; it is the design-system Badge, so the label always
 * carries the meaning and the dot only adds a colour channel on top (index.css rule 1: never colour
 * alone).
 */
export function RealtimeBadge({ realtime }: { realtime: boolean }) {
  const t = useT(realtimeDict);
  return (
    <Badge variant={realtime ? "ok" : "warn"} dot title={realtime ? t("live.title") : t("delayed.title")}>
      {realtime ? t("live") : t("delayed")}
    </Badge>
  );
}
