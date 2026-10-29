import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { Connector, ConnectorSync } from "@/shared/wfe";
import type { MockReply } from "../../mock/http";
import { setFoundryPlan } from "../../mock/routes/billing";
import { dataRoutes } from "../../mock/routes/data";
import { layout, type Listed } from "../../mock/routes/data-files";
import { keyHint } from "../../mock/routes/data-sources";
import { config, db, resetState } from "../../mock/state";

// The mock WFE's Data routes behave as the WFE's connector and sync RPCs
// do, so the e2e suite and dev:mock exercise
// the same refusals and states the real backend has. Above all, a
// credential never comes back in any response.

const ACME = "ws_acme",
  GLOBEX = "ws_globex";
const compiled = dataRoutes.map((r) => ({ ...r, c: compileTemplate(r.path) }));

function call(
  method: HttpMethod,
  path: string,
  body?: unknown,
  userId = "usr_ada",
): MockReply {
  const url = new URL(path, "http://mock");
  for (const r of compiled) {
    const params = r.method === method && matchTemplate(r.c, url.pathname);
    if (!params) continue;
    const reply = r.handle({
      method,
      path: url.pathname,
      params,
      query: url.searchParams,
      body,
      principal: { user: db.users.get(userId)!, sessionId: "s" },
    });
    if (reply instanceof Promise) throw new Error("async mock route");
    return reply;
  }
  throw new Error(`no mock route for ${method} ${path}`);
}
const ok = <T>(reply: MockReply): T => {
  expect(reply.status, JSON.stringify(reply.body)).toBe(200);
  return reply.body as T;
};
const refusal = (reply: MockReply) => ({
  status: reply.status,
  message: (reply.body as { message: string }).message,
});
const base = (ws = ACME) => `/v1/workspaces/${ws}/foundry`;
const stripeKey = "sk_test_51SecretMockKeyAbcd";
const create = (config: unknown, name = "Source", ws = ACME) =>
  ok<{ connector: Connector }>(
    call("POST", `${base(ws)}/connectors`, { name, config }),
  ).connector;
const syncOf = (connectorId: string, ws = ACME) =>
  ok<{ sync: ConnectorSync }>(
    call("POST", `${base(ws)}/connectors/${connectorId}:sync`, {}),
  ).sync;
/** A Google service account's JSON key, with `extra` over its fields. */
const googleKey = (extra: Record<string, unknown> = {}) =>
  JSON.stringify({
    type: "service_account",
    project_id: "acme-data",
    private_key_id: "0123456789abcdef9f3a",
    private_key:
      "-----BEGIN PRIVATE KEY-----\nMIIEmock\n-----END PRIVATE KEY-----\n",
    client_email: "foundry@acme-data.iam.gserviceaccount.com",
    client_id: "104857600000000000000",
    ...extra,
  });
// A tested table's foreign keys, and every table's key, in a line each.
const links = (body: any, table: string) =>
  body.discovery.tables
    .find((t: { name: string }) => t.name === table)
    .foreignKeys.map(
      (f: {
        column: string;
        referencedTable: string;
        referencedColumn: string;
      }) => `${f.column}>${f.referencedTable}.${f.referencedColumn}`,
    );
const keys = (body: any) =>
  Object.fromEntries(
    body.discovery.tables.map((t: { name: string; primaryKey: string[] }) => [
      t.name,
      t.primaryKey.join("+"),
    ]),
  );
const pg = (extra: object = {}) => ({
  postgres: {
    host: "db.example.com",
    database: "app",
    user: "ro",
    password: "pw",
    ...extra,
  },
});

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-10-18T12:00:00Z"));
  resetState();
  // Globex: a Free workspace where Ada is a member, and one she is not in.
  db.workspaces.set(GLOBEX, { id: GLOBEX, slug: "globex" });
  db.memberships.push({
    id: "m",
    workspaceId: GLOBEX,
    userId: "usr_ada",
    role: "WORKSPACE_ROLE_ADMIN",
    createdAt: "",
  });
  db.workspaces.set("ws_other", { id: "ws_other", slug: "other" });
});
afterEach(() => vi.useRealTimers());

describe("connector kinds", () => {
  it("offers every kind, the database and file ones only on Foundry Pro", () => {
    // Slack, Notion and Google Workspace are on every plan, as the other
    // apps are.
    // API sources (events) are on every plan.
    const kinds = (ws: string) =>
      ok<{ kinds: { kind: string; available: boolean }[] }>(
        call("GET", `${base(ws)}/connector-kinds`),
      ).kinds.map((k) => `${k.kind}:${k.available}`);
    expect(kinds(ACME)).toEqual([
      "stripe:true",
      "workos:true",
      "posthog:true",
      "github:true",
      "linear:true",
      "calcom:true",
      "granola:true",
      "slack:true",
      "notion:true",
      "google:true",
      "postgres:true",
      "mysql:true",
      "s3:true",
      "events:true",
    ]);
    expect(kinds(GLOBEX)).toEqual([
      "stripe:true",
      "workos:true",
      "posthog:true",
      "github:true",
      "linear:true",
      "calcom:true",
      "granola:true",
      "slack:true",
      "notion:true",
      "google:true",
      "postgres:false",
      "mysql:false",
      "s3:false",
      "events:true",
    ]);
    setFoundryPlan(GLOBEX, "FOUNDRY_TIER_PRO");
    expect(kinds(GLOBEX)).toEqual(
      expect.arrayContaining(["mysql:true", "s3:true"]),
    );
    // Max grants what Pro grants.
    setFoundryPlan(GLOBEX, "FOUNDRY_TIER_MAX");
    expect(kinds(GLOBEX)).toEqual(
      expect.arrayContaining(["postgres:true", "mysql:true", "s3:true"]),
    );
    setFoundryPlan(GLOBEX, "FOUNDRY_TIER_FREE");
    expect(kinds(GLOBEX)).toEqual(
      expect.arrayContaining(["mysql:false", "s3:false"]),
    );
  });

  it("gives s3 the role setup of the workspace, unless role access is off", () => {
    const setups = (ws: string) =>
      Object.fromEntries(
        ok<{ kinds: { kind: string; s3?: unknown }[] }>(
          call("GET", `${base(ws)}/connector-kinds`),
        ).kinds.map((k) => [k.kind, k.s3]),
      );
    expect(setups(ACME)).toEqual({
      stripe: undefined,
      workos: undefined,
      posthog: undefined,
      postgres: undefined,
      mysql: undefined,
      s3: {
        principalArn:
          "arn:aws:iam::000000000000:role/helix-mock-foundry-connector",
        externalId: "helix-foundry-ws_acme",
      },
    });
    // Each workspace has its own external ID.
    expect(setups(GLOBEX).s3).toMatchObject({
      externalId: "helix-foundry-ws_globex",
    });
    Object.assign(config, { s3RoleAccess: false });
    expect(setups(ACME).s3).toBeUndefined();
    // A reset turns it back on.
    resetState();
    expect(setups(ACME).s3).toBeDefined();
  });
});

