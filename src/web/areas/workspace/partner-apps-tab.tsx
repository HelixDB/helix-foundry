import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { BadgeCheck, Plug } from "lucide-react";
import type { PartnerAppInstall } from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import { sentence } from "../../api";
import {
  Button,
  ConfirmDialog,
  Dialog,
  EmptyState,
  Panel,
  SkeletonText,
  Tag,
  formatDate,
} from "../../kit";
import { canManage, useWorkspace } from "../../ui";
import {
  readsLine,
  SourcePicker,
  useAccessData,
  type AccessData,
} from "./access";
import { accessQuery, keysQuery, setGrants, sourcesQuery } from "./access-api";
import {
  grantsOf,
  setOwners,
  sourceTree,
  targetsOf,
  type Grant,
  type Principal,
} from "./access-model";
import { installsQuery, revokeInstall } from "./partner-apps-api";
import { installLine, scopesText } from "./partner-apps-model";

// Settings › Connected apps (hosted, owners and admins, where the build
// offers them): the partner apps an owner or admin approved for this
// workspace on /oauth/authorize. Each install reads through a key of its
// own, nobody holds the token of, and only the sources it was given: Edit
// sources changes them (the key's source grants, never its Usage), Revoke
// ends the install, its tokens and its sources at once.

const principalOf = (i: PartnerAppInstall): Principal => ({
  kind: "KIND_SERVICE_CREDENTIAL",
  id: i.credentialId,
});

export function PartnerAppsTab() {
  const { role } = useWorkspace();
  return canManage(role) ? (
    <Installs />
  ) : (
    <p className="wk-note">Only owners and admins manage connected apps.</p>
  );
}

function Installs() {
  const { id, name, notify } = useWorkspace(),
    qc = useQueryClient();
  const installs = useQuery(installsQuery(id)),
    data = useAccessData(id);
  const [editing, setEditing] = useState<PartnerAppInstall>(),
    [revoking, setRevoking] = useState<PartnerAppInstall>(),
    [pending, setPending] = useState(false);
  const people = new Map(
    data.people.flatMap((m) =>
      m.user ? [[m.user.id, m.user.displayName || m.user.email || ""]] : [],
    ),
  );
  const approver = (userId: string) => people.get(userId) || "a past member";
  const revoke = async (i: PartnerAppInstall) => {
    setPending(true);
    try {
      await revokeInstall(id, i.id);
      trackEvent("partner_app_revoked");
      await Promise.all(
        [installsQuery(id), accessQuery(id), keysQuery(id)].map((q) =>
          qc.invalidateQueries({ queryKey: q.queryKey }),
        ),
      );
      notify(`Revoked ${i.app?.name || "the app"}.`);
      setRevoking(undefined);
    } catch (e) {
      notify(sentence((e as Error).message), true);
    } finally {
      setPending(false);
    }
  };
  const list = installs.data || [];
  return (
    <>
      <Panel
        title="Connected apps"
        subtitle="Apps Helix registered that read this workspace through a key of their own. Each reads and asks only the sources it’s given."
        padded={false}
      >
        {installs.isPending || data.loading ? (
          <div className="wk-pad">
            <SkeletonText lines={2} />
          </div>
        ) : installs.error || data.failed ? (
          <div className="wk-pad">
            <p role="alert" className="hf-form-error">
              {sentence((installs.error || data.failed)!.message)}
            </p>
            <Button
              size="sm"
              onClick={() => {
                void installs.refetch();
                data.retry();
              }}
            >
              Retry
            </Button>
          </div>
        ) : list.length ? (
          <ul
            className="wk-members"
            aria-label="Connected apps"
            data-replay-block
          >
            {list.map((i) => {
              const title = i.app?.name || i.id;
              return (
                <li key={i.id} className="wk-member" aria-label={title}>
                  <span className="hf-avatar" aria-hidden>
                    <Plug size={15} />
                  </span>
                  <div className="wk-member-text">
                    <strong>
                      {title}
                      {i.app?.verified && (
                        <Tag tone="success" icon={BadgeCheck}>
                          Verified
                        </Tag>
                      )}
                      {i.appDisabled && (
                        <Tag tone="warning">Suspended by Helix</Tag>
                      )}
                    </strong>
                    <small>
                      {[
                        i.app?.publisher,
                        scopesText(i.scopes),
                        readsLine(data, principalOf(i)),
                      ]
                        .filter(Boolean)
                        .join(" · ")}
                    </small>
                    <small className="wk-access-note">
                      {installLine(i, approver, (at) => formatDate(at, false))}
                    </small>
                  </div>
                  <div className="wk-access-actions">
                    <Button
                      size="sm"
                      aria-label={`Edit sources for ${title}`}
                      onClick={() => setEditing(i)}
                    >
                      Edit sources
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={`Revoke ${title}`}
                      onClick={() => setRevoking(i)}
                    >
                      Revoke
                    </Button>
                  </div>
                </li>
              );
            })}
          </ul>
        ) : (
          <EmptyState
            size="sm"
            icon={Plug}
            title="No apps are connected"
            text="Apps Helix approved ask you to connect from their own site."
          />
        )}
      </Panel>
      {editing && (
        <AppSourcesDialog
          data={data}
          install={editing}
          onClose={() => setEditing(undefined)}
        />
      )}
      {revoking && (
        <ConfirmDialog
          title={`Revoke ${revoking.app?.name || "this app"}?`}
          description={`${revoking.app?.name || "The app"} stops reading ${name} at once, and its sources go with it. Connecting it again takes a new approval.`}
          confirmLabel="Revoke"
          tone="danger"
          pending={pending}
          onConfirm={() => revoke(revoking)}
          onCancel={() => setRevoking(undefined)}
        />
      )}
    </>
  );
}

