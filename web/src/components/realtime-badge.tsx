import { Badge } from "@/components/ui/badge";

const TITLE_LIVE = "Realtime is up: this view is fed by pushed WebSocket updates.";
const TITLE_DELAYED =
  "Realtime is unavailable — this console replica is not receiving the controller event stream. " +
  "Falling back to 15s REST polling, so data can be up to 15s old.";

/**
 * RealtimeBadge states the transport honestly. "Delayed data" is not an error
 * state: the M1 polling path is a supported mode (controller.events.enabled=false
 * is a valid deployment), so the badge explains rather than alarms — warn, not bad.
 *
 * It is the design-system Badge, so the label always carries the meaning and the
 * dot only adds a colour channel on top (index.css rule 1: never colour alone).
 * The health tokens come from the badge's own ok/warn variants; no new colour.
 */
export function RealtimeBadge({ realtime }: { realtime: boolean }) {
  return (
    <Badge variant={realtime ? "ok" : "warn"} dot title={realtime ? TITLE_LIVE : TITLE_DELAYED}>
      {realtime ? "Live" : "Delayed data"}
    </Badge>
  );
}
