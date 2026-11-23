// The review nudge, at the sidebar's foot beside the job dock (lazy, in
// ../../shell/Sidebar.tsx): tells an owner or admin, on any page, that a
// current load has cards to decide, by the review cards index's count (the
// summary), with Start (the Review page's walk) and Later, which hides it
// for that load until its count grows by a quarter (./nudge-model.ts), in
// this browser. One ontology at a time: the one with the most, but not a
// load whose card in the job dock offers its Review already (a card that
// shows a job's progress on the load offers none, so the nudge does).
import "../ontology/ontology.css";
import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useLocation } from "react-router-dom";
import { useQueries, useQuery } from "@tanstack/react-query";
import { ListChecks } from "lucide-react";
import { sizeBucket, trackEvent } from "../../analytics";
import { formatNumber, plural, readStorage, writeStorage } from "../../kit";
import { paths } from "../../paths";
import { canManage, useWorkspace } from "../../ui";
import { ontologyQueries } from "../ontology/queries";
import { ontologyName } from "../ontology/scope";
import { useSampleOntology } from "../ontology/use-sample-ontology";
import { useWatchedJobs } from "../ontology/use-watched-jobs";
import { shownOn } from "../ontology/watched-jobs";
import { useReviewSummary } from "./cards-api";
import {
  dockOffersReview,
  nudgeLater,
  nudgeShown,
  parseNudgeState,
  type NudgeState,
} from "./nudge-model";

export function ReviewNudge() {
  const { id: ws, role, grantee } = useWorkspace(),
    { pathname } = useLocation();
  const key = `foundry.review.nudge.v1:${ws}`;
  const [state, setState] = useState<NudgeState>(() =>
    parseNudgeState(readStorage(key, "{}")),
  );
  const summary = useReviewSummary();
  const list = useQuery(ontologyQueries.list(ws));
  // The loads the job dock shows a card of: once one is current, with
  // cards to decide, its card says Review N (../ontology/job-dock.tsx).
  const jobs = useWatchedJobs(ws),
    sample = useSampleOntology()?.id;
  const docked = jobs.filter(
    (j) => j.kind === "load" && !shownOn(j, pathname, sample),
  );
  // Each docked load as its card read it: the dock reads them, the nudge
  // only follows the cache.
  const loads = useQueries({
    queries: docked.map((j) => ({
      ...ontologyQueries.load(ws, j.ontologyId, j.id),
      enabled: false,
    })),
    combine: (rs) =>
      new Map(
        rs.flatMap((r) =>
          r.data?.load ? [[r.data.load.id, r.data.load]] : [],
        ),
      ),
  });
  const shown = useMemo(
    () =>
      nudgeShown(
        state,
        [...summary.values()].filter(
          (s) => !dockOffersReview(loads.get(s.loadId), s),
        ),
      ),
    [state, summary, loads],
  );
  const reported = useRef("");
  const open = shown?.open ?? 0;
  // The walk shows the count already.
  const visible =
    canManage(role) &&
    !grantee &&
    !!shown &&
    pathname !== paths.review(shown.ontologyId);
  // Reported once a load, when it first shows.
  useEffect(() => {
    const at =
      visible && shown ? `${shown.ontologyId}\u001f${shown.loadId}` : "";
    if (!at || reported.current === at) return;
    reported.current = at;
    trackEvent("review_nudge_shown", { size_bucket: sizeBucket(open) });
  }, [visible, shown?.ontologyId, shown?.loadId]);
  if (!visible || !shown) return null;
  const o = shown.ontologyId;
  const ontology = list.data?.ontologies?.find((x) => x.id === o);
  const later = () => {
    const next = nudgeLater(state, shown);
    setState(next);
    writeStorage(key, JSON.stringify(next));
  };
  return (
    <section className="on-job-card" aria-label="Cards to review" role="status">
      <span className="on-job-card-icon" aria-hidden>
        <ListChecks size={16} />
      </span>
      <div className="on-job-card-text">
        <strong>
          {open
            ? shown.more
              ? `${formatNumber(open)}+ cards to decide`
              : `${plural(open, "card")} to decide`
            : "Counting…"}
        </strong>
        <span className="on-job-card-name">
          {ontology ? ontologyName(ontology) : "An ontology"}
        </span>
        <span className="on-card-actions">
          <Link
            className="on-job-card-link"
            to={paths.review(o)}
            onClick={() =>
              trackEvent("review_nudge_started", {
                size_bucket: sizeBucket(open),
              })
            }
          >
            Start
          </Link>
          <button
            type="button"
            className="hf-btn hf-btn--ghost hf-btn--sm"
            onClick={later}
          >
            Later
          </button>
        </span>
      </div>
    </section>
  );
}
