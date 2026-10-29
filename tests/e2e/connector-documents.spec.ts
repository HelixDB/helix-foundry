import type { Page } from "@playwright/test";
import {
  expect,
  file,
  signIn,
  signInToNewWorkspace,
  startFirstOntology,
  test,
  uploadCard,
} from "./support/mock";

// A connection's own documents across the site: its sets on its page (the
// Transcripts setting turns one on and off), one of its documents' page,
// its sets in Generate's picker, grouped under it, and uploads in a
// workspace whose only sets are a connection's. The mock's Granola reads
// 418 notes a sync, newest first, the third edited before each one.

const WS = "/papi/v1/workspaces/ws_acme/foundry";

/**
 * Connects Granola from the Data page's form, with its transcripts or not,
 * and waits on the connection's page, where the form lands, for its first
 * sync to write its notes.
 */
async function connectGranola(page: Page, transcripts = false) {
  await page.goto("/data?add=granola");
  const form = page.getByRole("dialog", { name: "Granola" });
  await form.getByLabel("API key").fill("grn_e2eTeamGranolaKey");
  if (transcripts)
    await form.getByRole("checkbox", { name: /^Transcripts/ }).check();
  await form.getByRole("button", { name: "Test connection" }).click();
  await form.getByRole("button", { name: "Connect and sync" }).click();
  await expect(page).toHaveURL(/\/data\/connections\/[^/?]+$/);
  await expect(set(page, "Notes")).toContainText("418 documents", {
    timeout: 15_000,
  });
  return new URL(page.url()).pathname;
}
/** One of the connection page's sets. */
const set = (page: Page, name: string) =>
  page
    .getByRole("region", { name: "Documents" })
    .getByRole("listitem", { name });
/** A row of a page's Details. */
const detail = (page: Page, label: string) =>
  page
    .locator(".hf-kv-row")
    .filter({ has: page.locator("dt", { hasText: new RegExp(`^${label}$`) }) })
    .locator("dd");
/** The connection's Notes set, and its newest documents, from the WFE. */
async function notes(page: Page) {
  const sets: { id: string; name: string; connector?: object }[] = (
    await (await page.request.get(`${WS}/document-sets`)).json()
  ).sets;
  const notes = sets.find((s) => s.connector && s.name === "Notes")!;
  const documents: {
    id: string;
    name: string;
    source: { externalId: string };
  }[] = (
    await (await page.request.get(`${WS}/documents?setId=${notes.id}`)).json()
  ).documents;
  return { id: notes.id, documents };
}

