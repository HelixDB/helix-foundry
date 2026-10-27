import type {
  Connector,
  FoundryDocument,
  FoundryDocumentSet,
} from "../../../shared/wfe";
import type { SetFolder, TreeItem } from "./model";

// The Data tree's layout: how a workspace arranges its Data page, shared by
// the team. The WFE keeps it as one JSON object per workspace (GET and PUT
// .../foundry/data-layout) and never reads it; this module owns its shape.
// Version 1:
//
//   { v: 1, names: {"<container>": "<label>"}, folders: [{id, name}],
//     order: {"<container>": ["<child>"…]} }
//
// - Containers: "root", "kind:<kind>" (a connector kind's folder),
//   "folder:<id>" (a custom folder), "documents" and "set:<setId>".
// - An order lists a container's children: root's and documents' by their
//   container keys, the others' by connector or document id. Children it
//   doesn't list follow the listed ones in their default order: kinds in the
//   WFE's order, then custom folders, then documents/ at the root;
//   connectors, sets and documents as they were created. So a new one goes
//   to the end of its container.
// - A connector sits in the custom folder whose order lists it (the first,
//   if several do), else in its kind's folder. Custom folders are at the
//   root only and hold connectors only. Sets and documents sit where the
//   WFE puts them; their orders only order them.
// - A connector's own sets (FoundryDocumentSet.connector) sit under it,
//   after its tables, in the WFE's order, and are in no order: their
//   documents page in newest first. documents/ holds the sets people make.
// - `names` renames the built-in folders: a kind's and documents/.
// - Ids the workspace doesn't have are dropped on read, and every write
//   stores the layout without them (`prune`).
// - A layout of another version is left alone: the tree shows in its
//   default order and can't be rearranged (`parseLayout` says "unknown").
//
// Every change is a `LayoutChange`, applied by `applyChange` on top of the
// layout it is given, so a write refused for a stale revision reapplies the
// same change on the fresh layout.

/** The layout version this site reads and writes. */
export const LAYOUT_VERSION = 1;
/** The most the WFE stores: 64 KiB of compact JSON. */
export const MAX_LAYOUT_BYTES = 64 * 1024;
/** Custom folders a workspace can have. */
export const MAX_FOLDERS = 50;
/** The longest folder name, in characters. */
export const MAX_FOLDER_NAME = 120;
/** documents/'s name until someone renames it. */
export const DOCUMENTS_NAME = "Documents";

export type KindKey = `kind:${string}`;
export type FolderKey = `folder:${string}`;
export type SetKey = `set:${string}`;
/** A child of the tree's root: a kind's folder, a custom folder or documents/. */
export type RootKey = KindKey | FolderKey | "documents";
/** A folder the tree makes by itself, which `names` can rename. */
export type BuiltInKey = KindKey | "documents";
export type ContainerKey = "root" | RootKey | SetKey;

export type CustomFolder = { readonly id: string; readonly name: string };

/** A version 1 layout, as read: names trimmed, nothing repeated. */
export type Layout = {
  readonly names: Readonly<Partial<Record<BuiltInKey, string>>>;
  readonly folders: readonly CustomFolder[];
  readonly order: Readonly<Partial<Record<ContainerKey, readonly string[]>>>;
};

export const EMPTY_LAYOUT: Layout = { names: {}, folders: [], order: {} };

/** A stored layout: one this site arranges, or one of a version it doesn't know. */
export type ParsedLayout =
  | { version: 1; layout: Layout }
  | { version: "unknown" };

// A layout's orders as entries: parseLayout never leaves one undefined.
const orders = (order: Layout["order"]) =>
  Object.entries(order) as [ContainerKey, readonly string[]][];

const isObject = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === "object" && !Array.isArray(v);
const entries = (v: unknown) => (isObject(v) ? Object.entries(v) : []);
const isContainerKey = (k: string): k is ContainerKey =>
  /^(?:root|documents|(?:kind|folder|set):.+)$/s.test(k);
const isBuiltIn = (k: string): k is BuiltInKey =>
  k === "documents" || /^kind:./s.test(k);
// A name as stored: trimmed, 1 to MAX_FOLDER_NAME characters.
const storedName = (v: unknown) => {
  const name = typeof v === "string" ? v.trim() : "";
  return name && [...name].length <= MAX_FOLDER_NAME ? name : undefined;
};