describe("testing and creating", () => {
  it("tests a Stripe key without echoing it", () => {
    const reply = call("POST", `${base()}/connectors:test`, {
      config: { stripe: { apiKey: ` ${stripeKey} ` } },
    });
    const body = ok<any>(reply);
    expect(body.config).toEqual({ stripe: { apiKey: "" } });
    expect(body.discovery.tables).toHaveLength(15);
    expect(body.discovery.unavailable).toEqual([]);
    expect(JSON.stringify(body)).not.toContain(stripeKey);
  });

  it("lists what a restricted key cannot read and drops keys into it", () => {
    const body = ok<any>(
      call("POST", `${base()}/connectors:test`, {
        config: { stripe: { apiKey: "rk_live_restricted1" } },
      }),
    );
    expect(body.discovery.unavailable).toEqual([
      "disputes",
      "payouts",
      "balance_transactions",
    ]);
    const charges = body.discovery.tables.find(
      (t: { name: string }) => t.name === "charges",
    );
    expect(
      charges.foreignKeys.map((f: { column: string }) => f.column),
    ).toEqual(["customer_id", "invoice_id", "payment_intent_id"]);
  });

  it("refuses malformed settings and rejected credentials with the WFE's messages", () => {
    const test = (config: unknown) =>
      refusal(call("POST", `${base()}/connectors:test`, { config }));
    expect(test(undefined)).toEqual({
      status: 400,
      message: "config is required",
    });
    expect(test({ stripe: {}, workos: {} }).message).toBe("config is required");
    expect(test({ stripe: { apiKey: "pk_test_1" } }).message).toBe(
      "enter a Stripe secret or restricted key (sk_… or rk_…)",
    );
    expect(test({ stripe: { apiKey: "sk_test_badkey" } })).toEqual({
      status: 400,
      message: "Stripe rejected this key; check it and test again",
    });
    expect(test({ workos: { apiKey: "nope" } }).message).toBe(
      "enter a WorkOS API key (sk_…)",
    );
    expect(test({ workos: { apiKey: "sk_bad_1" } }).message).toBe(
      "WorkOS rejected this key; check it and test again",
    );
    expect(test({ posthog: { apiKey: "x" } }).message).toBe(
      "enter a PostHog personal API key (phx_…)",
    );
    expect(test({ posthog: { apiKey: "phx_bad" } }).message).toBe(
      "PostHog rejected this key; check it and the region, then test again",
    );
    expect(test({ posthog: { apiKey: "phx_1", region: "mars" } }).message).toBe(
      "PostHog region must be us, eu or custom",
    );
    expect(
      test({ posthog: { apiKey: "phx_1", region: "us", host: "https://x" } })
        .message,
    ).toBe("a PostHog host is only used with the custom region");
    for (const host of ["http://ph.example.com", "nonsense", "https://u:p@h"])
      expect(
        test({ posthog: { apiKey: "phx_1", region: "custom", host } }).message,
      ).toBe("enter the HTTPS address of your PostHog, without credentials");
    expect(
      test({ posthog: { apiKey: "phx_1", projectId: "abc" } }).message,
    ).toBe("the PostHog project ID is a number, found in project settings");
    expect(test({ posthog: { apiKey: "phx_1", eventDays: 400 } }).message).toBe(
      "PostHog event days must be between 1 and 365",
    );
    expect(test({ posthog: { apiKey: "phx_multi_1" } }).message).toBe(
      "enter a PostHog project ID: Web (12345), Mobile (67890)",
    );
    const pat = "github_pat_" + "a".repeat(22);
    for (const token of ["", "ghp_short", "glpat_" + "a".repeat(22)])
      expect(test({ github: { token } }).message).toBe(
        "enter a GitHub personal access token (github_pat_… or ghp_…)",
      );
    expect(test({ github: { token: "ghp_bad" + "a".repeat(20) } })).toEqual({
      status: 400,
      message: "GitHub rejected this token; check it and enter it again",
    });
    expect(
      test({ github: { token: pat, repos: ["https://github.com/acme/api"] } })
        .message,
    ).toBe(
      '"https://github.com/acme/api" is not a repository; write it as owner/name',
    );
    expect(
      test({
        github: {
          token: pat,
          repos: Array.from({ length: 101 }, (_, i) => `acme/r${i}`),
        },
      }).message,
    ).toBe("choose at most 100 repositories");
    expect(test({ github: { token: pat, reviewDays: 366 } }).message).toBe(
      "review days must be between 1 and 365",
    );
    expect(test({ github: { token: pat, reviewDays: -1 } }).message).toBe(
      "review days must be between 1 and 365",
    );
    expect(
      test({ github: { token: pat, repos: ["acme/api", "acme/missing"] } })
        .message,
    ).toBe("GitHub has no repository acme/missing that this token can read");
    expect(test({ linear: { apiKey: "lin_oauth_1" } }).message).toBe(
      "enter a Linear personal API key (lin_api_…)",
    );
    expect(test({ linear: { apiKey: "lin_api_bad1" } }).message).toBe(
      "Linear rejected this key; check it and enter it again",
    );
    for (const apiKey of ["", "live_1", "cal-live-1", "cal_live_1 2"])
      expect(test({ calcom: { apiKey } }).message).toBe(
        "enter a Cal.com API key (cal_live_… or cal_…)",
      );
    expect(test({ calcom: { apiKey: "cal_live_bad1" } })).toEqual({
      status: 400,
      message: "Cal.com rejected this key; check it and enter it again",
    });
    for (const apiKey of ["", "grn_", "grn-1", "grn_a 1", "lin_api_1"])
      expect(test({ granola: { apiKey } }).message).toBe(
        "enter a Granola API key (grn_…)",
      );
    expect(test({ granola: { apiKey: "grn_bad1" } })).toEqual({
      status: 400,
      message: "Granola rejected this key; check it and enter it again",
    });
  });

  it("lists what a Cal.com key without teams and a personal Granola key can't read, and drops keys into it", () => {
    const tested = (config: unknown) =>
      ok<any>(call("POST", `${base()}/connectors:test`, { config }));

    const cal = tested({ calcom: { apiKey: " cal_live_1 " } });
    expect(cal.config).toEqual({ calcom: { apiKey: "" } });
    expect(cal.discovery.unavailable).toEqual([]);
    expect(keys(cal)).toEqual({
      users: "user_id",
      teams: "team_id",
      team_memberships: "membership_id",
      event_types: "event_type_id",
      schedules: "schedule_id",
      bookings: "booking_uid",
      booking_attendees: "booking_uid+email",
      booking_hosts: "booking_uid+user_id",
    });
    expect(links(cal, "users")).toEqual([
      "default_schedule_id>schedules.schedule_id",
      "organization_id>teams.team_id",
    ]);
    expect(links(cal, "event_types")).toEqual([
      "owner_id>users.user_id",
      "team_id>teams.team_id",
      "parent_event_type_id>event_types.event_type_id",
      "schedule_id>schedules.schedule_id",
    ]);
    expect(links(cal, "bookings")).toEqual([
      "event_type_id>event_types.event_type_id",
      "rescheduled_from_uid>bookings.booking_uid",
      "rescheduled_to_uid>bookings.booking_uid",
    ]);
    expect(cal.discovery.warnings).toEqual([]);
    const member = tested({ calcom: { apiKey: "cal_live_member1" } });
    expect(member.discovery.warnings).toEqual([
      expect.stringContaining("not an admin of its Cal.com teams"),
    ]);
    const solo = tested({ calcom: { apiKey: "cal_live_solo1" } });
    expect(solo.discovery.unavailable).toEqual(["teams", "team_memberships"]);
    expect(links(solo, "users")).toEqual([
      "default_schedule_id>schedules.schedule_id",
    ]);
    expect(links(solo, "event_types")).toEqual([
      "owner_id>users.user_id",
      "parent_event_type_id>event_types.event_type_id",
      "schedule_id>schedules.schedule_id",
    ]);

    // Granola's keys may hold hyphens, as the WFE's check allows.
    const notes = tested({ granola: { apiKey: "grn_a-1" } });
    expect(notes.config).toEqual({
      granola: { apiKey: "", transcripts: false },
    });
    expect(notes.discovery.unavailable).toEqual([]);
    expect(keys(notes)).toEqual({
      notes: "note_id",
      note_attendees: "note_id+email",
      note_folders: "note_id+folder_id",
      folders: "folder_id",
      spaces: "space_id",
    });
    expect(links(notes, "folders")).toEqual([
      "parent_folder_id>folders.folder_id",
      "space_id>spaces.space_id",
    ]);
    expect(links(notes, "note_folders")).toEqual([
      "note_id>notes.note_id",
      "folder_id>folders.folder_id",
    ]);
    const personal = tested({ granola: { apiKey: "grn_personal1" } });
    expect(personal.discovery.unavailable).toEqual(["spaces"]);
    expect(links(personal, "folders")).toEqual([
      "parent_folder_id>folders.folder_id",
    ]);
  });

  it("reads GitHub's repositories once each, its default review days and what the token can't read", () => {
    const tested = ok<any>(
      call("POST", `${base()}/connectors:test`, {
        config: {
          github: {
            token: "ghp_nopr" + "a".repeat(20),
            repos: [" acme/api ", "ACME/API", "acme/web"],
          },
        },
      }),
    );
    expect(tested.config).toEqual({
      github: { token: "", repos: ["acme/api", "acme/web"], reviewDays: 90 },
    });
    expect(tested.discovery.unavailable).toEqual([
      "pull_requests",
      "pull_request_reviews",
    ]);
    // A link to an unreadable table is left out.
    expect(
      tested.discovery.tables
        .find((t: { name: string }) => t.name === "issue_comments")
        .foreignKeys.map((f: { referencedTable: string }) => f.referencedTable),
    ).toEqual(["issues", "repositories", "users"]);
    const full = ok<any>(
      call("POST", `${base()}/connectors:test`, {
        config: { github: { token: "github_pat_" + "b".repeat(22) } },
      }),
    );
    expect(full.config.github.repos).toEqual([]);
    expect(full.discovery.unavailable).toEqual([]);
    expect(
      full.discovery.tables
        .find((t: { name: string }) => t.name === "pull_requests")
        .foreignKeys.map(
          (f: { column: string; referencedTable: string }) =>
            `${f.column}>${f.referencedTable}`,
        ),
    ).toEqual([
      "issue_key>issues",
      "repository_id>repositories",
      "milestone_id>milestones",
      "user_id>users",
    ]);
  });

  it("syncs Cal.com and Granola tables with their key hints, never the key", () => {
    const calKey = "cal_live_" + "a".repeat(24) + "4f2a",
      grnKey = "grn_" + "b".repeat(24) + "QRST";
    const cal = create({ calcom: { apiKey: calKey } });
    expect(cal.secretHint).toBe("cal_live…4f2a");
    expect(cal.config).toEqual({ calcom: { apiKey: "" } });
    // A key without a mode keeps only its type.
    expect(
      create({ calcom: { apiKey: "cal_" + "c".repeat(12) + "WXYZ" } })
        .secretHint,
    ).toBe("cal…WXYZ");
    const grn = create({ granola: { apiKey: grnKey } });
    expect(grn.secretHint).toBe("grn…QRST");
    expect(grn.config).toEqual({ granola: { apiKey: "", transcripts: false } });
    const solo = create({
      calcom: { apiKey: "cal_live_solo" + "d".repeat(8) },
    });
    for (const c of [cal, grn, solo]) syncOf(c.id);
    vi.advanceTimersByTime(3000);
    const read = (id: string) =>
      ok<{ connector: Connector }>(call("GET", `${base()}/connectors/${id}`))
        .connector;
    expect(read(cal.id).tables!.map((t) => t.name)).toEqual([
      "booking_attendees",
      "booking_hosts",
      "bookings",
      "event_types",
      "schedules",
      "team_memberships",
      "teams",
      "users",
    ]);
    expect(read(solo.id).tables!.map((t) => t.name)).not.toContain("teams");
    expect(read(grn.id).tables!.map((t) => t.name)).toEqual([
      "folders",
      "note_attendees",
      "note_folders",
      "notes",
      "spaces",
    ]);
    const listed = JSON.stringify(call("GET", `${base()}/connectors`).body);
    expect(listed).not.toContain(calKey);
    expect(listed).not.toContain(grnKey);
  });

  it("checks Slack's, Notion's and Google Workspace's settings and refuses their rejected credentials", () => {
    const test = (config: unknown) =>
      refusal(call("POST", `${base()}/connectors:test`, { config }));
    for (const botToken of ["", "xoxp-1-2", "xoxb_1", "xoxb-1 2", "xoxb-"])
      expect(test({ slack: { botToken } }).message).toBe(
        "enter a Slack bot token (xoxb-…)",
      );
    for (const historyDays of [366, -1])
      expect(test({ slack: { botToken: "xoxb-1", historyDays } }).message).toBe(
        "keep 1 to 365 days of history",
      );
    expect(test({ slack: { botToken: "xoxb-1-bad" } })).toEqual({
      status: 400,
      message: "Slack rejected this token; check it and enter it again",
    });
    for (const token of ["", "ntn_", "secret-1", "ntn_a-1", "lin_api_1"])
      expect(test({ notion: { token } }).message).toBe(
        "enter a Notion integration secret (ntn_…)",
      );
    expect(test({ notion: { token: "secret_bad1" } })).toEqual({
      status: 400,
      message: "Notion rejected this secret; check it and enter it again",
    });
    // Nothing shared is a setting to fix in Notion, said as the WFE says it.
    expect(test({ notion: { token: "ntn_empty1" } })).toEqual({
      status: 400,
      message:
        "nothing is shared with this integration yet: in Notion, add it to a page or database from its ••• menu › Connections, then test again",
    });
    const subject = "ada@example.com";
    for (const serviceAccountKey of [
      "",
      "not json",
      "[]",
      googleKey({ type: "authorized_user" }),
      googleKey({ client_email: "" }),
      googleKey({ private_key: undefined }),
    ])
      expect(test({ google: { serviceAccountKey, subject } }).message).toBe(
        "paste the service account's JSON key",
      );
    for (const s of ["", "ada", "ada@example", "a da@example.com"])
      expect(
        test({ google: { serviceAccountKey: googleKey(), subject: s } })
          .message,
      ).toBe("enter the email of the Workspace user Foundry acts as");
    expect(
      test({
        google: { serviceAccountKey: googleKey(), subject, eventDays: 400 },
      }).message,
    ).toBe("keep 1 to 365 days of past events");
    expect(
      test({
        google: {
          serviceAccountKey: googleKey({ private_key: "bad" }),
          subject,
        },
      }),
    ).toEqual({
      status: 400,
      message:
        "Google rejected this service account key; check it and paste it again",
    });
    expect(
      test({
        google: {
          serviceAccountKey: googleKey(),
          subject: "nobody@example.com",
        },
      }),
    ).toEqual({
      status: 400,
      message:
        "Google has no user nobody@example.com, or the service account may not act as them; check the email",
    });
  });

  it("lists Slack's tables and says which scope a token lacks, or that the app is in no channel", () => {
    const tested = (botToken: string) =>
      ok<any>(
        call("POST", `${base()}/connectors:test`, {
          config: { slack: { botToken } },
        }),
      );
    const all = tested(" xoxb-1-2-abcd ");
    // Its days default to 90 and the token is never returned.
    expect(all.config).toEqual({ slack: { botToken: "", historyDays: 90 } });
    expect(keys(all)).toEqual({
      users: "user_id",
      channels: "channel_id",
      channel_members: "channel_id+user_id",
      messages: "message_id",
      channel_days: "channel_day_id",
    });
    expect(links(all, "channels")).toEqual(["creator_id>users.user_id"]);
    expect(links(all, "channel_members")).toEqual([
      "channel_id>channels.channel_id",
      "user_id>users.user_id",
    ]);
    expect(links(all, "messages")).toEqual([
      "channel_id>channels.channel_id",
      "user_id>users.user_id",
    ]);
    expect(links(all, "channel_days")).toEqual([
      "channel_id>channels.channel_id",
    ]);
    expect(all.discovery.unavailable).toEqual([]);
    expect(all.discovery.warnings).toEqual([]);
    // Without the history scope of its channels, the history's tables say
    // which scope to add; the members need only the channel lists.
    const noscope = tested("xoxb-1-noscope");
    expect(noscope.discovery.unavailable).toEqual(
      ["messages", "channel_days"].map(
        (t) =>
          `${t}: add the channels:history scope to the app and reinstall it`,
      ),
    );
    expect(Object.keys(keys(noscope))).toEqual([
      "users",
      "channels",
      "channel_members",
    ]);
    expect(noscope.discovery.warnings).toEqual([]);
    // An app in no channel reads every table, with a warning to invite it.
    const lonely = tested("xoxb-1-lonely");
    expect(lonely.discovery.unavailable).toEqual([]);
    expect(lonely.discovery.warnings).toEqual([
      "The app is in no channels yet: invite it to each channel to sync with /invite @Foundry, then test again",
    ]);
    expect(
      ok<any>(
        call("POST", `${base()}/connectors:test`, {
          config: { slack: { botToken: "xoxb-1", historyDays: 7 } },
        }),
      ).config.slack.historyDays,
    ).toBe(7);
  });

  it("lists Notion's users, pages and each shared database with its typed columns and first rows", () => {
    const tested = (token: string) =>
      ok<any>(
        call("POST", `${base()}/connectors:test`, {
          config: { notion: { token } },
        }),
      );
    const all = tested("ntn_team1234");
    expect(all.config).toEqual({ notion: { token: "" } });
    expect(keys(all)).toEqual({
      users: "user_id",
      pages: "page_id",
      customer_accounts: "page_id",
      projects: "page_id",
    });
    expect(links(all, "pages")).toEqual([
      "created_by_id>users.user_id",
      "last_edited_by_id>users.user_id",
    ]);
    const columns = (body: any, table: string) =>
      body.discovery.tables
        .find((t: { name: string }) => t.name === table)
        .columns.map(
          (c: { name: string; type: string; nullable: boolean }) =>
            `${c.name}:${c.type}${c.nullable ? "?" : ""}`,
        );
    // Every table is typed by the JSON it holds; times are strings.
    expect(columns(all, "users")).toEqual([
      "user_id:string",
      "type:string",
      "name:string?",
      "email:string?",
    ]);
    expect(columns(all, "pages")).toContain("created_by_id:string?");
    // A database's own columns, then its properties in name order, a
    // date's end beside it.
    expect(columns(all, "customer_accounts")).toEqual([
      "page_id:string",
      "url:string?",
      "created_at:string",
      "last_edited_at:string",
      "arr:number?",
      "active:boolean?",
      "name:string?",
      "owner:array?",
      "renewal_date:string?",
      "renewal_date_end:string?",
      "stage:string?",
    ]);
    const accounts = all.discovery.tables.find(
      (t: { name: string }) => t.name === "customer_accounts",
    );
    expect(accounts.preview).toHaveLength(5);
    expect(JSON.parse(accounts.preview[0])).toMatchObject({
      name: "Globex",
      stage: "Customer",
      arr: 48000,
      renewal_date: "2026-11-30",
      renewal_date_end: null,
      owner: ["6f1c0001-2b3a-4c5d-9e8f-1a2b3c4d5e6f"],
      active: true,
    });
    // Users and pages show no rows at a test.
    expect(all.discovery.tables[0].preview).toEqual([]);
    const nousers = tested("secret_nousers1");
    expect(nousers.discovery.unavailable).toEqual([
      "users: turn on Read user information in the integration's capabilities",
    ]);
    expect(links(nousers, "pages")).toEqual([]);
  });

  it("lists Google Workspace's tables, and what a member or an undelegated scope can't read", () => {
    const tested = (subject: string, email?: string) =>
      ok<any>(
        call("POST", `${base()}/connectors:test`, {
          config: {
            google: {
              serviceAccountKey: googleKey(
                email ? { client_email: email } : {},
              ),
              subject: ` ${subject} `,
            },
          },
        }),
      );
    const all = tested("ada@example.com");
    // The key is never returned; the subject is kept trimmed and in lower
    // case, and the days default to 90.
    expect(all.config).toEqual({
      google: {
        serviceAccountKey: "",
        subject: "ada@example.com",
        eventDays: 90,
      },
    });
    expect(tested("Ada@Example.com").config.google.subject).toBe(
      "ada@example.com",
    );
    expect(keys(all)).toEqual({
      users: "user_id",
      files: "file_id",
      calendars: "calendar_id",
      calendar_events: "event_id",
      event_attendees: "event_id+email",
    });
    expect(links(all, "files")).toEqual([]);
    expect(links(all, "calendar_events")).toEqual([
      "calendar_id>calendars.calendar_id",
    ]);
    expect(links(all, "event_attendees")).toEqual([
      "event_id>calendar_events.event_id",
    ]);
    expect(all.discovery.unavailable).toEqual([]);
    expect(tested("ada.member@example.com").discovery.unavailable).toEqual([
      "users: ada.member@example.com is not an admin, so the Directory's users can't be read",
    ]);
    const nocalendar = tested(
      "ada@example.com",
      "nocalendar@acme-data.iam.gserviceaccount.com",
    );
    expect(nocalendar.discovery.unavailable).toEqual(
      ["calendars", "calendar_events", "event_attendees"].map(
        (t) =>
          `${t}: allow the scope https://www.googleapis.com/auth/calendar.readonly for the service account's client ID in the Admin console's domain-wide delegation`,
      ),
    );
    expect(Object.keys(keys(nocalendar))).toEqual(["users", "files"]);
  });

  it("syncs Slack, Notion and Google Workspace with their hints, never the credential", () => {
    const slackToken = "xoxb-1234-5678-" + "a".repeat(16) + "WXYZ",
      notionToken = "ntn_" + "b".repeat(20) + "QRST",
      key = googleKey();
    const slack = create({ slack: { botToken: slackToken, historyDays: 30 } }),
      lonely = create({ slack: { botToken: "xoxb-lonely-" + "c".repeat(8) } }),
      noscope = create({
        slack: { botToken: "xoxb-noscope-" + "e".repeat(8) },
      }),
      notion = create({ notion: { token: notionToken } }),
      secret = create({ notion: { token: "secret_" + "d".repeat(12) } }),
      google = create({
        google: { serviceAccountKey: key, subject: "ada@example.com" },
      });
    expect(slack.secretHint).toBe("xoxb…WXYZ");
    expect(slack.config).toEqual({
      slack: { botToken: "", historyDays: 30 },
    });
    expect(notion.secretHint).toBe("ntn…QRST");
    expect(secret.secretHint).toBe("secret…dddd");
    // The key's service account and its key id's last four characters.
    expect(google.secretHint).toBe(
      "foundry@acme-data.iam.gserviceaccount.com…9f3a",
    );
    for (const c of [slack, lonely, noscope, notion, google]) syncOf(c.id);
    vi.advanceTimersByTime(3000);
    const rows = (id: string) =>
      Object.fromEntries(
        ok<{ connector: Connector }>(
          call("GET", `${base()}/connectors/${id}`),
        ).connector.tables!.map((t) => [t.name, Number(t.rowCount)]),
      );
    // 30 days of history: the 30 before the sync's, two of every three of
    // them with messages in each of four channels, two messages a day (a
    // thread's reply is no row).
    const days = rows(slack.id).channel_days!;
    expect(days).toBe(80);
    expect(rows(slack.id)).toEqual({
      channel_days: days,
      channel_members: 31,
      channels: 5,
      messages: days * 2,
      users: 18,
    });
    // Without the history scope, the members are still read.
    expect(rows(noscope.id)).toEqual({
      channel_members: 31,
      channels: 5,
      users: 18,
    });
    // An app in no channel reads no history.
    expect(rows(lonely.id)).toEqual({
      channel_days: 0,
      channel_members: 0,
      channels: 5,
      messages: 0,
      users: 18,
    });
    // Pages holds every page, the databases' rows among them.
    expect(rows(notion.id)).toEqual({
      customer_accounts: 38,
      pages: 74 + 38 + 21,
      projects: 21,
      users: 12,
    });
    expect(rows(google.id)).toEqual({
      calendar_events: 684,
      calendars: 6,
      event_attendees: 2140,
      files: 312,
      users: 26,
    });
    // An event says when Google left its attendees out.
    expect(
      ok<{ connector: Connector }>(
        call("GET", `${base()}/connectors/${google.id}`),
      )
        .connector.tables!.find((t) => t.name === "calendar_events")!
        .columns!.map((c) => c.name)
        .slice(-2),
    ).toEqual(["attendee_count", "attendees_omitted"]);
    // A Notion database keeps its typed columns once synced.
    const synced = ok<{ connector: Connector }>(
      call("GET", `${base()}/connectors/${notion.id}`),
    ).connector.tables!.find((t) => t.name === "projects")!;
    expect(synced.columns!.slice(4).map((c) => `${c.name}:${c.type}`)).toEqual([
      "due:string",
      "due_end:string",
      "lead:array",
      "name:string",
      "status:string",
      "tags:array",
    ]);
    const listed = JSON.stringify(call("GET", `${base()}/connectors`).body);
    for (const credential of [slackToken, notionToken, key, "PRIVATE KEY"])
      expect(listed).not.toContain(credential);
  });

  it("keeps a key's type and last four characters, never more of it", () => {
    expect(keyHint("cal_live_0123456789ab", 2)).toBe("cal_live…89ab");
    expect(keyHint("cal_0123456789ab", 2)).toBe("cal…89ab");
    expect(keyHint("sk_0123456789ab", 2)).toBe("sk…89ab");
    expect(keyHint("0123456789abcdef", 2)).toBe("…cdef");
    expect(keyHint("cal_short", 2)).toBe("…");
  });

  it("syncs GitHub and Linear tables with their key hints", () => {
    const gh = create({
      github: {
        token: "github_pat_" + "c".repeat(18) + "WXYZ",
        repos: ["acme/api"],
      },
    });
    expect(gh.secretHint).toBe("github_pat…WXYZ");
    expect(gh.config).toEqual({
      github: { token: "", repos: ["acme/api"], reviewDays: 90 },
    });
    const classic = create({
      github: { token: "ghp_" + "d".repeat(16) + "ABCD" },
    });
    expect(classic.secretHint).toBe("ghp…ABCD");
    const lin = create({
      linear: { apiKey: "lin_api_" + "e".repeat(8) + "QRST" },
    });
    expect(lin.secretHint).toBe("lin_api…QRST");
    expect(lin.config).toEqual({ linear: { apiKey: "" } });
    syncOf(gh.id);
    syncOf(lin.id);
    vi.advanceTimersByTime(3000);
    const tables = (id: string) =>
      ok<{ connector: Connector }>(
        call("GET", `${base()}/connectors/${id}`),
      ).connector.tables!.map((t) => t.name);
    expect(tables(gh.id)).toEqual([
      "issue_comments",
      "issues",
      "labels",
      "milestones",
      "pull_request_reviews",
      "pull_requests",
      "repositories",
      "users",
    ]);
    expect(tables(lin.id)).toEqual([
      "comments",
      "cycles",
      "issue_labels",
      "issues",
      "projects",
      "teams",
      "users",
      "workflow_states",
    ]);
  });

  it("resolves the PostHog project the key sees and keeps a custom host's origin", () => {
    expect(
      ok<any>(
        call("POST", `${base()}/connectors:test`, {
          config: { posthog: { apiKey: "phx_1" } },
        }),
      ).config,
    ).toEqual({
      posthog: {
        apiKey: "",
        region: "us",
        host: "",
        projectId: "12345",
        eventDays: 30,
      },
    });
    expect(
      ok<any>(
        call("POST", `${base()}/connectors:test`, {
          config: {
            posthog: {
              apiKey: "phx_1",
              region: "custom",
              host: "https://ph.example.com/path",
              projectId: "9",
            },
          },
        }),
      ).config.posthog,
    ).toMatchObject({ host: "https://ph.example.com", projectId: "9" });
  });

  it("checks database settings and refuses what the guard refuses", () => {
    const test = (config: unknown) =>
      refusal(call("POST", `${base()}/connectors:test`, { config }));
    expect(test(pg({ host: "" })).message).toBe(
      "enter the database server's host name or IP address",
    );
    expect(test(pg({ host: "db example" })).message).toBe(
      "enter the database server's host name or IP address",
    );
    expect(test(pg({ database: " " })).message).toBe("enter the database name");
    expect(test(pg({ user: "" })).message).toBe("enter the database user");
    expect(test(pg({ password: "" })).message).toBe(
      "enter the database password",
    );
    expect(test(pg({ port: 70000 })).message).toBe(
      "the database port must be between 1 and 65535",
    );
    expect(test(pg({ ca: "not pem" })).message).toBe(
      "the CA certificate is not a PEM certificate",
    );
    expect(test({ mysql: { ...pg().postgres, schema: "x" } }).message).toBe(
      "this database has no schema setting; use the database name",
    );
    for (const host of [
      "10.0.0.1",
      "localhost",
      "192.168.1.4",
      "169.254.169.254",
    ])
      expect(test(pg({ host }))).toEqual({
        status: 400,
        message:
          "that host is not on the public internet; Helix can only reach public database hosts",
      });
    expect(test(pg({ host: "down.invalid" })).message).toBe(
      "could not connect; check the host, port and connection settings, then test again",
    );
    expect(test(pg({ password: "wrong" })).message).toBe(
      "authentication failed; check the username, password and read permissions, then test again",
    );
    expect(test(pg({ tables: ["orders", "nope"] })).message).toBe(
      'table "nope" is not in this database, or the user cannot read it',
    );
  });

  it("discovers typed tables with keys, sorted, and none in another schema", () => {
    const tables = (config: unknown) =>
      ok<any>(call("POST", `${base()}/connectors:test`, { config })).discovery
        .tables;
    const found = tables(pg());
    expect(found.map((t: { name: string }) => t.name)).toEqual([
      "accounts",
      "order_items",
      "orders",
      "people",
      "products",
      "users",
    ]);
    expect(found[1]).toMatchObject({
      primaryKey: ["order_id", "line"],
      foreignKeys: [
        {
          column: "order_id",
          referencedTable: "orders",
          referencedColumn: "id",
        },
        {
          column: "product_id",
          referencedTable: "products",
          referencedColumn: "id",
        },
      ],
    });
    expect(found[0].columns[0]).toEqual({
      name: "id",
      type: "uuid",
      nullable: false,
    });
    expect(tables({ mysql: pg().postgres })[0].columns[0].type).toBe(
      "char(36)",
    );
    expect(tables(pg({ schema: "analytics" }))).toEqual([]);
  });

  it("gates the database kinds on Foundry Pro with the billing reason", () => {
    const reply = call("POST", `${base(GLOBEX)}/connectors`, {
      name: "Shop",
      config: pg(),
    });
    expect(reply.status).toBe(400);
    expect(reply.body).toMatchObject({
      code: 9,
      message: "PostgreSQL connectors need Foundry Pro",
      details: [{ reason: "FOUNDRY_PRO_REQUIRED" }],
    });
    expect(
      refusal(
        call("POST", `${base(GLOBEX)}/connectors:test`, {
          config: { mysql: pg().postgres },
        }),
      ).message,
    ).toBe("MySQL connectors need Foundry Pro");
  });

  it("creates a connector whose responses never carry the credential", () => {
    expect(
      refusal(
        call("POST", `${base()}/connectors`, {
          name: " ",
          config: { stripe: { apiKey: stripeKey } },
        }),
      ).message,
    ).toBe("name must be 1 to 120 characters");
    expect(
      call("POST", `${base()}/connectors`, {
        name: "x".repeat(121),
        config: { stripe: { apiKey: stripeKey } },
      }).status,
    ).toBe(400);
    const created = call("POST", `${base()}/connectors`, {
      name: " Billing ",
      config: { stripe: { apiKey: stripeKey } },
    });
    const body = ok<any>(created);
    expect(body.connector).toMatchObject({
      name: "Billing",
      kind: "stripe",
      secretHint: "sk_test…Abcd",
      tables: [],
      lastSync: null,
      config: { stripe: { apiKey: "" } },
    });
    expect(body.discovery.tables).toHaveLength(15);
    const db = create(pg({ password: "s3cret-pw" }), "Shop");
    expect(db.secretHint).toBe("password set");
    expect(db.config).toEqual({
      postgres: {
        host: "db.example.com",
        port: 5432,
        database: "app",
        user: "ro",
        password: "",
        schema: "public",
        tables: [],
        disableTls: false,
        ca: "",
      },
    });
    const everything = JSON.stringify([
      call("GET", `${base()}/connectors`).body,
      call("GET", `${base()}/connectors/${db.id}`).body,
      call("GET", `${base()}/connectors/${body.connector.id}`).body,
    ]);
    expect(everything).not.toContain(stripeKey);
    expect(everything).not.toContain("s3cret-pw");
  });
});

