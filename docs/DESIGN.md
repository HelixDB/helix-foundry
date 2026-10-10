# Helix design standards

These standards describe the current product styles, shell and component
contracts. They cover the workspace and its onboarding flow. The
implementation is the source of truth; reuse its tokens and components.

## Source map

| Source                                                                                          | Responsibility                                                                                        |
| ----------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| [`style.css`](../apps/web/src/style.css)                                                        | Tailwind and the original base styles; later layers override them                                     |
| [`minimal.css`](../apps/web/src/minimal.css)                                                    | Onboarding and earlier pages, including setup widths and responsive rules                             |
| [`workspace.css`](../apps/web/src/workspace.css)                                                | Workspace tokens, themes and base styles inside `.hf-app`, and a bridge for legacy tokens             |
| [`kit/index.ts`](../apps/web/src/kit/index.ts) and [`kit/kit.css`](../apps/web/src/kit/kit.css) | Shared controls, page containers, tables, panels and status components                                |
| [`shell/shell.css`](../apps/web/src/shell/shell.css)                                            | Workspace shell: sidebar, mobile bar and navigation drawer, command palette, activity tray and toasts |

`main.tsx` imports these stylesheets in this order, so a later rule wins over
an earlier one of the same specificity. Other stylesheets load with the
components that import them: [`ontology-scene.css`](../apps/web/src/ontology-scene.css)
(imported by `ontology-scene.tsx`) holds the setup illustration, its mint
activity marks and the data feed; `suggested-ontology.css` holds the suggested
ontology review; and each workspace area under `areas/` has its own
stylesheet. Read the final rule and its scope before copying a value from an
earlier stylesheet.

## Visual language

- Warm paper surfaces, olive ink, fine borders and restrained mint accents.
- Georgia display headings; Inter/system sans-serif for controls and body text.
- Calm layouts with generous space, short labels and plain explanations.
- Use icons with text; reserve strong color for activity, selection and status.

The workspace tokens live in `apps/web/src/workspace.css`: `--hf-*` fonts,
colors, type sizes, spacing, radii, shadows and motion. Workspace components
live in `apps/web/src/kit`. Onboarding renders outside `.hf-app` and uses
`minimal.css` for its light paper theme, 30px serif page headings, 14px body
text, 12–13px supporting text, a 760px centered page and, at 600px and
narrower, 22px side gutters. Its illustration and data feed live in
`ontology-scene.css`. Setup styles use fixed light colors rather than `--hf-*`
tokens and setup does not use the kit, so workspace dark mode does not reach
onboarding. Preserve that theme when adding setup UI.

## Progress and feedback