/**
 * The WFE's stored layout as this site reads it. `{}` (a workspace that
 * never wrote one) is the empty layout. A version 1 layout keeps what has
 * the right shape and drops the rest: names of built-in folders only,
 * folders with an id and a name (the first of an id, at most MAX_FOLDERS),
 * orders of containers as lists of distinct strings. Anything else,
 * another `v` included, is "unknown", and the site leaves it alone.
 *
 * @example
 * parseLayout({}); // { version: 1, layout: EMPTY_LAYOUT }
 * parseLayout({ v: 2, rows: [] }); // { version: "unknown" }
 * parseLayout({
 *   v: 1,
 *   names: { "kind:stripe": " Payments ", "set:x": "No" },
 *   folders: [{ id: "f1", name: "Finance" }, { id: "f1", name: "Again" }],
 *   order: { root: ["documents", "documents", 3], nowhere: ["a"] },
 * });
 * // { version: 1, layout: {
 * //   names: { "kind:stripe": "Payments" },
 * //   folders: [{ id: "f1", name: "Finance" }],
 * //   order: { root: ["documents"] } } }
 */
export function parseLayout(stored: unknown): ParsedLayout {
  if (!isObject(stored)) return { version: "unknown" };
  if (!Object.keys(stored).length) return { version: 1, layout: EMPTY_LAYOUT };
  if (stored.v !== LAYOUT_VERSION) return { version: "unknown" };
  const names = Object.fromEntries(
    entries(stored.names).flatMap(([key, value]) => {
      const name = storedName(value);
      return isBuiltIn(key) && name ? [[key, name]] : [];
    }),
  );
  const ids = new Set<string>();
  const folders = (Array.isArray(stored.folders) ? stored.folders : [])
    .flatMap((f): CustomFolder[] => {
      const name = isObject(f) ? storedName(f.name) : undefined;
      if (!isObject(f) || typeof f.id !== "string" || !f.id || !name) return [];
      if (ids.has(f.id)) return [];
      ids.add(f.id);
      return [{ id: f.id, name }];
    })
    .slice(0, MAX_FOLDERS);
  const order = Object.fromEntries(
    entries(stored.order).flatMap(([key, value]) =>
      isContainerKey(key) && Array.isArray(value)
        ? [
            [
              key,
              [...new Set(value.filter((x) => typeof x === "string"))],
            ] as const,
          ]
        : [],
    ),
  );
  return { version: 1, layout: { names, folders, order } };
}

/**
 * The JSON a PUT stores: version 1, without empty orders.
 *
 * @example
 * toStored(EMPTY_LAYOUT); // { v: 1, names: {}, folders: [], order: {} }
 */
export function toStored(layout: Layout) {
  return {
    v: LAYOUT_VERSION,
    names: layout.names,
    folders: layout.folders.map(({ id, name }) => ({ id, name })),
    order: Object.fromEntries(
      orders(layout.order).filter(([, ids]) => ids.length),
    ),
  };
}

/** The bytes the WFE counts against MAX_LAYOUT_BYTES: compact UTF-8 JSON. */
export const layoutBytes = (layout: Layout) =>
  new TextEncoder().encode(JSON.stringify(toStored(layout))).length;

// ---------------------------------------------------------------- Tree

/** What the tree arranges: the WFE's lists, each read in full. */
export type TreeData = {
  // The WFE's connector kinds, in its order.
  readonly kinds: readonly string[];
  readonly connectors: readonly Connector[];
  // Every set, a connector's too; oldest first, as the WFE lists them.
  readonly sets: readonly FoundryDocumentSet[];
  // The workspace's uploads.
  readonly documents: readonly FoundryDocument[];
  // A kind's name, as the kit labels it.
  readonly label?: (kind: string) => string;
};

/** A kind's name: `data.label`'s, else the kind itself. */
const kindName = (data: TreeData, kind: string) => data.label?.(kind) ?? kind;

/** A folder at the tree's root, its children in order. */
export type TreeNode =
  | {
      type: "kind";
      key: KindKey;
      kind: string;
      name: string;
      items: TreeItem[];
    }
  | {
      type: "folder";
      key: FolderKey;
      id: string;
      name: string;
      items: TreeItem[];
    }
  | { type: "documents"; key: "documents"; name: string; sets: SetFolder[] };
export type DocumentsNode = Extract<TreeNode, { type: "documents" }>;

