import type { ReactNode } from "react";
import { Copy } from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import { Button } from "../../kit";
import { useWorkspace } from "../../ui";
import type { ConnectionForm, CredentialKind } from "./model";

// How to make a read-only credential for each kind, shown above its form
// (S3's policies to paste sit with its role and key fields).
// Adapted from helix-foundry's connection-guides.tsx for Helix Cloud: syncs
// are snapshots run from Helix's network, so there is no replication setup.

const External = ({
  href,
  children,
}: {
  href: string;
  children: ReactNode;
}) => (
  <a href={href} target="_blank" rel="noopener noreferrer">
    {children}
  </a>
);

// The addresses connector tests and syncs reach a database from: the stamp's
// NAT egress IPs, comma-separated, set per Vercel environment.
const EGRESS_IPS = (process.env.NEXT_PUBLIC_HELIX_EGRESS_IPS || "")
  .split(",")
  .map((ip) => ip.trim())
  .filter(Boolean);

function DatabaseReach() {
  const { notify } = useWorkspace();
  if (LOCAL)
    return (
      <p>
        Foundry connects from inside Docker, so a database on this computer is{" "}
        <code>host.docker.internal</code>, not <code>localhost</code>, and must
        accept connections from Docker’s network. If it has no TLS, choose No
        TLS under Connection options.
      </p>
    );
  const intro =
    "Helix connects over TLS from its own network, so the database must accept connections from the internet.";
  if (!EGRESS_IPS.length)
    return (
      <p>
        {intro} If it only accepts known addresses, allow Helix’s outbound
        address.
      </p>
    );
  return (
    <p>
      {intro} If it only accepts known addresses, allow{" "}
      {EGRESS_IPS.length === 1 ? "this address" : "these addresses"}:{" "}
      {EGRESS_IPS.map((ip, i) => (
        <span key={ip}>
          {i > 0 && ", "}
          <code>{ip}</code>
        </span>
      ))}
      .{" "}
      <Button
        size="sm"
        variant="ghost"
        icon={Copy}
        onClick={() =>
          navigator.clipboard
            .writeText(EGRESS_IPS.join(", "))
            .then(() => notify("Copied Helix’s addresses."))
            .catch(() => notify("Couldn’t copy the addresses.", true))
        }
      >
        Copy
      </Button>
    </p>
  );
}

