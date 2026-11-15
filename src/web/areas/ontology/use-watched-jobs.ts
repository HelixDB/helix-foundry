import { useSyncExternalStore } from "react";
import { readWatched, subscribeWatched, type WatchedJob } from "./watched-jobs";

// The jobs a workspace follows (./watched-jobs.ts), for components: apart
// from the store, which server code reaches through queries.ts, so it
// imports no React.

const NONE: WatchedJob[] = [];

/** The jobs a workspace follows, newest first. */
export const useWatchedJobs = (ws: string) =>
  useSyncExternalStore(
    subscribeWatched,
    () => readWatched(ws),
    () => NONE,
  );
