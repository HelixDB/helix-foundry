import {
  expect,
  test as base,
  type Locator,
  type Page,
} from "@playwright/test";
import { e2ePorts } from "../../../mock/env";

// Talks to the mock WFE's test hooks, and signs in through its fake WorkOS
// page. Every test starts from a freshly seeded mock.

const MOCK = `http://127.0.0.1:${e2ePorts.mock}`;
/** The app's origin, for the Origin header of a request a test makes itself. */
export const APP_ORIGIN = `http://localhost:${e2ePorts.app}`;

export type MockStats = {
  loginStarts: number;
  refreshes: number;
  requests: string[];
};

export const mock = {
  reset: () => fetch(`${MOCK}/__mock/reset`, { method: "POST" }),
  stats: async (): Promise<MockStats> =>
    (await (await fetch(`${MOCK}/__mock/stats`)).json()).stats,
  /** The mock's settings now: what the knobs below have counted down to. */
  settings: async (): Promise<{ startingQueries?: Record<string, number> }> =>
    (await (await fetch(`${MOCK}/__mock/stats`)).json()).config,
  configure: (config: {
    accessTokenTtlSeconds?: number;
    allowedRegions?: string[];
    checkoutCardOnFile?: boolean;
    // A cancelled Foundry plan has reached its period end: Free from then.
    foundryPeriodEnded?: boolean;
    allowedEmails?: string[];
    // A generation's queued and running time, and how it ends ("auth": the
    // provider refused the workspace's key).
    ontologyStepMs?: number;
    ontologyOutcome?: "succeed" | "fail" | "retry" | "auth";
    // A revision measuring rules on a comparable load is over its budget.
    reviseOutcome?: "budget";
    // A load's step, how it ends, and whether reading a document fails.
    loadStepMs?: number;
    loadOutcome?: "succeed" | "fail" | "sources_changed" | "budget_exhausted";
    documentOutcome?: "succeed" | "fail";
    // Seeds an adjudicated variant and its informational review (off by default).
    documentAdjudicated?: boolean;
    // A load's delete fails for good (delete_failed).
    deleteOutcome?: "succeed" | "fail";
    // The next n presigned PUTs fail as a busy bucket does.
    documentUploadFailures?: number;
    // Acme holds more document sets, by these names, as a legacy merge may
    // have left it: past the 20 a create allows, or a name another has.
    legacyDocumentSets?: string[];
    // Every workspace's Helix plan, which a new database needs, has lapsed
    // and is not granted again. By default every workspace has one: a new
    // workspace gets its free plan with its first database.
    helixSubscription?: false;
    // Ontologies come without their project, tenant and database name, as
    // for a member who can't read the database's project.
    ontologyDatabaseHidden?: boolean;
    // The database with that slug refuses its next n requests with the
    // gateway's 503 rate_limit_unavailable, as a new database does.
    startingQueries?: Record<string, number>;
    // How many customers only Stripe's invoices table states, in a load
    // written from now on: Customer then has two sources.
    invoiceOnlyCustomers?: number;
    readsBusy?: number;
    readsPaused?: string;
    // A load's reads with rules: parts that fail in entity reads, by pred
    // ("derived:x", or "violations") and code (budget, hub or invalid).
    readErrors?: Record<string, "budget" | "hub" | "invalid">;
    // A question's step, how it ends, markup in its answer, and what the
    // month spent on questions before.
    askStepMs?: number;
    askOutcome?: "answered" | "cannot_answer" | "budget_exhausted" | "failed";
    askMarkup?: boolean;
    // false: answers as a 3.10.0 engine did, without outputs or visuals.
    askVisuals?: boolean;
    askSpentUsdMicros?: number;
    // false: the WFE has no foundry-connector role, so S3 offers keys only.
    s3RoleAccess?: boolean;
    // Another writer comes first on each of the next n data-layout PUTs.
    dataLayoutConflicts?: number;
    // Alan's role in Acme (a member by default).
    alanRole?: "WORKSPACE_ROLE_ADMIN" | "WORKSPACE_ROLE_MEMBER";
    // Alan can't query databases: he reads through source grants.
    memberQueryRead?: boolean;
    // People enrichment: a batch of lookups (and the wait for a slot), and
    // how the lookups stop: the credit cap, context.dev, the time limit, or
    // no free slot.
    enrichmentStepMs?: number;
    enrichmentOutcome?:
      | "cap"
      | "provider_credits"
      | "provider"
      | "time"
      | "busy";
    // false: the operators' flag is off (availability DISABLED).
    enrichmentFlag?: boolean;
    // How long an API source's sync takes from the send that queued it
    // (1 s by default), and the caps every table has, to fill one.
    eventSyncMs?: number;
    eventCaps?: { maxRows: number; maxBytes: number };
    // Every API source's allowance for the month is spent: a set holding
    // documents is paused, and jobs no person started hold its pieces back.
    eventSetAllowanceSpent?: boolean;
    // Settings › AI models: how long a save's test takes, and a test that
    // runs out of time.
    aiProbeMs?: number;
    aiProbeOutcome?: "timeout";
    // The kinds the WFE offers, as a deployment's foundry.ai.kinds.
    aiKinds?: string[];
    // false: no kind connects by signing in (pipes.enabled off).
    signIn?: boolean;
    // Every sign-in runs out now, once, as its 15 minutes would.
    signInsRunOut?: boolean;
    // Breaks these connectors' accounts, as a member leaving does.
    accountBroken?: Record<
      string,
      "member_left" | "access_revoked" | "expired" | "account_missing"
    >;
    // Suggestions: foundry.heal.enabled off; the current load's audit not
    // complete; an audit superseding a finding as it is dismissed.
    healEnabled?: boolean;
    healRun?: "running" | "failed" | "incomplete" | null;
    healDismiss?: "superseded" | "fails";
    // The current load's complete audit didn't check its rows.
    healRowsMeasured?: boolean;
  }) =>
    fetch(`${MOCK}/__mock/config`, {
      method: "POST",
      body: JSON.stringify(config),
    }),
  /**
   * Sends pieces to an API source as an agent does through the Helix MCP
   * (the site never sends any): stored whole, or refused whole with the
   * backend's message. Its answer's status and body.
   */
  sendPieces: async (
    connectorId: string,
    send: {
      clientToken?: string;
      pieces: { id: string; occurredAt?: string; data: unknown }[];
    },
  ): Promise<{
    status: number;
    body: { message?: string; replayed?: boolean };
  }> => {
    const res = await fetch(
      `${MOCK}/__mock/event-sources/${encodeURIComponent(connectorId)}/pieces`,
      { method: "POST", body: JSON.stringify(send) },
    );
    return { status: res.status, body: await res.json() };
  },
};

