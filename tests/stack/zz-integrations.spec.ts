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
  // This new ontology has no load yet, regardless of whether the stack has
  // models: the sample below loads it only with them.
  expect(ask.status()).toBe(412);
  expect((await ask.json()).error.code).toBe("failed_precondition");

  // The sample company through the same tools, as an agent builds an
  // ontology of its own: foundry connects the install's acme-db, syncs it and
  // uploads the documents to the store itself.
  const ids = { workspace_id: workspace.id, ontology_id: ontologyID };
  const run = async (op: object) => {
    const confirmed = await tool("helix_prepare_foundry_operation", op);
    return (
      await tool("helix_execute_foundry_operation", {
        ...op,
        confirmation_id: confirmed.confirmation_id,
        confirmation_token: confirmed.confirmation_token,
      })
    ).result;
  };
  const target = `ontology:${workspace.id}/${ontologyID}`;
  const sample = { operation: "add_sample_data", target, payload: {} };
  const synced = async (connectorID: string, syncID: string) =>
    expect
      .poll(
        async () =>
          (
            await tool("helix_foundry_get_sync", {
              workspace_id: workspace.id,
              connector_id: connectorID,
              sync_id: syncID,
              wait: true,
            })
          ).sync.status,
        { timeout: 180_000, intervals: [0] },
      )
      .toBe("succeeded");
  if (!process.env.FOUNDRY_STACK_MODELS) {
    // With no model provider an upload can't complete, so it stops there:
    // after connecting acme-db and sending the first document to the store.
    const confirmed = await tool("helix_prepare_foundry_operation", sample);
    const refused = await rpc("tools/call", {
      name: "helix_execute_foundry_operation",
      arguments: {
        ...sample,
        confirmation_id: confirmed.confirmation_id,
        confirmation_token: confirmed.confirmation_token,
      },
    });
    expect(refused.isError).toBe(true);
    expect(JSON.stringify(refused.content)).toContain("completing ");
    expect(JSON.stringify(refused.content)).toContain("No AI provider");
    const connector = (
      await tool("helix_foundry_list_connectors", {
        workspace_id: workspace.id,
      })
    ).connectors.find((c: { name: string }) => c.name === "Acme Corp");
    await synced(connector.id, connector.last_sync.id);
  }
  // With a model provider, all of it, then its ready-made draft loads and
  // answers.
  if (process.env.FOUNDRY_STACK_MODELS) {
    const added = await run(sample);
    expect(added.documents).toHaveLength(9);
    for (const doc of added.documents) {
      expect(doc.status).toBe("uploaded");
      expect(doc.set_id).toBe(added.document_set_id);
    }
    await synced(added.connector_id, added.sync_id);
    // Again, it makes nothing twice.
    const again = await run(sample);
    expect(again.connector_id).toBe(added.connector_id);
    expect(again.document_set_id).toBe(added.document_set_id);
    expect(again.documents.map((d: { id: string }) => d.id).sort()).toEqual(
      added.documents.map((d: { id: string }) => d.id).sort(),
    );
    const generation = await run({
      operation: "generate_ontology",
      target,
      payload: {
        sample: "acme",
        connector_ids: [added.connector_id],
        document_set_ids: [added.document_set_id],
      },
    });
    let versionID = "";
    await expect
      .poll(
        async () => {
          const g = (
            await tool("helix_foundry_get_generation", {
              ...ids,
              generation_id: generation.id,
              wait: true,
            })
          ).generation;
          versionID = g.result_version_id ?? "";
          return g.status;
        },
        { timeout: 120_000, intervals: [0] },
      )
      .toBe("succeeded");
    await run({
      operation: "set_current_version",
      target,
      payload: { version_id: versionID, expected_current_version_id: "" },
    });
    const load = await run({
      operation: "create_load",
      target,
      payload: { version_id: versionID },
    });
    await expect
      .poll(
        async () =>
          (
            await tool("helix_foundry_get_load", {
              ...ids,
              load_id: load.id,
              wait: true,
            })
          ).load.status,
        { timeout: 900_000, intervals: [0] },
      )
      .toMatch(/^(ready|partial)$/);
    const context = await tool("helix_foundry_get_context", ids);
    expect(context.ontology_toml).toContain("[types.Account]");
    expect(context.example_questions.length).toBeGreaterThan(0);
    let question = await tool("helix_foundry_ask", {
      ...ids,
      question: context.example_questions[0],
      client_token: "stack-sample-question",
    });
    await expect
      .poll(
        async () => {
          if (!question.done)
            question = await tool("helix_foundry_get_question", {
              ...ids,
              question_id: question.id,
            });
          return question.status;
        },
        { timeout: 300_000, intervals: [0] },
      )
      .toBe("answered");
  }
});
