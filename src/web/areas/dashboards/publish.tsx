import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Copy, ExternalLink, Globe, RefreshCw, Unlink } from "lucide-react";
import type { FoundryDashboard } from "../../../shared/wfe";
import { LOCAL } from "../../../shared/deployment";
import { trackEvent } from "../../analytics";
import { sentence } from "../../api";
import {
  Button,
  ButtonAnchor,
  ConfirmDialog,
  Dialog,
  formatDate,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { dashboardRefusal } from "./model";
import { dashboardPaths as paths } from "./paths";
import {
  dashboardQueries,
  keepDashboard,
  publishDashboard,
  unpublishDashboard,
} from "./queries";

// Publishing a dashboard (owners and admins, hosted only): a snapshot of
// what its tiles read now, as the publisher, at a public link that anyone
// who has it opens without signing in (the share page, /share/<token>,
// which reads the foundry-events door). Publishing again keeps the link
// and refreshes the snapshot; unpublishing deletes it, and the link shows
// nothing from then on. With public links off (here, or on the WFE: no
// canPublish), a published dashboard's owner or admin can still unpublish
// it (canUnpublish), so turning links on again revives no link they ended.

/**
 * Whether this build offers public links: never a local one, nor one
 * without the door (NEXT_PUBLIC_HELIX_EVENTS_URL) the share page reads.
 * The WFE says so per dashboard too (canPublish).
 */
export const publicLinks = !LOCAL && !!process.env.NEXT_PUBLIC_HELIX_EVENTS_URL;

/** Whether the viewer publishes d here, or ends its link: what the Share dialog offers. */
export const sharing = (d: FoundryDashboard) => ({
  publish: !!d.canPublish && publicLinks,
  unpublish: !!d.canUnpublish,
});

/** Why publishing was refused, in words for the person who asked. */
function publishError(e: unknown): string {
  switch (dashboardRefusal(e)) {
    case "FOUNDRY_DASHBOARD_EMPTY":
      return "Add a tile before you publish: the dashboard has none.";
    case "FOUNDRY_DASHBOARD_CHANGED":
      return "The dashboard changed since you opened it. Check it, then publish again.";
    case "FOUNDRY_DASHBOARD_TOO_LARGE":
      return `The snapshot would be too large: ${(e as Error).message}.`;
    case "FOUNDRY_DASHBOARDS_PUBLIC_OFF":
      return "Public links aren’t available here.";
    case "FOUNDRY_DASHBOARD_NOT_LOADED":
    case "FOUNDRY_DASHBOARD_TILE_FAILED":
    default:
      return sentence((e as Error).message);
  }
}

export function ShareDialog({
  dashboard: d,
  onClose,
}: {
  dashboard: FoundryDashboard;
  onClose: () => void;
}) {
  const { id: ws, notify } = useWorkspace(),
    qc = useQueryClient();
  const [pending, setPending] = useState<"publish" | "unpublish" | null>(null),
    [error, setError] = useState(""),
    [confirming, setConfirming] = useState(false);
  const share = d.share,
    offered = sharing(d),
    url =
      share?.token && typeof window !== "undefined"
        ? window.location.origin + paths.shared(share.token)
        : "",
    stale = !!share && share.specRevision !== d.revision;
  const publish = async () => {
    setPending("publish");
    setError("");
    try {
      keepDashboard(
        qc,
        ws,
        await publishDashboard(ws, d.id, d.revision || "", !share),
      );
      notify(share ? "Snapshot refreshed." : "Published at a public link.");
    } catch (e) {
      setError(publishError(e));
      if (dashboardRefusal(e) === "FOUNDRY_DASHBOARD_CHANGED")
        void qc.invalidateQueries({
          queryKey: dashboardQueries.dashboard(ws, d.id).queryKey,
        });
    } finally {
      setPending(null);
    }
  };
  const unpublish = async () => {
    setPending("unpublish");
    setError("");
    try {
      keepDashboard(qc, ws, await unpublishDashboard(ws, d.id));
      setConfirming(false);
      notify("Unpublished: the link shows nothing now.");
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setPending(null);
    }
  };
  return (
    <Dialog
      title={share ? "Public link" : "Publish this dashboard?"}
      size="md"
      onClose={onClose}
    >
      <div className="db-share">
        {offered.publish && (
          <p className="db-share-warn" role="note">
            <Globe size={15} aria-hidden />
            <span>
              Anyone with the link sees this dashboard’s data, without signing
              in. They see a snapshot of what its tiles read when it was
              published, as you can read it, and nothing else of the workspace.
            </span>
          </p>
        )}
        {share && !offered.publish ? (
          <>
            <p className="db-share-note">
              Public links are turned off here, so its link shows nothing now.
              Unpublish it to keep it that way if they’re turned on again.
            </p>
            <div className="db-share-actions">
              <Button
                variant="danger"
                icon={Unlink}
                disabled={!!pending}
                onClick={() => setConfirming(true)}
              >
                Unpublish
              </Button>
            </div>
          </>
        ) : share ? (
          <>
            <label className="hf-field">
              <span>Link</span>
              <input
                readOnly
                value={url}
                onFocus={(e) => e.currentTarget.select()}
              />
            </label>
            <p className="db-share-note">
              Snapshot from {formatDate(share.publishedAt)}.
              {stale &&
                " You’ve edited the dashboard since: refresh the snapshot to show your edits."}
            </p>
            <div className="db-share-actions">
              <Button
                icon={Copy}
                disabled={!url}
                onClick={() =>
                  void navigator.clipboard.writeText(url).then(
                    () => {
                      trackEvent("dashboard_link_copied");
                      notify("Link copied.");
                    },
                    () => notify("Couldn’t copy the link.", true),
                  )
                }
              >
                Copy link
              </Button>
              <ButtonAnchor
                href={url}
                target="_blank"
                rel="noopener noreferrer"
                icon={ExternalLink}
              >
                Open
              </ButtonAnchor>
              <Button
                icon={RefreshCw}
                pending={pending === "publish"}
                disabled={!!pending}
                onClick={() => void publish()}
              >
                Refresh snapshot
              </Button>
              <Button
                variant="danger"
                icon={Unlink}
                disabled={!!pending}
                onClick={() => setConfirming(true)}
              >
                Unpublish
              </Button>
            </div>
          </>
        ) : (
          <>
            <p className="db-share-note">
              Publishing reads every tile now and keeps what they show. Edits
              and new data don’t reach the link until you refresh it, and
              unpublishing takes it down at once.
            </p>
            <div className="hf-form-actions">
              <Button onClick={onClose}>Cancel</Button>
              <Button
                variant="primary"
                icon={Globe}
                pending={pending === "publish"}
                onClick={() => void publish()}
              >
                Publish
              </Button>
            </div>
          </>
        )}
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
      </div>
      {confirming && (
        <ConfirmDialog
          title="Unpublish this dashboard?"
          description="Its link stops showing anything at once, for everyone who has it. Publishing it again makes a new link."
          confirmLabel="Unpublish"
          tone="danger"
          pending={pending === "unpublish"}
          onCancel={() => setConfirming(false)}
          onConfirm={unpublish}
        />
      )}
    </Dialog>
  );
}
