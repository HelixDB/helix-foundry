import { useEffect } from "react";
import { trackEvent } from "@/web/analytics";
import { onboardingPrompt } from "@/web/areas/ontology/onboarding-prompt";
import { attach, start, type Anchor } from "./field/field";
import { page } from "./page";
import { PHONE } from "./phone";

// The elements the field pins its formations to, besides the schema and the
// Explorer, which pin their own.
const ANCHORS: [Anchor, string][] = [
  ["hero", '[data-field="hero"]'],
  ["cloud", "#cloud"],
  ["floor", '[data-field="floor"]'],
  ["analyst", '[data-field="analyst"]'],
  ["shout", '[data-field="shout"]'],
];

// Where a visit came from that the landing page greets (src/app/(landing)
// page.tsx reads it into the markup): a reminder's link, or the README's.
const VIA = new Set(["email", "github"]);

// Starts the field behind the page and the page's small behaviours, and
// stops them when the page goes. Writes the prompt Set up with your agent
// copies first: the app's, for someone without a workspace yet, on this
// stamp's Helix MCP (next.config.ts). A phone (./phone.ts) gets no field:
// it starts when a phone turns wide enough to show the figures the field
// draws for, and stops when it turns back.
export function Behaviours() {
  useEffect(() => {
    document.getElementById("agent-prompt")!.textContent = onboardingPrompt({
      origin: location.origin,
      mcpUrl:
        process.env.NEXT_PUBLIC_HELIX_MCP_URL || "https://mcp.helix-db.com/mcp",
      eventsUrl: process.env.NEXT_PUBLIC_HELIX_EVENTS_URL || "",
      workspace: null,
    });
    // A visit from a reminder or the README counts once, and the link's
    // ?via= goes, so a link copied from here doesn't count again.
    const url = new URL(location.href);
    const via = url.searchParams.get("via");
    if (via !== null) {
      if (VIA.has(via))
        trackEvent("landing_returned", { via: via as "email" | "github" });
      url.searchParams.delete("via");
      history.replaceState(history.state, "", url);
    }
    ANCHORS.forEach(([name, selector]) =>
      attach(name, document.querySelector<HTMLElement>(selector)),
    );
    // The field draws text, so it waits for the fonts before its first frame.
    const phone = matchMedia(PHONE);
    let stop: (() => void) | undefined,
      fonts = false,
      gone = false;
    const begin = () => {
      if (gone || stop || !fonts || phone.matches) return;
      document.documentElement.classList.add("has-field");
      stop = start(
        document.querySelector<HTMLCanvasElement>("[data-field-canvas]")!,
      );
    };
    const end = () => {
      stop?.();
      stop = undefined;
      document.documentElement.classList.remove("has-field");
    };
    const turned = () => (phone.matches ? end() : begin());
    document.fonts.ready.then(() => {
      fonts = true;
      begin();
    });
    phone.addEventListener("change", turned);
    const undo = page();
    return () => {
      gone = true;
      phone.removeEventListener("change", turned);
      end();
      undo();
      ANCHORS.forEach(([name]) => attach(name, null));
    };
  }, []);
  return null;
}
