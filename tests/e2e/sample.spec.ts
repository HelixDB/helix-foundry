import type { Page } from "@playwright/test";
import {
  createdOntology,
  expect,
  explorer,
  file,
  mock,
  signInToNewWorkspace,
  test,
  uploadCard,
  uploadInData,
} from "./support/mock";

// A new workspace's onboarding offers the sample company. This build is the
// dev stamp's, which has Acme Corp's database (next.config.ts), so the sample
// opens its own page: it connects the database over TLS, uploads the
// documents, shows the goal and the ontology made from its ready-made draft
// on a new Helix database, loads it, asks its first question and answers
// it, then offers its other questions and the user's own data. A reload
// carries on with what it made, and nothing is made or asked twice. Once the
// sample exists a banner says so, and links back to that page: to its last
// step, the user's own data, once its question has ended. Its data is its own
// ontology's alone: making or drafting one of the user's own never offers it.

/** A new workspace, on the plan the mock sells databases with, on the sample's page. */
async function openSample(page: Page) {
  await mock.configure({
    ontologyStepMs: 300,
    loadStepMs: 200,
    checkoutCardOnFile: true,
  });
  await signInToNewWorkspace(page);
  // The mock still sells databases with Pro; both stamps offer them on every
  // plan, Free included.
  await page.goto("/settings/billing");
  await page.getByRole("button", { name: "Upgrade", exact: true }).click();
  await page
    .getByRole("dialog", { name: "Upgrade Foundry" })
    .getByRole("button", { name: "Upgrade to Pro" })
    .click();
  await expect(
    page.getByRole("heading", { name: /Foundry Pro/ }),
  ).toContainText("Active", { timeout: 15_000 });
  await page.goto("/");
  await page
    .getByRole("button", { name: "See it on a sample company (1 min)" })
    .click();
  await expect(page).toHaveURL(/\/ontology\/sample$/);
  await expect(
    page.getByRole("heading", {
      level: 1,
      name: "Acme Corp, a sample company",
    }),
  ).toBeVisible();
}

