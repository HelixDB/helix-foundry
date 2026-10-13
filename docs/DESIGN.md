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
components that import them:
[`ontology-scene.css`](../apps/web/src/ontology-scene.css) (imported by
`ontology-scene.tsx`) holds the setup illustration, its mint activity marks
and the data feed; `suggested-ontology.css` holds the suggested ontology
review; and each workspace area under `areas/` has its own stylesheet. Read
the final rule and its scope before copying a value from an earlier
stylesheet.

## Visual language

- Warm paper surfaces, olive ink, fine borders and restrained mint accents.
- Georgia display headings; Inter/system sans-serif for controls and body text.
- Calm layouts with generous space, short labels and plain explanations.
- Use icons with text; reserve strong color for activity, selection and status.

The workspace tokens live in `apps/web/src/workspace.css`: `--hf-*` fonts,
colors, type sizes, spacing, radii, shadows and motion. Workspace components
live in `apps/web/src/kit`. Onboarding renders outside `.hf-app` and uses
`minimal.css` for its light paper theme, 14px body text, a 760px centered page
and, at 600px and narrower, 22px side gutters. Each step's heading
(`.setup-content > h1`) is 34px Georgia, and 29px at 600px and narrower; the
suggested ontology's heading (`.so-head h1` in `suggested-ontology.css`) is
also 34px, and 28px at 699px and narrower. Intro copy under a heading
(`.setup-intro`, and the suggestion's summary) is 15px; most other supporting
text is 12–13px. The suggested ontology's schema canvas is wider than the
page: up to 1120px, or 1320px with four or more columns. The illustration and
data feed live in `ontology-scene.css`. Setup styles use fixed light colors
rather than `--hf-*` tokens and setup does not use the kit, so workspace dark
mode does not reach onboarding. Preserve that theme when adding setup UI.

## Progress and feedback

Long-running setup work is shown with `OperationProgress` in
[`setup-components.tsx`](../apps/web/src/setup-components.tsx): imports in
the connection dialog (on setup's Data step, and from "Add data" on the
workspace's Data page), and the workspace build after the ontology is
confirmed. It shows only what the server reports:

- A status line with the operation's name, a spinner while it is queued or
  running, a check once it succeeds, and its state: "Ready", "Stopped" for a
  canceled operation, or otherwise the server's stage or status. The build
  turns its stages into plain words, such as "Building records" or
  "Publishing", with one line about what is happening (`buildStage` in
  `onboarding.tsx`).
- While it works, one line of detail: the latest progress message or event,
  or "Waiting to start", followed by a measured count when the job reports
  one. An import shows "12,345 records read", or "512 KB received" while it
  downloads a file from S3. While the build is publishing, it shows "12,345
  records and 678 relationships read" ("12,345 records read" until it has
  read a relationship). There are no percentages, totals or time estimates.
- When the operation fails or is stopped, an alert with the server's error, or
  "Progress has been saved. Retry when you’re ready."
- "Retry" for a failed or stopped operation and "Stop" while one is queued or
  running. The build hides "Stop" once publishing starts, because the server
  finishes publication atomically.
- The block is a polite live region, so changes are announced without
  interrupting.

No screen shows a progress bar. `OperationProgress` can draw a `<progress>`
when an operation reports a known total, but none of the operations it shows
reports one. The local model download is the only job that does, and no
screen shows it with `OperationProgress`: setup moves on while the model
downloads in the background, and if the download fails, the later steps show
a notice, with "Retry AI connection" and "Change AI choice" for an owner.
Known totals appear as text instead, such as the sample count and the data
feed's table count below.

`useSetup` polls the onboarding state every second while sample data loads,
the AI describes the suggested ontology or the workspace builds, and every 2.5
seconds otherwise. A failed poll leaves the last state on screen; setup shows
the error only when no state has loaded yet. While samples load, the button
counts known work: "Loading sample data… (3 of 11)". A sample load that stops
advancing for 12 seconds is resumed once.

The Ontology step's data feed (`DataFeed` in `ontology-scene.tsx`) shows each
source's tables arriving: "3 of 5 tables" while any is loading, then the
records read. A table name that does not fit is cut short with an ellipsis and
carries its full name in a tooltip. A polite status line reads the current
step and the tables read so far. The steps (`ontology-step.tsx`) mix server
state with pacing. Reading waits for the server to read the tables, matching
waits for the server's suggestion, and "AI is writing descriptions" waits for
the AI run, for at most 20 seconds after the suggestion arrives. "Finding keys"
and "Proposing entities" are timed on the client. Each step is held long
enough to read, so the suggestion appears at least 3.5 seconds after the step
opens, and never before the server has produced it. Returning to the step, or
reloading it, shows an existing suggestion at once.

The Analyst shows each reply's progress as a trace (`Trace` in
[`reply.tsx`](../apps/web/src/areas/analyst/reply.tsx)), between two fine
rules above the answer:

