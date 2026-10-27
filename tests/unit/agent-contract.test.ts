import { afterEach, describe, expect, it, vi } from "vitest";
import {
  AGENT_CONTRACT_VERSION,
  contractErrors,
} from "../../mock/agent-contract";

// The ontology agent's Read and Ask JSON, held to the schemas vendored in
// contract/ontology_agent, and its Probe result, whose
// model failures and probe details Settings › AI models words. Here: the vendored version,
// and the reads the SPA sends. The mock's answers are checked where they
// are made (mock-rules, foundry-mock and questions-mock tests), and the
// fixtures the parsers' tests read where they are read (foundry-read and
// ask-model tests).

afterEach(() => vi.unstubAllGlobals());

describe("the vendored agent contract", () => {
  it("is 4.1.1, the engine that keeps history", () => {
    expect(AGENT_CONTRACT_VERSION).toBe("4.1.1 sha256:96aafc72daf47574");
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
