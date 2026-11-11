import { createContext, useContext, useEffect, useState } from "react";
import { readStorage, writeStorage } from "./util";

// The kit's React hooks, apart from ./util so its pure helpers import on the
// server.

export function useStoredState(key: string, fallback: string) {
  const [value, setValue] = useState(() => readStorage(key, fallback));
  useEffect(() => writeStorage(key, value), [key, value]);
  return [value, setValue] as const;
}
// Told about every page title; the shell uses it for "recently visited".
export const PageVisitContext = createContext<(title: string) => void>(
  () => {},
);
export function useTitle(title?: string) {
  const visit = useContext(PageVisitContext);
  useEffect(() => {
    if (!title) return;
    const previous = document.title;
    document.title = `${title} · Helix Foundry`;
    visit(title);
    return () => {
      document.title = previous;
    };
  }, [title]);
}
// Tracks a media query, e.g. "(max-width: 800px)".
export function useMedia(query: string) {
  const get = () =>
    typeof window !== "undefined" && !!window.matchMedia?.(query).matches;
  const [matches, setMatches] = useState(get);
  useEffect(() => {
    const list = window.matchMedia?.(query);
    if (!list) return;
    const change = () => setMatches(list.matches);
    change();
    list.addEventListener("change", change);
    return () => list.removeEventListener("change", change);
  }, [query]);
  return matches;
}
// Re-renders every `ms` so relative times stay fresh.
export function useNow(ms = 30000) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}
