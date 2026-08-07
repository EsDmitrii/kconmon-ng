<!--
Status: current
Owner: @EsDmitrii
Source: authored in M0 (design language derived from DESIGN.md §6.1).
Enforced in review. The token names here are the contract the SPA implements
in web/src/index.css.
-->

# Design System v0

Dense, data-first, keyboard-friendly. Dark theme is the default; light is
available. Theme is driven by CSS variables plus `prefers-color-scheme` and
persisted per user. This v0 defines the token vocabulary M0 ships; component
detail grows with the milestones that add real screens.

## Color tokens (shadcn-compatible CSS variables)

Base UI tokens (HSL triplets, consumed as `hsl(var(--token))`):

| Token | Role |
| --- | --- |
| `--background` / `--foreground` | app surface / primary text |
| `--card` / `--card-foreground` | object-card surface |
| `--popover` / `--popover-foreground` | palette, menus |
| `--primary` / `--primary-foreground` | primary actions |
| `--secondary` / `--secondary-foreground` | secondary surfaces |
| `--muted` / `--muted-foreground` | de-emphasized text/surfaces |
| `--accent` / `--accent-foreground` | hover/active accents |
| `--destructive` / `--destructive-foreground` | dangerous actions |
| `--border` / `--input` / `--ring` | borders, inputs, focus ring |

Dark is the default `:root`; `.light` overrides for light mode.

## Health scale (semantic, used everywhere)

A single green → amber → red scale, plus a colorblind-safe alternative
(blue → yellow → magenta) toggled by a user preference.

| Token | Meaning |
| --- | --- |
| `--health-ok` | healthy / pass |
| `--health-warn` | degraded / elevated |
| `--health-bad` | failing / loss |
| `--health-unknown` | no data / stale |

Never encode health with hue alone in charts — pair with shape/label for
accessibility.

## Spacing & radius

- Spacing scale: 4px base (`0.25rem` step): 1, 2, 3, 4, 6, 8, 12, 16.
- Radius: `--radius: 0.5rem`; controls derive `sm/md/lg` from it.
- Density: default row height 32px; compact tables 28px.

## Typography

- UI font: system sans stack (`ui-sans-serif, system-ui, ...`).
- Mono (PromQL, hop tables, logs): `ui-monospace, SFMono-Regular, Menlo, monospace`.
- Sizes: `xs 12 / sm 13 / base 14 / lg 16 / xl 20`. Base UI text is 14px.

## Chart palette

Categorical series use a fixed 8-color palette chosen for dark-first contrast
and colorblind distinguishability; sequential heatmaps use a single-hue ramp
keyed off `--primary`. Defined with the charts that use them (ECharts, M1+).

## Component inventory (M0)

- `Button` (shadcn primitive, variants: default/secondary/ghost/outline).
- `Sidebar` nav with section links (§6.2).
- `ThemeToggle` (dark/light).
- `Banner` (anonymous-mode warning; variant `warning`).
- `StubPage` (placeholder page shell used by every M0 route).

Later milestones add: object cards, matrix heatmap, topology canvas, timeline,
command palette, PromQL editor, tables with hover/export.
