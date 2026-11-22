import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { LOCAL } from "../../../shared/deployment";
import type {
  Connector,
  ConnectorDiscovery,
  S3ConnectorSetup,
  SlackConnectorSetup,
} from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import { sentence } from "../../api";
import { Button, Segmented, Select, Tag, connectorLabel } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { ProGate, isProRequired } from "../workspace/pro-gate";
import {
  installFailed,
  installFailure,
  startSlackInstall,
} from "../workspace/slack-api";
import { ConnectionGuide, SignInGuide } from "./connection-guides";
import { FileTableDetails, S3Fields, fileSummary } from "./connection-s3";
import {
  createConnector,
  syncConnector,
  testConnector,
  updateConnector,
} from "./data-api";
import {
  SLACK_APP_HISTORY_DAYS,
  emptyForm,
  formFromConfig,
  formatBytes,
  hasCredential,
  hasTableChoice,
  isDatabase,
  keySummary,
  relatedTables,
  signInConfig,
  slackAppStep,
  slackNotice,
  suggestedName,
  tableSelection,
  toConfig,
  type ConnectionForm as Form,
  type CredentialKind,
  type SlackCredential,
} from "./model";
import { useSignInStart } from "./sign-in";
import {
  accountLine,
  isSignInKind,
  keyInstead,
  providerName,
  widenRefusal,
  widensReach,
} from "./sign-ins";

// One kind's credential form, in helix-foundry's connection-form style:
// fill in the credential, test it, choose what to sync, then connect. The
// credential lives in this form only: it is sent to the WFE, which never
// returns it, and is dropped when the dialog closes.
//
// Slack, where the kind carries its setup (`slack`, hosted with
// foundry.slack.feed on), opens on a choice: Helix's Slack app, through the
// workspace's install of it (Settings › Slack), or the customer's own app's
// token, which reads its full history at once. The app side has no test:
// it adds the app (or adds it again for what it lacks), or names the
// connection that reads the Slack workspace already, else binds once an
// owner or admin accepts the storage notice. A connection already reading
// through the app sends no credential, so it keeps it; one switching from
// its token keeps syncing by it until the app has read the history.
//
// Editing (`connector` set) starts from the stored settings with the
// credential blank; left blank, the WFE keeps the stored one.
//
// S3 offers an IAM role only where the kind carries its setup (the
// principal and external ID to trust); otherwise a new connection reads with
// a key, and a stored role can be renamed but not tested.
//
// Granola writes each note as a document, and its transcript too when
// Transcripts is on: a choice of what to sync, not of what the key reads,
// so it leaves a test standing.
//
// A Google Workspace key is a service account's JSON key, pasted whole:
// write-only like every credential, so an edit that leaves it blank keeps
// the stored one. A Notion test lists each shared database as a table of
// its own, with its columns and first rows.
//
// A kind that connects by signing in at its provider (`signIn`, hosted:
// GitHub, Linear, Slack, Notion, PostHog US Cloud, Cal.com, Stripe and Google
// Workspace) offers Connect with <provider> in place of its credential,
// with the settings the sign-in can't stand for above it (signInConfig:
// GitHub's repositories and review history, Slack's history, PostHog's
// project and event history, Google's event history), sent
// without a credential; the kind's own link ("Use a bot token instead",
// keyInstead) opens the credential's form, which PostHog's EU Cloud and
// self-hosted keep (Pipes' PostHog signs in to US Cloud only). One click starts the sign-in and leaves for the provider, which
// returns to /connect/callback. A connector that signs in has no
// credential to edit, and only the member whose access it reads with may
// widen it (widensReach: GitHub's repositories, PostHog's project).

// Kinds whose credential is one API key: its field's label and placeholder.
const keyFields: Record<
  "stripe" | "workos" | "linear" | "calcom" | "granola",
  [label: string, placeholder: string]
> = {
  stripe: ["Restricted API key", "rk_live_…"],
  workos: ["API key", "sk_…"],
  linear: ["Personal API key", "lin_api_…"],
  calcom: ["API key", "cal_live_…"],
  granola: ["API key", "grn_…"],
};

type Field = { key: string; label: string; placeholder?: string };
const databaseFields = (kind: "postgres" | "mysql"): Field[] => [
  { key: "host", label: "Host", placeholder: "db.example.com" },
  {
    key: "port",
    label: "Port",
    placeholder: kind === "mysql" ? "3306" : "5432",
  },
  { key: "database", label: "Database", placeholder: "app" },
  { key: "user", label: "Username", placeholder: "foundry_readonly" },
  { key: "password", label: "Password" },
  ...(kind === "postgres"
    ? [{ key: "schema", label: "Schema", placeholder: "public" }]
    : []),
];