Long-running setup work is shown with `OperationProgress` in
[`setup-components.tsx`](../apps/web/src/setup-components.tsx): imports in
the connection dialog (on setup's Data step, and from Add data on the
workspace's Data page), and the workspace build after the ontology is
confirmed. It shows only what the server reports:

- A status line with the operation's name, a spinner while it is queued or
  running, a check once it succeeds, and its state: “Ready”, “Stopped” for a
  canceled operation, or otherwise the server's stage or status. The build
  turns its stages into plain words, such as “Building records” or
  “Publishing”, with one line about what is happening (`buildStage` in
  `onboarding.tsx`).
- While it works, one line of detail: the latest progress message or event,
  or “Waiting to start”, followed by a measured count when the job reports
  one, such as `12,345 records read` or `512 KB received`. There are no
  percentages, totals or time estimates.
- A `<progress>` bar only when the operation reports how much of a known total
  is done. Only the local model download reports a total, and the bar is
  labelled for it.
- When the operation fails or is stopped, an alert with the server's error, or
  “Progress has been saved. Retry when you're ready.”
- Retry for a failed or stopped operation and Stop while one is queued or
  running. The build hides Stop once publishing starts, because the server
  finishes publication atomically.
- The block is a polite live region, so changes are announced without
  interrupting.

`useSetup` polls the onboarding state every second while sample data loads,
the AI describes the suggested ontology or the workspace builds, and every 2.5
seconds otherwise. A failed poll leaves the last state on screen; setup shows
the error only when no state has loaded yet. While samples load, the button
counts known work: “Loading sample data… (3 of 11)”. A sample load that stops
advancing for 12 seconds is resumed once.

The Ontology step's data feed (`DataFeed` in `ontology-scene.tsx`) shows each
source's tables arriving: “3 of 5 tables” while any is loading, then the
records read. A polite status line reads the current step and the tables read
so far. The steps advance on server events (the tables are read, the
suggestion is ready), and each is held for a moment so it can be read; the
suggestion appears only once the server has produced it.

Follow the same principles in new progress UI:

- Drive stages, status and completion from server state. Show counts that
  were measured; never invent a percentage or a time estimate.
- Name stages in plain words. Setup keeps the business outcome in view and
  infrastructure in disclosures.
- Keep Stop and Retry available, except Stop once publication has begun.
- Announce changes politely and never announce a ticking number: the
  Analyst's elapsed time while it works is hidden from screen readers.
- Pair icons with text, so status never depends on an icon or color alone.
- Use semantic lists for steps and native progress elements for known totals.
- Respect `prefers-reduced-motion`: setup and the workspace cut animation
  durations to 0.01ms, and the workspace also limits each animation to one
  iteration.
- Wrap long names rather than letting them overflow at narrow widths.

## Workspace foundations

Use semantic `--hf-*` tokens in workspace styles. The light palette below is a
reference for recognizing the product; dark mode overrides these same roles.

| Role               | Token              | Existing light value |
| ------------------ | ------------------ | -------------------- |
| Paper background   | `--hf-bg`          | `#f8f7f3`            |
| Raised panel       | `--hf-raised`      | `#fdfcf9`            |
| Sunken surface     | `--hf-sunken`      | `#f0efe9`            |
| Body text          | `--hf-text`        | `#292a26`            |
| Secondary text     | `--hf-text-2`      | `#4f534a`            |
| Supporting text    | `--hf-muted`       | `#65675e`            |
| Divider            | `--hf-border`      | `#e4e2da`            |
| Primary action ink | `--hf-ink`         | `#343932`            |
| Mint accent        | `--hf-accent`      | `#36b98a`            |
| Accent text        | `--hf-accent-text` | `#2d6a4b`            |
| Keyboard focus     | `--hf-focus`       | `#2a8f69`            |

Keep fills and text roles distinct: the brighter mint is an accent, while the
darker accent text is for labels. `--hf-faint` is for icons and decoration.
Do not replace semantic tokens with light-only hex colors in workspace pages.
The workspace supports system dark mode and explicit theme selection; setup
retains its light paper theme outside `.hf-app`.

The existing type scale is 11, 12, 13, 14, 16, 20, 26 and 34px. Use the matching
`--hf-text-*` role. Body and controls use `--hf-font-sans`; display headings use
`--hf-font-display`; identifiers and code use `--hf-font-mono`. The workspace
page heading is 26px, and 23px at 800px and narrower. Use tabular numbers
(`.hf-num`) for metrics, counts and timers.

Spacing tokens cover 4, 8, 12, 16, 24, 32, 40, 48 and 64px. Existing radii are
6, 8, 12 and 16px, plus a pill radius. Prefer fine borders and `--hf-shadow-1`
for ordinary controls; reserve larger shadows for elevated overlays. Default
control transitions use `--hf-duration` (160ms) and `--hf-ease`.

## Page and component patterns

| Need                     | Existing component and convention                                                                                                   |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------------- |
| Page layout              | `Page` with `narrow`, `default`, `wide` or `full` width; `PageHeader` for title, subtitle, metadata, actions and optional tabs      |
| Navigation within a page | `Breadcrumbs` for hierarchy; `Tabs` and `TabPanel` for peer views; keep the selected tab in the URL with `useTabParam`              |
| Actions                  | `Button`, `ButtonLink`, `ButtonAnchor` and `IconButton`; choose the element according to whether it performs an action or navigates |
| Data inspection          | `DataGrid`, `Toolbar`, `SearchInput`, `FilterSelect` and `FilterBar`; keep filtering and row actions consistent                     |
| Grouped content          | `Section`, `Panel`, `SplitView` and `KeyValue`; preserve the hierarchy between page, section and record detail                      |
| Metrics                  | `StatCard`, shared number formatting, and Home's `TrendChart` and `BreakdownChart`                                                  |
| Source identity          | `SourceLogo` and `SourceStack`, using existing connector labels and logos                                                           |
| State and feedback       | `StatusBadge`, `StatusDot`, `EmptyState`, `Skeleton` and `SkeletonText`                                                             |
| Overlays                 | `Dialog`, `ConfirmDialog` and `DropdownMenu`; use the shared focus, close and keyboard behavior                                     |

Buttons have five existing variants: primary, secondary, ghost, accent and
danger. Their small, medium and large heights are 28, 34 and 40px. Use primary
for the main action, secondary for regular alternatives, ghost for quieter
actions and danger for destructive operations. The component's `pending`
state disables repeated submission and exposes `aria-busy`. Icon-only buttons
require a meaningful label.

Status is shared language: queued and draft are neutral; running and syncing
use info; healthy and succeeded use success; stale and review use warning;
failed uses danger; paused and canceled are neutral. Use the existing
`toStatus`, `statusLabel` and `statusTone` mapping rather than assigning new
colors or labels independently.

Charts do not share one palette yet:

- Home's key metrics draw `TrendChart` and `BreakdownChart`
  ([`metrics.tsx`](../apps/web/src/areas/workspace/metrics.tsx)) in a single
  series color, `--wk-series`: `--hf-accent-strong` in light mode and a fixed
  `#32aa7f` in dark mode (`home.css`).
- Analyst answers reuse those two charts with `--wk-series` set to
  `--hf-chart-1` (`analyst.css`).
- Chart previews on the Review changes page (`review.tsx`) use Recharts in
  [`charts.tsx`](../apps/web/src/charts.tsx), with hard-coded light colors
  (`#548b72`, `#dfebe4` and `#679780`) that do not follow dark mode.
- Ontology types take hues from fixed eight-color palettes for each theme in
  [`colors.ts`](../apps/web/src/areas/ontology/colors.ts), in alphabetical
  order of type, so a type keeps its color; a ninth type and beyond are grey.
- `--hf-chart-2` to `--hf-chart-6` are defined but not used.

Give new charts theme-aware colors from tokens rather than new hex values.
Home and answer charts explain the data without color: each is a focusable
image whose label summarizes the values, arrow keys step through the points
and a polite live region reads each one, and bar values are printed beside
the bars. Format values with the kit's `formatNumber`, `formatCompact`,
`formatDate` and `formatRelative`; Home formats metric values, including
currency, with `formatMetric` in `metrics.tsx`.

## Responsive and accessible behavior

The workspace shell has a 236px sidebar and a 60px collapsed variant. At 800px
and narrower, a top bar with a navigation drawer replaces the sidebar. Use the
shell's responsive rules rather than giving a new page its own navigation
layout. At the same width, page-header actions move beneath the title.
Preserve readable labels, wrap supporting copy, and contain wide tables within
their data view. Check both the normal viewport and a 390px phone viewport.

Workspace focus uses a 2px `--hf-focus` outline; setup has its own visible
2px outline. Use real buttons, links, headings, labeled controls and table
semantics. Announce meaningful status changes and errors, and hide decorative
icons from assistive technology. `Dialog` is a native modal `<dialog>`: it
traps focus, closes with Escape or the backdrop, and returns focus when it
closes. Respect `prefers-reduced-motion` for both the shell and setup.

Before delivering a UI change, inspect its final style scope, pending and
failure states, keyboard labels, mobile wrapping and reduced-motion behavior.
Preserve connected sources and user data when improving feedback or recovery.