describe("syncs", () => {
  it("queues, runs and writes each table's newest snapshot", () => {
    const c = create(pg({ tables: ["orders", "users"] }));
    const queued = syncOf(c.id);
    expect(queued).toMatchObject({
      status: "CONNECTOR_SYNC_STATUS_QUEUED",
      attempts: 0,
      startedAt: null,
      finishedAt: null,
      rowsSynced: "0",
      error: "",
    });
    expect(
      refusal(call("POST", `${base()}/connectors/${c.id}:sync`, {})),
    ).toEqual({
      status: 409,
      message: "a sync of this connector is already queued or running",
    });
    const get = () =>
      ok<{ sync: ConnectorSync }>(
        call("GET", `${base()}/connector-syncs/${queued.id}`),
      ).sync;
    vi.advanceTimersByTime(1000);
    expect(get()).toMatchObject({
      status: "CONNECTOR_SYNC_STATUS_RUNNING",
      attempts: 1,
    });
    expect(get().startedAt).toBeTruthy();
    vi.advanceTimersByTime(2000);
    expect(get()).toMatchObject({
      status: "CONNECTOR_SYNC_STATUS_SUCCEEDED",
      rowsSynced: String(9604 + 2318),
    });
    const after = ok<{ connector: Connector }>(
      call("GET", `${base()}/connectors/${c.id}`),
    ).connector;
    expect(after.lastSync?.id).toBe(queued.id);
    expect(after.tables?.map((t) => [t.name, t.rowCount, t.syncId])).toEqual([
      ["orders", "9604", queued.id],
      ["users", "2318", queued.id],
    ]);
    expect(after.tables?.[0]?.columns?.[2]).toEqual({
      name: "user_id",
      type: "uuid",
      nullable: true,
    });
  });

  it("gives API tables the column names found in their rows", () => {
    const c = create({ workos: { apiKey: "sk_live_workos1" } });
    syncOf(c.id);
    vi.advanceTimersByTime(3000);
    const tables = ok<{ connector: Connector }>(
      call("GET", `${base()}/connectors/${c.id}`),
    ).connector.tables!;
    expect(tables).toHaveLength(9);
    const members = tables.find((t) => t.name === "directory_group_members")!;
    expect(members.primaryKey).toEqual([
      "directory_group_id",
      "directory_user_id",
    ]);
    expect(members.columns).toEqual([
      { name: "directory_group_id", type: "", nullable: false },
      { name: "directory_user_id", type: "", nullable: false },
    ]);
  });

  it("fails a PostHog sync over the row limit and keeps the history newest first", () => {
    const c = create({ posthog: { apiKey: "phx_1", eventDays: 120 } });
    const first = syncOf(c.id);
    vi.advanceTimersByTime(3000);
    const second = syncOf(c.id);
    vi.advanceTimersByTime(3000);
    const syncs = ok<{ syncs: ConnectorSync[] }>(
      call("GET", `${base()}/connectors/${c.id}/syncs`),
    ).syncs;
    expect(syncs.map((s) => s.id)).toEqual([second.id, first.id]);
    expect(syncs[0]).toMatchObject({
      status: "CONNECTOR_SYNC_STATUS_FAILED",
      error: "table events has more than 2000000 rows; choose fewer tables",
    });
    expect(
      ok<{ syncs: ConnectorSync[] }>(
        call("GET", `${base()}/connectors/${c.id}/syncs?limit=1`),
      ).syncs,
    ).toHaveLength(1);
    expect(
      ok<{ connector: Connector }>(call("GET", `${base()}/connectors/${c.id}`))
        .connector.tables,
    ).toEqual([]);
  });

  it("refuses a database sync once the workspace leaves Pro", () => {
    setFoundryPlan(GLOBEX, "FOUNDRY_TIER_PRO");
    const c = create(pg(), "Shop", GLOBEX);
    setFoundryPlan(GLOBEX, "FOUNDRY_TIER_FREE");
    expect(
      refusal(call("POST", `${base(GLOBEX)}/connectors/${c.id}:sync`, {}))
        .message,
    ).toBe("PostgreSQL connectors need Foundry Pro");
  });

  it("tests and creates a database on Max, as on Pro", () => {
    setFoundryPlan(GLOBEX, "FOUNDRY_TIER_MAX");
    expect(
      call("POST", `${base(GLOBEX)}/connectors:test`, { config: pg() }).status,
    ).toBe(200);
    expect(create(pg(), "Shop", GLOBEX).name).toBe("Shop");
  });

  it("seeds Acme with two synced connections", () => {
    const list = ok<{ connectors: Connector[] }>(
      call("GET", `${base()}/connectors`),
    ).connectors;
    expect(list.map((c) => [c.id, c.lastSync?.status])).toEqual([
      ["conn_acme_stripe", "CONNECTOR_SYNC_STATUS_SUCCEEDED"],
      ["conn_acme_shop", "CONNECTOR_SYNC_STATUS_SUCCEEDED"],
    ]);
    expect(list[0]!.tables).toHaveLength(15);
    expect(list[1]!.tables).toHaveLength(6);
  });
});

