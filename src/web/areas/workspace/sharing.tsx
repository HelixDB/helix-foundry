import { useState } from "react";
import {
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  Button,
  ConfirmDialog,
  Panel,
  SkeletonText,
  formatDate,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { dashboardQueries, setPublicLinks } from "../dashboards/queries";
import { membersQuery } from "./members";

// Settings › Sharing: whether the workspace's dashboards may be published at
// public links. Owners turn them off for the whole workspace: every public
// link of it then shows nothing at once and none can be published, until an
// owner turns them on again (the links not unpublished meanwhile, and not
// expired, show again then). Admins and members see the setting. Sharing a
// dashboard's own page inside the workspace is never affected.

export function SharingTab() {
  const { id, name, notify } = useWorkspace(),
    qc = useQueryClient();
  const query = useQuery(dashboardQueries.settings(id));
  // Who changed it, by name: the first page of Settings › Members.
  const members = useInfiniteQuery(membersQuery(id));
  const [confirmOff, setConfirmOff] = useState(false),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const s = query.data;
  const title = "Public dashboard links",
    subtitle = `Owners and admins can publish a snapshot of a dashboard at a link anyone can open without signing in. A dashboard’s own link, which only people in ${name} open, isn’t affected.`;
  if (query.isPending)
    return (
      <Panel title={title}>
        <SkeletonText lines={3} />
      </Panel>
    );
  if (!s)
    return (
      <Panel title={title}>
        <p role="alert" className="hf-form-error">
          Couldn’t read the setting. {query.error?.message}
        </p>
        <Button size="sm" onClick={() => void query.refetch()}>
          Retry
        </Button>
      </Panel>
    );
  const by = members.data?.pages
    .flatMap((p) => p.members || [])
    .find((m) => m.user?.id === s.updatedByUserId)?.user;
  const changed =
    s.updatedAt &&
    `${s.publicLinks ? "Turned on" : "Turned off"}${
      by ? ` by ${by.displayName || by.email}` : ""
    } on ${formatDate(s.updatedAt, false)}.`;
  const save = async (on: boolean) => {
    setPending(true);
    setError("");
    try {
      qc.setQueryData(
        dashboardQueries.settings(id).queryKey,
        await setPublicLinks(id, on),
      );
      // Every dashboard's link and Share menu read the setting.
      void qc.invalidateQueries({ queryKey: [id, "dashboards"] });
      notify(on ? "Public links are on" : "Public links are off");
      setConfirmOff(false);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPending(false);
    }
  };
  return (
    <Panel title={title} subtitle={subtitle}>
      {!s.available && (
        <p className="wk-note">
          Public links aren’t available here, so no dashboard is published
          whatever this says.
        </p>
      )}
      {s.canChange ? (
        <label className="checkbox">
          <input
            type="checkbox"
            role="switch"
            checked={!!s.publicLinks}
            disabled={pending}
            onChange={(e) => {
              setError("");
              if (e.target.checked) void save(true);
              else setConfirmOff(true);
            }}
          />
          Allow public dashboard links
        </label>
      ) : (
        <p className="wk-note">
          {s.publicLinks
            ? "Public links are on. Only an owner can turn them off."
            : "Public links are off for this workspace. Only an owner can turn them on."}
        </p>
      )}
      {changed && <p className="wk-note">{changed}</p>}
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
      {confirmOff && (
        <ConfirmDialog
          title="Turn off public links?"
          description={`Every public link of ${name}’s dashboards stops showing anything at once, and no one can publish a new one. Links nobody unpublishes meanwhile show again if you turn them back on.`}
          confirmLabel="Turn off"
          tone="danger"
          pending={pending}
          onCancel={() => setConfirmOff(false)}
          onConfirm={() => save(false)}
        />
      )}
    </Panel>
  );
}
