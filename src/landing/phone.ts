import { useSyncExternalStore } from "react";

// The landing page's phone layout: a screen at most this wide gets the short
// page (the hero with its reminder, the Analyst, the connectors), with no
// field and none of the figures that need one. The nav already drops its
// links at this width. styles/base.css's .phone-only and .desktop-only use
// the same width: change both together.
export const PHONE = "(max-width: 760px)";

const subscribe = (change: () => void) => {
  const query = matchMedia(PHONE);
  query.addEventListener("change", change);
  return () => query.removeEventListener("change", change);
};

/**
 * Whether the screen is wider than a phone's: null on the server and until
 * the page has hydrated, so a figure mounts only once the browser has said,
 * and again when a phone turns to landscape.
 */
export function useWide(): boolean | null {
  return useSyncExternalStore<boolean | null>(
    subscribe,
    () => !matchMedia(PHONE).matches,
    () => null,
  );
}