describe("editing and deleting", () => {
  it("renames, and keeps the stored credential when a config leaves it blank", () => {
    const c = create({ stripe: { apiKey: stripeKey } });
    expect(
      ok<{ connector: Connector }>(
        call("PATCH", `${base()}/connectors/${c.id}`, { name: "Payments" }),
      ).connector.name,
    ).toBe("Payments");
    const kept = ok<any>(
      call("PATCH", `${base()}/connectors/${c.id}`, {
        config: { stripe: { apiKey: "" } },
      }),
    );
    expect(kept.connector.secretHint).toBe("sk_test…Abcd");
    expect(kept.discovery.tables).toHaveLength(15);
    expect(JSON.stringify(kept)).not.toContain(stripeKey);
    const replaced = ok<any>(
      call("PATCH", `${base()}/connectors/${c.id}`, {
        config: { stripe: { apiKey: "rk_live_newkey9999" } },
      }),
    );
    expect(replaced.connector.secretHint).toBe("rk_live…9999");
    // Cal.com's and Granola's are their API keys; GitHub's is its token.
    // Slack's is its bot token, Notion's its secret and Google
    // Workspace's its service account key, each kept beside new settings.
    for (const [kind, field, key, hint, settings] of [
      [
        "calcom",
        "apiKey",
        "cal_live_" + "e".repeat(12) + "4f2a",
        "cal_live…4f2a",
        {},
      ],
      ["granola", "apiKey", "grn_" + "f".repeat(12) + "QRST", "grn…QRST", {}],
      [
        "slack",
        "botToken",
        "xoxb-1-" + "g".repeat(12) + "ABCD",
        "xoxb…ABCD",
        { historyDays: 14 },
      ],
      ["notion", "token", "ntn_" + "h".repeat(12) + "EFGH", "ntn…EFGH", {}],
      [
        "google",
        "serviceAccountKey",
        googleKey(),
        "foundry@acme-data.iam.gserviceaccount.com…9f3a",
        { subject: "alan@example.com", eventDays: 30 },
      ],
    ] as const) {
      const c = create(
        {
          [kind]: {
            [field]: key,
            ...(kind === "google" ? { subject: "ada@example.com" } : {}),
          },
        },
        kind,
      );
      const kept = ok<any>(
        call("PATCH", `${base()}/connectors/${c.id}`, {
          name: "Renamed",
          config: { [kind]: { [field]: "", ...settings } },
        }),
      );
      expect(kept.connector.secretHint).toBe(hint);
      expect(kept.connector.name).toBe("Renamed");
      expect(kept.connector.config[kind]).toMatchObject(settings);
      expect(JSON.stringify(kept)).not.toContain(key);
    }
    const gh = create({ github: { token: "ghp_" + "a".repeat(16) + "WXYZ" } });
    const repos = ok<any>(
      call("PATCH", `${base()}/connectors/${gh.id}`, {
        config: { github: { token: "", repos: ["acme/api"] } },
      }),
    );
    expect(repos.connector.secretHint).toBe("ghp…WXYZ");
    expect(repos.connector.config.github.repos).toEqual(["acme/api"]);
  });

  it("refuses what the WFE refuses", () => {
    const c = create({ stripe: { apiKey: stripeKey } });
    const patch = (body: unknown) =>
      refusal(call("PATCH", `${base()}/connectors/${c.id}`, body));
    expect(patch({})).toEqual({
      status: 400,
      message: "set a name or config to update",
    });
    expect(patch({ name: "" }).message).toBe(
      "name must be 1 to 120 characters",
    );
    expect(patch({ config: { workos: { apiKey: "sk_1" } } }).message).toBe(
      "a connector's kind cannot change; create a new connector",
    );
    expect(
      patch({ config: { stripe: { apiKey: "sk_test_bad1" } } }).message,
    ).toBe("Stripe rejected this key; check it and test again");
    expect(
      refusal(call("PATCH", `${base()}/connectors/nope`, { name: "x" })),
    ).toEqual({ status: 404, message: "connector not found" });
  });

  it("queues a sync when settings move, and refuses moving them mid-sync", () => {
    const c = create(pg());
    const moved = ok<{ connector: Connector }>(
      call("PATCH", `${base()}/connectors/${c.id}`, {
        config: pg({ password: "", tables: ["orders"] }),
      }),
    ).connector;
    expect(moved.lastSync?.status).toBe("CONNECTOR_SYNC_STATUS_QUEUED");
    expect(
      refusal(
        call("PATCH", `${base()}/connectors/${c.id}`, {
          config: pg({ password: "", tables: ["users"] }),
        }),
      ).message,
    ).toBe(
      "a sync of this connector is queued or running; change its settings once the sync ends",
    );
    // A rename is fine mid-sync, and so are unchanged settings.
    expect(
      call("PATCH", `${base()}/connectors/${c.id}`, {
        name: "Renamed",
        config: pg({ password: "", tables: ["orders"] }),
      }).status,
    ).toBe(200);
    vi.advanceTimersByTime(3000);
    expect(
      ok<{ connector: Connector }>(
        call("GET", `${base()}/connectors/${c.id}`),
      ).connector.tables?.map((t) => t.name),
    ).toEqual(["orders"]);
  });

  it("deletes a connector once its sync has ended", () => {
    const c = create({ stripe: { apiKey: stripeKey } });
    syncOf(c.id);
    expect(refusal(call("DELETE", `${base()}/connectors/${c.id}`))).toEqual({
      status: 400,
      message:
        "a sync of this connector is queued or running; delete it once the sync ends",
    });
    vi.advanceTimersByTime(3000);
    expect(ok(call("DELETE", `${base()}/connectors/${c.id}`))).toEqual({});
    expect(call("GET", `${base()}/connectors/${c.id}`).status).toBe(404);
    expect(call("GET", `${base()}/connectors/${c.id}/syncs`).status).toBe(404);
    expect(call("POST", `${base()}/connectors/${c.id}:sync`, {}).status).toBe(
      404,
    );
  });
});

