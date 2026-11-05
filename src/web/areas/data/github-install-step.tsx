import { useEffect, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { LoaderCircle, PlugZap } from "lucide-react";
import { ApiError } from "../../api";
import {
  Button,
  ButtonAnchor,
  ButtonLink,
  EmptyState,
  Page,
  PageHeader,
  useTitle,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { Callout } from "../ontology/parts";
import { syncConnector } from "./data-api";
import {
  installAction,
  installReason,
  installStorage,
  rememberInstall,
  takeInstall,
  type PendingInstall,
} from "./github-install";

/**
 * The install step: why a signed-in GitHub connector reads nothing yet,
 * and Install on GitHub, which leaves for the app's install page in this
 * tab, remembering the connection and where to come back to. GitHub's
 * Setup URL returns to ConnectGitHubPage, which syncs it.
 */
export function GitHubInstallStep({
  problem,
  installUrl,
  install,
  onSkip,
  skipLabel = "Skip for now",
}: {
  problem: string;
  installUrl: string;
  install: Omit<PendingInstall, "at">;
  onSkip?: () => void;
  skipLabel?: string;
}) {
  return (
    <Callout
      icon={PlugZap}
      tone="muted"
      title="Install Helix Foundry on GitHub"
      text={`${installReason(problem)}. It reads only where it is installed, and only the repositories chosen there: install it on the account or organization that holds them, and GitHub brings you back to sync them. On an organization, an owner installs it; anyone else asks them to from the same page.`}
      action={
        <span className="on-card-actions">
          <ButtonAnchor
            size="sm"
            variant="primary"
            href={installUrl}
            onClick={() => rememberInstall(installStorage(), install)}
          >
            Install on GitHub
          </ButtonAnchor>
          {onSkip && (
            <Button size="sm" variant="ghost" onClick={onSkip}>
              {skipLabel}
            </Button>
          )}
        </span>
      }
    />
  );
}

// /connect/github: where GitHub returns after Helix Foundry's GitHub App
// was installed, its repositories changed, or an owner was asked to install
// it (the app's Setup URL, setup_action). The connection this browser left
// to install for syncs (unless only asked for), and the user goes back where
// they were. GitHub's own parameters are never trusted (installation_id can
// be anyone's), and nothing syncs for a link this browser didn't leave from:
// that page says what to do instead. A ref keeps <StrictMode>'s second
// effect from taking the install twice.
export function ConnectGitHubPage() {
  const navigate = useNavigate(),
    qc = useQueryClient(),
    { notify } = useWorkspace(),
    [search] = useSearchParams(),
    action = installAction(search),
    taken = useRef(false),
    [lost, setLost] = useState(false);
  useTitle("Connecting GitHub");
  useEffect(() => {
    if (taken.current) return;
    taken.current = true;
    const install = takeInstall(installStorage());
    if (!install) return setLost(true);
    void (async () => {
      if (action === "request")
        notify(
          "Asked the organization’s owners to install Helix Foundry. Once one approves, Sync now reads its repositories",
        );
      else {
        try {
          await syncConnector(
            install.workspaceId,
            install.connectorId,
            "install",
          );
          notify(
            "Helix Foundry is installed on GitHub. Syncing its repositories now",
          );
        } catch (e) {
          // A sync already queued or running reads the install all the same.
          if (!(e instanceof ApiError && e.status === 409))
            notify((e as Error).message, true);
        }
        await qc.invalidateQueries({
          queryKey: [install.workspaceId, "connectors"],
        });
      }
      navigate(install.to, { replace: true, state: install.state });
    })();
  }, []);
  return (
    <Page width="narrow">
      <PageHeader title="Connecting GitHub" />
      {lost ? (
        <EmptyState
          icon={PlugZap}
          title={
            action === "request"
              ? "Installation requested"
              : "Helix Foundry is installed on GitHub"
          }
          text={
            action === "request"
              ? "Once one of the organization’s owners approves it, sync your GitHub connection to read its repositories."
              : "Sync your GitHub connection to read the repositories you chose: Sync now on its page in Data, or ask your agent to sync it."
          }
          action={<ButtonLink to={paths.data()}>Open Data</ButtonLink>}
        />
      ) : (
        <Callout icon={LoaderCircle} live title="Back from GitHub…" />
      )}
    </Page>
  );
}