// `items` with the ones `listed` names first, as listed, then the rest in
// their own order.
function inOrder<T>(
  items: readonly T[],
  key: (item: T) => string,
  listed: readonly string[] = [],
): T[] {
  const rest = new Map(items.map((item) => [key(item), item]));
  const first = listed.flatMap((k) => {
    const item = rest.get(k);
    rest.delete(k);
    return item === undefined ? [] : [item];
  });
  return [...first, ...rest.values()];
}

const byAge = (documents: readonly FoundryDocument[]) =>
  [...documents].sort(
    (a, b) =>
      (a.createdAt || "").localeCompare(b.createdAt || "") ||
      a.id.localeCompare(b.id),
  );

// The tree without a search: what a change starts from.
function tree(layout: Layout, data: TreeData): TreeNode[] {
  const byId = new Map(data.connectors.map((c) => [c.id, c])),
    placed = new Set<string>();
  const item = (connector: Connector): TreeItem => ({
    connector,
    tables: connector.tables || [],
    sets: data.sets.filter((s) => s.connector?.connectorId === connector.id),
  });
  const folders = layout.folders.map((f): TreeNode => {
    const ids = (layout.order[`folder:${f.id}`] || []).filter(
      (id) => byId.has(id) && !placed.has(id),
    );
    for (const id of ids) placed.add(id);
    return {
      type: "folder",
      key: `folder:${f.id}`,
      id: f.id,
      name: f.name,
      items: ids.map((id) => item(byId.get(id)!)),
    };
  });
  const kinds = [
    ...new Set([...data.kinds, ...data.connectors.map((c) => c.kind)]),
  ].flatMap((kind): TreeNode[] => {
    const key = `kind:${kind}` as const;
    const held = data.connectors.filter(
      (c) => c.kind === kind && !placed.has(c.id),
    );
    return held.length
      ? [
          {
            type: "kind",
            key,
            kind,
            name: layout.names[key] ?? kindName(data, kind),
            items: inOrder(held, (c) => c.id, layout.order[key]).map(item),
          },
        ]
      : [];
  });
  const sets = inOrder(
    data.sets.filter((s) => !s.connector),
    (s) => `set:${s.id}`,
    layout.order.documents,
  ).map((set) => ({
    set,
    documents: inOrder(
      byAge(data.documents.filter((d) => d.setId === set.id)),
      (d) => d.id,
      layout.order[`set:${set.id}`],
    ),
  }));
  const documents: TreeNode = {
    type: "documents",
    key: "documents",
    name: layout.names.documents ?? DOCUMENTS_NAME,
    sets,
  };
  return inOrder(
    [...kinds, ...folders, documents],
    (n) => n.key,
    layout.order.root,
  );
}

/**
 * The Data page's tree: the root's folders in the layout's order, each with
 * its children in order. A kind's folder shows while it holds a connector,
 * and each connector lists its tables and its own sets; custom folders and
 * documents/ always show. A search `term` (any case) keeps a folder or set
 * whose name matches with all it holds (its connectors listing nothing);
 * else the connectors whose name or kind matches (listing nothing), or with
 * tables or sets whose names match (those), and the documents whose names
 * match; and drops what it empties.
 *
 * @example
 * arrange(
 *   { names: {}, folders: [{ id: "f", name: "Finance" }],
 *     order: { root: ["folder:f"], "folder:f": ["b"] } },
 *   { kinds: ["stripe", "postgres"], sets: [], documents: [],
 *     connectors: [
 *       { id: "a", kind: "postgres" },
 *       { id: "b", kind: "stripe" },
 *     ] },
 * ).map((n) => n.key);
 * // ["folder:f", "kind:postgres", "documents"]
 */
export function arrange(layout: Layout, data: TreeData, term = ""): TreeNode[] {
  const nodes = tree(layout, data),
    wanted = term.trim().toLowerCase();
  if (!wanted) return nodes;
  const matches = (text = "") => text.toLowerCase().includes(wanted);
  const bare = (i: TreeItem): TreeItem => ({ ...i, tables: [], sets: [] });
  const found = (items: TreeItem[]) =>
    items.flatMap((i): TreeItem[] => {
      const kind = kindName(data, i.connector.kind);
      if (matches(`${i.connector.name || kind} ${kind}`)) return [bare(i)];
      const tables = i.tables.filter((t) => matches(t.name)),
        sets = i.sets.filter((s) => matches(s.name));
      return tables.length || sets.length ? [{ ...i, tables, sets }] : [];
    });
  return nodes.flatMap((n): TreeNode[] => {
    if (matches(n.name))
      return [n.type === "documents" ? n : { ...n, items: n.items.map(bare) }];
    if (n.type !== "documents") {
      const items = found(n.items);
      return items.length ? [{ ...n, items }] : [];
    }
    const sets = n.sets.flatMap((f): SetFolder[] => {
      if (matches(f.set.name)) return [f];
      const documents = f.documents.filter((d) => matches(d.name));
      return documents.length ? [{ set: f.set, documents }] : [];
    });
    return sets.length ? [{ ...n, sets }] : [];
  });
}