describe("access", () => {
  it("lets members read but not change connectors", () => {
    db.users.set("usr_bob", {
      id: "usr_bob",
      workosId: "user_bob",
      email: "bob@example.com",
      firstName: "Bob",
      lastName: "",
      createdAt: "",
    });
    db.memberships.push({
      id: "m2",
      workspaceId: ACME,
      userId: "usr_bob",
      role: "WORKSPACE_ROLE_MEMBER",
      createdAt: "",
    });
    const as = (method: HttpMethod, path: string, body?: unknown) =>
      call(method, path, body, "usr_bob").status;
    expect(as("GET", `${base()}/connectors`)).toBe(200);
    expect(as("GET", `${base()}/connector-kinds`)).toBe(200);
    expect(as("GET", `${base()}/connectors/conn_acme_shop/syncs`)).toBe(200);
    expect(
      as("POST", `${base()}/connectors:test`, {
        config: { stripe: { apiKey: stripeKey } },
      }),
    ).toBe(403);
    expect(as("POST", `${base()}/connectors`, { name: "x", config: {} })).toBe(
      403,
    );
    expect(as("POST", `${base()}/connectors/conn_acme_shop:sync`, {})).toBe(
      403,
    );
    expect(
      as("PATCH", `${base()}/connectors/conn_acme_shop`, { name: "x" }),
    ).toBe(403);
    expect(as("DELETE", `${base()}/connectors/conn_acme_shop`)).toBe(403);
  });

  it("keeps each workspace's connectors and syncs to itself", () => {
    expect(call("GET", `${base("ws_other")}/connectors`).status).toBe(403);
    expect(
      call("GET", `${base(GLOBEX)}/connectors/conn_acme_shop`).status,
    ).toBe(404);
    const sync = ok<{ syncs: ConnectorSync[] }>(
      call("GET", `${base()}/connectors/conn_acme_shop/syncs`),
    ).syncs[0]!;
    expect(call("GET", `${base()}/connector-syncs/${sync.id}`).status).toBe(
      200,
    );
    expect(
      call("GET", `${base(GLOBEX)}/connector-syncs/${sync.id}`).status,
    ).toBe(404);
  });
});

