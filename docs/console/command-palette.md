# Command palette

<!-- screenshot: console-command-palette.png pending post-redesign reshoot -->

One keystroke to anywhere. It answers: **how do I get to a page or start an action without hunting the sidebar?**

## Opening the palette

- `Cmd+K` (macOS) / `Ctrl+K` — anywhere in the console. The hotkey never steals a keystroke from a field you are
  typing in, with one deliberate exception: the palette's own input, so `Cmd/Ctrl+K` closes what it opened. The
  [PromQL editor](promql.md) re-binds the key so it works there too.
- The sidebar footer shows the hotkey hint ("⌘K — search and commands").

Inside: type to search; `↑`/`↓` move, `Enter` runs, `Esc` closes.

## Commands

Three groups, in this order:

**Navigation** — one entry per sidebar page, searchable by its title, its description, and its path. Search matches
**both languages regardless of the interface language** — an operator who reads «Матрица» and types "matrix" finds
it, and vice versa. The pre-rename page names (Live, Investigate, Explore, Console, Diagnostics, Targets) remain in
the search corpus, "because operators' muscle memory keeps typing them".

**Actions** — deep links to the page that owns the affordance (never inline forms):

| Command | Lands on | Needs |
| --- | --- | --- |
| Run a diagnostic check… | [Run checks](run-checks.md) | `runs:create` |
| Start an investigation… | [Incidents](incidents.md) | — |
| Create an alert rule… | [Alerting](alerting.md) | `alerts:manage` |
| Declare a maintenance window… | [Metrics](metrics.md#annotations-and-maintenance-windows) | `maintenance:write` |
| Add an annotation… | [Metrics](metrics.md#annotations-and-maintenance-windows) | `annotations:write` |

Entries whose permission you lack are not listed at all. With the [Time Machine](time-machine.md) engaged, write
actions stay visible but disabled, tagged **Live only**.

**View** — *Toggle Time Machine — pick a time…* (only while Live, and only on pages that have the picker), *Return
to Live* (only while engaged), and *Switch to light/dark theme* (the label names the theme it switches **to**).

## Navigation shortcuts

The palette is the console's only global keyboard surface — pages do not define their own single-letter shortcuts.
Two page-local bindings exist and are documented on their pages: `Cmd/Ctrl+Enter` runs a query in the
[PromQL editor](promql.md), and `Ctrl+wheel` zooms the [Matrix](matrix.md) grid.

## Use it when

- You know where you are going — two keystrokes and a word beat any amount of clicking.
- You forgot a page's new name — the old one still finds it.
- You want the fastest route into "run a check" / "start an investigation" mid-incident.

Verified against `web/src/lib/commands.ts`, `web/src/components/command-palette.tsx`,
`web/src/lib/i18n/dict/palette.ts`.