/**
 * The layout without what the workspace doesn't have: names of kinds it
 * doesn't know, orders of containers it doesn't have, and ids that aren't
 * in their container (a connector listed by a second folder or by a kind's
 * folder while a custom folder holds it, a connector's own set, which
 * documents/ doesn't hold, or its documents). Custom folders stay, empty
 * ones too. Only prune with every list read in full: an id missing from a list
 * still loading would be dropped for good.
 *
 * @example
 * prune(
 *   { names: { "kind:gone": "Old" }, folders: [],
 *     order: { "kind:stripe": ["deleted", "b"], "set:gone": ["d"] } },
 *   { kinds: ["stripe"], sets: [], documents: [],
 *     connectors: [{ id: "b", kind: "stripe" }] },
 * );
 * // { names: {}, folders: [], order: { "kind:stripe": ["b"] } }
 */
export function prune(layout: Layout, data: TreeData): Layout {
  const kinds = new Set([...data.kinds, ...data.connectors.map((c) => c.kind)]);
  const children = new Map<string, Set<string>>([
    [
      "root",
      new Set([
        ...[...kinds].map((k) => `kind:${k}`),
        ...layout.folders.map((f) => `folder:${f.id}`),
        "documents",
      ]),
    ],
  ]);
  for (const n of tree(layout, data))
    if (n.type !== "documents")
      children.set(n.key, new Set(n.items.map((i) => i.connector.id)));
    else {
      children.set("documents", new Set(n.sets.map((f) => `set:${f.set.id}`)));
      for (const f of n.sets)
        children.set(`set:${f.set.id}`, new Set(f.documents.map((d) => d.id)));
    }
  return {
    names: Object.fromEntries(
      Object.entries(layout.names).filter(
        ([key]) => key === "documents" || kinds.has(key.slice("kind:".length)),
      ),
    ),
    folders: layout.folders,
    order: Object.fromEntries(
      orders(layout.order).flatMap(([key, ids]) => {
        const held = children.get(key),
          kept = ids.filter((id) => held?.has(id));
        return kept.length ? [[key, kept]] : [];
      }),
    ),
  };
}

// ---------------------------------------------------------------- Changes

/**
 * One change to the layout. A move places an item before `before` (an item
 * of the same container), or last for null. A connector moves into a custom
 * folder, or back to its kind's folder for `folder: null`; a set moves only
 * within documents/, and a document only within its set (moving it to
 * another set is the WFE's move).
 */
export type LayoutChange =
  | { type: "move-root"; key: RootKey; before: RootKey | null }
  | {
      type: "move-connector";
      id: string;
      folder: string | null;
      before: string | null;
    }
  | { type: "move-set"; id: string; before: string | null }
  | { type: "move-document"; id: string; before: string | null }
  | { type: "create-folder"; folder: CustomFolder }
  | { type: "rename-folder"; id: string; name: string }
  | { type: "delete-folder"; id: string }
  | { type: "rename"; key: BuiltInKey; name: string };

/** A change applied, or why it can't be. */
export type Changed = { layout: Layout } | { problem: string };

// `ids` with `id` moved before `before`, or last when `before` isn't there.
function placeBefore(
  ids: readonly string[],
  id: string,
  before: string | null,
): string[] {
  const rest = ids.filter((x) => x !== id),
    at = before === null ? -1 : rest.indexOf(before);
  return at < 0 ? [...rest, id] : [...rest.slice(0, at), id, ...rest.slice(at)];
}

/**
 * Why `name` can't name a folder at the root, if it can't: blank, longer
 * than MAX_FOLDER_NAME characters, or another root folder's name (case and
 * accents aside). `except` is the folder being renamed.
 *
 * @example
 * folderNameProblem(" stripe ", [
 *   { type: "kind", key: "kind:stripe", kind: "stripe", name: "Stripe", items: [] },
 * ]);
 * // "There’s already a folder called Stripe."
 */
