import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { BadgeCheck, Link2Off, Plug } from "lucide-react";
import type { GetPartnerAppAuthorizationResponse } from "../../../shared/wfe";
import { trackBeforeLeaving, trackEvent } from "../../analytics";
import { sentence } from "../../api";
import {
  Button,
  EmptyState,
  Page,
  PageHeader,
  Panel,
  Select,
  SkeletonText,
  Tag,
  useTitle,
} from "../../kit";
import { useWorkspace, type ShellWorkspace } from "../../ui";
import {
  shellWorkspaces,
  whoamiQuery,
  workspacesQuery,
} from "../../workspaces";
import { tabStorage } from "../data/sign-ins";
import { SourcePicker, useAccessData } from "./access";
import { sourcesQuery } from "./access-api";
import {
  grantsOf,
  setOwners,
  sourceTree,
  targetsOf,
  type Grant,
} from "./access-model";
import { approve, authorizationQuery, installsQuery } from "./partner-apps-api";
import {
  allowedScopes,
  approveBody,
  approvedBack,
  cancelUrl,
  existingInstall,
  partnerRequest,
  SCOPE_COPY,
  scopesEvent,
  scopesShown,
  type PartnerRequest,
  type PartnerScope,
} from "./partner-apps-model";
import { clearSlackReturn, readSlackReturn } from "./slack-return";

// /oauth/authorize (hosted, where the build offers connected apps): a
// partner app Helix registered asks to read one of the caller's workspaces.
// The MCP host checked the app, its redirect URI and PKCE, then sent the
// browser here with its request in the fragment, which the root layout's
// SLACK_RETURN_BOOT moved into this tab before the app ran, so it survives a
// sign-in and never reaches the server. An owner or admin chooses the
// workspace, what the app may do and which sources it reads; Approve sends
// the browser back to the app with a one-time code, Cancel with
// error=access_denied. The app's install then reads only those sources,
// changed or revoked in Settings › Connected apps.

type App = NonNullable<GetPartnerAppAuthorizationResponse["app"]>;

export function PartnerAuthorizePage() {
  useTitle("Connect an app");
  const [kept] = useState(() => readSlackReturn(tabStorage(), "app_authorize")),
    request = kept && partnerRequest(kept);
  const auth = useQuery({
    ...authorizationQuery(
      request || { clientId: "", redirectUri: "", scope: "" },
    ),
    enabled: !!request,
  });
  const me = useQuery(whoamiQuery),
    list = useQuery(workspacesQuery);
  const choices =
    me.data && list.data
      ? shellWorkspaces(me.data, list.data.workspaces || []).filter(
          (w) => w.role === "owner" || w.role === "admin",
        )
      : null;
  const app = auth.data?.app,
    deny = request && cancelUrl(auth.data?.denyUrl, request);
  const cancel = async () => {
    if (!deny) return;
    await trackBeforeLeaving("partner_app_denied");
    clearSlackReturn(tabStorage());
    window.location.assign(deny);
  };
  const body = !kept ? (
    <EmptyState
      icon={Plug}
      title="Nothing to approve here"
      text="Start connecting from the app again, in this tab."
    />
  ) : !request ? (
    <div role="alert">
      <EmptyState
        icon={Link2Off}
        title="This link can’t be used"
        text="The app didn’t name a way back that Helix could have registered. Start connecting from the app again."
      />
    </div>
  ) : auth.isPending || me.isPending || list.isPending ? (
    <Panel title="Connect an app to Helix Foundry">
      <SkeletonText lines={3} />
    </Panel>
  ) : auth.error || !app || !deny ? (
    <div role="alert">
      <EmptyState
        icon={Link2Off}
        title="This app can’t connect"
        text={
          auth.error
            ? sentence(auth.error.message)
            : "Helix’s answer didn’t go back to the app. Start connecting from the app again."
        }
      />
    </div>
  ) : !me.data || !list.data ? (
    // Not knowing who signed in, or their workspaces, is no refusal: the
    // page offers no Cancel, so the app isn't told access_denied for it.
    <div role="alert">
      <EmptyState
        icon={Link2Off}
        title="Couldn’t read your workspaces"
        text={
          me.error || list.error
            ? sentence((me.error || list.error)!.message)
            : "Try again in a moment."
        }
        action={
          <Button
            onClick={() => {
              if (!me.data) void me.refetch();
              if (!list.data) void list.refetch();
            }}
          >
            Retry
          </Button>
        }
      />
    </div>
  ) : !choices?.length ? (
    <div role="alert">
      <EmptyState
        icon={Link2Off}
        title="Only a workspace’s owners and admins connect apps"
        text={`Ask an owner or admin to connect ${app.name}, or sign in as one.`}
        action={<Button onClick={() => void cancel()}>Cancel</Button>}
      />
    </div>
  ) : (
    <Consent
      request={request}
      app={app}
      asked={scopesShown(auth.data?.scopes)}
      redirectOrigin={auth.data?.redirectOrigin || ""}
      choices={choices}
      email={me.data?.user.email || ""}
      onCancel={cancel}
    />
  );
  return (
    <Page width="narrow">
      <PageHeader title="Connected apps" />
      {body}
    </Page>
  );
}