- A status line ("Analyzing your question", "Analysis complete", "Taking
  longer than usual", "Connection lost, reconnecting…") with a status dot,
  the time, "Show steps" and, while it works, "Stop".
- At most three major steps, each one line: its name in bold, then what it did
  or is doing in normal weight. Folded, a line is cut short with an ellipsis
  and its full text in a tooltip; expanded, every step wraps and shows how long
  it took. The steps come from the run's stages (`runSteps` in `chat-data.ts`):
  understand the question, analyze the data, check the results and write the
  answer, or a build's steps. An answer's planned steps show as pending
  rather than done, and a failed or stopped run keeps what finished.
- Expanded, a closed "Query used" disclosure and the model calls and tokens.
  There are no tabs and no raw events or prompts.
- While it works, the elapsed time, and "About 10–20s" once the server has
  measured three or more comparable answers that agree (`estimate` on the run,
  set once when the question is asked). Past that range the status says
  "Taking longer than usual". A failed refresh is shown as a lost connection,
  never as a failed analysis.
- A polite status announces a new step, a long wait, a lost connection and
  how it ended, but never the clock.

The answer under it leads with the summary's first sentence as a bold heading,
then the rest of the summary with the quantities its rows back (a value, a
difference between two values or a row count) and their units in bold. There
is no tint, card or accent behind the answer. The rows show as a chart when the
server's check found a ranking, a trend or a small breakdown, with a
Chart/Table switch; signed breakdowns use bars around a visible zero baseline.
Lowest-first rankings use "Lowest" labels; highest-first rankings use "Top".
A chart of only the result's first rows says so
("Top 10 of 1,000", "First 100 of 1,000 rows"). The table puts the label and
the measure first and keys last and quieter.

Follow the same principles in new progress UI:

- Take status, completion and failure from server state. Cosmetic sub-steps
  may be paced on the client so each can be read, but the result never
  appears before the server reports it. Show counts that were measured; never
  invent a percentage or a time estimate. A time range comes only from
  measured comparable runs, as the Analyst's does.
- Name stages in plain words. Setup keeps the business outcome in view and
  infrastructure in disclosures.
- Keep "Stop" and "Retry" available, except "Stop" once publication has
  begun.
- Announce changes politely and never announce a ticking number: the
  Analyst's elapsed time while it works is hidden from screen readers.
- Pair icons with text, so status never depends on an icon or color alone.
- Mark up steps semantically. The data feed's steps are an ordered list; the
  setup stepper ("AI", "Data", "Ontology") is a `nav` that marks the current
  step with `aria-current="step"`.
- Respect `prefers-reduced-motion`. Setup cuts animation durations to 0.01ms
  and drops transitions; the illustration, data feed and suggested ontology
  stop animating; the workspace cuts animation and transition durations to
  0.01ms and limits each animation to one iteration.
- Never let a long name overflow at narrow widths. An operation's name wraps
  (`overflow-wrap: anywhere`); where a name is cut short instead, as in the
  data feed, its full text is in a tooltip.

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
  `--hf-chart-1` (`analyst.css`). A ranking numbers its bars and draws first
  place at full strength and the rest lighter.
- Chart previews on the Review changes page (`review.tsx`) use Recharts in
  [`charts.tsx`](../apps/web/src/charts.tsx), with hard-coded light colors
  (`#548b72`, `#dfebe4` and `#679780`) that do not follow dark mode.
- Ontology types take hues from fixed eight-color palettes for each theme in
  [`colors.ts`](../apps/web/src/areas/ontology/colors.ts), in alphabetical
  order of the types a view knows; a ninth type and beyond are gray. The
  Explorer's graph orders the types of the records it has loaded, and its
  table orders every type that has records. Hiding a type in the graph or
  choosing one in the table leaves every color in place, but a type's color
  can differ between the full graph, the graph focused on one record and the
  table.
- `--hf-chart-2` to `--hf-chart-6` are defined but not used.

Give new charts theme-aware colors from tokens rather than new hex values.
Home and answer charts explain the data without color: each is a focusable
image whose label summarizes the values, arrow keys step through the points
and a polite live region reads each one, and bar values are printed beside
the bars. Tooltips and the live region spell each value out ("$12,400" where
the chart's label or a bar shows "$12.4K") but still round it as
`formatMetric` does: numbers and percentages to at most two decimals below 10
and one from 10, and currency to the cent, or to the dollar for a whole amount
or one of $10,000 or more. An integer that an answer sends as digits keeps
every digit there, even beyond 2^53. Format values with the kit's
`formatNumber`, `formatCompact`, `formatDate` and `formatRelative`; Home
formats metric values, including currency, with `formatMetric` in
`metrics.tsx`. DuckDB intervals read as DuckDB writes them ("1 day
02:00:00") with `formatInterval` from the shared package, and sort with
`compareIntervals` in the order of DuckDB's `ORDER BY`.

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