export function folderNameProblem(
  name: string,
  nodes: readonly TreeNode[],
  except?: RootKey,
): string | undefined {
  const value = name.trim();
  if (!value) return "Give the folder a name.";
  if ([...value].length > MAX_FOLDER_NAME)
    return `A folder’s name is at most ${MAX_FOLDER_NAME} characters.`;
  const taken = nodes.find(
    (n) =>
      n.key !== except &&
      n.name.localeCompare(value, undefined, { sensitivity: "base" }) === 0,
  );
  return taken ? `There’s already a folder called ${taken.name}.` : undefined;
}

const setsOf = (nodes: readonly TreeNode[]) =>
  nodes.flatMap((n) => (n.type === "documents" ? n.sets : []));

const gone = (what: string): Changed => ({
  problem: `${what} isn’t in Connectors any more.`,
});

/**
 * `change` applied to `layout`, pruned (see `prune`; so `data` must hold
 * every list in full), or why it can't be: an item or folder that's gone,
 * a folder name that can't be used, too many folders, or a layout over
 * MAX_LAYOUT_BYTES. A move only writes the orders it changes, in full, so
 * the items it passed keep their places.
 *
 * @example
 * const data = { kinds: ["stripe"], sets: [], documents: [],
 *   connectors: [{ id: "a", kind: "stripe" }, { id: "b", kind: "stripe" }] };
 * applyChange(EMPTY_LAYOUT, data,
 *   { type: "move-connector", id: "b", folder: null, before: "a" });
 * // { layout: { names: {}, folders: [], order: { "kind:stripe": ["b", "a"] } } }
 */
export function applyChange(
  layout: Layout,
  data: TreeData,
  change: LayoutChange,
): Changed {
  const base = prune(layout, data),
    nodes = tree(base, data);
  const next = ((): Changed => {
    // The orders `order` changes, an emptied one left out.
    const withOrder = (order: Layout["order"]): Changed => ({
      layout: {
        ...base,
        order: Object.fromEntries(
          orders({ ...base.order, ...order }).filter(([, ids]) => ids.length),
        ),
      },
    });
    switch (change.type) {
      case "move-root": {
        const keys = nodes.map((n) => n.key);
        if (!keys.includes(change.key)) return gone("That folder");
        return withOrder({
          root: placeBefore(keys, change.key, change.before),
        });
      }
      case "move-connector": {
        const c = data.connectors.find((x) => x.id === change.id);
        if (!c) return gone("That connection");
        const to: RootKey =
          change.folder === null ? `kind:${c.kind}` : `folder:${change.folder}`;
        if (
          change.folder !== null &&
          !base.folders.some((f) => f.id === change.folder)
        )
          return gone("That folder");
        const ids = (key: RootKey) => {
          const n = nodes.find((x) => x.key === key);
          return n && n.type !== "documents"
            ? n.items.map((i) => i.connector.id)
            : [];
        };
        const from = nodes.find(
          (n) =>
            n.type !== "documents" &&
            n.items.some((i) => i.connector.id === c.id),
        )!.key;
        // It leaves the order it was listed in; a folder's lists it for sure.
        return withOrder({
          ...(from === to
            ? {}
            : { [from]: (base.order[from] || []).filter((id) => id !== c.id) }),
          [to]: placeBefore(ids(to), c.id, change.before),
        });
      }
      case "move-set": {
        const keys = setsOf(nodes).map((f) => `set:${f.set.id}`),
          key = `set:${change.id}`;
        if (!keys.includes(key)) return gone("That set");
        return withOrder({
          documents: placeBefore(
            keys,
            key,
            change.before === null ? null : `set:${change.before}`,
          ),
        });
      }
      case "move-document": {
        const d = data.documents.find((x) => x.id === change.id),
          set = setsOf(nodes).find((f) => f.set.id === d?.setId);
        if (!d || !set) return gone("That document");
        return withOrder({
          [`set:${set.set.id}`]: placeBefore(
            set.documents.map((x) => x.id),
            d.id,
            change.before,
          ),
        });
      }
      case "create-folder": {
        const { id, name } = change.folder;
        if (base.folders.length >= MAX_FOLDERS)
          return {
            problem: `Connectors holds up to ${MAX_FOLDERS} folders.`,
          };
        if (!id || base.folders.some((f) => f.id === id))
          return { problem: "That folder is already there." };
        const problem = folderNameProblem(name, nodes);
        if (problem) return { problem };
        return {
          layout: {
            ...base,
            folders: [...base.folders, { id, name: name.trim() }],
          },
        };
      }
      case "rename-folder": {
        if (!base.folders.some((f) => f.id === change.id))
          return gone("That folder");
        const problem = folderNameProblem(
          change.name,
          nodes,
          `folder:${change.id}`,
        );
        if (problem) return { problem };
        return {
          layout: {
            ...base,
            folders: base.folders.map((f) =>
              f.id === change.id ? { id: f.id, name: change.name.trim() } : f,
            ),
          },
        };
      }
      case "delete-folder": {
        if (!base.folders.some((f) => f.id === change.id))
          return gone("That folder");
        const key = `folder:${change.id}`;
        return {
          layout: {
            ...base,
            folders: base.folders.filter((f) => f.id !== change.id),
            order: Object.fromEntries(
              orders(base.order).flatMap(([k, ids]) =>
                k === key
                  ? []
                  : [[k, k === "root" ? ids.filter((x) => x !== key) : ids]],
              ),
            ),
          },
        };
      }
      case "rename": {
        const problem = folderNameProblem(change.name, nodes, change.key);
        if (problem) return { problem };
        const name = change.name.trim(),
          usual =
            change.key === "documents"
              ? DOCUMENTS_NAME
              : kindName(data, change.key.slice("kind:".length)),
          names = Object.fromEntries(
            Object.entries(base.names).filter(([k]) => k !== change.key),
          );
        return {
          layout: {
            ...base,
            names: name === usual ? names : { ...names, [change.key]: name },
          },
        };
      }
    }
  })();
  if ("problem" in next) return next;
  return layoutBytes(next.layout) > MAX_LAYOUT_BYTES
    ? {
        problem:
          "The Connectors layout is full. Delete a folder or shorten a name, then try again.",
      }
    : next;
}