export const test = base.extend<{ fresh: void }>({
  fresh: [
    // Playwright fixtures must destructure their first argument.
    // eslint-disable-next-line no-empty-pattern
    async ({}, use) => {
      await mock.reset();
      await use();
    },
    { auto: true },
  ],
});
export { expect };

/** Signs in from the sign-in page as `email`, waiting to leave sign-in. */
export async function signIn(
  page: Page,
  email = "ada@example.com",
  provider: "Google" | "GitHub" | "Microsoft" = "GitHub",
) {
  if (!new URL(page.url(), "http://x").pathname.startsWith("/login"))
    await page.goto("/login");
  await page.getByRole("button", { name: `Continue with ${provider}` }).click();
  await page.getByLabel("Email").fill(email);
  await page.getByRole("button", { name: "Continue", exact: true }).click();
  await page.waitForURL((url) => !url.pathname.startsWith("/login"));
}

/**
 * Signs in as `email`, a user with no workspace yet, and creates `name` from
 * the picker: a new workspace, so on Foundry Free, with no data and no
 * ontology. It opens on its onboarding, Get started (the sample, the user's
 * own data or their agent), which is all it offers besides Settings until it
 * has an ontology (createFirstOntology).
 */
export async function signInToNewWorkspace(
  page: Page,
  name = "Globex",
  email = "grace@example.com",
) {
  await signIn(page, email);
  await page.getByRole("button", { name: "Create workspace" }).click();
  const create = page.getByRole("dialog", { name: "Create a workspace" });
  await create.getByLabel("Workspace name").fill(name);
  await create.getByRole("button", { name: "Create workspace" }).click();
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(
    page.getByRole("button", { name: `Workspace: ${name}` }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { level: 1, name: "Get started" }),
  ).toBeVisible();
}

/** Opens /ontology/new and names the ontology: the database step shows. */
export async function startOntology(page: Page, name: string) {
  await page.goto("/ontology/new");
  await page.getByLabel("Ontology name").fill(name);
  await page.getByRole("button", { name: "Continue" }).click();
}

/**
 * Opens a new workspace's first ontology from its onboarding, with Use my own
 * data (the other side of Get started's toggle): the sources step shows, the
 * first of the only steps it asks (no name, no database).
 */
export async function startFirstOntology(page: Page) {
  await page.goto("/ontology/new");
  await page.getByRole("radio", { name: "Use my own data" }).click();
  await page.getByRole("button", { name: "Continue" }).click();
}

/**
 * Walks the wizard on from the database step, once one is chosen: the
 * sources step as it stands (every synced connector unless changed, no
 * document sets) and no goal, then Create and generate.
 */
export async function finishOntology(page: Page) {
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("button", { name: "Skip" }).click();
  await page.getByRole("button", { name: "Create and generate" }).click();
}

/**
 * Where a new ontology's wizard lands, its first generation's page: the
 * ontology's overview path.
 */
export async function createdOntology(page: Page): Promise<string> {
  await expect(page).toHaveURL(/\/ontology\/ont_\w+\/generations\/gen_/);
  return new URL(page.url()).pathname.replace(/\/generations\/.*$/, "");
}

/**
 * Creates a new workspace's first ontology from its onboarding, with Use my
 * own data: drafted from one uploaded document (into General), with nothing
 * connected, so named General, on a new database (which brings the
 * workspace its free Helix plan), and made current and loaded once drafted.
 * The workspace then offers every page. The ontology's overview path.
 */