describe("S3", () => {
  const role = "arn:aws:iam::123456789012:role/helix-foundry",
    secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY";
  const viaRole = (extra: object = {}, roleArn = role) => ({
    s3: { bucket: "acme-exports", prefix: "e2e", role: { roleArn }, ...extra },
  });
  const viaKeys = (keys: object = {}, extra: object = {}) => ({
    s3: {
      bucket: "acme-exports",
      prefix: "e2e/",
      keys: { accessKeyId: "AKIAEXAMPLE", secretAccessKey: secret, ...keys },
      ...extra,
    },
  });
  const test = (config: unknown, ws = ACME) =>
    call("POST", `${base(ws)}/connectors:test`, { config });
  const message = (config: unknown) => refusal(test(config)).message;

  it("checks the settings as Prepare does (M1-M10, M19)", () => {
    const M1 = "enter the bucket name, like acme-exports";
    for (const bucket of [
      "",
      "ab",
      "Acme",
      "-acme",
      "acme-",
      "acme..exports",
      "192.168.10.4",
      "a".repeat(64),
      "acme_exports",
    ])
      expect(message(viaRole({ bucket })), bucket).toBe(M1);
    expect(message(viaRole({ bucket: "helix-mock-foundry-data" }))).toBe(
      "helix-mock-foundry-data is a Helix bucket",
    );
    const M2 = "enter the bucket's region, like eu-west-2";
    for (const region of [
      "eu",
      "EU-WEST-2",
      "eu-west",
      "cn-north-1",
      "us-gov-west-1",
      "us-isob-east-1",
    ])
      expect(message(viaRole({ region })), region).toBe(M2);
    const M3 =
      "enter the role ARN from your AWS account, like arn:aws:iam::123456789012:role/helix-foundry";
    for (const arn of [
      "",
      "helix-foundry",
      "arn:aws:iam::12345:role/x",
      "arn:aws:iam::123456789012:user/x",
      // Helix's own account.
      "arn:aws:iam::000000000000:role/helix-foundry",
    ])
      expect(message(viaRole({}, arn)), arn).toBe(M3);
    const M4 = "choose IAM role or access key access";
    expect(message({ s3: { bucket: "acme-exports" } })).toBe(M4);
    expect(
      message({
        s3: {
          bucket: "acme-exports",
          role: { roleArn: role },
          keys: { accessKeyId: "AKIAEXAMPLE", secretAccessKey: secret },
        },
      }),
    ).toBe(M4);
    expect(message(viaKeys({ secretAccessKey: " " }))).toBe(
      "enter the secret access key",
    );
    expect(message(viaKeys({ accessKeyId: "ASIAEXAMPLE" }))).toBe(
      "temporary keys (ASIA…) expire; use an IAM user's key or a role",
    );
    // An S3-compatible store's temporary key is its own business.
    expect(
      test(
        viaKeys({
          accessKeyId: "ASIAEXAMPLE",
          endpoint: "https://minio.example.com",
        }),
      ).status,
    ).toBe(200);
    for (const accessKeyId of ["", "AK", "AKIA EXAMPLE", "A".repeat(129)])
      expect(message(viaKeys({ accessKeyId })), accessKeyId).toBe(
        "enter the access key ID",
      );
    const M7 =
      "enter the HTTPS address of your S3-compatible storage, without a path";
    for (const endpoint of [
      "minio.example.com",
      "http://minio.example.com",
      "https://u:p@minio.example.com",
      "https://minio.example.com/bucket",
      "https://minio.example.com/?x=1",
      "https://minio.example.com#frag",
      "https://",
    ])
      expect(message(viaKeys({ endpoint })), endpoint).toBe(M7);
    for (const endpoint of [
      "https://s3.eu-west-2.amazonaws.com",
      "https://s3.cn-north-1.amazonaws.com.cn",
    ])
      expect(message(viaKeys({ endpoint })), endpoint).toBe(
        "for AWS, leave the endpoint empty",
      );
    const M9 =
      "Helix can only reach public storage outside Helix; check the endpoint";
    for (const endpoint of [
      "https://helix-db.com",
      "https://files.helix-db.com",
      // The guard refuses private addresses when it dials.
      "https://127.0.0.1:9000",
      "https://10.0.0.8",
    ])
      expect(message(viaKeys({ endpoint })), endpoint).toBe(M9);
    Object.assign(config, { s3RoleAccess: false });
    expect(message(viaRole())).toBe(
      "IAM role access is not available here; use an access key",
    );
    expect(test(viaKeys()).status).toBe(200);
  });

  it("maps what the source says to the WFE's messages (M11-M14, M17, M18, M29)", () => {
    expect(
      refusal(test(viaRole({}, "arn:aws:iam::123456789012:role/untrusted"))),
    ).toEqual({
      status: 400,
      message:
        "Helix could not assume the role; check its trust policy names arn:aws:iam::000000000000:role/helix-mock-foundry-connector and requires external ID helix-foundry-ws_acme",
    });
    expect(
      message(viaRole({}, "arn:aws:iam::123456789012:role/open-trust")),
    ).toBe(
      "your role's trust policy must require external ID helix-foundry-ws_acme; Helix won't use a role that doesn't",
    );
    expect(message(viaRole({ bucket: "missing-bucket" }))).toBe(
      "bucket missing-bucket does not exist",
    );
    expect(message(viaRole({ bucket: "denied-bucket" }))).toBe(
      "Helix cannot list s3://denied-bucket/e2e/; allow s3:ListBucket on this folder",
    );
    expect(message(viaKeys({ secretAccessKey: "bad-secret" }))).toBe(
      "S3 rejected the access key; check the key ID and secret",
    );
    expect(message(viaRole({ bucket: "empty-bucket", prefix: "" }))).toBe(
      "no files Helix can read under s3://empty-bucket/; Helix reads Parquet, JSON, JSON Lines and CSV",
    );
    expect(message(viaRole({ tables: ["orders", "invoices"] }))).toBe(
      'table "invoices" is not in this folder any more',
    );
    // Pro gates it like the databases.
    expect(refusal(test(viaRole(), GLOBEX)).message).toBe(
      "Amazon S3 connectors need Foundry Pro",
    );
    setFoundryPlan(GLOBEX, "FOUNDRY_TIER_MAX");
    expect(test(viaRole(), GLOBEX).status).toBe(200);
  });

  it("lists the folder's tables with formats, files, sizes and previews", () => {
    const body = ok<any>(test(viaRole()));
    const tables = body.discovery.tables as Record<string, any>[];
    expect(
      tables.map((t) => [t.name, t.format, t.fileCount, t.bytes, t.problem]),
    ).toEqual([
      ["api", "json", 1, "47312", ""],
      ["customers", "csv", 1, "86016", ""],
      ["events", "json", 2, "6292455", ""],
      ["orders", "parquet", 2, "1288490189", ""],
      ["products", "json", 1, "18944", ""],
    ]);
    expect(body.discovery.unavailable).toEqual([
      "archive/old.csv: archived in Glacier; restore it first",
      "notes.xlsx: unsupported file type",
    ]);
    expect(body.discovery.maxBytes).toBe(String(20 * 2 ** 30));
    // JSON and CSV have up to 5 preview rows, each a JSON object; Parquet
    // has columns only.
    for (const t of tables) {
      expect(t.preview.length).toBe(t.format === "parquet" ? 0 : 5);
      for (const row of t.preview) {
        expect(row.length).toBeLessThanOrEqual(4096);
        expect(Object.keys(JSON.parse(row))).toEqual(
          t.columns.map((c: { name: string }) => c.name),
        );
      }
    }
    const customers = tables[1]!;
    expect(customers.primaryKey).toEqual(["id"]);
    expect(customers.columns[0]).toEqual({
      name: "id",
      type: "integer",
      nullable: false,
    });
    expect(tables[2]!.columns.at(-1)).toEqual({
      name: "session_id",
      type: "string",
      nullable: true,
    });
    expect(tables[3]!.columns.at(-1)).toMatchObject({ name: "dt" });
    expect(tables[3]!.primaryKey).toEqual([]);
    // The detected region comes back to be stored; the folder is normalised.
    expect(body.config).toEqual({
      s3: {
        bucket: "acme-exports",
        prefix: "e2e/",
        region: "eu-west-2",
        tables: [],
        role: { roleArn: role },
      },
    });
    // Databases' tables carry the file fields empty, as the gateway emits them.
    const pg = ok<any>(
      test({
        postgres: {
          host: "db.example.com",
          database: "app",
          user: "ro",
          password: "pw",
        },
      }),
    ).discovery;
    expect(pg.maxBytes).toBe("0");
    expect(pg.tables[0]).toMatchObject({
      format: "",
      fileCount: 0,
      bytes: "0",
      preview: [],
      problem: "",
    });
  });

  it("keeps a store's own region and names a malformed sample's problem", () => {
    const store = ok<any>(
      test(
        viaKeys(
          { endpoint: "https://minio.example.com:9000/" },
          { region: "" },
        ),
      ),
    );
    expect(store.config.s3).toMatchObject({
      region: "",
      keys: {
        accessKeyId: "AKIAEXAMPLE",
        secretAccessKey: "",
        endpoint: "https://minio.example.com:9000",
      },
    });
    const bad = ok<any>(test(viaRole({ bucket: "bad-records" })));
    const events = bad.discovery.tables.find(
      (t: { name: string }) => t.name === "events",
    );
    expect(events.problem).toBe(
      "events/2026-10-02.jsonl.zst record 12: not a JSON object",
    );
    // A problem never makes a table unavailable.
    expect(bad.discovery.tables).toHaveLength(5);
  });

  it("creates, syncs the chosen tables with their measured keys, and never returns the secret", () => {
    const keyed = create(
      viaKeys({}, { tables: ["orders", "customers"] }),
      "Lake",
    );
    expect(keyed.secretHint).toBe("secret …EKEY");
    expect(keyed.config).toEqual({
      s3: {
        bucket: "acme-exports",
        prefix: "e2e/",
        region: "eu-west-2",
        tables: ["orders", "customers"],
        keys: {
          accessKeyId: "AKIAEXAMPLE",
          secretAccessKey: "",
          endpoint: "",
        },
      },
    });
    const sync = syncOf(keyed.id);
    vi.advanceTimersByTime(3000);
    const synced = ok<{ connector: Connector }>(
      call("GET", `${base()}/connectors/${keyed.id}`),
    ).connector;
    expect(synced.lastSync).toMatchObject({
      id: sync.id,
      status: "CONNECTOR_SYNC_STATUS_SUCCEEDED",
      rowsSynced: String(1000 + 24_310),
    });
    expect(
      synced.tables?.map((t) => [t.name, t.rowCount, t.primaryKey]),
    ).toEqual([
      ["customers", "1000", ["id"]],
      // The sync measured what the test could only guess.
      ["orders", "24310", ["id"]],
    ]);
    expect(synced.tables?.[1]?.columns?.[4]).toEqual({
      name: "placed_at",
      type: "timestamptz",
      nullable: false,
    });

    const roled = create(viaRole(), "Lake role");
    expect(roled.secretHint).toBe("IAM role");
    syncOf(roled.id);
    vi.advanceTimersByTime(3000);
    expect(
      ok<{ connector: Connector }>(
        call("GET", `${base()}/connectors/${roled.id}`),
      ).connector.tables,
    ).toHaveLength(5);

    const everything = JSON.stringify([
      call("GET", `${base()}/connectors`).body,
      call("GET", `${base()}/connectors/${keyed.id}`).body,
      ok(test(viaKeys())),
    ]);
    expect(everything).not.toContain(secret);
  });

  it("fails a sync of a table whose records are malformed", () => {
    const c = create(viaRole({ bucket: "bad-records" }));
    syncOf(c.id);
    vi.advanceTimersByTime(3000);
    expect(
      ok<{ connector: Connector }>(call("GET", `${base()}/connectors/${c.id}`))
        .connector.lastSync,
    ).toMatchObject({
      status: "CONNECTOR_SYNC_STATUS_FAILED",
      error: "events/2026-10-02.jsonl.zst record 12: not a JSON object",
    });
    // Without events, the rest syncs.
    const rest = create(
      viaRole({ bucket: "bad-records", tables: ["orders", "products"] }),
    );
    syncOf(rest.id);
    vi.advanceTimersByTime(3000);
    expect(
      ok<{ connector: Connector }>(
        call("GET", `${base()}/connectors/${rest.id}`),
      ).connector.lastSync?.status,
    ).toBe("CONNECTOR_SYNC_STATUS_SUCCEEDED");
  });

  it("keeps the stored key in key mode, and drops it for a role", () => {
    const c = create(viaKeys());
    const patch = (body: unknown) =>
      call("PATCH", `${base()}/connectors/${c.id}`, body);
    // A blank secret keeps the stored one, which still opens the bucket.
    const kept = ok<any>(
      patch({ config: viaKeys({ secretAccessKey: "" }, { prefix: "e2e/v2" }) }),
    );
    expect(kept.connector.secretHint).toBe("secret …EKEY");
    expect(kept.connector.config.s3.prefix).toBe("e2e/v2/");
    expect(kept.discovery.tables).toHaveLength(5);
    // A role stores no secret.
    vi.advanceTimersByTime(3000);
    const moved = ok<any>(patch({ config: viaRole() }));
    expect(moved.connector.secretHint).toBe("IAM role");
    expect(moved.connector.config.s3).toEqual({
      bucket: "acme-exports",
      prefix: "e2e/",
      region: "eu-west-2",
      tables: [],
      role: { roleArn: role },
    });
    // Back to keys: the old key is gone, so the secret is needed again.
    vi.advanceTimersByTime(3000);
    expect(
      refusal(patch({ config: viaKeys({ secretAccessKey: "" }) })).message,
    ).toBe("enter the secret access key");
    expect(ok<any>(patch({ config: viaKeys() })).connector.secretHint).toBe(
      "secret …EKEY",
    );
  });
});

