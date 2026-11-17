import { expect, test, type APIRequestContext } from "@playwright/test";

// Runs last, in the disposable stack: an agent's use case runs through MCP.
// A run that never calls the ontology isn't assessed (no model call, no
// spend) and counts apart on the Usage page; a run that reads it is
// assessed, on the stack's model when it has one, and counts as called.
// Do not capture bearer headers.
test.use({ trace: "off" });

const models =
  !!process.env.FOUNDRY_STACK_MODELS ||
  !!process.env.FOUNDRY_STACK_OLLAMA_MODEL;

// The MCP's tools, on the install's owner token.
function mcp(request: APIRequestContext, token: string) {
  const base = process.env.FOUNDRY_INTEGRATION_URL || "http://localhost:8081";
  return async (name: string, args: object) => {
    const response = await request.post(`${base}/mcp`, {
      headers: {
        Authorization: `Bearer ${token}`,
        Accept: "application/json, text/event-stream",
      },
      data: {
        jsonrpc: "2.0",
        id: 1,
        method: "tools/call",
        params: { name, arguments: args },
      },
    });
    expect(response.status()).toBe(200);
    const result = (await response.json()).result;
    expect(result.isError, JSON.stringify(result.content)).not.toBe(true);
    return result.structuredContent;
  };
}

test("counts a run that never called the ontology apart, and assesses one that read it", async ({
  request,
  page,
}) => {
  const token = process.env.FOUNDRY_STACK_OWNER_TOKEN;
  test.skip(
    !token,
    "run through tests/stack/run.sh for the disposable owner token",
  );
  test.setTimeout((models ? 20 : 5) * 60_000);
  const tool = mcp(request, token!);
  const workspace = (await tool("helix_list_workspaces", {})).workspaces[0];
  const ontologies = (
    await tool("helix_foundry_list_ontologies", { workspace_id: workspace.id })
  ).ontologies as { id: string; current_load?: { id: string } }[];
  // One with a load when the suite loaded one (with models), so a run can
  // read it; any other way.
  const ontology = ontologies.find((o) => o.current_load) ?? ontologies[0];
  expect(ontology).toBeTruthy();
  const ids = { workspace_id: workspace.id, ontology_id: ontology!.id };
  const useCase = await tool("helix_foundry_create_use_case", {
    ...ids,
    name: "Usage check",
    client_token: "stack-usage-check",
  });
  const runOf = async (input: string) =>
    (
      await tool("helix_foundry_start_use_case_run", {
        ...ids,
        use_case_id: useCase.id,
        input,
      })
    ).id as string;
  const finish = (runID: string, output: string) =>
    tool("helix_foundry_finish_use_case_run", {
      ...ids,
      use_case_id: useCase.id,
      run_id: runID,
      output,
    });
  const get = async (runID: string) =>
    (
      await tool("helix_foundry_get_use_case_run", {
        ...ids,
        use_case_id: useCase.id,
        run_id: runID,
      })
    ).run;

  // No call: finished unassessed, at no cost.
  const silent = await runOf("What is the weather like?");
  await finish(silent, "Sunny.");
  const silentRun = await get(silent);
  expect(silentRun.counts.accesses ?? 0).toBe(0);
  expect(silentRun.assessment).toBeUndefined();

  // One read: assessed, on the stack's model.
  let read = "";
  if (ontology!.current_load) {
    read = await runOf("Which records does the ontology hold?");
    const context = await tool("helix_foundry_get_context", ids);
    const type = /\[types\.([A-Za-z0-9_]+)\]/.exec(context.ontology_toml)?.[1];
    expect(type).toBeTruthy();
    await tool("helix_foundry_read_graph", {
      ...ids,
      reads: [{ entity: { type, id: "1" } }],
      use_case_run_id: read,
    });
    await finish(read, "It holds what the read returned.");
    const readRun = await get(read);
    expect(readRun.counts.accesses).toBe(1);
    expect(readRun.assessment?.status).toMatch(/^(queued|running|done)$/);
    if (models)
      await expect
        .poll(async () => (await get(read)).assessment?.status, {
          timeout: 15 * 60_000,
          intervals: [5_000],
        })
        .toMatch(/^(done|failed)$/);
  }
  // Still none, and nothing spent on it.
  expect((await get(silent)).assessment).toBeUndefined();

  // The Usage page counts it apart and says what to check.
  await page.goto(
    `/ontology/${ontology!.id}/usage/${useCase.id}?verdict=not_called`,
  );
  const filter = page.getByRole("radiogroup", { name: "Show runs" });
  await expect(
    filter.getByRole("radio", { name: /^Didn’t use the ontology/ }),
  ).toHaveAttribute("aria-checked", "true");
  await expect(
    filter.getByRole("radio", { name: /^Didn’t use the ontology\s*1$/ }),
  ).toBeVisible();
  await expect(page.getByText(/use_case_run_id/).first()).toBeVisible();
  const runs = page.getByRole("list", { name: "Runs", exact: true });
  await expect(runs.getByText("What is the weather like?")).toBeVisible();
  await expect(runs.getByText("No calls", { exact: true })).toBeVisible();
  await expect(
    page
      .getByRole("region", { name: "Numbers" })
      .getByText(/1 didn’t use the ontology/),
  ).toBeVisible();
  if (read)
    await expect(
      runs.getByText("Which records does the ontology hold?"),
    ).toHaveCount(0);
  await page.screenshot({
    path: test.info().outputPath("usage-not-called.png"),
    fullPage: true,
  });
});