/** The request as Helix read it, for an owner or admin to approve. */
function Consent({
  request,
  app,
  asked,
  redirectOrigin,
  choices,
  email,
  onCancel,
}: {
  request: PartnerRequest;
  app: App;
  asked: PartnerScope[];
  redirectOrigin: string;
  choices: ShellWorkspace[];
  email: string;
  onCancel: () => Promise<void>;
}) {
  const { id: current } = useWorkspace();
  const [picked, setPicked] = useState(""),
    workspace =
      choices.find((w) => w.id === picked) ||
      choices.find((w) => w.id === current) ||
      choices[0]!;
  const [scopes, setScopes] = useState<PartnerScope[]>(asked);
  const shown = useRef(false);
  useEffect(() => {
    if (shown.current) return;
    shown.current = true;
    trackEvent("partner_app_consent_shown");
  }, []);
  const homepage = app.homepageUrl?.startsWith("https://") && app.homepageUrl;
  return (
    <Panel title={`Connect ${app.name} to Helix Foundry`}>
      <div className="wk-pad wk-consent" data-replay-block>
        <p className="wk-consent-publisher">
          <span>
            By <strong>{app.publisher}</strong>
          </span>
          {app.verified ? (
            <Tag tone="success" icon={BadgeCheck}>
              Verified by Helix
            </Tag>
          ) : (
            <Tag tone="warning">Unverified by Helix</Tag>
          )}
          {homepage && (
            <a href={homepage} target="_blank" rel="noopener noreferrer">
              {new URL(homepage).host}
            </a>
          )}
        </p>
        {!app.verified && (
          <p className="wk-note">
            Helix hasn’t checked who runs this app. Connect it only if you trust{" "}
            {app.publisher}.
          </p>
        )}
        <p>
          Signed in as <strong>{email}</strong>, connecting{" "}
          <strong>{workspace.name}</strong>.
        </p>
        {choices.length > 1 && (
          <label className="hf-field">
            <span>Workspace</span>
            <Select
              label="Workspace"
              value={workspace.id}
              onChange={setPicked}
              options={choices.map((w) => ({ value: w.id, label: w.name }))}
            />
          </label>
        )}
        <fieldset className="wk-access-sources">
          <legend>{app.name} may</legend>
          <ul aria-label="What the app may do">
            {asked.map((s) => {
              const on = scopes.includes(s);
              return (
                <li key={s}>
                  <label className="checkbox wk-access-source">
                    <input
                      type="checkbox"
                      checked={on}
                      // At least one stays on.
                      disabled={on && scopes.length === 1}
                      onChange={() =>
                        setScopes((now) =>
                          scopesShown(
                            on ? now.filter((x) => x !== s) : [...now, s],
                          ),
                        )
                      }
                    />
                    <span>
                      {SCOPE_COPY[s].label}
                      <small> · {SCOPE_COPY[s].text}</small>
                    </span>
                  </label>
                </li>
              );
            })}
          </ul>
        </fieldset>
        <ConsentSources
          key={workspace.id}
          workspace={workspace}
          request={request}
          app={app}
          scopes={scopes}
          redirectOrigin={redirectOrigin}
          onCancel={onCancel}
        />
      </div>
    </Panel>
  );
}

