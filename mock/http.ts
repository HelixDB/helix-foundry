import type { HttpMethod, RouteTemplate } from "../src/shared/route-template";
import type { Visibility } from "./graph/view";
import type { MockUser } from "./state";

// What a mock WFE route sees and returns. Routes are registered in
// mock/routes/index.ts; each area keeps its routes in a module of its own.

export type Principal = { user: MockUser; sessionId: string };

export type MockRequest = {
  method: HttpMethod;
  path: string;
  // The {params} of the route's template.
  params: Record<string, string>;
  query: URLSearchParams;
  // Parsed JSON body (camelCase, as the swagger names fields), or undefined.
  body: any;
  // The signed-in caller. Always set unless the route is `public`.
  principal: Principal;
  // Set when the caller reads only through source grants (./routes/access.ts
  // admitted it, as the backend's access check does): handlers answer only
  // what its grants cover.
  grantee?: MockGrantee;
};

/** A current load a grantee's grants hold: `partial` when only some of it. */
export type MockGranteeLoad = {
  loadId: string;
  versionId: string;
  partial: boolean;
};

/**
 * A caller admitted only through source grants, as the backend's
 * access.Grantee: who it is (a principal key, "KIND_USER:usr_eve"), the
 * current loads its grants hold any of by ontology, the one an ontology's
 * route was admitted on, whether its grants cover a document set, whether a
 * load they hold holds a document in a set they cover (of `ontologyId`'s, or
 * any), what they hold of an ontology's current load, for the agent, and
 * whether they still hold what an answer read.
 */
export type MockGrantee = {
  principal: string;
  covered: Map<string, MockGranteeLoad>;
  load?: MockGranteeLoad;
  coversSet: (setId: string) => boolean;
  holds: (documentId: string, ontologyId?: string) => boolean;
  visibility: (ontologyId: string) => Visibility;
  // The part of the admitted ontology's load it holds, to keep with a
  // question; whether its grants still hold such a part.
  seen: (ontologyId: string) => MockSeen;
  stillSees: (seen: MockSeen) => boolean;
};

/** What a grantee held of a load when it read it (access.Inputs). */
export type MockSeen = {
  tables: { connectorId: string; table: string }[];
  sets: { setId: string; connectorId: string }[];
  enrichment: boolean;
  uncoverable: boolean;
};

export type MockReply = {
  status: number;
  body?: unknown;
  headers?: Record<string, string>;
};

export type MockRoute = {
  method: HttpMethod;
  // Written exactly as in contract/wfe.swagger.json or contract/foundry.swagger.json.
  path: RouteTemplate;
  // No bearer token needed (sign-in endpoints only).
  public?: boolean;
  handle: (req: MockRequest) => MockReply | Promise<MockReply>;
};

export const ok = (body: unknown = {}): MockReply => ({ status: 200, body });

// gRPC status codes and how grpc-gateway maps them to HTTP.
const grpc = {
  INVALID_ARGUMENT: [3, 400],
  NOT_FOUND: [5, 404],
  ALREADY_EXISTS: [6, 409],
  PERMISSION_DENIED: [7, 403],
  RESOURCE_EXHAUSTED: [8, 429],
  FAILED_PRECONDITION: [9, 400],
  UNIMPLEMENTED: [12, 501],
  INTERNAL: [13, 500],
  UNAVAILABLE: [14, 503],
  UNAUTHENTICATED: [16, 401],
} as const;
export type GrpcCode = keyof typeof grpc;

/** A WFE error: { code, message, details } with grpc-gateway's HTTP status. */
export function fail(code: GrpcCode, message: string): MockReply {
  const [number, status] = grpc[code];
  return { status, body: { code: number, message, details: [] } };
}

export const now = () => new Date().toISOString();

let counter = 0;
/** A fresh ID with a readable prefix, e.g. id("ws") → "ws_01k…". */
export function id(prefix: string) {
  counter += 1;
  return `${prefix}_${Date.now().toString(36)}${counter.toString(36).padStart(4, "0")}`;
}