// ---------------------------------------------------------------- Moves

/**
 * The `before` that moves `id` one place up (-1) or down (1) among
 * `siblings`, or undefined when it is already first or last.
 *
 * @example
 * stepBefore(["a", "b", "c"], "a", 1); // "c"
 * stepBefore(["a", "b", "c"], "b", 1); // null (last)
 * stepBefore(["a", "b", "c"], "a", -1); // undefined
 */
export function stepBefore<T extends string>(
  siblings: readonly T[],
  id: T,
  step: -1 | 1,
): T | null | undefined {
  const at = siblings.indexOf(id);
  if (at < 0) return undefined;
  if (step < 0) return at > 0 ? siblings[at - 1] : undefined;
  return at < siblings.length - 1 ? (siblings[at + 2] ?? null) : undefined;
}

/**
 * The `before` that drops `id` on `over`'s place among `siblings`, as a
 * sortable list shows it: after `over` going down, before it going up.
 * Undefined when either isn't there or they are the same.
 *
 * @example
 * dropBefore(["a", "b", "c"], "a", "b"); // "c"
 * dropBefore(["a", "b", "c"], "c", "a"); // "a"
 */
export function dropBefore<T extends string>(
  siblings: readonly T[],
  id: T,
  over: T,
): T | null | undefined {
  const from = siblings.indexOf(id),
    to = siblings.indexOf(over);
  if (from < 0 || to < 0 || from === to) return undefined;
  return from < to ? (siblings[to + 1] ?? null) : over;
}

/**
 * Where a connector can move: its kind's folder when a custom folder holds
 * it (`folder: null`, named as the tree names it), then each other custom
 * folder.
 *
 * @example
 * destinations(
 *   { names: {}, folders: [{ id: "f", name: "Finance" }, { id: "g", name: "Ops" }],
 *     order: { "folder:f": ["a"] } },
 *   { kinds: ["stripe"], sets: [], documents: [],
 *     connectors: [{ id: "a", kind: "stripe" }],
 *     label: () => "Stripe" },
 *   "a",
 * );
 * // [{ folder: null, name: "Stripe" }, { folder: "g", name: "Ops" }]
 */
export function destinations(
  layout: Layout,
  data: TreeData,
  id: string,
): { folder: string | null; name: string }[] {
  const c = data.connectors.find((x) => x.id === id);
  if (!c) return [];
  const home = tree(layout, data).find(
    (n) => n.type !== "documents" && n.items.some((i) => i.connector.id === id),
  );
  const kind = `kind:${c.kind}` as const;
  return [
    ...(home?.type === "folder"
      ? [{ folder: null, name: layout.names[kind] ?? kindName(data, c.kind) }]
      : []),
    ...layout.folders
      .filter((f) => home?.key !== `folder:${f.id}`)
      .map((f) => ({ folder: f.id, name: f.name })),
  ];
}