/**
 * An install's sources alone, saved as one PUT of its key's grants. It never
 * sends Usage (`workflows`): an app's key has it off for good.
 */
function AppSourcesDialog({
  data,
  install,
  onClose,
}: {
  data: AccessData;
  install: PartnerAppInstall;
  onClose: () => void;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const principal = principalOf(install),
    title = install.app?.name || install.id,
    saved = grantsOf(data.access.data?.grants || [], principal);
  const [chosen, setChosen] = useState<Grant[]>(saved),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const sets = data.sets.data || [],
    owners = setOwners(sets),
    tree = sourceTree(data.connectors.data?.connectors || [], sets),
    follows = new Set(
      (useQuery(sourcesQuery(id)).data || []).map((a) => a.connectorId),
    );
  const key = (gs: readonly Grant[]) =>
    targetsOf(gs, owners)
      .map((t) => JSON.stringify(t))
      .sort()
      .join();
  const save = async () => {
    setPending(true);
    setError("");
    try {
      await setGrants(id, principal, targetsOf(chosen, owners));
      trackEvent("partner_app_sources_saved");
      await qc.invalidateQueries({ queryKey: accessQuery(id).queryKey });
      notify(`Saved what ${title} can read.`);
      onClose();
    } catch (e) {
      setError(sentence((e as Error).message));
      setPending(false);
    }
  };
  return (
    <Dialog
      title={`Sources for ${title}`}
      description="It reads and asks only what these sources hold of each ontology’s current load."
      size="lg"
      onClose={onClose}
      footer={
        <>
          <Button onClick={onClose}>Close</Button>
          <Button
            variant="primary"
            pending={pending}
            disabled={key(chosen) === key(saved) || chosen.length === 0}
            onClick={() => void save()}
          >
            Save
          </Button>
        </>
      }
    >
      <div data-replay-block>
        <SourcePicker
          tree={tree}
          follows={follows}
          chosen={chosen}
          onChange={setChosen}
          audience="app"
          disabled={pending}
        />
        {chosen.length === 0 && (
          <p className="wk-note">
            Choose at least one source, or revoke the app.
          </p>
        )}
      </div>
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
    </Dialog>
  );
}
