import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import {
  Copy,
  ExternalLink,
  Globe,
  Link2,
  Lock,
  RefreshCw,
  Share2,
  Unlink,
} from "lucide-react";
import type {
  DashboardLinkExpiry,
  FoundryDashboard,
} from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import { sentence } from "../../api";
import {
  Button,
  ButtonAnchor,
  Dialog,
  DropdownMenu,
  MenuHeader,
  MenuItem,
  MenuSeparator,
  Select,
  formatDate,
} from "../../kit";
import { useWorkspace } from "../../ui";
import {
  LINK_EXPIRIES,
  dashboardRefusal,
  linkState,
  publishBlockText,
  workspaceLink,
} from "./model";
import { dashboardPaths as paths } from "./paths";
import { publicLinks } from "./public-links";
import {
  dashboardQueries,
  keepDashboard,
  publishDashboard,
  unpublishDashboard,
} from "./queries";

// Sharing a dashboard, in two ways:
//  - In the workspace (Copy link, the default): its page's address with
//    ?workspace=, live, which each member opens signed in and reads as
//    themselves (a grantee through its grants). No snapshot is made.
//  - At a public link (owners and admins, hosted only, while the workspace
//    has public links on): a snapshot of what its tiles read now, as the
//    publisher, that anyone with the link opens without signing in (the
//    share page, /share/<token>, which reads the foundry-events door),
//    unchanged until it is published again, optionally ending after 7, 30
//    or 90 days. Publishing again keeps the link (a new one once it
//    expired) and refreshes the snapshot; Unpublish deletes it at once.
//    With public links off (the stamp's, or the workspace's: no
//    canPublish), its owner or admin can still unpublish it (canUnpublish),
//    so turning links on again revives no link they ended.

/** Whether the viewer publishes d here, or ends its link: what the public link's dialog offers. */
export const sharing = (d: FoundryDashboard) => ({
  publish: !!d.canPublish && publicLinks,
  unpublish: !!d.canUnpublish,
});

/** A day as the page writes an expiry: "16 Dec 2026". */
const day = (iso: string) => formatDate(iso, false);

/** Copies a dashboard's in-app link, saying who opens it. */
export function useCopyLink() {
  const { id: ws, name, notify } = useWorkspace();
  return (d: FoundryDashboard, from: "page" | "card") =>
    void navigator.clipboard
      .writeText(workspaceLink(window.location.origin, ws, d.id))
      .then(
        () => {
          trackEvent("dashboard_workspace_link_copied", { from });
          notify(
            `Link copied. Anyone in ${name} who can read its data can open it.`,
          );
        },
        () => notify("Couldn’t copy the link.", true),
      );
}

/** Why publishing was refused, in words for the person who asked. */
function publishError(e: unknown): string {
  switch (dashboardRefusal(e)) {
    case "FOUNDRY_DASHBOARD_EMPTY":
      return "Add a tile before you publish: the dashboard has none.";
    case "FOUNDRY_DASHBOARD_CHANGED":
      return "The dashboard changed since you opened it. Check it, then publish again.";
    case "FOUNDRY_DASHBOARD_TOO_LARGE":
      return `The snapshot would be too large: ${(e as Error).message}.`;
    case "FOUNDRY_DASHBOARD_PUBLIC_UNAVAILABLE":
      return "Public links aren’t available here.";
    case "FOUNDRY_DASHBOARD_PUBLIC_OFF":
      return "An owner turned public links off for this workspace.";
    case "FOUNDRY_DASHBOARD_PUBLIC_ADMIN_ONLY":
      return "Only owners and admins make public links.";
    case "FOUNDRY_DASHBOARD_NOT_LOADED":
    case "FOUNDRY_DASHBOARD_TILE_FAILED":
    default:
      return sentence((e as Error).message);
  }
}

/**
 * The page's Share menu: Copy link, for the workspace, first; then the
 * public link, which opens its dialog for owners and admins, and says why
 * not to anyone else. Where the stamp or build has no public links, the
 * menu offers the workspace link alone.
 */
export function ShareMenu({
  dashboard: d,
  onPublic,
}: {
  dashboard: FoundryDashboard;
  onPublic: () => void;
}) {
  const { name } = useWorkspace(),
    copy = useCopyLink();
  const offered = sharing(d),
    blocked = publishBlockText(d.publishBlock),
    state = linkState(d.share, day);
  const manage = offered.publish || offered.unpublish;
  return (
    <DropdownMenu
      label="Share"
      align="end"
      triggerClassName="hf-btn hf-btn--primary hf-btn--md"
      trigger={
        <>
          <Share2 size={16} aria-hidden />
          Share
        </>
      }
    >
      <MenuHeader
        title={`Share in ${name}`}
        subtitle={`Anyone in ${name} who can read its data`}
      />
      <MenuItem icon={Link2} onSelect={() => copy(d, "page")}>
        Copy link
      </MenuItem>
      {publicLinks && (manage || blocked) && (
        <>
          <MenuSeparator />
          <MenuHeader
            title="Public link"
            subtitle={
              state
                ? state.detail
                  ? `On, ${state.detail}`
                  : "On, with no end date"
                : "For people outside the workspace, no sign-in"
            }
          />
          {manage ? (
            <MenuItem icon={Globe} onSelect={onPublic}>
              {d.share ? "Manage public link…" : "Make a public link…"}
            </MenuItem>
          ) : (
            <MenuItem icon={Lock} disabled>
              {blocked}
            </MenuItem>
          )}
        </>
      )}
    </DropdownMenu>
  );
}

