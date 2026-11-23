// The review nudge, at the sidebar's foot beside the job dock (lazy, in
// ../../shell/Sidebar.tsx): tells an owner or admin, on any page, that a
// current load has cards to decide, by the review cards index's count (the
// summary), with Start (the Review page's walk) and Later, which hides it
// for that load until its count grows by a quarter (./nudge-model.ts), in
// this browser. One ontology at a time: the one with the most.
import "../ontology/ontology.css";
import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ListChecks } from "lucide-react";
import { sizeBucket, trackEvent } from "../../analytics";
import { formatNumber, readStorage, writeStorage } from "../../kit";
import { paths } from "../../paths";
import { canManage, useWorkspace } from "../../ui";
import { ontologyQueries } from "../ontology/queries";
import { ontologyName } from "../ontology/scope";
import { useReviewSummary } from "./cards-api";
import {
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
  const shown = useMemo(
    () => nudgeShown(state, [...summary.values()]),
    [state, summary],
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
            ? `${formatNumber(open)}${shown.more ? "+" : ""} to review`
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
