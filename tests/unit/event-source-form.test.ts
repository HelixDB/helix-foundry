import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Connector } from "@/shared/wfe";
import { EventSourceForm } from "@/web/areas/data/event-source-form";

// The API source form's payloads, from the form itself: an edit sends the
// name and the description, each only when it changed, and never a text
// field, which the WFE leaves as it is when an update leaves it out (one
// that sent it would change it and queue a full sync). A create sends none
// either. The text field is the source page's to set (EventSourceTextField,
// setEventTextField).
//
// The form is called as a function under a stand-in for React's state:
// each call is a render, and what a setter set shows at the next.

const hooks = vi.hoisted(() => ({ slots: [] as unknown[], at: 0 }));
vi.mock("react", async (original) => ({
  ...(await original<object>()),
  useState: (initial: unknown) => {
    const i = hooks.at++;
    if (!(i in hooks.slots)) hooks.slots[i] = initial;
    return [
      hooks.slots[i],
      (value: unknown) => {
        hooks.slots[i] =
          typeof value === "function"
            ? (value as (before: unknown) => unknown)(hooks.slots[i])
            : value;
      },
    ];
  },
}));
vi.mock("@tanstack/react-query", async (original) => ({
  ...(await original<object>()),
  useQueryClient: () => ({ invalidateQueries: async () => {} }),
}));
vi.mock("@/web/ui", async (original) => ({
  ...(await original<object>()),
  useWorkspace: () => ({ id: "ws_1" }),
}));
const sent = vi.hoisted(() => ({
  updates: [] as unknown[][],
  creates: [] as unknown[][],
}));
vi.mock("@/web/areas/data/data-api", async (original) => ({
  ...(await original<object>()),
  updateConnector: async (...args: unknown[]) => {
    sent.updates.push(args);
    return { connector: { id: "c1", kind: "events" } };
  },
  createConnector: async (...args: unknown[]) => {
    sent.creates.push(args);
    return { connector: { id: "c2", kind: "events", name: "Made" } };
  },
}));

type El = { type: unknown; props: Record<string, any> };
/** The first element of a rendered tree with this tag, depth first. */
function find(node: unknown, tag: string): El | undefined {
  if (Array.isArray(node)) return node.map((n) => find(n, tag)).find(Boolean);
  if (!node || typeof node !== "object" || !("props" in node)) return;
  const el = node as El;
  return el.type === tag ? el : find(el.props.children, tag);
}

/**
 * The form for `connector` (none makes one), with `edit` typed into its
 * fields (each a render of its own), then submitted, its calls settled.
 */
async function submit(
  connector: Connector | undefined,
  edit: { name?: string; description?: string },
) {
  hooks.slots = [];
  const render = () => {
    hooks.at = 0;
    return EventSourceForm({ connector, onDone: () => {} });
  };
  if (edit.name !== undefined)
    find(render(), "input")!.props.onChange({ target: { value: edit.name } });
  if (edit.description !== undefined)
    find(render(), "textarea")!.props.onChange({
      target: { value: edit.description },
    });
  find(render(), "form")!.props.onSubmit({ preventDefault() {} });
  // The stand-ins answer at once: a macrotask settles every step after.
  await new Promise((done) => setTimeout(done));
}

const source: Connector = {
  id: "c1",
  kind: "events",
  name: "Orders",
  config: {
    events: {
      description: "One order",
      textField: "reply",
      fields: [{ name: "reply", type: "string" }],
    },
  },
};

beforeEach(() => {
  sent.updates.length = 0;
  sent.creates.length = 0;
});

describe("the API source form", () => {
  it.each<[string, { name?: string; description?: string }, unknown]>([
    [
      "a new description",
      { description: "One line of an order" },
      { config: { events: { description: "One line of an order" } } },
    ],
    [
      "a description, trimmed",
      { description: "  One line  " },
      { config: { events: { description: "One line" } } },
    ],
    [
      "a description cleared",
      { description: "" },
      { config: { events: { description: "" } } },
    ],
    ["a new name alone", { name: "Order lines" }, { name: "Order lines" }],
    [
      "a new name and description",
      { name: "Order lines", description: "One line" },
      { name: "Order lines", config: { events: { description: "One line" } } },
    ],
  ])("sends %s, and never the text field", async (_, edit, want) => {
    await submit(source, edit);
    expect(sent.updates).toEqual([["ws_1", "c1", want]]);
    expect(JSON.stringify(sent.updates)).not.toContain("textField");
    expect(sent.creates).toEqual([]);
  });

  it.each<[string, { name?: string; description?: string }]>([
    ["nothing typed", {}],
    ["the same name", { name: "Orders" }],
    ["the same description, spaced", { description: " One order " }],
    ["a blank name", { name: "  ", description: "Another" }],
  ])("sends nothing for %s", async (_, edit) => {
    await submit(source, edit);
    expect(sent.updates).toEqual([]);
  });

  it("makes a source with its name and description, and no text field", async () => {
    await submit(undefined, { name: " Order lines ", description: "One line" });
    expect(sent.creates).toEqual([
      [
        "ws_1",
        {
          name: "Order lines",
          config: { events: { description: "One line" } },
        },
      ],
    ]);
    expect(JSON.stringify(sent.creates)).not.toContain("textField");
    expect(sent.updates).toEqual([]);
  });
});