const guides: Record<CredentialKind, (form: ConnectionForm) => ReactNode> = {
  stripe: () => (
    <ol>
      <li>
        Open{" "}
        <External href="https://dashboard.stripe.com/apikeys">
          API keys in Stripe
        </External>{" "}
        and click <strong>Create restricted key</strong>. Use a sandbox or test
        mode first if you want to try Foundry without live data.
      </li>
      <li>
        Name it “Helix Foundry”, then set <strong>Read</strong> for the objects
        you want (for example Customers, Products, Subscriptions, Invoices,
        Charges, Payment Intents, Disputes, Payouts, Balance). Leave everything
        else as <strong>None</strong>.
      </li>
      <li>
        Click <strong>Create key</strong>, copy the <code>rk_…</code> value, and
        paste it below. Stripe shows it only once.
      </li>
    </ol>
  ),
  workos: () => (
    <ol>
      <li>
        Open{" "}
        <External href="https://dashboard.workos.com/api-keys">
          API Keys in WorkOS
        </External>{" "}
        and switch to the environment you want to import (Staging or
        Production).
      </li>
      <li>
        Copy the secret key (<code>sk_…</code>). Production keys are shown only
        when created, so create a new one if you no longer have it.
      </li>
      <li>
        Paste it below. WorkOS keys can make changes, but Foundry only reads.
      </li>
    </ol>
  ),
  posthog: (form) => {
    const p = form.kind === "posthog" ? form : undefined,
      host =
        p?.region === "custom"
          ? /^https:\/\/[^/]+/.exec(p.host)?.[0] || null
          : `https://${p?.region === "eu" ? "eu" : "us"}.posthog.com`;
    return (
      <ol>
        <li>
          In PostHog, open{" "}
          {host ? (
            <External href={`${host}/settings/user-api-keys`}>
              Settings → Personal API keys
            </External>
          ) : (
            "Settings → Personal API keys"
          )}{" "}
          and click <strong>Create personal API key</strong>.
        </li>
        <li>
          Give it the <strong>Query</strong> and <strong>Project</strong> read
          scopes only, limited to the project you want to import.
        </li>
        <li>
          Copy the <code>phx_…</code> key and paste it below. If the key can see
          several projects, copy the project ID from{" "}
          {host ? (
            <External href={`${host}/settings/project`}>
              project settings
            </External>
          ) : (
            "project settings"
          )}
          .
        </li>
      </ol>
    );
  },
  github: () => (
    <ol>
      <li>
        Open{" "}
        <External href="https://github.com/settings/personal-access-tokens/new">
          Fine-grained tokens in GitHub
        </External>{" "}
        and name it “Helix Foundry”. For an organization’s repositories, choose
        the organization as the resource owner; it may need to approve the
        token.
      </li>
      <li>
        Choose the repositories to import, then set <strong>Issues</strong> and{" "}
        <strong>Pull requests</strong> to <strong>Read-only</strong>. Leave
        everything else as it is.
      </li>
      <li>
        Click <strong>Generate token</strong>, copy the{" "}
        <code>github_pat_…</code> value, and paste it below. A classic token (
        <code>ghp_…</code>) with the <code>repo</code> scope works too.
      </li>
      <li>
        Foundry syncs issues, pull requests, comments, reviews, labels,
        milestones and people, not code.
      </li>
    </ol>
  ),
  linear: () => (
    <ol>
      <li>
        Open{" "}
        <External href="https://linear.app/settings/account/security">
          Security &amp; access in Linear
        </External>{" "}
        and create a personal API key named “Helix Foundry” with{" "}
        <strong>Read</strong> access.
      </li>
      <li>
        Copy the <code>lin_api_…</code> key and paste it below. It reads every
        team you can see in Linear.
      </li>
    </ol>
  ),
  calcom: () => (
    <ol>
      <li>
        Open{" "}
        <External href="https://app.cal.com/settings/developer/api-keys">
          Settings → Developer → API keys in Cal.com
        </External>{" "}
        and create a key named “Helix Foundry”. Turn on{" "}
        <strong>Never expires</strong>, or choose a long expiry: syncs stop when
        the key expires.
      </li>
      <li>
        Copy the <code>cal_live_…</code> key and paste it below. Cal.com shows
        it only once.
      </li>
      <li>
        Cal.com keys can make changes, but Foundry only reads: bookings and
        their attendees, event types, schedules, and teams on a Teams plan. Only
        a team admin’s key reads the team’s other members.
      </li>
    </ol>
  ),
  granola: () => (
    <ol>
      <li>
        In the Granola desktop app, open{" "}
        <strong>Settings → Connectors → API keys</strong> and click{" "}
        <strong>Create new key</strong>. API keys come with{" "}
        <External href="https://docs.granola.ai/help-center/sharing/integrations/granola-api">
          Granola Business and Enterprise
        </External>
        .
      </li>
      <li>
        Choose the <strong>Personal notes</strong> scope, and{" "}
        <strong>Public notes</strong> too for notes shared with your team, then
        click <strong>Generate API Key</strong>.
      </li>
      <li>
        Copy the <code>grn_…</code> key and paste it below. Foundry syncs your
        notes with their summaries and attendees, and their folders and spaces,
        and keeps each summary as a document in Notes. Transcripts are kept only
        when you turn them on below.
      </li>
    </ol>
  ),
  slack: () => (
    <ol>
      <li>
        Open{" "}
        <External href="https://api.slack.com/apps">
          your apps in Slack
        </External>
        , click <strong>Create New App</strong> and choose{" "}
        <strong>From scratch</strong>. Name it “Helix Foundry” and pick your
        workspace.
      </li>
      <li>
        Under <strong>OAuth &amp; Permissions</strong>, add the Bot Token Scopes{" "}
        <code>channels:read</code> and <code>channels:history</code> for public
        channels, <code>groups:read</code> and <code>groups:history</code> for
        private ones, and <code>users:read</code> and{" "}
        <code>users:read.email</code> to name people.
      </li>
      <li>
        Click <strong>Install to Workspace</strong>, copy the{" "}
        <strong>Bot User OAuth Token</strong> (<code>xoxb-…</code>) and paste it
        below.
      </li>
      <li>
        Invite the app to each channel to sync, public or private, with{" "}
        <code>/invite @Helix Foundry</code>. The messages of a kind of channel
        whose history scope the app lacks are not read. Direct messages are
        never read.
      </li>
    </ol>
  ),
  notion: () => (
    <ol>
      <li>
        Open{" "}
        <External href="https://www.notion.so/profile/integrations">
          Integrations in Notion
        </External>
        , click <strong>New integration</strong> and make an{" "}
        <strong>Internal</strong> one named “Helix Foundry” for your workspace.
      </li>
      <li>
        Under <strong>Capabilities</strong>, keep <strong>Read content</strong>{" "}
        only, and choose{" "}
        <strong>Read user information including email addresses</strong>.
      </li>
      <li>
        Copy the <strong>Internal Integration Secret</strong> (
        <code>ntn_…</code>) and paste it below.
      </li>
      <li>
        Share each page or database to sync with it, from its{" "}
        <strong>•••</strong> menu › <strong>Connections</strong>. Sharing a page
        shares the pages under it.
      </li>
    </ol>
  ),
  google: () => (
    <ol>
      <li>
        In the{" "}
        <External href="https://console.cloud.google.com/iam-admin/serviceaccounts">
          Google Cloud console
        </External>
        , create a service account and add a JSON key under its{" "}
        <strong>Keys</strong>. In its project, enable the Google Drive API,
        Google Calendar API and Admin SDK API.
      </li>
      <li>
        In the{" "}
        <External href="https://admin.google.com">Admin console</External>, open
        Security › Access and data control › API controls ›{" "}
        <strong>Manage domain-wide delegation</strong> and add the service
        account’s client ID with these scopes:
        <pre>
          {[
            "https://www.googleapis.com/auth/drive.readonly",
            "https://www.googleapis.com/auth/calendar.readonly",
            "https://www.googleapis.com/auth/admin.directory.user.readonly",
          ].join(",")}
        </pre>
      </li>
      <li>
        Paste the JSON key below, and an admin’s email as Acts as. Foundry reads
        Drive and Calendar as that user, and each Google Doc they can open and
        download becomes a document.
      </li>
    </ol>
  ),
  postgres: () => (
    <>
      <p>Run this as an administrator, replacing the database and password:</p>
      <pre>{`CREATE ROLE foundry_readonly LOGIN PASSWORD 'choose-a-password';
GRANT CONNECT ON DATABASE app TO foundry_readonly;
GRANT USAGE ON SCHEMA public TO foundry_readonly;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO foundry_readonly;`}</pre>
      <DatabaseReach />
    </>
  ),
  mysql: () => (
    <>
      <p>Run this as an administrator, replacing the database and password:</p>
      <pre>{`CREATE USER 'foundry'@'%' IDENTIFIED BY 'choose-a-password';
GRANT SELECT ON app.* TO 'foundry'@'%';`}</pre>
      <DatabaseReach />
    </>
  ),
  s3: () => (
    <>
      <p>
        Helix reads Parquet, JSON, JSON Lines and CSV files (plain, gzip, zstd
        or bzip2) from one folder of a bucket. Files with the same shape become
        one table, named after their folder or file name, and every sync reads
        them all again.
      </p>
      {!LOCAL && (
        <p>
          With an IAM role, Helix assumes it with this workspace’s external ID.
          The role’s trust policy must require that ID: Helix won’t use a role
          that doesn’t, and checks this on every test and sync with an ID that
          must fail, which your CloudTrail shows as a denied AssumeRole.
        </p>
      )}
      <p>
        If the bucket uses a customer-managed KMS key, also allow{" "}
        <code>kms:Decrypt</code> on that key. AWS bills your account for the
        requests and for data transferred out of the bucket’s region. For
        Cloudflare R2, MinIO, Backblaze B2 or Wasabi, use an access key and set
        the endpoint under Advanced.
      </p>
    </>
  ),
};

export function ConnectionGuide({ form }: { form: ConnectionForm }) {
  return (
    <div className="span2 muted small connection-guide">
      {guides[form.kind](form)}
    </div>
  );
}
