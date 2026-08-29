# Events

<!-- screenshot: console-events.png pending post-redesign reshoot -->

The controller's event feed, newest first. It answers: **what has the fleet reported, and in what order?**

## What this page shows

Live, the page is fed by pushed WebSocket updates (topic `live` on `ws(s)://<console-host>/ws`) — the page header
carries a **Live** badge while the stream is up and **Delayed data** when it is not. The browser holds the most recent
**2 000** events in a ring; anything older is served from event history (`GET /api/v1/events`), which exists only when
the console has a database.

With the [Time Machine](time-machine.md) engaged, the live tail is off: the feed becomes scrollback ending at the
viewed instant, and **Load older** walks back from there.

Feed columns: **Time**, **Severity**, **Summary**, **Scope**. There is no Type column — the summary opens with the
type's own words. Operator [annotations](metrics.md#annotations-and-maintenance-windows) are interleaved into the feed
at their own timestamp with a **Note** badge (ranged ones read *Annotation (span)*); they are not affected by the
event filters.

## Event kinds

The **Type** filter offers, besides *All types*:

- **Topology changed** — a node or agent joined, left, or changed readiness.
- **Check observed** — a scheduled or continuous check produced a result worth reporting.
- **MTR triggered** — a probe failure fired a reactive traceroute (see [Routes · MTR](routes-mtr.md#reactive-vs-manual-traces)).
- **MTR completed** — that trace finished and its path was recorded.
- **Diagnostic progress** — a run started from [Run checks](run-checks.md) reported progress.

Severities are **Info**, **Warn**, **Error**. Unknown wire values still render raw — the feed never hides an event it
cannot classify.

## Filtering

- **Severity** and **Type** selects (both default *All*).
- **Scope contains** — a case-insensitive substring match on the event's scope; pair input is normalised, so
  `node-a->node-b`, `node-a => node-b` and `node-a > node-b` all match the same pair.
- **Clear filters** resets all three.

The toolbar also has **Pause** / **Resume**: paused, arrivals are buffered ("Paused · 3 buffered") and the badge says
whether the socket is still alive. A counter line reads "Showing {shown} of {held} events · capped at 2000". When the
feed detects holes in the controller's event numbering (or the tab dropped frames while hidden), it says how many
events *may* have been missed and offers a "Why?" explanation — the feed reports its own gaps rather than papering
over them.

If this console replica is not receiving the controller event stream, the page says so explicitly: the feed is
"not broken, it is unfed", Matrix and Topology fall back to 15 s polling, and the feed resumes on its own.

## Deep links

- *open Live* on the [Overview](overview.md) lands here.
- **Load older** pages history back through `GET /api/v1/events` — requires the database
  (Helm: `database.existingSecret`, see [Enable the console](../getting-started/enable-the-console.md)).

## Use it when

- You want the raw, ordered record of what the controller saw — restarts, readiness flaps, reactive MTRs.
- You are watching a change land in real time (pause the feed, read, resume with the buffer intact).
- You need to reconstruct "what happened around 14:32" — engage the Time Machine and walk back.

Verified against `web/src/pages/live.tsx`, `web/src/lib/i18n/dict/live.ts`, `web/src/lib/ws.ts`.