// ---------------------------------------------------------------- Explorer

/**
 * What a Data page's URL opens: a connection, on its own page or one of its
 * tables' (`table`), or a document.
 */
export type Selection =
  | { type: "connection"; id: string; table: string | null }
  | { type: "document"; id: string };

/**
 * What the Data page at `pathname` (as the address bar has it, encoded)
 * opens, or null for any other page or a segment that doesn't decode.
 *
 * @example
 * selectionOf("/data/connections/conn_1"); // { type: "connection", id: "conn_1", table: null }
 * selectionOf("/data/connections/conn_1/tables/order%2Fitems");
 * // { type: "connection", id: "conn_1", table: "order/items" }
 * selectionOf("/data/documents/doc_1"); // { type: "document", id: "doc_1" }
 * selectionOf("/data"); // null
 */
export function selectionOf(pathname: string): Selection | null {
  let parts: string[];
  try {
    parts = pathname.replace(/\/+$/, "").split("/").map(decodeURIComponent);
  } catch {
    return null;
  }
  const [root, area, type, id, tables, table, ...rest] = parts;
  if (root !== "" || area !== "data" || !id || rest.length) return null;
  if (type === "documents")
    return tables === undefined ? { type: "document", id } : null;
  if (type !== "connections") return null;
  if (tables === undefined) return { type: "connection", id, table: null };
  return tables === "tables" && table
    ? { type: "connection", id, table }
    : null;
}

/**
 * A row of the Data explorer, in the order shown: a root folder (its key),
 * a connection (`c:<id>`), one of its tables (`tableKey`), a set
 * (`set:<id>`, in documents/ or a connection's own) or a document
 * (`d:<id>`). A folder is open or closed, and so is a connection that lists
 * something; tables and documents are leaves.
 */
export type ExplorerRow = {
  key: string;
  // 1 at the root.
  level: number;
  state: "open" | "closed" | "leaf";
};

/** The explorer row of a connection's table. */
export const tableKey = (connectorId: string, table: string) =>
  `t:${connectorId}/${table}`;

/**
 * The explorer's rows as shown: each root folder and, while it is open
 * (`isOpen`, by key; a search opens every one), what it holds, down to the
 * documents of an open set and the tables and sets of an open connection.
 * A connection's own set holds the documents `documentsOf` it has read so
 * far, newest first; a set of documents/ those the tree has.
 *
 * @example
 * explorerRows(
 *   arrange(EMPTY_LAYOUT, {
 *     kinds: ["stripe"],
 *     connectors: [{ id: "a", kind: "stripe" }],
 *     sets: [{ id: "g", name: "General" }],
 *     documents: [{ id: "d", name: "a.md", setId: "g" }],
 *   }),
 *   (key) => key !== "set:g",
 * );
 * // [{ key: "kind:stripe", level: 1, state: "open" },
 * //  { key: "c:a", level: 2, state: "leaf" },
 * //  { key: "documents", level: 1, state: "open" },
 * //  { key: "set:g", level: 2, state: "closed" }]
 */
export function explorerRows(
  nodes: readonly TreeNode[],
  isOpen: (key: string) => boolean,
  documentsOf: (setId: string) => readonly FoundryDocument[] = () => [],
): ExplorerRow[] {
  const folder = (
    key: string,
    level: number,
    held: () => ExplorerRow[],
  ): ExplorerRow[] =>
    isOpen(key)
      ? [{ key, level, state: "open" }, ...held()]
      : [{ key, level, state: "closed" }];
  const leaf = (key: string, level: number): ExplorerRow => ({
    key,
    level,
    state: "leaf",
  });
  return nodes.flatMap((n) =>
    folder(n.key, 1, () =>
      n.type === "documents"
        ? n.sets.flatMap((f) =>
            folder(`set:${f.set.id}`, 2, () =>
              f.documents.map((d) => leaf(`d:${d.id}`, 3)),
            ),
          )
        : n.items.flatMap(({ connector: c, tables, sets }) =>
            tables.length || sets.length
              ? folder(`c:${c.id}`, 2, () => [
                  ...tables.map((t) => leaf(tableKey(c.id, t.name), 3)),
                  ...sets.flatMap((s) =>
                    folder(`set:${s.id}`, 3, () =>
                      documentsOf(s.id).map((d) => leaf(`d:${d.id}`, 4)),
                    ),
                  ),
                ])
              : [leaf(`c:${c.id}`, 2)],
          ),
    ),
  );
}

