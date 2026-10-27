import { describe, expect, it } from "vitest";
import agentBundle from "../../mock/fixtures/agent-bundle.json";
import contractsBundle from "../../mock/fixtures/agent-bundle-contracts.json";
import foundryDefinition from "../../mock/fixtures/foundry-definition.json";
import { normName } from "../../mock/graph/extract";
import {
  MAX_NAMING_KEYS,
  NO_NAMING,
  isInternal,
  namingKeys,
  namingOf,
  normalizeName,
  readable,
  shortId,
  type Naming,
} from "@/web/areas/explore/naming";

// The readable-name rules: by the current version's types, names first,
// then the key, then the stored name, then a short id; labels the version
// doesn't know keep their raw label.

const contracts = namingOf({
  id: "ver_3",
  format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
  definition: contractsBundle,
});
const shop = namingOf({
  id: "ver_2",
  format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
  definition: agentBundle,
});
const v1 = namingOf({
  id: "ver_1",
  format: "ONTOLOGY_FORMAT_FOUNDRY_V1",
  definition: foundryDefinition,
});

describe("namingOf", () => {
  it("reads an agent draft's types, keys, names and links", () => {
    expect(contracts.version).toBe("ver_3");
    expect([...contracts.types.keys()]).toEqual([
      "Contract",
      "Customer",
      "Invoice",
    ]);
    expect(contracts.types.get("Customer")).toMatchObject({
      key: ["customer_id"],
      names: ["company_name"],
    });
    expect(contracts.types.get("Customer")!.props.map((p) => p.name)).toEqual([
      "company_name",
      "currency",
      "customer_id",
      "email",
    ]);
    expect(contracts.types.get("Contract")).toMatchObject({
      key: [],
      names: [],
      props: [
        { name: "signed_on", type: "date" },
        { name: "term_months", type: "integer" },
      ],
    });
    expect([...contracts.links]).toEqual(["billed_to", "signed_with"]);
  });

  it("keys Definition v1's entities by their primary key, with no names", () => {
    expect(v1.version).toBe("ver_1");
    expect(v1.types.get("Customer")).toMatchObject({
      key: ["customer_id"],
      names: [],
    });
    expect(v1.types.get("Customer")!.props.map((p) => p.name)).toContain(
      "email",
    );
    expect(v1.links.size).toBeGreaterThan(0);
  });

  it("goes by nothing without a version, or with one that doesn't parse", () => {
    expect(namingOf(null)).toBe(NO_NAMING);
    expect(namingOf(undefined)).toBe(NO_NAMING);
    const broken = namingOf({
      id: "ver_x",
      format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
      definition: { ontology_toml: "[types.A" },
    });
    expect(broken).toMatchObject({ version: "ver_x" });
    expect(broken.types.size).toBe(0);
    // A Definition v1 without entities knows no type either, and an entity
    // without a primary key is known by its name.
    expect(namingOf({ id: "ver_y" }).types.size).toBe(0);
    expect(
      namingOf({
        id: "ver_z",
        definition: { entities: [{ name: "Note" }] },
      }).types.get("Note"),
    ).toEqual({ name: "Note", props: [], key: [], names: [] });
  });
});