test("walks a new workspace through the sample company, from its database to Ask", async ({
  page,
}) => {
  await openSample(page);
  const steps = page.getByRole("list", { name: "Steps" });
  await expect(steps).toContainText("6 tables, 1,121 rows", {
    timeout: 30_000,
  });
  await expect(steps).toContainText("Foundry models Acme Corp as 9 types", {
    timeout: 30_000,
  });
  await page.reload();
  // Once loaded, the tour asks the draft's first question itself and shows
  // how it ended, as Ask does (the mock can't answer this one); the others
  // are a click away, then the user's own data.
  const asked = steps.getByRole("article", { name: /^Question: / });
  await expect(asked).toBeVisible({ timeout: 60_000 });
  await expect(
    steps.getByRole("group", { name: "Suggested questions" }),
  ).toBeVisible({ timeout: 30_000 });
  await expect(
    page.getByRole("link", { name: "Connect your own data" }),
  ).toHaveAttribute("href", "/ontology/new");
  const others = steps.getByRole("group", { name: "Suggested questions" });
  await expect(others.getByRole("button").first()).toBeEnabled();
  // A reload shows the same question: nothing is asked twice.
  const first = (await asked.getAttribute("aria-label"))!;
  await page.reload();
  await expect(asked).toHaveCount(1, { timeout: 30_000 });
  await expect(asked).toHaveAttribute("aria-label", first);
  const next = (await others.getByRole("button").first().textContent())!;
  await others.getByRole("button", { name: next }).click();
  await expect(page).toHaveURL(/\/ask\/[^?]+\?thread=q_/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(next);

  // One Acme Corp database in Data, and one set of its documents.
  await page.goto("/data");
  await expect(explorer(page).getByText("Acme Corp sample")).toBeVisible();
  await expect(
    explorer(page).getByText("Acme Corp", { exact: true }),
  ).toHaveCount(1);
});

test("says it is sample data, and its link back to the tour shows every step done and makes nothing", async ({
  page,
}) => {
  test.slow();
  await openSample(page);
  const banner = page.getByRole("region", { name: "Sample data" }),
    back = banner.getByRole("link", { name: "Get started with your own data" }),
    steps = page.getByRole("list", { name: "Steps" }),
    others = steps.getByRole("group", { name: "Suggested questions" });
  // Nothing to say until the sample exists.
  await expect(banner).toHaveCount(0);
  await expect(others).toBeVisible({ timeout: 60_000 });
  // On the tour itself it has nowhere to go back to.
  await expect(banner).toHaveText(
    "Sample data: you’re exploring Acme Corp, a made-up company.",
  );
  await expect(back).toHaveCount(0);
  // Nor is Getting into production, which is the Analyst's.
  const production = page.getByRole("region", {
    name: "Getting into production",
  });
  await expect(production).toHaveCount(0);

  // From here on, nothing is written: reads over POST aside, no call at all.
  const writes: string[] = [];
  page.on("request", (request) => {
    const path = new URL(request.url()).pathname;
    if (
      request.method() !== "GET" &&
      path.startsWith("/papi/") &&
      !/:(query-)?read$/.test(path)
    )
      writes.push(`${request.method()} ${path}`);
  });
  const asked = steps.getByRole("article", { name: /^Question: / });
  const everyStepDone = async () => {
    await expect(others).toBeVisible({ timeout: 30_000 });
    await expect(steps.getByText("(done)")).toHaveCount(5);
    await expect(steps.locator('[aria-current="step"]')).toContainText(
      "Now connect your own data",
    );
    await expect(steps).toContainText("6 tables, 1,121 rows");
    await expect(steps).toContainText("Foundry models Acme Corp as 9 types");
    await expect(asked).toHaveCount(1);
  };

  // The load's page, where the tour's link leaves the reader, has the way back.
  await steps.getByRole("link", { name: "See the load" }).click();
  await expect(page).toHaveURL(/\/ontology\/[^/]+\/loads\/[^/]+$/);
  await back.click();
  await expect(page).toHaveURL(/\/ontology\/sample#own-data$/);
  await everyStepDone();
  await expect(back).toHaveCount(0);
  await expect(
    page.getByRole("link", { name: "Connect your own data" }),
  ).toBeInViewport();

  // Opened afresh, with nothing in the cache.
  await page.reload();
  await everyStepDone();

  // The only ontology: the banner is on every page, and there is one of
  // everything.
  await page.goto("/data");
  await expect(explorer(page).getByText("Acme Corp sample")).toBeVisible();
  await expect(
    explorer(page).getByText("Acme Corp", { exact: true }),
  ).toHaveCount(1);
  // The sample isn't the workspace's own ontology: creating one comes first.
  await page.goto("/ask");
  await expect(page).toHaveURL(/\/ask\/[^/]+$/);
  await expect(production).toContainText("0 of 2");
  await expect(
    production.getByRole("link", { name: "New ontology" }),
  ).toHaveAttribute("href", "/ontology/new");
  // The prompt waits for an ontology of the workspace's own.
  await expect(
    production.getByRole("button", { name: "Copy prompt" }),
  ).toBeDisabled();
  // Making one of the workspace's own is no longer the sample.
  await page.goto("/ontology/new");
  await expect(
    page.getByRole("heading", { level: 1, name: "New ontology" }),
  ).toBeVisible();
  await expect(banner).toHaveCount(0);
  await page.goto("/data");
  await back.click();
  await expect(page).toHaveURL(/\/ontology\/sample#own-data$/);
  await everyStepDone();
  expect(writes).toEqual([]);

  await page.goto("/ontology");
  await expect(
    page
      .getByRole("complementary", { name: "Ontologies" })
      .getByRole("link", { name: /Acme Corp customers/ }),
  ).toHaveCount(1);
});

test("links back to the tour until its question has ended, then to the user's own data", async ({
  page,
}) => {
  test.slow();
  await openSample(page);
  await mock.configure({ askStepMs: 1500 });
  await expect(
    page
      .getByRole("list", { name: "Steps" })
      .getByRole("article", { name: /^Question: / }),
  ).toBeVisible({ timeout: 60_000 });
  await page.goto("/data");
  const banner = page.getByRole("region", { name: "Sample data" });
  await expect(
    banner.getByRole("link", { name: "Back to getting started" }),
  ).toHaveAttribute("href", "/ontology/sample");

  const own = banner.getByRole("link", {
    name: "Get started with your own data",
  });
  await expect(own).toHaveAttribute("href", "/ontology/sample#own-data", {
    timeout: 60_000,
  });
  await own.click();
  const connect = page.getByRole("link", { name: "Connect your own data" });
  await expect(connect).toBeInViewport({ timeout: 30_000 });
  await page.reload();
  await expect(connect).toBeInViewport({ timeout: 30_000 });
});

test("stays on the tour's last step however long the steps above take to read, until the reader moves", async ({
  page,
}) => {
  test.slow();
  await openSample(page);
  await expect(
    page.getByRole("group", { name: "Suggested questions" }),
  ).toBeVisible({ timeout: 60_000 });
  // As a browser without scroll anchoring (Safari) has it, which keeps nothing
  // in view by itself.
  await page.addInitScript(() =>
    document.addEventListener("DOMContentLoaded", () => {
      const style = document.createElement("style");
      style.textContent = "* { overflow-anchor: none }";
      document.head.append(style);
    }),
  );
  await page.goto("/data");
  await page
    .getByRole("region", { name: "Sample data" })
    .getByRole("link", { name: "Get started with your own data" })
    .click({ timeout: 30_000 });
  const connect = page.getByRole("link", { name: "Connect your own data" });
  await expect(connect).toBeInViewport({ timeout: 30_000 });

  // What sits above it arrives when it arrives: a real answer's charts and
  // names are read once they are near the view.
  const arrive = () =>
    page.evaluate(() => {
      const late = document.createElement("div");
      late.style.height = "800px";
      document.getElementById("own-data")!.before(late);
    });
  await page.waitForTimeout(6_000);
  await arrive();
  await expect(connect).toBeInViewport();

  // A resting finger on a trackpad isn't the reader moving.
  await page.mouse.wheel(0, -1);
  await arrive();
  await expect(connect).toBeInViewport();

  // Scrolling up is, and what arrives then no longer moves the page.
  await page.mouse.wheel(0, -300);
  await arrive();
  await expect(connect).not.toBeInViewport();
});

test("keeps the sample's data out of an ontology of the user's own", async ({
  page,
  context,
}) => {
  test.slow();
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const clipboard = () => page.evaluate(() => navigator.clipboard.readText());
  await openSample(page);
  await expect(
    page.getByRole("group", { name: "Suggested questions" }),
  ).toBeVisible({ timeout: 60_000 });

  // The tour's way on to the user's own data: an agent told to set one up
  // hears nothing of the sample's.
  await page.getByRole("button", { name: "Set up with your agent" }).click();
  await expect.poll(clipboard).toContain("# Set up Helix Foundry with me");
  expect(await clipboard()).not.toContain("Acme Corp");

  // The sample's own ontology keeps them, chosen as its version reads them, so
  // it can be drafted again.
  await page.goto("/ontology");
  await expect(page).toHaveURL(/\/ontology\/ont_\w+$/);
  const sample = new URL(page.url()).pathname,
    ws: string = (await (await page.request.get("/papi/v1/workspaces")).json())
      .workspaces[0].id,
    read = async (list: string) =>
      (
        await page.request.get(`/papi/v1/workspaces/${ws}/foundry/${list}`)
      ).json(),
    connectorId: string = (await read("connectors")).connectors[0].id,
    setId: string = (await read("document-sets")).sets[0].id;
  await page.goto(`${sample}/generate`);
  const connectors = page.getByRole("list", { name: "Connectors" }),
    sets = page.getByRole("list", { name: "Document sets" });
  await expect(
    connectors.getByRole("checkbox", { name: /Acme Corp/ }),
  ).toBeChecked();
  await expect(
    sets.getByRole("checkbox", { name: /Acme Corp sample/ }),
  ).toBeChecked();

  // Making one of the user's own offers neither, with what it would have
  // chosen left to the pickers' empty states.
  await page.goto("/ontology/new");
  const noConnectors = page.getByRole("heading", { name: "No connectors yet" }),
    noDocuments = page.getByRole("heading", { name: "No documents yet" }),
    steps = page.getByRole("list", { name: "Steps" }),
    onward = page.getByRole("button", { name: "Continue" });
  await expect(noConnectors).toBeVisible();
  await expect(noDocuments).toBeVisible();
  await expect(steps).not.toContainText("Acme Corp");
  await expect(steps).toContainText(
    "Connect your data or upload documents to continue.",
  );
  await expect(onward).toBeDisabled();

  // Nor do choices this tab kept that name them.
  await page.evaluate(
    ([connector, set]) => {
      const key = Object.keys(sessionStorage).find((k) =>
        k.startsWith("foundry.new-ontology.v1:"),
      )!;
      sessionStorage.setItem(
        key,
        JSON.stringify({
          ...JSON.parse(sessionStorage.getItem(key)!),
          connectorIds: [connector],
          setIds: [set],
        }),
      );
    },
    [connectorId, setId],
  );
  await page.reload();
  await expect(noConnectors).toBeVisible();
  await expect(noDocuments).toBeVisible();
  await expect(onward).toBeDisabled();

  // An upload makes General rather than joining the sample's set, which is
  // then all that is chosen.
  const chooser = page.waitForEvent("filechooser");
  await page.getByRole("button", { name: "Upload documents" }).click();
  await (await chooser).setFiles([file("ours.md", "# Ours\n\nOur own.\n")]);
  await expect(uploadCard(page, "ours.md")).toContainText(
    "Uploaded into General",
  );
  await expect(sets.getByRole("checkbox", { name: /General/ })).toBeChecked();
  await expect(sets).not.toContainText("Acme Corp");

  // Its goal's prompt tells of what is chosen and offered, not the sample's.
  await onward.click();
  await expect(steps).toContainText("General: 1 document");
  await page
    .getByRole("button", { name: "Copy prompt for your agent" })
    .click();
  await expect.poll(clipboard).toContain("General");
  expect(await clipboard()).not.toContain("Acme Corp");

  // Nothing of the sample's goes out: no connector, never an empty list.
  await page.getByRole("button", { name: "Skip" }).click();
  const generating = page.waitForRequest((r) => r.url().endsWith(":generate"));
  await page.getByRole("button", { name: "Create and generate" }).click();
  const asked = (await generating).postDataJSON();
  expect(asked.noConnectors).toBe(true);
  expect(asked.connectorIds).toBeUndefined();
  expect(asked.documentSetIds).toHaveLength(1);
  expect(asked.documentSetIds).not.toContain(setId);
  const own = await createdOntology(page);

  // Drafting it again, a link that names them is no way in either.
  await page.goto(`${own}/generate?connectors=${connectorId}&sets=${setId}`);
  await expect(noConnectors).toBeVisible();
  await expect(sets.getByRole("checkbox", { name: /General/ })).toBeVisible();
  await expect(
    sets.getByRole("checkbox", { name: /General/ }),
  ).not.toBeChecked();
  await expect(sets).not.toContainText("Acme Corp");
  await expect(
    page.getByRole("button", { name: "Generate", exact: true }),
  ).toBeDisabled();

  // Data still lists them as any other, and the sample's set still takes an
  // upload sent to it from its own row.
  await page.goto("/data");
  await expect(
    explorer(page).getByText("Acme Corp", { exact: true }),
  ).toHaveCount(1);
  await uploadInData(page, [file("extra.md", "# Extra\n")], "Acme Corp sample");
  await expect(uploadCard(page, "extra.md")).toContainText(
    "Uploaded into Acme Corp sample",
  );
});
