import type { ReactNode } from "react";

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

type Guide = (form: Record<string, any>) => ReactNode;

const guides: Record<string, Guide> = {
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
    const host =
      form.region === "custom"
        ? /^https?:\/\/[^/]+/.exec(form.host || "")?.[0] || null
        : `https://${form.region === "eu" ? "eu" : "us"}.posthog.com`;
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
  postgres: () => (
    <>
      <p>Run this as an administrator, replacing the database and password:</p>
      <pre>{`CREATE ROLE foundry_readonly LOGIN PASSWORD 'choose-a-password';
GRANT CONNECT ON DATABASE app TO foundry_readonly;
GRANT USAGE ON SCHEMA public TO foundry_readonly;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO foundry_readonly;`}</pre>
      <p>
        Live sync also needs replication permission. Without it, Foundry
        refreshes a complete snapshot every five minutes.
      </p>
    </>
  ),
  mysql: () => (
    <>
      <p>Run this as an administrator, replacing the database and password:</p>
      <pre>{`CREATE USER 'foundry'@'%' IDENTIFIED BY 'choose-a-password';
GRANT SELECT ON app.* TO 'foundry'@'%';
-- Optional, for live sync from the binlog:
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'foundry'@'%';`}</pre>
    </>
  ),
  neo4j: () => (
    <>
      <p>
        Use the HTTP URL of your Neo4j server (port 7474), not the bolt URL. If
        Neo4j runs on this computer and Foundry runs in Docker, use{" "}
        <code>http://host.docker.internal:7474</code>, because{" "}
        <code>localhost</code> is Foundry's own container. Foundry needs Neo4j 5
        or later with the Query API, which Neo4j 5.25 and later turn on by
        default. On Enterprise Edition, run this as an administrator to create a
        read-only user:
      </p>
      <pre>{`CREATE USER foundry SET PASSWORD 'choose-a-password' CHANGE NOT REQUIRED;
GRANT ROLE reader TO foundry;`}</pre>
      <p>
        Community Edition has no roles, so connect with its user. Each label
        becomes a dataset of its nodes and each relationship type a dataset of
        its relationships, with the start and end node ids as columns.
      </p>
    </>
  ),
  s3: () => (
    <>
      <p>
        Create an IAM user or role with this policy, then{" "}
        <External href="https://docs.aws.amazon.com/IAM/latest/UserGuide/id_credentials_access-keys.html">
          create an access key
        </External>{" "}
        for it. Replace <code>your-bucket</code> with your bucket name.
      </p>
      <pre>{`{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::your-bucket" },
    { "Effect": "Allow", "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::your-bucket/*" }
  ]
}`}</pre>
      <p>
        For other S3-compatible storage, use its read-only keys and set the
        custom endpoint.
      </p>
    </>
  ),
  rest: () => (
    <ul>
      <li>
        <strong>Endpoint URL</strong> returns a list of records as JSON.
      </li>
      <li>
        If the list is nested, such as <code>{`{"data": [...]}`}</code>, set{" "}
        <strong>Items path</strong> to <code>data</code>.
      </li>
      <li>
        For more than one page, choose how the API pages: a next link, a cursor,
        or an offset and limit.
      </li>
    </ul>
  ),
};

export function ConnectionGuide({
  kind,
  form,
}: {
  kind: string;
  form: Record<string, any>;
}) {
  const guide = guides[kind];
  if (!guide) return null;
  return (
    <div className="span2 muted small connection-guide">{guide(form)}</div>
  );
}
