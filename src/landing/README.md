# The landing page

The hosted site's `/` for a visitor who isn't signed in (`src/app/(landing)`, a root layout of its own so the app's global styles never reach it; anyone signed in, and every local build, goes to the app). It came from HelixDB/foundry-website (`8c49e40`): static markup in `landing.tsx`, with React rendering only the four product figures and the grid of connectors, all in the browser (`figures.tsx`).

The figures are the local app's own components as they were at `00d7927`, before this repository held the site: `vendor/web` (that app's `apps/web/src`) and `vendor/shared` (its `packages/shared/src`), changed only to build here. They are not today's app's, which no longer has the setup scene or the Analyst, so the figures don't follow changes to it.

## What it shows

- **Schema** is the app's own `SuggestedOntologyView` (`vendor/web/suggested-ontology.tsx`), read-only, with a dark skin in `styles/schema.css`.
- **Explorer** is a demo, not a control. It reuses the markup of the local app's `ObjectGraph`, its force layout and its type colours. The layout was worked out by the data export (`field/force.ts`) and stored with the records, so the page never runs the simulation. Its links are drawn as Fig. 1's connectors.
- **Ontology** (Fig. 1) is the top of the app's setup scene, `AiWorkingScene` (`vendor/web/ontology-scene.tsx`): the ontology taking shape and the apps above it. `styles/scene.css` hides the data and models plates and crops the drawing under the ontology. `scene.tsx` loops it through its stages while it is on screen.
- **Analyst** (Fig. 2) is the app's own `Reply` (`vendor/web/areas/analyst/reply.tsx`) in a copy of its chat, `analyst.tsx`, with a dark skin in `styles/analyst.css`. It replays three recorded runs: each question types itself out, the work shows as one status line, and the answer writes itself in. Nothing in it takes the pointer.
- **Connectors** is a grid of every connector and AI provider, each on a placard like the setup scene's tiles, `connect.tsx` and `styles/connect.css`. It uses the app's marks in `public/connectors` and `public/providers` and the site's own in `public/landing/connectors` and `public/landing/providers`, whose READMEs give their sources and licences.
- All of them show **Loopwell**, the local app's old sample company, from the three files in `data`, exported by running that app's setup, a build and the Analyst in memory. The export ran in that app, which this repository no longer has, so the data is frozen.

## On a phone

At most 760px wide (`phone.ts`, mirrored by `.phone-only` and `.desktop-only` in `styles/base.css`), a screen gets a shorter page. Most phone visits come from social posts, and nothing on the full page can be done on a phone:

- **The hero** keeps its heading and line, then offers the hand-off in place of the calls to action that need a computer:
  - **Try it on your computer** (`remind.tsx`): an email, and Foundry reminds the visitor to try it on a computer. The first reminder goes at the next weekday 9am in the browser's time zone. The consent (`CONSENT_TEXT`, `src/shared/reminders.ts`) shows once something is typed. The form posts to `/remind` (`src/app/remind`, `src/server/reminders.ts`), which forwards to Foundry's door with a key of the visitor's address, so the backend can cap what one visitor asks for (5 an hour). A rate limit says how long to wait.
  - **Star on GitHub to find it later**.
  - **Or start on Helix Cloud now**.
  - Without `FOUNDRY_REMINDERS_SECRET` (`remind` is false) the form gives way to a line saying to open the page on a computer.
- **What stays below:** the Analyst, the shout (its call to action jumps back to the form), the connectors and the footer.
- **What goes:**
  - the field (`behaviours.tsx` never starts it, and the footer's copy of the graph is hidden);
  - the schema and the Explorer, which the field draws for;
  - the setup scene;
  - the self-hosted box.

  `figures.tsx` mounts those figures only on a wider screen (`useWide`), so their chunks never load on a phone. A phone turned wide enough mounts them and starts the field, and turned back stops it and hides its canvas.

## Coming back

A reminder's links carry `?via=email`, and the repository README's carry `?via=github`. `src/app/(landing)/page.tsx` reads `via` into the markup:

- On a computer, both put **Try the sample company** first (`/login?return_to=/ontology/sample`), with a line on why. A reminder's visit also gets a Welcome back.
- `behaviours.tsx` counts the visit (`landing_returned`) and takes `via` off the address.
- A signed-in visitor's redirect to the app drops it too.

## The field

`field/field.ts` draws one fixed canvas behind the page. Its particles move through three formations as you scroll, each pinned to an element that registers with `attach()`:

| Formation | Pinned to                  | What happens                                                                                                                                                                                   |
| --------- | -------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| hero      | `[data-field="hero"]`      | A turning graph of tiny nodes and hairline links that fills the screen, and stays put as you scroll.                                                                                           |
| schema    | the schema's `.so-stage`   | From the first scroll, the nodes ease down into the stage's dotted ground, one particle per 20 px dot. Then the cards rise.                                                                    |
| explorer  | the Explorer's `.og-stage` | Some dots take their type's colour and drop in as the records; the rest become its ground. Once they have landed they are drawn on the stage's own canvas, so they move with it as you scroll. |

The footer, `[data-field="floor"]`, shows the hero's graph again, centred and pinned to the footer so it scrolls with the page. It only sways a little, and the particles never travel there.

The field also sets `--fill` on the box after Fig. 2, `[data-field="shout"]`, so it fills green as you reach it.

Positions are a function of scroll and time only, so scrolling back reverses everything and a resize rebuilds. Scrolling drives the formations through a damped spring: a fast flick down still plays each move out, and going back up replays it quickly. With reduced motion, the graph stays still and the formations cross-fade instead of moving.
