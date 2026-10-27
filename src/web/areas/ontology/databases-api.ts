import type {
  CreateProjectRequest,
  CreateTenantRequest,
  CreateTenantResponse,
  ListProjectsResponse,
  ListTenantsResponse,
  Project,
  Tenant,
} from "../../../shared/wfe";
import { api, ApiError } from "../../api";

// The workspace's Helix projects and their databases (tenants), read and
// created through the routes helix-website uses (the allowlist's Databases
// section). An ontology binds to one live database; a new one goes on the
// shared cluster: Foundry never offers dedicated clusters.

const enc = encodeURIComponent;
const PAGES = 20;

async function pages<P extends { nextPageToken?: string }, T>(
  path: string,
  pick: (page: P) => T[] | undefined,
): Promise<T[]> {
  const items: T[] = [];
  let token = "";
  for (let n = 0; n < PAGES; n++) {
    const page = await api<P>(
      `${path}&page.size=100${token ? `&page.token=${enc(token)}` : ""}`,
    );
    items.push(...(pick(page) || []));
    token = page.nextPageToken || "";
    if (!token) break;
  }
  return items;
}

/** The workspace's projects (ListProjects), which name an ontology's. */
export const projectsQuery = (ws: string) => ({
  queryKey: [ws, "projects"],
  queryFn: () =>
    pages<ListProjectsResponse, Project>(
      `/v1/projects?workspaceId=${enc(ws)}`,
      (p) => p.projects,
    ),
});

/** A project and its live databases, as the create form lists them. */
export type ProjectDatabases = { project: Project; databases: Tenant[] };

/**
 * Every project of the workspace with its live databases: ListProjects, then
 * ListTenants per project (the WFE lists deleted ones too).
 */
export const databasesQuery = (ws: string) => ({
  queryKey: [ws, "databases"],
  queryFn: async (): Promise<ProjectDatabases[]> => {
    const projects = await projectsQuery(ws).queryFn();
    return Promise.all(
      projects.map(async (project) => ({
        project,
        databases: (
          await pages<ListTenantsResponse, Tenant>(
            `/v1/tenants?projectId=${enc(project.id)}`,
            (p) => p.tenants,
          )
        ).filter((t) => t.status === "RESOURCE_STATUS_ACTIVE"),
      })),
    );
  },
});

export const createProject = (body: CreateProjectRequest) =>
  api<Project>("/v1/projects", body);

/**
 * A database on the shared cluster. Its slug is unique across that cluster,
 * which every workspace shares, so when it is taken (ALREADY_EXISTS) the
 * database is created once more with a short random suffix: "crm" becomes
 * "crm-3f9a1c". Helix also makes it a first key and returns its token once;
 * Foundry never keeps or shows it, and the ontology's page links to the
 * database's keys in the Helix dashboard instead.
 */
export async function createDatabase(body: CreateTenantRequest) {
  const create = async (slug: string) =>
    (await api<CreateTenantResponse>("/v1/tenants", { ...body, slug })).tenant;
  try {
    return await create(body.slug);
  } catch (e) {
    if (!(e instanceof ApiError && e.code === 6)) throw e;
    return create(
      `${body.slug.slice(0, 56).replace(/-+$/, "")}-${crypto.randomUUID().slice(0, 6)}`,
    );
  }
}