test("lists a connection's sets on its page and opens one of its notes", async ({
  page,
}) => {
  await signIn(page);
  const connection = await connectGranola(page);
  // Notes only, while transcripts are off.
  await expect(
    page.getByRole("region", { name: "Documents" }).getByRole("listitem"),
  ).toHaveCount(1);
  await expect(set(page, "Notes")).toContainText(
    "No ontology reads Notes yet.",
  );
  await expect(set(page, "Notes")).toContainText("Not read");
  await expect(detail(page, "Documents")).toHaveText("Notes");

  // A note's page says where it comes from, opens at the source, and has
  // nothing that changes it.
  const { documents } = await notes(page);
  expect(documents.map((d) => d.name).slice(0, 3)).toEqual([
    "Weekly pipeline review",
    "Globex renewal call",
    "Pricing review (edited)",
  ]);
  const [, globex, edited] = documents;
  await page.goto(`/data/documents/${globex!.id}`);
  await expect(
    page.getByRole("heading", { level: 1, name: "Globex renewal call" }),
  ).toBeVisible();
  const meta = page.locator(".hf-page-header-meta");
  await expect(meta).toContainText("From Granola · Notes");
  await expect(meta.getByRole("link", { name: "Granola" })).toHaveAttribute(
    "href",
    connection,
  );
  // Its time at the source: the meeting's, Oct 19.
  await expect(meta).toContainText("2026");
  const open = page.getByRole("link", { name: "Open in Granola" });
  await expect(open).toHaveAttribute(
    "href",
    `https://notes.granola.ai/d/${globex!.source.externalId}`,
  );
  await expect(open).toHaveAttribute("target", "_blank");
  await expect(open).toHaveAttribute("rel", "noopener noreferrer");
  for (const name of ["Move", "Delete"])
    await expect(
      page.getByRole("main").getByRole("button", { name, exact: true }),
    ).toHaveCount(0);
  await expect(detail(page, "Source ID")).toHaveText(globex!.source.externalId);
  await expect(detail(page, "Connection")).toHaveText("Granola");
  // Its text, as Markdown.
  await expect(
    page.getByRole("heading", { name: "Summary", exact: true }),
  ).toBeVisible();
  // The breadcrumb goes back through its connection.
  await expect(
    page
      .getByRole("navigation", { name: "Breadcrumb" })
      .getByRole("link", { name: "Granola" }),
  ).toHaveAttribute("href", connection);

  // A sync after the edited note changed at the source replaces it.
  await page.goto(connection);
  await page.getByRole("button", { name: "Sync now" }).click();
  await expect(page.getByRole("button", { name: "Syncing…" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Sync now" })).toBeVisible({
    timeout: 15_000,
  });
  await page.goto(`/data/documents/${edited!.id}`);
  await expect(
    page.getByText("Replaced or removed at the source", { exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("link", { name: "Open in Granola" })).toHaveCount(
    0,
  );
  // Its replacement is the set's third newest, with a new id.
  const after = (await notes(page)).documents[2]!;
  expect(after.name).toBe("Pricing review (edited)");
  expect(after.id).not.toBe(edited!.id);
});

test("turns a Granola connection's transcripts on and off", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data?add=granola");
  // Off to start with, and the form says what they cost.
  const form = page.getByRole("dialog", { name: "Granola" });
  await expect(
    form.getByRole("checkbox", { name: /^Transcripts/ }),
  ).not.toBeChecked();
  await expect(form).toContainText(
    "Transcripts are long: reading one into an ontology costs several times what a note does.",
  );
  await form.getByRole("button", { name: "Close" }).click();
  await connectGranola(page);
  await expect(set(page, "Transcripts")).toHaveCount(0);

  const edit = async (on: boolean) => {
    await page.getByRole("button", { name: "Connection actions" }).click();
    await page.getByRole("menuitem", { name: "Edit connection" }).click();
    const dialog = page.getByRole("dialog", { name: "Edit Granola" });
    const box = dialog.getByRole("checkbox", { name: /^Transcripts/ });
    // The form starts from the stored setting.
    await expect(box).toBeChecked({ checked: !on });
    await box.setChecked(on);
    await dialog.getByRole("button", { name: "Save changes" }).click();
    await expect(dialog).toBeHidden();
  };
  // On: the sync the change starts writes each note's transcript into a
  // set of its own, after Notes.
  await edit(true);
  await expect(set(page, "Transcripts")).toContainText("418 documents", {
    timeout: 15_000,
  });
  await expect(
    page
      .getByRole("region", { name: "Documents" })
      .locator(".dt-set-text strong"),
  ).toHaveText(["Notes", "Transcripts"]);
  await expect(detail(page, "Documents")).toHaveText("Notes and transcripts");
  // Off: the set stays, empty.
  await edit(false);
  await expect(set(page, "Transcripts")).toContainText("0 documents", {
    timeout: 15_000,
  });
  await expect(set(page, "Notes")).toContainText("418 documents");
  await expect(detail(page, "Documents")).toHaveText("Notes");
});

test("picks a connection's sets in Generate, grouped under it", async ({
  page,
}) => {
  await signIn(page);
  await connectGranola(page, true);
  await expect(set(page, "Transcripts")).toContainText("418 documents");
  const { id: notesId } = await notes(page);

  await page.goto("/ontology/ont_acme_contracts/generate");
  const sets = page.getByRole("list", { name: "Document sets", exact: true }),
    granola = sets.getByRole("checkbox", { name: /^Granola/ }),
    own = page.getByRole("list", { name: "Granola sets" });
  // People's sets first, then the connection's, under it.
  await expect(sets.locator(".on-connector-text strong")).toHaveText([
    "General",
    "Granola",
    "Notes",
    "Transcripts",
  ]);
  await expect(sets).toContainText(
    "its syncs write these sets; extraction draws on the month’s budget",
  );
  await expect(own.getByRole("listitem").first()).toContainText(
    "418 documents; the agent reads the 5 newest",
  );
  await expect(granola).not.toBeChecked();
  // One of the two: partly chosen. The connection's choice takes both,
  // then neither.
  const notesBox = own.getByRole("checkbox", { name: /^Notes/ });
  await notesBox.check();
  await expect(granola).toBeChecked({ indeterminate: true });
  await granola.click();
  await expect(
    own.getByRole("checkbox", { name: /^Transcripts/ }),
  ).toBeChecked();
  await expect(granola).toBeChecked();
  await granola.click();
  await expect(granola).not.toBeChecked();
  await expect(notesBox).not.toBeChecked();

  // Notes alone, without a connector, is something to generate from.
  await notesBox.check();
  const connectors = page.getByRole("list", {
    name: "Connectors",
    exact: true,
  });
  for (const box of await connectors.getByRole("checkbox").all())
    await box.uncheck();
  await expect(
    page.getByText("Granola / Notes: 418 documents.", { exact: true }),
  ).toBeVisible();
  const sent = page.waitForRequest(/ont_acme_contracts:generate$/);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  expect((await sent).postDataJSON()).toMatchObject({
    noConnectors: true,
    documentSetIds: [notesId],
  });
});

test("uploads into a new General where a connection's sets are the only ones", async ({
  page,
}) => {
  // A new workspace is onboarding: Use my own data opens the sources,
  // which connect data and upload documents in place.
  await signInToNewWorkspace(page);
  await startFirstOntology(page);

  // Granola, connected from the sources step: its first sync writes its
  // Notes, which the picker lists under it.
  await page.getByRole("button", { name: "Add data" }).first().click();
  await page.getByRole("button", { name: "Granola", exact: true }).click();
  const form = page.getByRole("dialog", { name: "Granola" });
  await form.getByLabel("API key").fill("grn_e2eTeamGranolaKey");
  await form.getByRole("button", { name: "Test connection" }).click();
  await form.getByRole("button", { name: "Connect and sync" }).click();
  const sets = page.getByRole("list", { name: "Document sets", exact: true });
  await expect(sets.locator(".on-connector-text strong")).toHaveText(
    ["Granola", "Notes"],
    { timeout: 15_000 },
  );

  // Its sets take no upload: the first one makes General, which is chosen.
  const chooser = page.waitForEvent("filechooser");
  await page.getByRole("button", { name: "Upload documents" }).click();
  await (await chooser).setFiles(file("minutes.md", "# Minutes\n\nAgreed.\n"));
  await expect(uploadCard(page, "minutes.md")).toContainText("General", {
    timeout: 15_000,
  });
  await expect(sets.locator(".on-connector-text strong")).toHaveText([
    "General",
    "Granola",
    "Notes",
  ]);
  await expect(sets.getByRole("checkbox", { name: /^General/ })).toBeChecked();
  await expect(page.getByRole("list", { name: "Granola sets" })).toContainText(
    "418 documents",
  );
  await expect(
    page.getByRole("button", { name: "Upload into Notes" }),
  ).toHaveCount(0);
});