export function ConnectionForm({
  kind,
  connector,
  setup,
  slack,
  signIn = false,
  installUrl,
  returnPath,
  onDone,
}: {
  kind: CredentialKind;
  // Editing this connector rather than creating one.
  connector?: Connector;
  // The kind's S3 role setup, where role access is configured.
  setup?: S3ConnectorSetup | null;
  // Slack's setup, where a connection may read through Helix's Slack app.
  slack?: SlackConnectorSetup | null;
  // The kind connects by signing in at its provider, and GitHub's app's
  // install page.
  signIn?: boolean;
  installUrl?: string;
  // Where the sign-in's callback returns once connected; none: the new
  // connector's page.
  returnPath?: string;
  onDone: (connector: Connector, message: string) => void | Promise<void>;
}) {
  const { id, notify, user } = useWorkspace();
  const editing = !!connector,
    stored = connector?.config,
    storedTables =
      (stored &&
        ("postgres" in stored
          ? stored.postgres.tables
          : "mysql" in stored
            ? stored.mysql.tables
            : "s3" in stored
              ? stored.s3.tables
              : undefined)) ||
      [],
    storedKey = !!stored && "s3" in stored && "keys" in stored.s3;
  const roleOff = !setup?.principalArn;
  const [form, setForm] = useState<Form>(() => {
      // An edit keeps the stored access, so a stored role stays one.
      if (editing) return formFromConfig(kind, stored);
      const f = emptyForm(kind);
      if (f.kind === "slack" && slack && !LOCAL)
        return { ...f, credential: "app", historyDays: "90" };
      return f.kind === "s3" && roleOff ? { ...f, access: "keys" } : f;
    }),
    [name, setName] = useState(connector?.name || ""),
    [dirty, setDirty] = useState(false),
    [discovered, setDiscovered] = useState<ConnectorDiscovery | null>(null),
    [chosen, setChosen] = useState<Set<string>>(new Set()),
    // What is in flight: a test or a save.
    [pending, setPending] = useState<"test" | "save" | null>(null),
    [error, setError] = useState<Error | null>(null),
    // The settings changed after a test passed, or while one ran: Connect
    // waits for another test, and the form says so.
    [retest, setRetest] = useState(false),
    // A kind that signs in: its credential's form instead.
    [useKey, setUseKey] = useState(false);
  // One sign-in per form (its client token): a retry after a refusal finds
  // it again, until the name or a setting changes (renew).
  const start = useSignInStart(),
    { renew } = start,
    account = connector?.account,
    signInKind = isSignInKind(form.kind) ? form.kind : null,
    // PostHog signs in to US Cloud only: EU Cloud and self-hosted keep a key.
    keyOnly = form.kind === "posthog" && form.region !== "us",
    signingIn =
      !editing && !LOCAL && signIn && !useKey && !!signInKind && !keyOnly,
    // No credential field: signing in, or signed in already.
    noKey = signingIn || !!account;
  // Slack through Helix's app: what its side shows, and whether this save
  // binds the connection to the install (a new one, a switch from a token,
  // or the workspace's Slack changed), which takes the storage notice.
  const slackApp = form.kind === "slack" && form.credential === "app",
    storedApp = !!stored && "slack" in stored && !!stored.slack.helixApp,
    binds =
      slackApp &&
      (!storedApp ||
        connector?.slackStatus?.state === "SLACK_FEED_STATE_TEAM_CHANGED"),
    step = slack ? slackAppStep(slack, connector?.id) : null,
    notice = slackNotice(slack?.noticeVersion),
    appReady =
      slackApp &&
      step?.kind === "ready" &&
      (!binds || (!!notice && form.noticeVersion === slack!.noticeVersion));
  const [installing, setInstalling] = useState(false),
    [installError, setInstallError] = useState("");
  // Adding the app (again) leaves for Slack, which comes back here: the
  // form (Add data's Slack) or the connection's page.
  const addToSlack = async () => {
    setInstalling(true);
    setInstallError("");
    try {
      window.location.assign(
        await startSlackInstall(
          id,
          connector ? paths.connection(connector.id) : "/data?add=slack",
        ),
      );
    } catch (e) {
      installFailed(installFailure(e));
      setInstallError(sentence((e as Error).message));
      setInstalling(false);
    }
  };
  const chooseCredential = (credential: SlackCredential) => {
    trackEvent("slack_connector_mode_chosen", { mode: credential });
    change({
      credential,
      // The app keeps at most 90 days.
      ...(credential === "app" &&
      form.kind === "slack" &&
      Number(form.historyDays) > SLACK_APP_HISTORY_DAYS
        ? { historyDays: String(SLACK_APP_HISTORY_DAYS) }
        : {}),
    } as Partial<Form>);
  };
  // Another name is another sign-in, as other settings are (change()).
  useEffect(() => void renew(), [name, renew]);
  const label = connectorLabel(kind),
    database = isDatabase(kind),
    choice = hasTableChoice(kind),
    secretHint = connector?.secretHint;
  const value = (key: string) => String((form as Record<string, unknown>)[key]);
  // Counts changes to the settings, so a test can tell they changed while
  // it ran (an S3 test can take most of a minute).
  const edits = useRef(0);
  // Any change to the settings or credential invalidates the last test.
  const change = (patch: Partial<Form>) => {
    edits.current++;
    if (discovered || pending === "test") setRetest(true);
    setForm((f) => ({ ...f, ...patch }) as Form);
    setDirty(true);
    setDiscovered(null);
    setChosen(new Set());
    renew();
  };
  const update = (key: string, v: string) => change({ [key]: v });
  const run = async (what: "test" | "save", fn: () => Promise<void>) => {
    setPending(what);
    setError(null);
    try {
      await fn();
    } catch (e) {
      setError(e as Error);
    } finally {
      setPending(null);
    }
  };
  const tables = discovered?.tables || [],
    names = tables.map((t) => t.name),
    related = relatedTables(tables, chosen),
    unavailable = discovered?.unavailable || [],
    // Entries the source says why of ("name: reason") read as a list.
    reasons = unavailable.some((u) => u.includes(": "));
  const cantRead = (
    <div className="muted small">
      Helix can’t read:
      <ul className="dc-unavailable">
        {unavailable.map((u) => (
          <li key={u}>{u}</li>
        ))}
      </ul>
    </div>
  );
  const selection = choice
    ? discovered
      ? tableSelection(names, chosen)
      : storedTables
    : [];
  // File sources cap the stored bytes a connection syncs.
  const maxBytes = Number(discovered?.maxBytes || 0),
    chosenBytes = tables
      .filter((t) => chosen.has(t.name))
      .reduce((n, t) => n + Number(t.bytes || 0), 0),
    overLimit = maxBytes > 0 && chosenBytes > maxBytes;
  const test = () =>
    run("test", async () => {
      setRetest(false);
      const at = edits.current;
      // A test of settings changed since reports nothing, pass or fail: its
      // tables and its error describe the old ones.
      const result = await testConnector(id, toConfig(form, [])).catch(
        (e: unknown) => {
          if (edits.current !== at) return;
          trackEvent("connector_test_failed", { kind: form.kind });
          throw e;
        },
      );
      if (!result || edits.current !== at) return;
      const found = result.discovery || { tables: [] };
      // Pin syncs to the PostHog project the test resolved, and keep the
      // bucket's region it detected so syncs need not.
      const project =
          result.config && "posthog" in result.config
            ? result.config.posthog.projectId
            : "",
        region =
          result.config && "s3" in result.config ? result.config.s3.region : "";
      setForm((f) =>
        f.kind === "posthog" && project && !f.projectId
          ? { ...f, projectId: project }
          : f.kind === "s3" && region && region !== f.region
            ? { ...f, region }
            : f,
      );
      setDiscovered(found);
      const all = (found.tables || []).map((t) => t.name);
      // A new connection syncs every table until some are cleared; an edit
      // keeps its stored choice.
      setChosen(
        new Set(
          editing && storedTables.length
            ? all.filter((t) => storedTables.includes(t))
            : all,
        ),
      );
    });
  const submit = () =>
    run("save", async () => {
      if (selection === "none") return;
      // A signed-in connector's settings go without a credential.
      const config =
        (account && signInConfig(form)) || toConfig(form, selection);
      if (connector) {
        const renamed = name.trim() !== connector.name,
          reconfigured = dirty || !!discovered;
        const saved = await updateConnector(id, connector.id, {
          ...(renamed ? { name: name.trim() } : {}),
          ...(reconfigured ? { config } : {}),
        });
        await onDone(
          saved.connector,
          slackApp && !storedApp && saved.connector.slackStatus?.switching
            ? "Switching to Helix Foundry’s Slack app. Your token keeps syncing until it has read the history"
            : "Connection saved",
        );
        return;
      }
      const created = await createConnector(id, {
        name: name.trim() || suggestedName(form),
        config,
      });
      trackEvent("connector_created", { kind: form.kind });
      // Through the app, Slack sends its messages and the app reads the
      // history: there is no first sync to start.
      if (slackApp) {
        await onDone(
          created.connector,
          "Connected. Helix Foundry is reading the channels’ history",
        );
        return;
      }
      // The first sync starts with the connection; a refusal is reported but
      // the connection is kept.
      await syncConnector(id, created.connector.id, "created").catch((e) =>
        notify((e as Error).message, true),
      );
      await onDone(created.connector, "Connected. The first sync is queued");
    });
  // Signed in, only the account's member widens what it reads, as the
  // WFE's widensReach decides: a GitHub repository it didn't name or
  // clearing the list, PostHog's project.
  const settings = signInConfig(form),
    widened =
      !!account &&
      !!settings &&
      account.userId !== user.id &&
      widensReach(stored, settings);
  const connectWithProvider = () =>
    void start.go({
      kind: form.kind,
      name: name.trim() || suggestedName(form),
      // The settings alone: the provider's sign-in is the credential.
      config: settings,
      clientToken: start.clientToken,
      returnPath,
    });
  const renamedOnly =
    editing && !dirty && !discovered && name.trim() !== connector!.name;
  const canSubmit =
    selection !== "none" &&
    !overLimit &&
    !widened &&
    (slackApp
      ? // Through the app there is nothing to test; a rename alone sends no
        // settings.
        editing
        ? name.trim() !== "" && (renamedOnly || (dirty && appReady))
        : appReady
      : editing
        ? name.trim() !== "" &&
          (name.trim() !== connector!.name || dirty || !!discovered)
        : !!discovered);
  // Leaving the app for a token takes a token: a blank one would keep the
  // app.
  const credentialRequired = !editing || (form.kind === "slack" && storedApp),
    // A role can't be tested where role access is off.
    untestable = form.kind === "s3" && form.access === "role" && roleOff,
    testable =
      !signingIn &&
      !slackApp &&
      (!editing || hasCredential(form)) &&
      !untestable;
  return (
    <div className="connect-data">
      <form
        className="form-grid"
        onSubmit={(e) => {
          e.preventDefault();
          // Enter connects once a test passed, and tests until then: Test is
          // the form's submit button until one passes (a form of several
          // fields submits on Enter only through one).
          if (pending) return;
          const by = (e.nativeEvent as SubmitEvent).submitter;
          if (by instanceof HTMLElement && by.dataset.action === "test")
            void test();
          else if (canSubmit) void submit();
          else if (testable && !discovered) void test();
        }}
      >
        {/* The steps end "paste it below": they come before the fields. */}
        {signingIn && signInKind ? (
          <SignInGuide kind={signInKind} installUrl={installUrl} />
        ) : (
          !editing && <ConnectionGuide form={form} />
        )}
        {account && (
          <p className="span2 muted small">
            {accountLine(connector!)}. Sign it in again from its page to change
            the account.
          </p>
        )}
        {editing && (
          <label className="span2">
            Connection name
            <input
              required
              maxLength={120}
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
        )}
        {(form.kind === "stripe" ||
          form.kind === "workos" ||
          form.kind === "linear" ||
          form.kind === "calcom" ||
          form.kind === "granola") &&
          !noKey && (
            <label className="span2">
              {keyFields[form.kind][0]}
              <input
                type="password"
                autoComplete="off"
                spellCheck={false}
                required={credentialRequired}
                placeholder={
                  secretHint
                    ? `Leave blank to keep ${secretHint}`
                    : keyFields[form.kind][1]
                }
                value={form.apiKey}
                onChange={(e) => update("apiKey", e.target.value)}
              />
            </label>
          )}
        {form.kind === "granola" && (
          <div className="span2 selection-row">
            <label className="checkbox">
              <input
                type="checkbox"
                checked={form.transcripts}
                onChange={(e) => {
                  const transcripts = e.target.checked;
                  setForm((f) => ({ ...f, transcripts }) as Form);
                  setDirty(true);
                }}
              />
              <span>
                Transcripts
                <small>
                  Each note’s transcript as a document too, in a Transcripts set
                  beside Notes. Transcripts are long: reading one into an
                  ontology costs several times what a note does.
                </small>
              </span>
            </label>
          </div>
        )}
        {form.kind === "slack" && (
          <>
            {slack && !LOCAL && !account && (
              <div className="span2 dc-slack-choice">
                <Segmented
                  label="Read Slack with"
                  value={form.credential}
                  onChange={(v) => chooseCredential(v as SlackCredential)}
                  options={[
                    { value: "app", label: "Use the Helix Slack app" },
                    { value: "token", label: "Use your own app’s token" },
                  ]}
                />
                <small className="muted">
                  {slackApp
                    ? "New messages arrive as they’re posted, and older history fills in over the next days."
                    : "Reads the full history at once, through an app you make in Slack."}
                </small>
              </div>
            )}
            {!slackApp && !noKey && (
              <label className="span2">
                Bot token
                <input
                  type="password"
                  autoComplete="off"
                  spellCheck={false}
                  required={credentialRequired}
                  placeholder={
                    secretHint && !storedApp
                      ? `Leave blank to keep ${secretHint}`
                      : "xoxb-…"
                  }
                  value={form.botToken}
                  onChange={(e) => update("botToken", e.target.value)}
                />
              </label>
            )}
            {slackApp && !step && (
              <p className="span2 muted small">
                Syncing Slack through Helix Foundry’s app isn’t available here.
                Use your own app’s token.
              </p>
            )}
            {slackApp && step && step.kind !== "ready" && (
              <div className="span2 dc-slack-step" role="status">
                {step.kind === "in_use" ? (
                  <p className="small">
                    <Link to={paths.connection(step.connectorId)}>
                      Another connection
                    </Link>{" "}
                    already reads this Slack workspace: a Slack workspace is
                    read by one connection.
                  </p>
                ) : (
                  <>
                    <p className="small">
                      {step.kind === "install"
                        ? "Add Helix Foundry to your Slack workspace first: Slack asks you to allow it, then brings you back here."
                        : step.why === "scopes"
                          ? "Helix Foundry needs new Slack permissions to sync channels. Add it to Slack again to grant them; it keeps answering in Slack meanwhile."
                          : "A Slack workspace admin or owner must add Helix Foundry to Slack before it can sync channels. It was added by someone who isn’t one."}
                    </p>
                    {step.canInstall ? (
                      <Button
                        variant="primary"
                        pending={installing}
                        onClick={() => void addToSlack()}
                      >
                        {step.kind === "install"
                          ? "Add Helix Foundry to Slack"
                          : "Reinstall"}
                      </Button>
                    ) : (
                      <p className="muted small">
                        An owner or admin of this workspace adds it from
                        Settings › Slack.
                      </p>
                    )}
                    {installError && (
                      <p role="alert" className="small error-text">
                        {installError}
                      </p>
                    )}
                  </>
                )}
              </div>
            )}
            {slackApp && step?.kind === "ready" && (
              <p className="span2 small">
                Reads <strong>{step.team}</strong> through Helix Foundry, the
                app added in Settings › Slack.
                {editing &&
                  !storedApp &&
                  " Your token keeps syncing until Helix Foundry has read the history."}
              </p>
            )}
            {(!slackApp || step?.kind === "ready") && (
              <label>
                Days of history
                <input
                  type="number"
                  min={1}
                  max={slackApp ? SLACK_APP_HISTORY_DAYS : 365}
                  value={form.historyDays}
                  onChange={(e) => update("historyDays", e.target.value)}
                />
              </label>
            )}
            {binds &&
              step?.kind === "ready" &&
              (notice ? (
                <div className="span2 selection-row">
                  <label className="checkbox">
                    <input
                      type="checkbox"
                      checked={form.noticeVersion === slack!.noticeVersion}
                      onChange={(e) =>
                        change({
                          noticeVersion: e.target.checked
                            ? slack!.noticeVersion || 0
                            : 0,
                        } as Partial<Form>)
                      }
                    />
                    <span>
                      I’m allowed to have Helix keep this copy of {step.team}’s
                      channels
                      <small>{notice}</small>
                    </span>
                  </label>
                </div>
              ) : (
                <p role="alert" className="span2 small error-text">
                  Reload the page to read Helix Foundry’s current storage notice
                  before connecting.
                </p>
              ))}
          </>
        )}
        {form.kind === "notion" && !noKey && (
          <label className="span2">
            Integration secret
            <input
              type="password"
              autoComplete="off"
              spellCheck={false}
              required={credentialRequired}
              placeholder={
                secretHint ? `Leave blank to keep ${secretHint}` : "ntn_…"
              }
              value={form.token}
              onChange={(e) => update("token", e.target.value)}
            />
          </label>
        )}
        {form.kind === "google" && (
          <>
            {/* Signed in, it reads as the person who signed in: no service
                account, and no one to act as. */}
            {!noKey && (
              <>
                <label className="span2">
                  Service account JSON key
                  <textarea
                    autoComplete="off"
                    spellCheck={false}
                    required={credentialRequired}
                    placeholder={
                      secretHint
                        ? `Leave blank to keep ${secretHint}`
                        : '{"type": "service_account", …}'
                    }
                    value={form.serviceAccountKey}
                    onChange={(e) =>
                      update("serviceAccountKey", e.target.value)
                    }
                  />
                </label>
                <label className="span2">
                  Acts as
                  <input
                    type="email"
                    required
                    autoComplete="off"
                    spellCheck={false}
                    placeholder="admin@example.com"
                    value={form.subject}
                    onChange={(e) => update("subject", e.target.value)}
                  />
                </label>
              </>
            )}
            <label>
              Days of past events
              <input
                type="number"
                min={1}
                max={365}
                value={form.eventDays}
                onChange={(e) => update("eventDays", e.target.value)}
              />
            </label>
          </>
        )}
        {form.kind === "databuddy" && (
          <>
            <label className="span2">
              API key
              <input
                type="password"
                autoComplete="off"
                spellCheck={false}
                required={credentialRequired}
                placeholder={
                  secretHint ? `Leave blank to keep ${secretHint}` : "dbdy_…"
                }
                value={form.apiKey}
                onChange={(e) => update("apiKey", e.target.value)}
              />
            </label>
            <label className="span2">
              Websites (optional)
              <textarea
                spellCheck={false}
                placeholder={"web_…\nweb_…"}
                value={form.websiteIds}
                onChange={(e) => update("websiteIds", e.target.value)}
              />
              <small className="muted">
                One website id per line. Leave blank to sync every website the
                key can read.
              </small>
            </label>
            <label>
              Days of history
              <input
                type="number"
                min={1}
                max={365}
                value={form.historyDays}
                onChange={(e) => update("historyDays", e.target.value)}
              />
            </label>
          </>
        )}
        {form.kind === "github" && (
          <>
            {!noKey && (
              <label className="span2">
                Personal access token
                <input
                  type="password"
                  autoComplete="off"
                  spellCheck={false}
                  required={credentialRequired}
                  placeholder={
                    secretHint
                      ? `Leave blank to keep ${secretHint}`
                      : "github_pat_…"
                  }
                  value={form.token}
                  onChange={(e) => update("token", e.target.value)}
                />
              </label>
            )}
            <label className="span2">
              Repositories (optional)
              <textarea
                spellCheck={false}
                placeholder={"acme/api\nacme/web"}
                value={form.repos}
                onChange={(e) => update("repos", e.target.value)}
                aria-describedby={widened ? "dc-widen" : undefined}
              />
              <small className="muted">
                {noKey
                  ? "One owner/name per line. Leave blank to sync every repository Helix Foundry is installed on that the account can see, up to 100."
                  : "One owner/name per line. Leave blank to sync every repository the token can read, up to 100."}
              </small>
            </label>
            <label>
              Review history (days)
              <input
                type="number"
                min={1}
                max={365}
                value={form.reviewDays}
                onChange={(e) => update("reviewDays", e.target.value)}
              />
            </label>
          </>
        )}
        {form.kind === "posthog" && (
          <>
            {!noKey && (
              <label className="span2">
                Personal API key
                <input
                  type="password"
                  autoComplete="off"
                  spellCheck={false}
                  required={credentialRequired}
                  placeholder={
                    secretHint ? `Leave blank to keep ${secretHint}` : "phx_…"
                  }
                  value={form.apiKey}
                  onChange={(e) => update("apiKey", e.target.value)}
                />
              </label>
            )}
            {/* Signed in, it is US Cloud's: Pipes' PostHog has no region. */}
            {!account && (
              <label>
                Region
                <Select
                  label="Region"
                  value={form.region}
                  onChange={(v) => update("region", v)}
                  options={[
                    { value: "us", label: "US Cloud" },
                    { value: "eu", label: "EU Cloud" },
                    { value: "custom", label: "Self-hosted" },
                  ]}
                />
              </label>
            )}
            {!editing && !LOCAL && signIn && keyOnly && (
              <p className="span2 muted small">
                EU Cloud and self-hosted PostHog connect with a personal API
                key: signing in reaches US Cloud only.
              </p>
            )}
            <label>
              Project ID (optional)
              <input
                inputMode="numeric"
                placeholder="Found in project settings"
                value={form.projectId}
                onChange={(e) => update("projectId", e.target.value)}
                aria-describedby={widened ? "dc-widen" : undefined}
              />
            </label>
            {form.region === "custom" && (
              <label className="span2">
                PostHog URL
                <input
                  required
                  type="url"
                  placeholder="https://posthog.example.com"
                  value={form.host}
                  onChange={(e) => update("host", e.target.value)}
                />
              </label>
            )}
            <label>
              Event history (days)
              <input
                type="number"
                min={1}
                max={365}
                value={form.eventDays}
                onChange={(e) => update("eventDays", e.target.value)}
              />
            </label>
          </>
        )}
        {form.kind === "s3" && (
          <S3Fields
            form={form}
            setup={setup}
            editing={editing}
            storedKey={storedKey}
            onChange={change}
          />
        )}
        {(form.kind === "postgres" || form.kind === "mysql") &&
          databaseFields(form.kind).map(({ key, label, placeholder }) => (
            <label key={key}>
              {label}
              <input
                type={
                  key === "password"
                    ? "password"
                    : key === "port"
                      ? "number"
                      : "text"
                }
                min={key === "port" ? 1 : undefined}
                max={key === "port" ? 65535 : undefined}
                autoComplete="off"
                spellCheck={false}
                required={
                  key === "password"
                    ? credentialRequired
                    : !["port", "schema"].includes(key)
                }
                placeholder={
                  key === "password" && editing
                    ? "Leave blank to keep the stored password"
                    : placeholder
                }
                value={value(key)}
                onChange={(e) => update(key, e.target.value)}
              />
            </label>
          ))}
        {(!editing || database) && (
          <details className="span2 disclosure">
            <summary>Connection options</summary>
            <div className="form-grid">
              {!editing && (
                <label className="span2">
                  Connection name (optional)
                  <input
                    maxLength={120}
                    value={name}
                    placeholder={suggestedName(form)}
                    onChange={(e) => setName(e.target.value)}
                  />
                </label>
              )}
              {(form.kind === "postgres" || form.kind === "mysql") && (
                <>
                  <label className="span2">
                    Encryption
                    <Select
                      label="Encryption"
                      value={form.tls}
                      onChange={(v) => update("tls", v)}
                      options={[
                        {
                          value: "verify",
                          label: "Require TLS with certificate verification",
                        },
                        { value: "disable", label: "No TLS" },
                      ]}
                    />
                  </label>
                  {form.tls === "verify" && (
                    <label className="span2">
                      CA certificate (optional)
                      <textarea
                        placeholder="-----BEGIN CERTIFICATE-----"
                        value={form.ca}
                        onChange={(e) => update("ca", e.target.value)}
                      />
                    </label>
                  )}
                </>
              )}
            </div>
          </details>
        )}
        {editing && choice && !discovered && (
          <p className="span2 muted small">
            {storedTables.length
              ? `Syncs ${storedTables.join(", ")}.`
              : "Syncs every table."}{" "}
            To change which tables sync,{" "}
            {form.kind === "s3"
              ? form.access === "keys"
                ? "enter the secret access key and test the connection."
                : untestable
                  ? "use an access key and test the connection."
                  : "test the connection."
              : "enter the password and test the connection."}
          </p>
        )}
        {signingIn && (
          <div className="span2 dc-sign-in">
            <button
              className="button primary"
              type="button"
              disabled={start.pending}
              onClick={connectWithProvider}
            >
              {start.pending
                ? `Opening ${providerName(form.kind)}…`
                : `Connect with ${providerName(form.kind)}`}
            </button>
            <button
              type="button"
              className="text-button"
              onClick={() => setUseKey(true)}
            >
              {keyInstead(signInKind!)}
            </button>
          </div>
        )}
        {!editing && !LOCAL && signIn && useKey && (
          <div className="span2">
            <button
              type="button"
              className="text-button"
              onClick={() => {
                setUseKey(false);
                // PostHog signs in to US Cloud only.
                if (keyOnly) change({ region: "us" } as Partial<Form>);
              }}
            >
              Connect with {providerName(form.kind)} instead
            </button>
          </div>
        )}
        {widened && (
          <p id="dc-widen" className="span2 small error-text" role="alert">
            {widenRefusal(connector!)}
          </p>
        )}
        {testable && (
          <div className="span2">
            <button
              className={discovered ? "button" : "button primary"}
              // The WFE says what a test lacks, not the browser.
              type={discovered ? "button" : "submit"}
              formNoValidate
              data-action="test"
              disabled={!!pending}
              onClick={discovered ? () => void test() : undefined}
            >
              {pending === "test" ? "Testing connection…" : "Test connection"}
            </button>
            {retest && !discovered && pending !== "test" && (
              <p className="muted small" role="status">
                The settings changed since the last test. Test the connection
                again to connect.
              </p>
            )}
          </div>
        )}
        {discovered && choice && (
          <fieldset className="span2 plain-fieldset">
            <legend>Choose tables to sync</legend>
            {form.kind === "s3" && form.region && (
              <p className="muted small">The bucket is in {form.region}.</p>
            )}
            {names.length > 1 && (
              <div className="selection-actions">
                <button
                  type="button"
                  className="text-button"
                  disabled={chosen.size === names.length}
                  onClick={() => setChosen(new Set(names))}
                >
                  Select all
                </button>
                <button
                  type="button"
                  className="text-button"
                  disabled={chosen.size === 0}
                  onClick={() => setChosen(new Set())}
                >
                  Clear
                </button>
              </div>
            )}
            <div className="table-selection">
              {tables.map((t) => (
                <div
                  className={"selection-row" + (t.format ? " dc-file-row" : "")}
                  key={t.name}
                  role={t.format ? "group" : undefined}
                  aria-label={t.format ? t.name : undefined}
                >
                  <label className="checkbox">
                    <input
                      type="checkbox"
                      checked={chosen.has(t.name)}
                      onChange={(e) =>
                        setChosen((s) => {
                          const next = new Set(s);
                          if (e.target.checked) next.add(t.name);
                          else next.delete(t.name);
                          return next;
                        })
                      }
                    />
                    <span>
                      {t.name}
                      {t.format && (
                        <Tag className="dc-format">
                          {t.format.toUpperCase()}
                        </Tag>
                      )}
                      <small>
                        {t.format ? fileSummary(t) : keySummary(t)}
                        {related.has(t.name) && " · Related to your selection"}
                      </small>
                      {t.problem && (
                        <small className="dc-problem">
                          {t.problem}. The sync will fail until this is fixed.
                        </small>
                      )}
                    </span>
                  </label>
                  {t.format && <FileTableDetails table={t} />}
                </div>
              ))}
            </div>
            {maxBytes > 0 && (
              <p
                className={"small " + (overLimit ? "error-text" : "muted")}
                role="status"
              >
                Selected {formatBytes(String(chosenBytes))} of{" "}
                {formatBytes(discovered.maxBytes)}
                {overLimit &&
                  ". Choose fewer tables or a narrower folder to connect"}
                .
              </p>
            )}
            <p className="muted small">
              {names.length
                ? "Only selected tables are synced. Selecting every table also syncs tables added later."
                : "No tables found. Check the schema and the user’s read permissions."}
            </p>
            {!!unavailable.length && cantRead}
          </fieldset>
        )}
        {discovered && !choice && (
          <fieldset className="span2 plain-fieldset">
            <legend>{label} objects this key can read</legend>
            <div className="table-selection" role="list">
              {tables.map((t) => (
                <div
                  className={
                    "selection-row" + (t.preview?.length ? " dc-file-row" : "")
                  }
                  key={t.name}
                  role="listitem"
                >
                  <span className="checkbox">
                    <span>
                      {t.name}
                      <small>{keySummary(t)}</small>
                    </span>
                  </span>
                  {/* A source with tables of its own making, such as a
                      Notion database, shows their columns and first rows. */}
                  {!!t.preview?.length && <FileTableDetails table={t} />}
                </div>
              ))}
            </div>
            <p className="muted small">
              Every object listed is synced.
              {!!unavailable.length &&
                !reasons &&
                ` This key cannot read: ${unavailable.join(", ")}.`}
            </p>
            {reasons && cantRead}
          </fieldset>
        )}
        {!!discovered?.warnings?.length && (
          <ul className="span2 small dc-warnings" aria-label="Warnings">
            {discovered.warnings.map((w) => (
              <li key={w}>{w.charAt(0).toUpperCase() + w.slice(1)}.</li>
            ))}
          </ul>
        )}
        {(editing || discovered || (slackApp && step?.kind === "ready")) && (
          <button className="button primary" disabled={!!pending || !canSubmit}>
            {pending === "save"
              ? "Saving connection…"
              : editing
                ? "Save changes"
                : slackApp
                  ? "Connect"
                  : choice
                    ? `Connect and sync ${
                        selection === "none"
                          ? "tables"
                          : selection.length
                            ? `${selection.length} ${selection.length === 1 ? "table" : "tables"}`
                            : "every table"
                      }`
                    : "Connect and sync"}
          </button>
        )}
      </form>
      {start.error && (
        <p role="alert" className="error-box">
          {start.error.message}
        </p>
      )}
      {/* The plan can change under an open form: the WFE's Pro refusal
          becomes the gate, with its way to upgrade. */}
      {error &&
        (isProRequired(error) ? (
          <ProGate feature={`${label} connections`} />
        ) : (
          <p role="alert" className="error-box">
            {error.message}
          </p>
        ))}
    </div>
  );
}
