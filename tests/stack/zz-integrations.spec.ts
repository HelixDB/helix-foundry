import { expect, test } from "@playwright/test";

// Runs after the UI flow, in the disposable stack. Do not capture bearer headers.
test.use({ trace: "off" });

test("local agents discover, create and read through MCP and reach HTTP Ask", async ({
  request,
}) => {
  const token = process.env.FOUNDRY_STACK_OWNER_TOKEN;
  test.skip(
    !token,
    "run through tests/stack/run.sh for the disposable owner token",
  );
  const base = process.env.FOUNDRY_INTEGRATION_URL || "http://localhost:8081";
  const headers = { Authorization: `Bearer ${token}` };
  expect((await request.get(`${base}/mcp`)).status()).toBe(401);
  expect(
    (
      await request.get(`${base}/v1/ontologies/missing/questions/missing`)
    ).status(),
  ).toBe(401);
  // Published on the host, this listener serves no probes.
  for (const path of ["/healthz", "/readyz", "/metrics"])
    expect((await request.get(`${base}${path}`)).status()).toBe(404);

  const rpc = async (method: string, params: object) => {
    const response = await request.post(`${base}/mcp`, {
      headers: { ...headers, Accept: "application/json, text/event-stream" },
      data: { jsonrpc: "2.0", id: 1, method, params },
    });
    expect(response.status()).toBe(200);
    const body = await response.json();
    expect(body.error).toBeUndefined();
    return body.result;
  };
  const tool = async (name: string, args: object) => {
    const result = await rpc("tools/call", { name, arguments: args });
    expect(result.isError, JSON.stringify(result.content)).not.toBe(true);
    // A client that reads only content gets the same data.
    expect(JSON.parse(result.content.at(-1).text)).toEqual(
      result.structuredContent,
    );
    return result.structuredContent;
  };

  await rpc("initialize", {
    protocolVersion: "2025-03-26",
    capabilities: {},
    clientInfo: { name: "stack-test", version: "1" },
  });
  const names = (await rpc("tools/list", {})).tools.map(
    (t: { name: string }) => t.name,
  );
  expect(names).toContain("helix_foundry_ask");
  expect(names).toContain("helix_prepare_foundry_operation");
  expect(names).not.toContain("helix_foundry_send_pieces");
  const workspace = (await tool("helix_list_workspaces", {})).workspaces[0];
  expect(workspace.id).toBeTruthy();
  const kinds = await tool("helix_foundry_list_connector_kinds", {
    workspace_id: workspace.id,
  });
  expect(kinds.kinds.some((k: { kind: string }) => k.kind === "events")).toBe(
    false,
  );
  // connector_link's next step, and the only tool that names a new connector.
  expect(
    (
      await tool("helix_foundry_list_connectors", {
        workspace_id: workspace.id,
      })
    ).connectors,
  ).toBeInstanceOf(Array);

  const operation = {
    operation: "create_ontology",
    target: `workspace:${workspace.id}`,
    payload: {
      name: "Agent integration test",
      database: {
        name: "Agent test",
        slug: "agent-integration-test",
        project_name: "Agent test",
      },
    },
  };
  const confirmation = await tool("helix_prepare_foundry_operation", operation);
  const execution = {
    ...operation,
    confirmation_id: confirmation.confirmation_id,
    confirmation_token: confirmation.confirmation_token,
  };
  const created = await tool("helix_execute_foundry_operation", execution);
  const ontologyID = created.result.id;
  expect(ontologyID).toBeTruthy();
  expect(
    (
      await rpc("tools/call", {
        name: "helix_execute_foundry_operation",
        arguments: execution,
      })
    ).isError,
  ).toBe(true);
  await tool("helix_foundry_get_ontology", {
    workspace_id: workspace.id,
    ontology_id: ontologyID,
  });

  const ask = await request.post(`${base}/v1/ontologies/${ontologyID}/ask`, {
    headers: { ...headers, "Idempotency-Key": "stack-integration-question" },
    data: { question: "What data is loaded?" },
  });
  // This new ontology has no load, regardless of whether the stack has models.
  expect(ask.status()).toBe(412);
  expect((await ask.json()).error.code).toBe("failed_precondition");
});