/**
 * The sources of the workspace chosen, as the grants editor lists them,
 * prefilled with what the app's install reads there already; then Approve
 * and Cancel.
 */
function ConsentSources({
  workspace,
  request,
  app,
  scopes,
  redirectOrigin,
  onCancel,
}: {
  workspace: ShellWorkspace;
  request: PartnerRequest;
  app: App;
  scopes: PartnerScope[];
  redirectOrigin: string;
  onCancel: () => Promise<void>;
}) {
  const ws = workspace.id;
  const data = useAccessData(ws),
    installs = useQuery({ ...installsQuery(ws), retry: false }),
    follows = new Set(
      (useQuery(sourcesQuery(ws)).data || []).map((a) => a.connectorId),
    );
  const [chosen, setChosen] = useState<Grant[] | null>(null),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  if (data.loading || installs.isPending) return <SkeletonText lines={4} />;
  if (data.failed)
    return (
      <>
        <p role="alert" className="hf-form-error">
          Couldn’t load the workspace’s sources. {data.failed.message}
        </p>
        <Button size="sm" onClick={data.retry}>
          Retry
        </Button>
      </>
    );
  // Without its installs the page can't say the app is connected already,
  // or start from what it reads, so nothing is approved until they read.
  if (installs.error)
    return (
      <>
        <p role="alert" className="hf-form-error">
          Couldn’t read the workspace’s connected apps.{" "}
          {sentence(installs.error.message)}
        </p>
        <div className="hf-form-actions">
          <Button size="sm" onClick={() => void installs.refetch()}>
            Retry
          </Button>
          <Button size="sm" onClick={() => void onCancel()}>
            Cancel
          </Button>
        </div>
      </>
    );
  const existing = existingInstall(installs.data || [], app.clientId),
    prefill = existing
      ? grantsOf(data.access.data?.grants || [], {
          kind: "KIND_SERVICE_CREDENTIAL",
          id: existing.credentialId,
        })
      : [],
    sources = chosen ?? prefill;
  const sets = data.sets.data || [],
    tree = sourceTree(data.connectors.data?.connectors || [], sets),
    body = approveBody(request, scopes, targetsOf(sources, setOwners(sets)));
  const allow = async () => {
    const allowed = allowedScopes(scopes);
    if (!body || !allowed) return;
    setPending(true);
    setError("");
    try {
      const back = await approve(ws, body);
      if (!approvedBack(back, request))
        throw new Error(
          `Helix’s answer didn’t go back to ${app.name}. Start connecting from the app again.`,
        );
      await trackBeforeLeaving("partner_app_approved", {
        scopes: scopesEvent(allowed),
      });
      clearSlackReturn(tabStorage());
      window.location.assign(back);
    } catch (e) {
      setError(sentence((e as Error).message));
      setPending(false);
    }
  };
  return (
    <>
      {existing && (
        <p className="wk-note">
          {app.name} is already connected to {workspace.name}. Approving
          replaces what it can read.
        </p>
      )}
      <SourcePicker
        tree={tree}
        follows={follows}
        chosen={sources}
        onChange={setChosen}
        audience="app"
        disabled={pending}
      />
      <p className="wk-note">
        {app.name} reads only what these sources hold of each ontology’s current
        load. You’ll be sent back to{" "}
        <span className="hf-mono">{redirectOrigin}</span>. Change what it reads,
        or revoke it, any time in Settings › Connected apps.
      </p>
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
      {!sources.length && (
        <p className="wk-note">Choose at least one source.</p>
      )}
      <div className="hf-form-actions">
        <Button
          variant="primary"
          pending={pending}
          disabled={!body}
          onClick={() => void allow()}
        >
          Approve
        </Button>
        <Button disabled={pending} onClick={() => void onCancel()}>
          Cancel
        </Button>
      </div>
    </>
  );
}