export async function createFirstOntology(page: Page): Promise<string> {
  await startFirstOntology(page);
  const chooser = page.waitForEvent("filechooser");
  await page.getByRole("button", { name: "Upload documents" }).click();
  await (await chooser).setFiles([file("notes.md", "# Notes\n")]);
  await expect(uploadCard(page, "notes.md")).toContainText("Uploaded");
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("button", { name: "Skip" }).click();
  await page.getByRole("button", { name: "Create and generate" }).click();
  return createdOntology(page);
}

/** Makes Customer contracts' version 3 current and loads it: Stripe's customers and invoices. */
export async function loadContracts(page: Page) {
  await mock.configure({ loadStepMs: 100 });
  await page.goto("/ontology/ont_acme_contracts/versions/ver_seed_3");
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
  await page.goto("/ontology/ont_acme_contracts#loads");
  await page
    .getByRole("main")
    .getByRole("button", { name: "Load data" })
    .first()
    .click();
  await page
    .getByRole("dialog", { name: "Load version 3 into contracts?" })
    .getByRole("button", { name: "Load" })
    .click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 20_000 });
}

/**
 * A kind that signs in opens its form on Connect with <provider>: its key's
 * form is behind the kind's "Use … instead".
 */
export async function chooseKeyForm(form: Locator) {
  await form.getByRole("button", { name: /^Use (a|an) .+ instead$/ }).click();
}

/** A CSRF token for the signed-in page's session. */
export async function csrfToken(page: Page): Promise<string> {
  const res = await page.request.get("/papi/csrf");
  expect(res.status()).toBe(200);
  return (await res.json()).token;
}

export const sessionCookie = async (page: Page) =>
  (await page.context().cookies()).find((c) => c.name === "foundry-session");

/**
 * A DataGrid's columns fill its width, or sit at their minimum widths and
 * scroll when those don't fit: never wider, never short of the frame. No
 * cell clips a link or button in it, as a column too narrow for its
 * actions does.
 */
export async function expectGridFits(grid: Locator) {
  const { row, client, columns, clipped } = await grid.evaluate(
    (el: HTMLElement) => ({
      row: el.querySelector(".hf-grid-head")!.getBoundingClientRect().width,
      client: el.clientWidth,
      columns: [
        ...el.style
          .getPropertyValue("--hf-grid-template")
          .matchAll(/minmax\((\d+)px/g),
      ].reduce((n, m) => n + Number(m[1]), 0),
      clipped: [
        ...el.querySelectorAll<HTMLElement>(".hf-grid-td :is(a, button)"),
      ]
        .filter((c) => {
          const cell = c.closest(".hf-grid-td")!.getBoundingClientRect(),
            r = c.getBoundingClientRect();
          return (
            r.top < cell.top - 0.5 ||
            r.bottom > cell.bottom + 0.5 ||
            r.left < cell.left - 0.5 ||
            r.right > cell.right + 0.5
          );
        })
        .map((c) => c.textContent),
    }),
  );
  expect(columns).toBeGreaterThan(0);
  expect(row).toBeCloseTo(Math.max(client, columns), 0);
  expect(clipped).toEqual([]);
}

/**
 * Picks the option named `option` in a kit Select: opens its list from the
 * trigger (a combobox button, which shows the chosen label) and clicks it.
 */
export async function choose(select: Locator, option: string | RegExp) {
  await select.click();
  await select
    .page()
    .getByRole("listbox")
    .getByRole("option", { name: option, exact: true })
    .click();
}

/** A file for a file chooser: Markdown by its extension, else plain text. */
export const file = (name: string, text: string | Buffer) => ({
  name,
  mimeType: name.endsWith(".md") ? "text/markdown" : "text/plain",
  buffer: Buffer.isBuffer(text) ? text : Buffer.from(text, "utf8"),
});

/** The Data explorer, beside every Data page. */
export const explorer = (page: Page) =>
  page.getByRole("complementary", { name: "Connectors explorer" });
/** The Data explorer's tree. */
export const dataTree = (page: Page) =>
  explorer(page).getByRole("tree", { name: "Connectors" });

/**
 * The card of an upload, in the explorer's or the Data page's uploads
 * (each lists the uploads it started).
 */
export const uploadCard = (page: Page, name: string) =>
  page.getByRole("list", { name: "Uploads" }).getByRole("listitem", { name });

/**
 * Uploads files from the Data explorer: into `set` with its row's button,
 * else with Upload documents (the default set, General), and waits for
 * each to end. Opens /data unless a Data page is open.
 */
export async function uploadInData(
  page: Page,
  files: ReturnType<typeof file>[],
  set?: string,
) {
  if (!new URL(page.url(), "http://x").pathname.startsWith("/data"))
    await page.goto("/data");
  const chooser = page.waitForEvent("filechooser");
  await (
    set
      ? dataTree(page)
          .getByRole("treeitem", { name: set, exact: true })
          .getByRole("button", { name: `Upload into ${set}` })
      : explorer(page).getByRole("button", { name: "Upload documents" })
  ).click();
  await (await chooser).setFiles(files);
  for (const f of files)
    await expect(uploadCard(page, f.name)).toContainText(
      /Uploaded|Already uploaded/,
    );
}