describe("readable", () => {
  it("names a type by its first non-empty names prop", () => {
    expect(
      readable(
        contracts,
        "Customer",
        {
          customer_id: "cus_GLOBEX01",
          company_name: "  Globex   Corporation ",
          name: "Old name",
        },
        4,
      ),
    ).toEqual({
      name: "Globex Corporation",
      type: "Customer",
      known: true,
      by: "names",
    });
    const two: Naming = {
      ...NO_NAMING,
      version: "v",
      types: new Map([
        [
          "Person",
          { name: "Person", props: [], key: ["id"], names: ["nick", "full"] },
        ],
      ]),
    };
    expect(
      readable(two, "Person", { id: "p1", nick: "", full: "Ada" }, 1),
    ).toMatchObject({ name: "Ada", by: "names" });
    expect(readable(two, "Person", { id: "p1", nick: 42 }, 1)).toMatchObject({
      name: "42",
      by: "names",
    });
  });

  it("falls back to the key props, joined", () => {
    expect(
      readable(contracts, "Customer", { customer_id: "cus_GLOBEX01" }, 4),
    ).toMatchObject({ name: "cus_GLOBEX01", by: "key" });
    expect(
      readable(shop, "OrderLine", { order_number: "ORD-1001", line_no: 2 }, 9),
    ).toMatchObject({ name: "ORD-1001 · 2", by: "key" });
    // A key part that is missing is left out rather than shown blank.
    expect(readable(shop, "OrderLine", { line_no: 2 }, 9)).toMatchObject({
      name: "2",
      by: "key",
    });
    // Definition v1 names nothing: a Stripe customer reads as its id.
    expect(
      readable(
        v1,
        "Customer",
        { customer_id: "cus_GLOBEX01", name: "Globex Corporation" },
        4,
      ),
    ).toMatchObject({ name: "cus_GLOBEX01", by: "key", known: true });
  });

  it("falls back to the stored name, then a short id", () => {
    expect(
      readable(
        contracts,
        "Contract",
        { norm_name: "msa", name: "Master services agreement" },
        12,
      ),
    ).toMatchObject({ name: "Master services agreement", by: "name" });
    expect(readable(contracts, "Contract", { norm_name: "msa" }, 12)).toEqual({
      name: "#12",
      type: "Contract",
      known: true,
      by: "id",
    });
    expect(
      readable(contracts, "Invoice", { amount_due: 5, name: { x: 1 } }, 3),
    ).toMatchObject({ name: "#3", by: "id" });
    expect(shortId(0)).toBe("#0");
  });

  it("keeps the raw label and id of a label the version doesn't know", () => {
    expect(readable(contracts, "Account", { email: "a@x" }, 3)).toEqual({
      name: "#3",
      type: "Account",
      known: false,
      by: "id",
    });
    expect(readable(contracts, "Account", { name: "Ada Lovelace" }, 3)).toEqual(
      {
        name: "Ada Lovelace",
        type: "Account",
        known: false,
        by: "name",
      },
    );
    // No version at all: every label is unknown.
    expect(
      readable(NO_NAMING, "Customer", { company_name: "Globex" }, 4),
    ).toMatchObject({ name: "#4", known: false });
  });
});

describe("isInternal", () => {
  it("hides normalized names everywhere", () => {
    for (const naming of [contracts, NO_NAMING])
      for (const target of ["node", "edge"] as const)
        expect(isInternal(naming, target, "Anything", "company_name__nn")).toBe(
          true,
        );
  });

  it("hides an entity's identity and a fact's key on the version's own", () => {
    for (const key of ["entity_key", "norm_name", "ontology_version"]) {
      expect(isInternal(contracts, "node", "Customer", key)).toBe(true);
      // The team's own nodes keep every property.
      expect(isInternal(contracts, "node", "Account", key)).toBe(false);
    }
    expect(isInternal(contracts, "node", "Customer", "name")).toBe(false);
    expect(isInternal(contracts, "node", "Customer", "company_name")).toBe(
      false,
    );
    expect(isInternal(contracts, "edge", "billed_to", "fact_key")).toBe(true);
    expect(isInternal(contracts, "edge", "billed_to", "ontology_version")).toBe(
      true,
    );
    expect(isInternal(contracts, "edge", "billed_to", "confidence")).toBe(
      false,
    );
    expect(isInternal(contracts, "edge", "PLACED", "fact_key")).toBe(false);
    expect(isInternal(contracts, "node", "billed_to", "fact_key")).toBe(false);
  });

  it("hides Foundry's stamps on everything but Foundry's own labels", () => {
    const stamps = ["foundry_tx", "foundry_part", "foundry_later"];
    for (const naming of [contracts, NO_NAMING])
      for (const key of stamps) {
        // The version's types and links, and labels it doesn't know.
        for (const label of ["Customer", "Account"])
          expect(isInternal(naming, "node", label, key), key).toBe(true);
        for (const label of ["billed_to", "PLACED"])
          expect(isInternal(naming, "edge", label, key), key).toBe(true);
        // On Foundry's own they are its data: a FoundryTx is its
        // `foundry_tx`, a FoundryVersion's names the request that wrote it.
        expect(isInternal(naming, "node", "FoundryTx", key), key).toBe(false);
        expect(isInternal(naming, "node", "FoundryVersion", key), key).toBe(
          false,
        );
        expect(isInternal(naming, "edge", "FOUNDRY_ABOUT", key), key).toBe(
          false,
        );
      }
    // The load is said apart, Foundry's own labels included.
    for (const naming of [contracts, NO_NAMING]) {
      expect(isInternal(naming, "node", "Customer", "foundry_load")).toBe(true);
      expect(isInternal(naming, "edge", "billed_to", "foundry_load")).toBe(
        true,
      );
      expect(isInternal(naming, "node", "FoundryTx", "foundry_load")).toBe(
        true,
      );
    }
    // A label is Foundry's by its side's prefix, and a prop by its own.
    expect(isInternal(contracts, "edge", "FoundryTx", "foundry_tx")).toBe(true);
    for (const key of ["foundry", "Foundry_tx", "my_foundry_tx", "tx"])
      expect(isInternal(contracts, "node", "Customer", key), key).toBe(false);
  });
});