/**
 * The explorer row `selection` opens and the folders holding it, outermost
 * first, or null when `nodes` don't hold it. A table's page selects the
 * table's row while its connection lists it, else its connection's. A
 * document is an upload of documents/, or one of a connection's own set
 * `setId` (the document's, once read): its row may not be read yet.
 *
 * @example
 * selectedRow(
 *   arrange(EMPTY_LAYOUT, {
 *     kinds: [],
 *     connectors: [],
 *     sets: [{ id: "g", name: "General" }],
 *     documents: [{ id: "d", name: "a.md", setId: "g" }],
 *   }),
 *   { type: "document", id: "d" },
 * );
 * // { key: "d:d", folders: ["documents", "set:g"] }
 */
export function selectedRow(
  nodes: readonly TreeNode[],
  selection: Selection | null,
  setId?: string,
): { key: string; folders: string[] } | null {
  if (!selection) return null;
  const found = nodes.flatMap((n) => {
    if (n.type === "documents")
      return selection.type === "document"
        ? n.sets
            .filter((f) => f.documents.some((d) => d.id === selection.id))
            .map((f) => ({
              key: `d:${selection.id}`,
              folders: [n.key, `set:${f.set.id}`],
            }))
        : [];
    if (selection.type === "document")
      return n.items
        .filter((i) => i.sets.some((s) => s.id === setId))
        .map((i) => ({
          key: `d:${selection.id}`,
          folders: [n.key, `c:${i.connector.id}`, `set:${setId}`],
        }));
    const { id, table } = selection;
    return n.items
      .filter((i) => i.connector.id === id)
      .map((i) =>
        table !== null && i.tables.some((t) => t.name === table)
          ? { key: tableKey(id, table), folders: [n.key, `c:${id}`] }
          : { key: `c:${id}`, folders: [n.key] },
      );
  });
  return found[0] ?? null;
}

/** What a key pressed on an explorer row does, to that row or another. */
export type RowKeyAction = {
  type: "focus" | "toggle" | "open" | "rename";
  key: string;
};

/**
 * What the key `pressed` (a KeyboardEvent's `key`) does on the row `key`,
 * as in a tree: Down and Up move to the next or previous row, Home and End
 * to the first or last; Right opens a closed folder or moves into an open
 * one, Left closes an open one or moves to the row's folder; Enter opens
 * the row (a folder opens or closes) and F2 renames it. Null for any other
 * key, a move past either end, or a row `rows` don't hold.
 *
 * @example
 * const rows: ExplorerRow[] = [
 *   { key: "kind:stripe", level: 1, state: "open" },
 *   { key: "c:a", level: 2, state: "leaf" },
 * ];
 * rowKeyAction(rows, "c:a", "ArrowLeft"); // { type: "focus", key: "kind:stripe" }
 * rowKeyAction(rows, "kind:stripe", "ArrowLeft"); // { type: "toggle", key: "kind:stripe" }
 * rowKeyAction(rows, "c:a", "ArrowDown"); // null
 */
export function rowKeyAction(
  rows: readonly ExplorerRow[],
  key: string,
  pressed: string,
): RowKeyAction | null {
  const at = rows.findIndex((r) => r.key === key),
    row = rows[at];
  if (!row) return null;
  const focus = (to: ExplorerRow | undefined): RowKeyAction | null =>
    to ? { type: "focus", key: to.key } : null;
  const child =
    rows[at + 1]?.level === row.level + 1 ? rows[at + 1] : undefined;
  switch (pressed) {
    case "ArrowDown":
      return focus(rows[at + 1]);
    case "ArrowUp":
      return focus(rows[at - 1]);
    case "Home":
      return focus(rows[0]);
    case "End":
      return focus(rows.at(-1));
    case "ArrowRight":
      if (row.state === "closed") return { type: "toggle", key };
      return row.state === "open" ? focus(child) : null;
    case "ArrowLeft":
      if (row.state === "open") return { type: "toggle", key };
      return focus(rows.slice(0, at).findLast((r) => r.level < row.level));
    case "Enter":
      return { type: "open", key };
    case "F2":
      return { type: "rename", key };
    default:
      return null;
  }
}