/** The public link's dialog: make it, or copy, refresh, re-time or end it. */
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
    [error, setError] = useState("");
  const share = d.share,
    offered = sharing(d),
    state = linkState(share, day),
    expired = share?.status === "FOUNDRY_DASHBOARD_SHARE_STATUS_EXPIRED",
    // A live link keeps its own end unless another is chosen; a new one
    // (none yet, or once it expired) starts with no end.
    keeps = !!share && !expired,
    [expiry, setExpiry] = useState<DashboardLinkExpiry>(
      keeps
        ? "FOUNDRY_DASHBOARD_LINK_EXPIRY_UNSPECIFIED"
        : "FOUNDRY_DASHBOARD_LINK_EXPIRY_NEVER",
    ),
    url =
      share?.token && state?.live && typeof window !== "undefined"
        ? window.location.origin + paths.shared(share.token)
        : "",
    stale = !!share && share.specRevision !== d.revision;
  const expiries = keeps
    ? [
        {
          value: "FOUNDRY_DASHBOARD_LINK_EXPIRY_UNSPECIFIED" as const,
          label: share?.expiresAt
            ? `Keep: ${day(share.expiresAt)}`
            : "Keep: never",
        },
        ...LINK_EXPIRIES,
      ]
    : LINK_EXPIRIES;
  const publish = async () => {
    setPending("publish");
    setError("");
    try {
      keepDashboard(
        qc,
        ws,
        await publishDashboard(
          ws,
          d.id,
          d.revision || "",
          !keeps,
          expiry === "FOUNDRY_DASHBOARD_LINK_EXPIRY_UNSPECIFIED"
            ? undefined
            : expiry,
        ),
      );
      notify(keeps ? "Snapshot refreshed." : "Published at a public link.");
      setExpiry("FOUNDRY_DASHBOARD_LINK_EXPIRY_UNSPECIFIED");
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
  // One click: ending a link only takes data out of view.
  const unpublish = async () => {
    setPending("unpublish");
    setError("");
    try {
      keepDashboard(qc, ws, await unpublishDashboard(ws, d.id));
      notify(
        "Unpublished: the link shows nothing now. Publishing again makes a new link.",
      );
      onClose();
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setPending(null);
    }
  };
  const unpublishButton = (
    <Button
      variant="danger"
      icon={Unlink}
      pending={pending === "unpublish"}
      disabled={!!pending}
      onClick={() => void unpublish()}
    >
      Unpublish
    </Button>
  );
  const expiryField = (
    <label className="hf-field">
      <span>Link ends</span>
      <Select
        label="Link ends"
        value={expiry}
        onChange={setExpiry}
        options={expiries}
        disabled={!!pending}
      />
    </label>
  );
  return (
    <Dialog
      title={share && !expired ? "Public link" : "Make a public link?"}
      size="md"
      onClose={onClose}
    >
      <div className="db-share">
        {offered.publish && (
          <div className="db-share-warn" role="note">
            <Globe size={15} aria-hidden />
            <ul>
              <li>Anyone with the link can view it, without signing in.</li>
              <li>
                It’s a snapshot of what you can see now, not of what each
                viewer may read.
              </li>
              <li>It doesn’t change until you publish it again.</li>
            </ul>
          </div>
        )}
        {share && !offered.publish ? (
          <>
            <p className="db-share-note">
              {share.status === "FOUNDRY_DASHBOARD_SHARE_STATUS_WORKSPACE_OFF"
                ? "An owner turned public links off for this workspace, so its link shows nothing now."
                : "Public links are turned off here, so its link shows nothing now."}{" "}
              Unpublish it to keep it that way if they’re turned on again.
            </p>
            <div className="db-share-actions">{unpublishButton}</div>
          </>
        ) : share && !expired ? (
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
              Snapshot from {formatDate(share.publishedAt)}.{" "}
              {share.expiresAt
                ? `The link ends on ${day(share.expiresAt)}.`
                : "The link has no end date."}
              {stale &&
                " You’ve edited the dashboard since: refresh the snapshot to show your edits."}
            </p>
            {expiryField}
            <div className="db-share-actions">
              <Button
                icon={Copy}
                disabled={!url}
                onClick={() =>
                  void navigator.clipboard.writeText(url).then(
                    () => {
                      trackEvent("dashboard_link_copied");
                      notify("Public link copied.");
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
                {expiry === "FOUNDRY_DASHBOARD_LINK_EXPIRY_UNSPECIFIED"
                  ? "Refresh snapshot"
                  : "Refresh and set end"}
              </Button>
              {unpublishButton}
            </div>
          </>
        ) : (
          <>
            {expired && share?.expiresAt && (
              <p className="db-share-note">
                Its link ended on {day(share.expiresAt)} and shows nothing
                now. Publishing again makes a new link.
              </p>
            )}
            {expiryField}
            <div className="hf-form-actions">
              {expired ? unpublishButton : <Button onClick={onClose}>Cancel</Button>}
              <Button
                variant="primary"
                icon={Globe}
                pending={pending === "publish"}
                disabled={!!pending}
                onClick={() => void publish()}
              >
                {expired ? "Publish a new link" : "Publish"}
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
    </Dialog>
  );
}