describe("namingKeys", () => {
  it("lists every key and names prop once, without name", () => {
    expect(namingKeys(contracts)).toEqual([
      "company_name",
      "customer_id",
      "invoice_id",
    ]);
    expect(namingKeys(shop)).toEqual([
      "code",
      "customer_number",
      "line_no",
      "order_number",
      "return_ref",
      "shipment_ref",
      "sku",
      "title",
      "tracking_no",
    ]);
    expect(namingKeys(NO_NAMING)).toEqual([]);
  });

  it("stays bounded", () => {
    const many: Naming = {
      ...NO_NAMING,
      version: "v",
      types: new Map(
        Array.from({ length: 300 }, (_, i) => [
          `T${i}`,
          { name: `T${i}`, props: [], key: [`k${i}`], names: ["name", "$id"] },
        ]),
      ),
    };
    const keys = namingKeys(many);
    expect(keys).toHaveLength(MAX_NAMING_KEYS);
    expect(keys).not.toContain("name");
    expect(keys).not.toContain("$id");
  });
});

describe("normalizeName", () => {
  it("folds names as the ontology agent does", () => {
    const cases: [string, string][] = [
      ["  Helio Biosciences, Inc. ", "helio biosciences inc"],
      ["Ｈｅｌｉｏ", "helio"],
      ["Dana  Ruiz", "dana ruiz"],
      ["O'Brien & Co.", "o brien & co"],
      ["...", ""],
      ["Node.js", "node.js"],
      ["v2.0", "v2.0"],
      ["U.S.", "u s"],
      [".NET", ".net"],
      ["İZMİR", "izmir"],
      // Letter by letter, as the agent lowercases: no final-sigma rule.
      ["ΟΔΥΣΣΕΥΣ", "οδυσσευσ"],
      ["Κως", "κωσ"],
      // A dot after a kept symbol separates: the agent's run counts only
      // letters and digits, and a symbol doesn't start a word.
      ["C#.NET", "c# net"],
      ["A&.B", "a& b"],
      ["x..NET", "x .net"],
      ["José", "josé".normalize("NFKC")],
    ];
    for (const [given, folded] of cases)
      expect(normalizeName(given), given).toBe(folded);
    for (const [a, b] of [
      ["J.K. Rowling", "J. K. Rowling"],
      ["J.P. Morgan", "J P Morgan"],
    ])
      expect(normalizeName(a)).toBe(normalizeName(b));
    for (const [a, b] of [
      ["C++", "C#"],
      ["C", "C++"],
      [".NET", "NET"],
      ["A&E", "A+E"],
    ])
      expect(normalizeName(a)).not.toBe(normalizeName(b));
  });

  it("is idempotent, and the mock's loads fold the same way", () => {
    for (const name of [
      "Helio Biosciences, Inc.",
      "C++",
      ".NET",
      "Alder & Finch LLP",
      "Globex Corporation",
      "J.K. Rowling",
      "ΟΔΥΣΣΕΥΣ",
      "C#.NET",
    ]) {
      expect(normalizeName(normalizeName(name))).toBe(normalizeName(name));
      expect(normName(name)).toBe(normalizeName(name));
    }
  });
});
