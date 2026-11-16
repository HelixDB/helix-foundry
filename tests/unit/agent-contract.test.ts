import { afterEach, describe, expect, it, vi } from "vitest";
import {
  AGENT_CONTRACT_VERSION,
  contractErrors,
} from "../../mock/agent-contract";
import agentBundle from "../../mock/fixtures/agent-bundle.json";
import {
  ENGINE_QUESTIONS,
  ZONE_QUESTIONS,
} from "../../mock/fixtures/generation-questions";
import { draftFromTables } from "../../mock/routes/ontology-agent";

// The ontology agent's Read and Ask JSON, held to the schemas vendored in
// contract/ontology_agent, and its Probe result, whose
// model failures and probe details Settings › AI models words. Here: the vendored version,
// and the reads the SPA sends. The mock's answers are checked where they
// are made (mock-rules, foundry-mock and questions-mock tests), and the
// fixtures the parsers' tests read where they are read (foundry-read and
// ask-model tests).

afterEach(() => vi.unstubAllGlobals());

describe("the vendored agent contract", () => {
  it("is 4.26.0, the engine whose queries compute from two props", () => {
    expect(AGENT_CONTRACT_VERSION).toBe("4.26.0 sha256:e3e5d59e0ff8ae10");
  });

  it("checks a value against a schema or one of its definitions", () => {
    expect(
      contractErrors("read_request#/$defs/ReadItem", {
        entity: { type: "Customer", id: "cus_1" },
      }),
    ).toEqual([]);
    expect(
      contractErrors("read_request#/$defs/ReadItem", {
        entity: { type: "Customer", id: "cus_1", name: "x" },
      }),
    ).not.toEqual([]);
    expect(() => contractErrors("ask_event#/$defs/Nope", {})).toThrow(
      /no schema/,
    );
  });
});

describe("the reads the SPA sends", () => {
  it("fit read_request.json's ReadItem", async () => {
    const sent: unknown[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        if (url === "/papi/csrf")
          return Response.json({ token: "t", expiresInSeconds: 600 });
        sent.push(...JSON.parse(JSON.parse(String(init?.body)).readsJson));
        return Response.json({
          resultJson: JSON.stringify({
            as_of: "2026-10-19",
            results: [{ error: { code: "unknown_entity", message: "none" } }],
            stats: { instances: 0, wall_ms: 1 },
          }),
        });
      }),
    );
    const { graphReadQueries } = await import("@/web/areas/explore/graph-read");
    const globex = { type: "Customer", key: "cus_GLOBEX01" };
    await graphReadQueries.entity("ws", "o", "load_1", globex).queryFn({});
    await graphReadQueries
      .entity("ws", "o", "load_1", { type: "Contract", name: "msa" })
      .queryFn({});
    await graphReadQueries
      .explain(
        "ws",
        "o",
        "load_1",
        { kind: "violation", name: "invoice_under_contract" },
        [{ type: "Invoice", id: "in_1" }],
      )
      .queryFn({});
    expect(sent).toHaveLength(3);
    for (const read of sent)
      expect(contractErrors("read_request#/$defs/ReadItem", read)).toEqual([]);
  });
});

describe("the questions the mock's ontology agent asks", () => {
  it("fit turn_result.json's Question: before a draft, in drafts, in the fixture", () => {
    const drafted = draftFromTables(
      [
        {
          connector: { id: "con_1", name: "Shop" },
          table: {
            name: "orders",
            columns: [
              { name: "number", type: "text" },
              { name: "status", type: "text" },
              { name: "customer_id", type: "text" },
            ],
            foreignKeys: [
              {
                column: "customer_id",
                referencedTable: "customers",
                referencedColumn: "id",
              },
            ],
          },
        },
        {
          connector: { id: "con_1", name: "Shop" },
          table: { name: "notes", columns: [{ name: "body", type: "text" }] },
        },
      ],
      "Orders",
    );
    const questions = [
      ...ENGINE_QUESTIONS,
      ...ZONE_QUESTIONS,
      ...(drafted.questions || []),
      ...agentBundle.questions,
    ];
    // A keyless table with columns to choose from, a key into a table the
    // draft didn't read, and a keyless table of one column (asked in words).
    expect(drafted.questions?.map((q) => q.options?.length)).toEqual([3, 2, 0]);
    expect(questions.length).toBeGreaterThan(ENGINE_QUESTIONS.length + 3);
    for (const q of questions)
      expect(contractErrors("turn_result#/$defs/Question", q)).toEqual([]);
  });
});