describe("the S3 layout port", () => {
  // The layout's cases, under the folder e2e/.
  const tables = (...keys: (string | Listed)[]) =>
    layout(
      "e2e",
      keys.map((k) => (typeof k === "string" ? { key: k, size: 10 } : k)),
    );
  const names = (...keys: string[]) =>
    tables(...keys).datasets.map((d) => d.name);
  const uuid = "3f1e9c2a-7b4d-4e1f-9a2b-1c3d5e7f9a0b";

  it("names tables from folders and file families", () => {
    expect(names("customers.csv", "orders.json")).toEqual([
      "customers",
      "orders",
    ]);
    expect(
      names(
        `orders/dt=2026-10-01/part-00000-${uuid}-c000.snappy.parquet`,
        `orders/dt=2026-10-02/part-00001-${uuid}-c000.snappy.parquet`,
      ),
    ).toEqual(["orders"]);
    expect(names("dt=2026-10-01/part-0.parquet")).toEqual(["e2e"]);
    expect(
      tables("events/2026-10-01.jsonl.gz", "events/2026-10-02.jsonl.zst")
        .datasets,
    ).toEqual([
      {
        name: "events",
        format: "json",
        files: [
          { key: "events/2026-10-01.jsonl.gz", size: 10 },
          { key: "events/2026-10-02.jsonl.zst", size: 10 },
        ],
      },
    ]);
    expect(names("exports/customers.csv", "exports/orders.csv")).toEqual([
      "exports/customers",
      "exports/orders",
    ]);
    expect(
      names("daily/2026-10-01/customers.csv", "daily/2026-10-02/customers.csv"),
    ).toEqual(["daily"]);
    expect(
      names("sales/orders_2026_01.csv", "sales/orders_2026_02.csv"),
    ).toEqual(["sales"]);
    expect(names("api/invoices.json")).toEqual(["api"]);
    // Files with no family of their own in a folder with others: the folder.
    expect(names("x/orders.csv", "x/2026-01.csv")).toEqual(["x", "x/orders"]);
    // Upper-case extensions, a .tsv, a .pq, two compression layers.
    expect(
      tables("A.PARQUET", "b.tsv", "cc.pq", "d.csv.gz.zst").datasets.map(
        (d) => [d.name, d.format],
      ),
    ).toEqual([
      ["a", "parquet"],
      ["b", "csv"],
      ["cc", "parquet"],
      ["d", "csv"],
    ]);
  });

  it("skips what it can't read and ignores what isn't data", () => {
    expect(tables("x/a.csv", "x/a.json").skipped).toEqual([
      { name: "x", reason: "mixed formats (csv, json)" },
    ]);
    expect(tables("lake/t/_delta_log/0.json", "lake/t/p.parquet")).toEqual({
      datasets: [],
      skipped: [
        { name: "lake/t", reason: "Delta Lake tables are not supported yet" },
      ],
    });
    expect(
      tables("h/.hoodie/hoodie.properties", "h/p.parquet").skipped,
    ).toEqual([{ name: "h", reason: "Hudi tables are not supported yet" }]);
    expect(
      tables("ice/metadata/v1.metadata.json", "ice/data/p.parquet"),
    ).toEqual({
      datasets: [],
      skipped: [
        { name: "ice", reason: "Iceberg tables are not supported yet" },
      ],
    });
    // Metadata without a data folder beside it is not Iceberg.
    expect(names("t/metadata/v1.metadata.json")).toEqual(["t/metadata"]);
    // A noise word alone ("c", "part") is no family: the folder's fallback.
    expect(names("c.csv")).toEqual(["e2e"]);
    expect(tables("_delta_log/0.json", "p.parquet").skipped).toEqual([
      { name: "e2e", reason: "Delta Lake tables are not supported yet" },
    ]);
    expect(
      tables(
        "notes.xlsx",
        "_SUCCESS",
        ".DS_Store",
        "dir/",
        "x.csv.crc",
        "y.tmp",
        "_tmp/z.csv",
        { key: "empty.csv", size: 0 },
        { key: "cold.csv", size: 10, archived: true },
      ),
    ).toEqual({
      datasets: [],
      skipped: [
        { name: "notes.xlsx", reason: "unsupported file type" },
        { name: "cold.csv", reason: "archived in Glacier; restore it first" },
      ],
    });
    const long = "t".repeat(256);
    expect(tables(`${long}.csv`).skipped).toEqual([
      { name: long, reason: "name too long" },
    ]);
    expect(names("é/données.csv")).toEqual(["é"]);
  });
});
