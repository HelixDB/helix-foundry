import type {
  DataLayoutResponse,
  PutDataLayoutRequest,
} from "../../src/shared/wfe";
import { fail, ok, type MockRequest, type MockRoute } from "../http";
import { config, isReply, membership, onReset } from "../state";
import { manages } from "./workspaces";

// The Data tree's layout, as the WFE keeps it (the backend's
// foundry_data_layouts): one JSON object per workspace, opaque here beyond
// being an object of at most 64 KiB, with a revision that counts writes.
// - GET needs membership: {} and "0" before the first write.
// - PUT needs owner or admin, and names the revision it read. Any other
//   revision is FAILED_PRECONDITION, a first write at "0" that another first
//   write beat included (the backend's duplicate key).
// - The size is measured on compact JSON (no space outside strings), as the
//   backend measures and stores it.
// - `POST /__mock/config {"dataLayoutConflicts": n}`: another writer comes
//   first on each of the next n PUTs, moving the revision on, so they are
//   refused.

export const MAX_DATA_LAYOUT_BYTES = 64 << 10;

type Knobs = { dataLayoutConflicts?: number };
const knobs = config as typeof config & Knobs;

type StoredLayout = { layout: Record<string, unknown>; revision: number };
const layouts = new Map<string, StoredLayout>();

const isObject = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === "object" && !Array.isArray(v);

const reply = (l?: StoredLayout): DataLayoutResponse => ({
  layout: l?.layout ?? {},
  revision: String(l?.revision ?? 0),
});

function caller(req: MockRequest, manage: boolean) {
  const m = membership(req.params.workspaceId!, req.principal.user.id);
  if (isReply(m)) return m;
  return manage && !manages(m)
    ? fail("PERMISSION_DENIED", "permission denied")
    : undefined;
}

const PATH = "/v1/workspaces/{workspaceId}/foundry/data-layout" as const;

export const dataLayoutRoutes: MockRoute[] = [
  {
    method: "GET",
    path: PATH,
    handle: (req) =>
      caller(req, false) || ok(reply(layouts.get(req.params.workspaceId!))),
  },
  {
    method: "PUT",
    path: PATH,
    handle: (req) => {
      const refused = caller(req, true);
      if (refused) return refused;
      const ws = req.params.workspaceId!,
        body = (req.body || {}) as Partial<PutDataLayoutRequest>,
        expected = Number(body.expectedRevision ?? 0);
      if (!isObject(body.layout))
        return fail("INVALID_ARGUMENT", "layout is required: a JSON object");
      if (!Number.isSafeInteger(expected) || expected < 0)
        return fail(
          "INVALID_ARGUMENT",
          "expected_revision is the revision read, 0 or more",
        );
      const size = new TextEncoder().encode(JSON.stringify(body.layout)).length;
      if (size > MAX_DATA_LAYOUT_BYTES)
        return fail(
          "INVALID_ARGUMENT",
          `a layout holds up to 64 KiB; this one is ${size} bytes`,
        );
      if ((knobs.dataLayoutConflicts || 0) > 0) {
        knobs.dataLayoutConflicts! -= 1;
        const theirs = layouts.get(ws);
        layouts.set(ws, {
          layout: theirs?.layout ?? {},
          revision: (theirs?.revision ?? 0) + 1,
        });
      }
      const current = layouts.get(ws)?.revision ?? 0;
      if (expected !== current)
        return fail(
          "FAILED_PRECONDITION",
          "the layout changed since you read it; read it again",
        );
      const stored = { layout: body.layout, revision: current + 1 };
      layouts.set(ws, stored);
      return ok(reply(stored));
    },
  },
];

onReset(() => {
  layouts.clear();
  delete knobs.dataLayoutConflicts;
});
