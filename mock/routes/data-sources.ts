import type {
  ConnectorColumn,
  ConnectorConfig,
  ConnectorDiscovery,
  ConnectorForeignKey,
  ConnectorKindName,
  ConnectorTable,
  DatabaseConnectorConfig,
  EventsConnectorConfig,
  GitHubConnectorConfig,
  PostHogConnectorConfig,
  S3ConnectorConfig,
  S3ConnectorSetup,
} from "../../src/shared/wfe";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { columnsOf, textFieldProblem } from "../event-pieces";
import { PEOPLE, PEOPLE_COLUMNS } from "../fixtures/sales-graph";
import { config, onReset } from "../state";
import { layout, type Listed } from "./data-files";

// The mock's stand-ins for the WFE's connector kinds
// (the backend's connector package): the same objects,
// keys, settings checks and customer messages, with fake sources behind them.
// Nothing is dialed. Mock-only behaviour, so tests and dev:mock can reach
// every path:
//
// - API keys: a key containing "bad" is rejected by the source;
//   a Stripe restricted key (rk_) cannot read payouts, balance transactions
//   or disputes. A PostHog key containing "multi" sees two projects. A GitHub
//   token containing "nopr" cannot read pull requests or their reviews, and
//   a repository named missing (acme/missing) is one the token can't read.
//   A Cal.com key containing "solo" belongs to an account with no teams, so
//   teams and team memberships can't be read, and one containing "member"
//   is warned that it isn't a team admin; a Granola key containing
//   "personal" cannot read spaces. A Slack token containing "noscope"
//   lacks channels:history and groups:history (messages and channel_days
//   can't be read, and nothing is a document), and one containing "lonely"
//   is in no channels, so the test warns and syncs read no history. A
//   Notion secret containing "nousers" can't read users, and one containing
//   "empty" has nothing shared with it. A Google Workspace key is rejected
//   when its JSON contains "bad"; a subject containing "member" is no
//   admin (users can't be read), one containing "nobody" is no user, and a
//   key whose client_email contains "nocalendar" wasn't delegated
//   Calendar's scope (calendars and their events can't be read).
// - Databases: the password "wrong" fails authentication, a host ending in
//   ".invalid" cannot be reached, and private or loopback hosts are refused
//   as the connector guard refuses them. Every database holds the same small
//   shop schema (in `public` for PostgreSQL); other schemas are empty. But
//   the database named acme, the sample company's (a local stack's
//   acme-db or a stamp's RDS), holds Acme Corp's tables as
//   local/acme/acme.sql creates them.
// - Syncs: PostHog with more than 90 event days fails on the row limit.
// - Documents (`documentStreams`): a Granola sync reads each of its 418
//   notes into a Notes set, and its transcript into a Transcripts set when
//   `transcripts` is on. A note whose title holds "edited" (the third
//   newest) is edited before every sync: a new version with new text. A
//   Slack sync reads each day with messages of the channels the app is in
//   (two of every three of the history_days before the sync's, in each of
//   four channels) into Conversations, a Notion sync its 60 pages with a
//   body into Pages, and a Google Workspace sync its 14 Docs into Docs.
// - S3 (Pro): every bucket holds the plan's V1 fixture folder (./data-files.ts
//   lays it out as the WFE does): tables api, customers, events, orders and
//   products, in eu-west-2, with notes.xlsx and an archived archive/old.csv
//   it can't read. A role ARN containing "untrusted" can't be assumed, one
//   containing "open" doesn't require the external ID, and a secret
//   containing "bad" is rejected. The bucket missing-bucket doesn't exist,
//   denied-bucket can't be listed, empty-bucket holds nothing readable, and
//   in bad-records a sample of events is malformed, so its syncs fail.
//   `{"s3RoleAccess": false}` turns role access off, as a WFE without the
//   foundry-connector role has it.
// - API sources (events, on every plan): no credential and nothing to dial.
//   Their pieces, fields and syncs are ./event-sources.ts's.

/** A connector's stored settings and its credential, as the WFE splits them. */
export type Source = {
  kind: ConnectorKindName;
  // The kind's config with the credential removed and defaults applied.
  settings: ConnectorConfig;
  secret: string;
  // Never from settings: the S3 kind derives its external ID from it.
  workspaceId: string;
};

/** Why a source was refused: a setting to fix, or the source saying no. */
export type Refusal = { code: "INVALID_ARGUMENT" | "FAILED_PRECONDITION" };
export type SourceError = Refusal & { message: string };

export type MockKind = {
  name: ConnectorKindName;
  displayName: string;
  requiresPro: boolean;
};
// The WFE's catalog, in display order.
export const kinds: MockKind[] = [
  { name: "stripe", displayName: "Stripe", requiresPro: false },
  { name: "workos", displayName: "WorkOS", requiresPro: false },
  { name: "posthog", displayName: "PostHog", requiresPro: false },
  { name: "github", displayName: "GitHub", requiresPro: false },
  { name: "linear", displayName: "Linear", requiresPro: false },
  { name: "calcom", displayName: "Cal.com", requiresPro: false },
  { name: "granola", displayName: "Granola", requiresPro: false },
  { name: "slack", displayName: "Slack", requiresPro: false },
  { name: "notion", displayName: "Notion", requiresPro: false },
  { name: "google", displayName: "Google Workspace", requiresPro: false },
  { name: "postgres", displayName: "PostgreSQL", requiresPro: true },
  { name: "mysql", displayName: "MySQL", requiresPro: true },
  { name: "s3", displayName: "Amazon S3", requiresPro: true },
  { name: "events", displayName: "API source", requiresPro: false },
];
/** An API source's description, in characters. */
export const MAX_EVENT_DESCRIPTION = 1000;

const invalid = (message: string): SourceError => ({
  code: "INVALID_ARGUMENT",
  message,
});
const refused = (message: string): SourceError => ({
  code: "FAILED_PRECONDITION",
  message,
});

// ---------------------------------------------------------------- API sources

type ApiObject = {
  name: string;
  key: string[];
  // Columns the first sync finds in the rows (names only).
  columns: string[];
  rows: number;
};

// Foreign keys reference the object that owns the column as its key, as
// connector.go's referencedBy does.
function referencedBy(
  objects: ApiObject[],
  columns: Record<string, string[]>,
): Record<string, ConnectorForeignKey[]> {
  const owner = new Map<string, string>();
  for (const o of objects)
    if (o.key.length === 1 && !owner.has(o.key[0]!))
      owner.set(o.key[0]!, o.name);
  return Object.fromEntries(
    Object.entries(columns).map(([table, cols]) => [
      table,
      cols.map((column) => ({
        column,
        referencedTable: owner.get(column)!,
        referencedColumn: column,
      })),
    ]),
  );
}

const object = (
  name: string,
  key: string | string[],
  columns: string[],
  rows: number,
): ApiObject => ({
  name,
  key: typeof key === "string" ? [key] : key,
  columns,
  rows,
});

const stripeObjects = [
  object(
    "customers",
    "customer_id",
    ["customer_id", "email", "name", "currency", "created", "livemode"],
    1284,
  ),
  object(
    "products",
    "product_id",
    ["product_id", "name", "active", "price_id", "created"],
    12,
  ),
  object(
    "prices",
    "price_id",
    ["price_id", "product_id", "currency", "unit_amount", "recurring"],
    24,
  ),
  object(
    "coupons",
    "coupon_id",
    ["coupon_id", "name", "percent_off", "duration"],
    6,
  ),
  object(
    "promotion_codes",
    "promotion_code_id",
    ["promotion_code_id", "code", "coupon_id", "customer_id", "active"],
    9,
  ),
  object(
    "subscriptions",
    "subscription_id",
    ["subscription_id", "customer_id", "invoice_id", "status", "created"],
    842,
  ),
  object(
    "subscription_items",
    "subscription_item_id",
    ["subscription_item_id", "subscription_id", "price_id", "quantity"],
    911,
  ),
  object(
    "invoices",
    "invoice_id",
    [
      "invoice_id",
      "customer_id",
      "subscription_id",
      "charge_id",
      "payment_intent_id",
      "status",
      "amount_due",
      "created",
    ],
    6120,
  ),
  object(
    "invoice_line_items",
    "line_item_id",
    [
      "line_item_id",
      "invoice_id",
      "price_id",
      "subscription_id",
      "subscription_item_id",
      "amount",
    ],
    7034,
  ),
  object(
    "charges",
    "charge_id",
    [
      "charge_id",
      "customer_id",
      "invoice_id",
      "payment_intent_id",
      "balance_transaction_id",
      "amount",
      "status",
    ],
    5980,
  ),
  object(
    "payment_intents",
    "payment_intent_id",
    ["payment_intent_id", "customer_id", "invoice_id", "charge_id", "amount"],
    6012,
  ),
  object(
    "refunds",
    "refund_id",
    [
      "refund_id",
      "charge_id",
      "payment_intent_id",
      "balance_transaction_id",
      "amount",
    ],
    143,
  ),
  object(
    "disputes",
    "dispute_id",
    ["dispute_id", "charge_id", "payment_intent_id", "reason", "status"],
    11,
  ),
  object(
    "payouts",
    "payout_id",
    ["payout_id", "balance_transaction_id", "amount", "arrival_date"],
    96,
  ),
  object(
    "balance_transactions",
    "balance_transaction_id",
    ["balance_transaction_id", "type", "amount", "fee", "net", "created"],
    12650,
  ),
];
const stripeForeignKeys = referencedBy(stripeObjects, {
  products: ["price_id"],
  prices: ["product_id"],
  promotion_codes: ["coupon_id", "customer_id"],
  subscriptions: ["customer_id", "invoice_id"],
  subscription_items: ["subscription_id", "price_id"],
  invoices: [
    "customer_id",
    "subscription_id",
    "charge_id",
    "payment_intent_id",
  ],
  invoice_line_items: [
    "invoice_id",
    "price_id",
    "subscription_id",
    "subscription_item_id",
  ],
  charges: [
    "customer_id",
    "invoice_id",
    "payment_intent_id",
    "balance_transaction_id",
  ],
  payment_intents: ["customer_id", "invoice_id", "charge_id"],
  refunds: ["charge_id", "payment_intent_id", "balance_transaction_id"],
  disputes: ["charge_id", "payment_intent_id"],
  payouts: ["balance_transaction_id"],
});
// What a restricted key is not granted in the mock.
const stripeRestricted = new Set([
  "payouts",
  "balance_transactions",
  "disputes",
]);

const workosObjects = [
  object(
    "organizations",
    "organization_id",
    ["organization_id", "name", "domains", "created_at"],
    58,
  ),
  object(
    "users",
    "user_id",
    ["user_id", "email", "first_name", "last_name", "created_at"],
    740,
  ),
  object(
    "organization_memberships",
    "organization_membership_id",
    ["organization_membership_id", "organization_id", "user_id", "role"],
    812,
  ),
  object(
    "invitations",
    "invitation_id",
    ["invitation_id", "organization_id", "email", "state"],
    37,
  ),
  object(
    "sso_connections",
    "connection_id",
    ["connection_id", "organization_id", "connection_type", "state"],
    14,
  ),
  object(
    "directories",
    "directory_id",
    ["directory_id", "organization_id", "type", "state"],
    9,
  ),
  object(
    "directory_users",
    "directory_user_id",
    ["directory_user_id", "directory_id", "organization_id", "email"],
    322,
  ),
  object(
    "directory_groups",
    "directory_group_id",
    ["directory_group_id", "directory_id", "organization_id", "name"],
    41,
  ),
  object(
    "directory_group_members",
    ["directory_group_id", "directory_user_id"],
    ["directory_group_id", "directory_user_id"],
    530,
  ),
];
const workosForeignKeys = referencedBy(workosObjects, {
  organization_memberships: ["organization_id", "user_id"],
  invitations: ["organization_id"],
  sso_connections: ["organization_id"],
  directories: ["organization_id"],
  directory_users: ["directory_id", "organization_id"],
  directory_groups: ["directory_id", "organization_id"],
  directory_group_members: ["directory_group_id", "directory_user_id"],
});

const posthogObjects = [
  object(
    "persons",
    "person_id",
    ["person_id", "email", "name", "created_at", "properties"],
    3120,
  ),
  object(
    "person_distinct_ids",
    "distinct_id",
    ["distinct_id", "person_id"],
    4410,
  ),
  object(
    "groups",
    ["group_type", "group_key"],
    ["group_type", "group_key", "properties"],
    58,
  ),
  object(
    "events",
    "event_id",
    [
      "event_id",
      "event",
      "timestamp",
      "distinct_id",
      "person_id",
      "session_id",
      "current_url",
    ],
    184_220,
  ),
  object(
    "daily_event_counts",
    ["day", "event", "person_id"],
    ["day", "event", "person_id", "count"],
    21_430,
  ),
];
const posthogForeignKeys = referencedBy(posthogObjects, {
  person_distinct_ids: ["person_id"],
  events: ["person_id", "distinct_id"],
  daily_event_counts: ["person_id"],
});
const posthogRowLimitDays = 90;

const githubObjects = [
  object(
    "repositories",
    "repository_id",
    ["repository_id", "full_name", "private", "default_branch", "created_at"],
    3,
  ),
  object("users", "user_id", ["user_id", "login", "type", "site_admin"], 24),
  object(
    "labels",
    "label_id",
    ["label_id", "repository_id", "name", "color", "description"],
    41,
  ),
  object(
    "milestones",
    "milestone_id",
    ["milestone_id", "repository_id", "number", "title", "state", "due_on"],
    9,
  ),
  object(
    "issues",
    "issue_key",
    [
      "issue_key",
      "repository_id",
      "number",
      "title",
      "state",
      "user_id",
      "user_login",
      "milestone_id",
      "label_ids",
      "is_pull_request",
      "created_at",
      "closed_at",
    ],
    1630,
  ),
  object(
    "pull_requests",
    "issue_key",
    [
      "issue_key",
      "repository_id",
      "number",
      "title",
      "state",
      "user_id",
      "milestone_id",
      "head_ref",
      "base_ref",
      "merged_at",
    ],
    912,
  ),
  object(
    "issue_comments",
    "comment_id",
    ["comment_id", "issue_key", "repository_id", "user_id", "body"],
    5208,
  ),
  object(
    "pull_request_reviews",
    "review_id",
    ["review_id", "issue_key", "repository_id", "user_id", "state"],
    1377,
  ),
];
const githubForeignKeys = referencedBy(githubObjects, {
  labels: ["repository_id"],
  milestones: ["repository_id"],
  issues: ["repository_id", "milestone_id", "user_id"],
  pull_requests: ["issue_key", "repository_id", "milestone_id", "user_id"],
  issue_comments: ["issue_key", "repository_id", "user_id"],
  pull_request_reviews: ["issue_key", "repository_id", "user_id"],
});
// What a token without Pull requests cannot read.
const githubPullRequests = new Set(["pull_requests", "pull_request_reviews"]);
const maxGitHubRepos = 100;

const linearObjects = [
  object("teams", "team_id", ["team_id", "key", "name", "parent_id"], 4),
  object("users", "user_id", ["user_id", "name", "email", "active"], 18),
  object(
    "projects",
    "project_id",
    [
      "project_id",
      "name",
      "status_name",
      "status_type",
      "lead_id",
      "creator_id",
    ],
    22,
  ),
  object("cycles", "cycle_id", ["cycle_id", "number", "team_id"], 36),
  object(
    "workflow_states",
    "workflow_state_id",
    ["workflow_state_id", "name", "type", "team_id"],
    24,
  ),
  object(
    "issue_labels",
    "label_id",
    ["label_id", "name", "team_id", "parent_id", "creator_id"],
    31,
  ),
  object(
    "issues",
    "issue_id",
    [
      "issue_id",
      "identifier",
      "title",
      "priority",
      "team_id",
      "workflow_state_id",
      "assignee_id",
      "project_id",
      "cycle_id",
      "creator_id",
      "parent_id",
      "label_ids",
    ],
    2410,
  ),
  object(
    "comments",
    "comment_id",
    ["comment_id", "issue_id", "user_id", "parent_id", "body"],
    6120,
  ),
];
// linear.go's linearForeignKeys: a nested object's column names its table.
const fk = (column: string, table: string, referenced = column) => ({
  column,
  referencedTable: table,
  referencedColumn: referenced,
});
const linearForeignKeys: Record<string, ConnectorForeignKey[]> = {
  teams: [fk("parent_id", "teams", "team_id")],
  projects: [
    fk("lead_id", "users", "user_id"),
    fk("creator_id", "users", "user_id"),
  ],
  cycles: [fk("team_id", "teams")],
  workflow_states: [fk("team_id", "teams")],
  issue_labels: [
    fk("team_id", "teams"),
    fk("parent_id", "issue_labels", "label_id"),
    fk("creator_id", "users", "user_id"),
  ],
  issues: [
    fk("team_id", "teams"),
    fk("workflow_state_id", "workflow_states"),
    fk("assignee_id", "users", "user_id"),
    fk("creator_id", "users", "user_id"),
    fk("project_id", "projects"),
    fk("cycle_id", "cycles"),
    fk("parent_id", "issues", "issue_id"),
  ],
  comments: [
    fk("issue_id", "issues"),
    fk("user_id", "users"),
    fk("parent_id", "comments", "comment_id"),
  ],
};

// Cal.com's API v2 (api.cal.com), as calcom.go reads it: the key's user, its
// teams and their members, event types, schedules, and bookings with their
// attendees and hosts. A booking is keyed by its uid, which a reschedule
// replaces; an organization is a team.
const calcomObjects = [
  object(
    "users",
    "user_id",
    [
      "user_id",
      "email",
      "name",
      "username",
      "time_zone",
      "default_schedule_id",
      "organization_id",
    ],
    14,
  ),
  object(
    "teams",
    "team_id",
    ["team_id", "name", "slug", "parent_id", "is_organization"],
    3,
  ),
  object(
    "team_memberships",
    "membership_id",
    ["membership_id", "team_id", "user_id", "role", "accepted"],
    16,
  ),
  object(
    "event_types",
    "event_type_id",
    [
      "event_type_id",
      "title",
      "slug",
      "length_in_minutes",
      "owner_id",
      "team_id",
      "parent_event_type_id",
      "schedule_id",
      "hidden",
    ],
    11,
  ),
  object(
    "schedules",
    "schedule_id",
    ["schedule_id", "owner_id", "name", "time_zone", "is_default"],
    8,
  ),
  object(
    "bookings",
    "booking_uid",
    [
      "booking_uid",
      "booking_id",
      "title",
      "status",
      "start",
      "end",
      "event_type_id",
      "rescheduled_from_uid",
      "rescheduled_to_uid",
      "created_at",
      "updated_at",
    ],
    864,
  ),
  object(
    "booking_attendees",
    ["booking_uid", "email"],
    ["booking_uid", "email", "name", "time_zone", "absent"],
    1237,
  ),
  object(
    "booking_hosts",
    ["booking_uid", "user_id"],
    ["booking_uid", "user_id", "email", "name"],
    871,
  ),
];
const calcomForeignKeys: Record<string, ConnectorForeignKey[]> = {
  users: [
    fk("default_schedule_id", "schedules", "schedule_id"),
    fk("organization_id", "teams", "team_id"),
  ],
  teams: [fk("parent_id", "teams", "team_id")],
  team_memberships: [fk("team_id", "teams"), fk("user_id", "users")],
  event_types: [
    fk("owner_id", "users", "user_id"),
    fk("team_id", "teams"),
    fk("parent_event_type_id", "event_types", "event_type_id"),
    fk("schedule_id", "schedules"),
  ],
  schedules: [fk("owner_id", "users", "user_id")],
  bookings: [
    fk("event_type_id", "event_types"),
    fk("rescheduled_from_uid", "bookings", "booking_uid"),
    fk("rescheduled_to_uid", "bookings", "booking_uid"),
  ],
  booking_attendees: [fk("booking_uid", "bookings")],
  booking_hosts: [fk("booking_uid", "bookings"), fk("user_id", "users")],
};
// What a key of an account without teams cannot read.
const calcomTeams = new Set(["teams", "team_memberships"]);

// Granola's public API (public-api.granola.ai), as granola.go reads it: the
// notes the key's scopes read with their owner, calendar event and summary,
// one row per person a note lists as an attendee or its event invited, the
// folders notes sit in, and the spaces folders sit in. Attachments are not
// synced; each note's summary, and its transcript when the connection says
// so, are documents (`documentStreams`).
const granolaObjects = [
  object(
    "notes",
    "note_id",
    [
      "note_id",
      "title",
      "owner_name",
      "owner_email",
      "created_at",
      "updated_at",
      "web_url",
      "calendar_event_id",
      "calendar_event_title",
      "organiser_email",
      "scheduled_start_time",
      "scheduled_end_time",
      "summary_text",
      "summary_markdown",
    ],
    418,
  ),
  object(
    "note_attendees",
    ["note_id", "email"],
    ["note_id", "email", "name", "attended", "invited"],
    1306,
  ),
  object(
    "note_folders",
    ["note_id", "folder_id"],
    ["note_id", "folder_id"],
    377,
  ),
  object(
    "folders",
    "folder_id",
    ["folder_id", "name", "parent_folder_id", "space_id"],
    12,
  ),
  object("spaces", "space_id", ["space_id", "name"], 3),
];
const granolaForeignKeys: Record<string, ConnectorForeignKey[]> = {
  note_attendees: [fk("note_id", "notes")],
  note_folders: [fk("note_id", "notes"), fk("folder_id", "folders")],
  folders: [
    fk("parent_folder_id", "folders", "folder_id"),
    fk("space_id", "spaces"),
  ],
};

// Slack's Web API (slack.com/api), as slack.go reads it with a bot token:
// the workspace's people, its channels, and the members and the messages
// of the channels the app is in, over the last history_days. A message is
// keyed by its channel and ts, and is one conversations.history lists: a
// thread's replies show only in its day's document. channel_days holds one
// row per channel and day with messages, each a document
// (`documentStreams`). Messages and days need the history scope of the
// channels the app is in (channels:history for public ones, groups:history
// for private); without it they are unavailable, saying which scope to add.
const slackObjects = [
  object(
    "users",
    "user_id",
    [
      "user_id",
      "team_id",
      "name",
      "real_name",
      "display_name",
      "email",
      "title",
      "is_bot",
      "is_admin",
      "is_owner",
      "deleted",
      "tz",
      "updated_at",
    ],
    18,
  ),
  object(
    "channels",
    "channel_id",
    [
      "channel_id",
      "name",
      "is_private",
      "is_archived",
      "is_member",
      "topic",
      "purpose",
      "creator_id",
      "created_at",
      "num_members",
    ],
    5,
  ),
  object(
    "channel_members",
    ["channel_id", "user_id"],
    ["channel_id", "user_id"],
    31,
  ),
  object(
    "messages",
    "message_id",
    [
      "message_id",
      "channel_id",
      "ts",
      "thread_ts",
      "user_id",
      "bot_id",
      "subtype",
      "text",
      "sent_at",
      "edited_at",
      "reply_count",
      "reply_users_count",
      "latest_reply_at",
    ],
    0,
  ),
  object(
    "channel_days",
    "channel_day_id",
    [
      "channel_day_id",
      "channel_id",
      "day",
      "message_count",
      "first_message_at",
      "last_message_at",
    ],
    0,
  ),
];
const slackForeignKeys: Record<string, ConnectorForeignKey[]> = {
  channels: [fk("creator_id", "users", "user_id")],
  channel_members: [fk("channel_id", "channels"), fk("user_id", "users")],
  messages: [fk("channel_id", "channels"), fk("user_id", "users")],
  channel_days: [fk("channel_id", "channels")],
};
// The scope each table needs of the mock's public channels; a token
// containing "noscope" lacks channels:history (and groups:history).
const slackScopes: Record<string, string> = {
  users: "users:read",
  channels: "channels:read",
  channel_members: "channels:read",
  messages: "channels:history",
  channel_days: "channels:history",
};
const SLACK_TEAM = "T0MOCKACME",
  // The mock app's bot user, which the no-channels warning names.
  SLACK_BOT = "Foundry";
// The workspace's public channels; the app is in all but #random, unless
// its token contains "lonely" (in none). Each channel says one of its lines
// a day.
const slackChannels = [
  {
    id: "C0MOCKGEN",
    name: "general",
    lines: [
      "Morning all: the weekly metrics are in Notion.",
      "Reminder that the all-hands moves to Thursday this week.",
      "Welcome Margaret, who joins the platform team today!",
      "The office is closed on Friday for the move.",
      "The Q4 planning doc is ready for comments.",
    ],
  },
  {
    id: "C0MOCKSAL",
    name: "sales",
    lines: [
      "The Globex renewal is on track for the end of the month.",
      "Initech asked for a revised quote for 40 seats.",
      "Umbrella Holdings wants a call about the Enterprise plan.",
      "Closed the Hooli expansion: 25 more seats from November.",
      "Pipeline review moved to 3pm.",
    ],
  },
  {
    id: "C0MOCKSUP",
    name: "support",
    lines: [
      "Globex reported slow CSV exports again; the ticket is with engineering.",
      "Initech can't reset passwords for two users, looking now.",
      "The queue is clear, nice work everyone.",
      "Umbrella Holdings asked how to rotate their API key.",
      "The new macro for refund requests is live.",
    ],
  },
  {
    id: "C0MOCKENG",
    name: "engineering",
    lines: [
      "The billing webhooks fix is deployed.",
      "The Postgres upgrade is scheduled for Saturday 06:00 UTC.",
      "The flaky export test is quarantined until Monday.",
      "Code freeze starts Wednesday for the release.",
      "Error rates are back to normal after the rollback.",
    ],
  },
  { id: "C0MOCKRND", name: "random", lines: [] },
];
// The mock's people in Slack, Notion and Google Workspace.
const PEOPLE_AT_WORK = [
  "Ada Lovelace",
  "Alan Turing",
  "Hedy Lamarr",
  "Margaret Hamilton",
];
const SLACK_REPLIES = [
  "Thanks, on it.",
  "Great news!",
  "Can you add it to the tracker?",
  "I'll follow up this afternoon.",
  "Noted, thanks for the heads-up.",
];

// Notion's API (api.notion.com), as notion.go reads it with an internal
// integration's secret: the workspace's people, the pages shared with it
// (every shared database's rows among them), and each shared database
// (data source) as a table of its own, named by its title in snake_case
// and keyed by its pages' ids, with page_id, url, created_at,
// last_edited_at and a column per property in name order (a date's end
// too), typed by the JSON it holds. The kind gives a name two would take
// to the oldest, and the others, or a title that is users or pages, take
// it with "__" and their id's suffix; the mock's titles and properties
// take none. A test shows the first rows of each of the oldest 20
// databases. Each shared page with a body is a document.
type NotionTable = {
  name: string;
  columns: [name: string, type: string, nullable?: true][];
  preview: Record<string, unknown>[];
  rows: number;
};
const notionUsers = Object.fromEntries(
  PEOPLE_AT_WORK.map((name, i) => [
    name,
    `6f1c${String(i + 1).padStart(4, "0")}-2b3a-4c5d-9e8f-1a2b3c4d5e6f`,
  ]),
);
const notionRow = (n: number, title: string, row: Record<string, unknown>) => {
  const id = `1b7e${String(n).padStart(4, "0")}-3c2d-4e5f-8a9b-0c1d2e3f4a5b`;
  return {
    page_id: id,
    url: `https://www.notion.so/${title.replace(/\W+/g, "-")}-${id.replaceAll("-", "")}`,
    created_at: `2026-0${(n % 9) + 1}-12T10:00:00Z`,
    last_edited_at: `2026-10-${String((n % 27) + 1).padStart(2, "0")}T16:30:00Z`,
    ...row,
  };
};
const notionRowColumns: NotionTable["columns"] = [
  ["page_id", "string"],
  ["url", "string", true],
  ["created_at", "string"],
  ["last_edited_at", "string"],
];
const notionTables: NotionTable[] = [
  {
    name: "users",
    columns: [
      ["user_id", "string"],
      ["type", "string"],
      ["name", "string", true],
      ["email", "string", true],
    ],
    preview: [],
    rows: 12,
  },
  {
    // 74 pages outside a database, 60 of them with a body, and the 59 rows
    // of the two databases below.
    name: "pages",
    columns: [
      ["page_id", "string"],
      ["title", "string", true],
      ["url", "string", true],
      ["parent_type", "string", true],
      ["parent_id", "string", true],
      ["data_source_id", "string", true],
      ["created_at", "string"],
      ["last_edited_at", "string"],
      ["created_by_id", "string", true],
      ["last_edited_by_id", "string", true],
    ],
    preview: [],
    rows: 74 + 38 + 21,
  },
  {
    // "Customer Accounts": ARR, Active, Name, Owner, Renewal date, Stage.
    name: "customer_accounts",
    columns: [
      ...notionRowColumns,
      ["arr", "number", true],
      ["active", "boolean", true],
      ["name", "string", true],
      ["owner", "array", true],
      ["renewal_date", "string", true],
      ["renewal_date_end", "string", true],
      ["stage", "string", true],
    ],
    preview: (
      [
        ["Globex", "Customer", 48000, "2026-11-30", "Ada Lovelace", true],
        ["Initech", "Customer", 31200, "2027-01-15", "Alan Turing", true],
        ["Umbrella Holdings", "Prospect", null, null, "Ada Lovelace", true],
        ["Hooli", "Customer", 60000, "2027-03-01", "Hedy Lamarr", true],
        ["Vandelay Industries", "Churned", 0, null, "Alan Turing", false],
      ] as const
    ).map(([name, stage, arr, renewal, owner, active], i) =>
      notionRow(i + 1, name, {
        arr,
        active,
        name,
        owner: [notionUsers[owner]],
        renewal_date: renewal,
        renewal_date_end: null,
        stage,
      }),
    ),
    rows: 38,
  },
  {
    // "Projects": Due, Lead, Name, Status, Tags.
    name: "projects",
    columns: [
      ...notionRowColumns,
      ["due", "string", true],
      ["due_end", "string", true],
      ["lead", "array", true],
      ["name", "string", true],
      ["status", "string", true],
      ["tags", "array", true],
    ],
    preview: (
      [
        ["Self-serve onboarding", "In progress", "Hedy Lamarr", "2026-11-14"],
        ["Billing migration", "Done", "Alan Turing", "2026-09-30"],
        ["SSO for Enterprise", "Not started", "Margaret Hamilton", null],
        ["Data export API", "In progress", "Ada Lovelace", "2026-12-05"],
        ["Q4 launch", "Planned", "Ada Lovelace", "2026-12-15"],
      ] as const
    ).map(([name, status, lead, due], i) =>
      notionRow(i + 101, name, {
        due,
        due_end: null,
        lead: [notionUsers[lead]],
        name,
        status,
        tags: i % 2 ? ["platform"] : ["growth", "q4"],
      }),
    ),
    rows: 21,
  },
];
const notionForeignKeys: Record<string, ConnectorForeignKey[]> = {
  pages: [
    fk("created_by_id", "users", "user_id"),
    fk("last_edited_by_id", "users", "user_id"),
  ],
};
const NOTION_PAGE_TITLES = [
  "Onboarding playbook",
  "Globex account plan",
  "Incident runbook",
  "Pricing FAQ",
  "Hiring process",
  "Weekly metrics",
  "Roadmap 2027",
  "Security policy",
  "Support escalation guide",
  "Engineering principles",
];
// How often notion.go reads a page's document again though it shows no
// edit, in seconds.
const NOTION_REREAD = 7 * 24 * 3600;
// The pages with a body, newest edit first: each a document.
const notionPages = Array.from({ length: 60 }, (_, i) => {
  const title = NOTION_PAGE_TITLES[i % NOTION_PAGE_TITLES.length]!,
    id = `2a1f${String(i + 1).padStart(4, "0")}-6c3b-4d2e-9f1a-7b8c9d0e1f2a`;
  return {
    id,
    title,
    url: `https://www.notion.so/${title.replace(/\W+/g, "-")}-${id.replaceAll("-", "")}`,
    editedAt: new Date(
      Date.parse("2026-10-27T14:05:00.000Z") - i * 29 * 3_600_000,
    ).toISOString(),
    owner: PEOPLE_AT_WORK[i % PEOPLE_AT_WORK.length]!,
  };
});

// Google Workspace's APIs (googleapis.com), as google.go reads them as one
// user through a service account's domain-wide delegation: the Directory's
// users (an admin's), the Drive files the user can open (My Drive and what
// is shared with them, then each shared drive), their calendars, and the
// events of the last event_days (and the next 30) with their attendees,
// saying when Google left them out. Each Google Doc the user may download
// is a document, exported as Markdown without its images. A scope the
// admin didn't delegate makes its API's tables unavailable.
const googleObjects = [
  object(
    "users",
    "user_id",
    [
      "user_id",
      "primary_email",
      "full_name",
      "given_name",
      "family_name",
      "is_admin",
      "suspended",
      "archived",
      "org_unit_path",
      "title",
      "department",
      "manager_email",
      "created_at",
      "last_login_at",
    ],
    26,
  ),
  object(
    "files",
    "file_id",
    [
      "file_id",
      "name",
      "mime_type",
      "drive_id",
      "parent_id",
      "owner_email",
      "last_modifying_user_email",
      "created_at",
      "modified_at",
      "size",
      "web_view_link",
      "shared",
      "starred",
    ],
    312,
  ),
  object(
    "calendars",
    "calendar_id",
    [
      "calendar_id",
      "summary",
      "description",
      "time_zone",
      "access_role",
      "primary",
    ],
    6,
  ),
  object(
    "calendar_events",
    "event_id",
    [
      "event_id",
      "calendar_id",
      "ical_uid",
      "summary",
      "description",
      "location",
      "status",
      "start_at",
      "end_at",
      "all_day",
      "organizer_email",
      "creator_email",
      "recurring_event_id",
      "html_link",
      "created_at",
      "updated_at",
      "attendee_count",
      "attendees_omitted",
    ],
    684,
  ),
  object(
    "event_attendees",
    ["event_id", "email"],
    [
      "event_id",
      "email",
      "display_name",
      "response_status",
      "optional",
      "organizer",
      "is_self",
    ],
    2140,
  ),
];
const googleForeignKeys: Record<string, ConnectorForeignKey[]> = {
  calendar_events: [fk("calendar_id", "calendars")],
  event_attendees: [fk("event_id", "calendar_events")],
};
// The scope each table's API needs; a key whose client_email contains
// "nocalendar" wasn't delegated Calendar's.
const googleScopes: Record<string, string> = {
  users: "admin.directory.user.readonly",
  files: "drive.readonly",
  calendars: "calendar.readonly",
  calendar_events: "calendar.readonly",
  event_attendees: "calendar.readonly",
};
const GOOGLE_DOC_NAMES = [
  "Q4 planning",
  "Globex renewal brief",
  "Board update: October",
  "Customer onboarding checklist",
  "Security review",
  "Sales playbook",
  "Initech proposal",
  "Hiring plan 2027",
  "Support escalation guide",
  "Product roadmap",
  "Team offsite notes",
  "Pricing proposal",
  "Umbrella Holdings call notes",
  "Engineering principles",
];
// The Docs in Drive, newest change first, each owned by one of the people
// ("ada@example.com").
const googleDocs = GOOGLE_DOC_NAMES.map((name, i) => {
  const id = `1mockDoc${String(i + 1).padStart(4, "0")}aBcDeFgHiJkLmNoPq`;
  return {
    id,
    name,
    // Its webViewLink.
    link: `https://docs.google.com/document/d/${id}/edit?usp=drivesdk`,
    owner: `${PEOPLE_AT_WORK[i % PEOPLE_AT_WORK.length]!.split(" ")[0]!.toLowerCase()}@example.com`,
    modifiedAt: new Date(
      Date.parse("2026-10-27T16:20:00.000Z") - i * 61 * 3_600_000,
    ).toISOString(),
    // Drive's version of the file.
    driveVersion: String(40 + i * 3),
  };
});

// ---------------------------------------------------------------- Documents

/**
 * A stream: one kind of document a table's rows carry, kept per connector in
 * a set of its own (connector.Stream). `name` never changes; `title` names
 * the set.
 */
export type MockStream = {
  name: string;
  table: string;
  title: string;
  description: string;
  mediaType: "text/markdown" | "text/plain";
  // connector.KeepNewest(n): the set never holds more than the newest n by
  // their time, at every sync, and a document offered again with the same
  // text and a newer time moves up in place. Left out, the set keeps
  // foundry.documents.max_per_stream, as a sync of everything trims it.
  keepNewest?: number;
};
/** The text of one row of a stream's table, as the source has it now. */
export type MockSourceDocument = {
  externalId: string;
  version: string;
  // The item's time at the source: it orders the stream, newest first.
  at: string;
  title: string;
  url: string;
  text: string;
};

const granolaStreams: MockStream[] = [
  {
    name: "notes",
    table: "notes",
    title: "Notes",
    description:
      "Each Granola meeting note as Markdown: its title, date, attendees and note id, then the summary Granola wrote.",
    mediaType: "text/markdown",
  },
  {
    name: "transcripts",
    table: "notes",
    title: "Transcripts",
    description:
      "Each Granola meeting's transcript as plain text: its title, date, attendees and note id, then one " +
      '"Speaker: words" line per thing said, naming the speaker when Granola knows them, else Me (the note\'s owner) or Them.',
    mediaType: "text/plain",
  },
];

// Granola's notes, one per row of the notes table, newest first. Note 3 is
// edited at the source before every sync: its title says "edited", and each
// sync reads a new updated_at and new text.
const NOTE_TITLES = [
  "Weekly pipeline review",
  "Globex renewal call",
  "Design crit: onboarding",
  "Hiring sync",
  "Initech kickoff",
  "1:1 Ada and Alan",
  "Roadmap planning",
  "Incident retro: billing webhooks",
];
const NOTE_PEOPLE = [
  ["Ada Lovelace", "ada@example.com"],
  ["Alan Turing", "alan@example.com"],
  ["Grace Hopper", "grace@globex.example.com"],
  ["Katherine Johnson", "katherine@initech.example.com"],
] as const;
const granolaNotes = Array.from({ length: 418 }, (_, i) => {
  const created = Date.parse("2026-10-20T15:00:00.000Z") - i * 17 * 3_600_000;
  return {
    id: `not_mock${String(i + 1).padStart(4, "0")}`,
    title:
      i === 2
        ? "Pricing review (edited)"
        : NOTE_TITLES[i % NOTE_TITLES.length]!,
    createdAt: new Date(created).toISOString(),
    updatedAt: new Date(created + 3_600_000).toISOString(),
    attendees: [NOTE_PEOPLE[i % 4]!, NOTE_PEOPLE[(i + 1) % 4]!] as const,
  };
});

const slackStream: MockStream = {
  name: "conversations",
  table: "channel_days",
  title: "Conversations",
  description:
    "Each day of one Slack channel the app is in, as Markdown: a heading with the channel and the UTC day, " +
    'the channel\'s name and id, then each message posted that day in time order as "**Name** HH:MM: text", in UTC, ' +
    "with the replies of a thread it started listed beneath it, dated when sent on a later day. Replies show only here, " +
    "so replies to a thread started before the kept days are left out, and a reply edited later reads as first written " +
    "until its day changes again. A day too long for one document keeps its earliest messages. People are named " +
    "by their Slack real name, else their handle, and mentions read @Name and #channel.",
  mediaType: "text/markdown",
};
const notionStream: MockStream = {
  name: "pages",
  table: "pages",
  title: "Pages",
  description:
    "Each Notion page shared with the integration, database rows included, as Markdown: its title and " +
    "page id, then the page's content as Notion exports it.",
  mediaType: "text/markdown",
};
const googleStream: MockStream = {
  name: "docs",
  table: "files",
  title: "Docs",
  description:
    "Each Google Doc the connection's user can open and download in Drive, as Markdown: its title, owner, last " +
    "modified time and file id, then the Doc's text as Drive exports it, with its headings, lists, tables and links; its " +
    "images are left out, keeping their alt text, and a Doc whose export is over Drive's 10 MB limit is its plain text.",
  mediaType: "text/markdown",
};

const clock = (at: number) => new Date(at).toISOString().slice(11, 16);
/** The first `digits` hex digits of `text`'s SHA-256. */
const sha = (text: string, digits: number) =>
  createHash("sha256").update(text).digest("hex").slice(0, digits);
/**
 * The channel days a Slack sync at `syncedAt` reads, most recently active
 * first: each of the history_days before the sync's (the mock's channels
 * say nothing on the sync's own day) that a channel the app is in has
 * messages on (two of every three), each with a message, a reply in its
 * thread (every other day in the afternoon, after the day's last message)
 * and a later message. A day's messages are the same at every sync. Its
 * time is its latest activity, as slack.go's: its last message, or a
 * thread's latest reply when later.
 */
function slackDays(src: Source, syncedAt: string) {
  if (!("slack" in src.settings) || src.secret.includes("lonely")) return [];
  const days = src.settings.slack.historyDays || 90,
    today = Date.parse(syncedAt.slice(0, 10));
  return Array.from({ length: days }, (_, i) => today - (i + 1) * 86_400_000)
    .flatMap((midnight) => {
      const n = midnight / 86_400_000;
      return slackChannels
        .filter((c) => c.lines.length)
        .flatMap((c, j) => {
          if ((n + j) % 3 === 0) return [];
          const minute = ((n * 7 + j * 11) % 40) + 5,
            at = (h: number, m: number) => midnight + (h * 60 + m) * 60_000,
            who = (k: number) =>
              PEOPLE_AT_WORK[(n + j + k) % PEOPLE_AT_WORK.length]!;
          const first = {
            at: at(9, minute),
            who: who(0),
            text: c.lines[n % c.lines.length]!,
            replies: [
              {
                at: at((n + j) % 2 ? 16 : 9, minute + 12),
                who: who(1),
                text: SLACK_REPLIES[(n + j) % SLACK_REPLIES.length]!,
              },
            ],
          };
          const second = {
            at: at(14, ((n * 5 + j * 13) % 50) + 5),
            who: who(2),
            text: c.lines[(n + 3) % c.lines.length]!,
            replies: [],
          };
          const messages = [first, second];
          return [
            {
              channel: c,
              day: new Date(midnight).toISOString().slice(0, 10),
              messages,
              active: Math.max(
                ...messages.flatMap((m) => [
                  m.at,
                  ...m.replies.map((r) => r.at),
                ]),
              ),
            },
          ];
        });
    })
    .sort(
      (a, b) =>
        b.active - a.active ||
        `${a.channel.id}:${a.day}`.localeCompare(`${b.channel.id}:${b.day}`),
    );
}

/**
 * The streams `src`'s kind has, each with the documents a sync at `syncedAt`
 * reads into it, newest first, or null when its settings turn it off, or
 * its table can't be read. A kind without documents has none. A
 * document's version is a render revision and what changes it at the
 * source, as the kinds write it: a Granola note's "1|" and updated_at (an
 * edited one's is the sync's time), a channel day's "2|" and hashes of its
 * messages and of the names they show, a Notion page's "2|", last edit and
 * week of its weekly re-read, and a Doc's "1|", modified time, Drive's
 * version and a hash of its name, owner and link.
 */
export function documentStreams(
  src: Source,
  syncedAt: string,
): { stream: MockStream; documents: MockSourceDocument[] | null }[] {
  const s = src.settings;
  if ("slack" in s)
    return [
      {
        stream: slackStream,
        documents: src.secret.includes("noscope")
          ? null
          : slackDays(src, syncedAt).map(
              ({ channel, day, messages, active }) => {
                // slack.go's document: each message after a blank line, its
                // thread's replies listed beneath it.
                const line = (m: { at: number; who: string; text: string }) =>
                  `**${m.who}** ${clock(m.at)}: ${m.text}\n`;
                const text =
                  `# #${channel.name}, ${day}\n\nChannel: #${channel.name} (${channel.id})\n` +
                  messages
                    .map(
                      (m) =>
                        "\n" +
                        line(m) +
                        m.replies.map((r) => "  - " + line(r)).join(""),
                    )
                    .join("");
                return {
                  externalId: `${channel.id}:${day}`,
                  // slack.go's "2|<content>|<people>": 40 hex digits of a hash
                  // of the channel's name and what history says of each
                  // message (its time, text, author, reply count and latest
                  // reply), then 16 of the names they show. A reply's edit,
                  // or the name of who wrote it, changes neither.
                  version: `2|${sha(
                    [
                      channel.name,
                      ...messages.flatMap((m) => [
                        m.at,
                        m.text,
                        m.who,
                        m.replies.length,
                        m.replies.at(-1)?.at ?? "",
                      ]),
                    ].join("\0"),
                    40,
                  )}|${sha([...new Set(messages.map((m) => m.who))].sort().join("\0"), 16)}`,
                  at: new Date(active).toISOString(),
                  title: `#${channel.name}, ${day}`,
                  url: `https://app.slack.com/client/${SLACK_TEAM}/${channel.id}`,
                  text,
                };
              },
            ),
      },
    ];
  if ("notion" in s)
    return [
      {
        stream: notionStream,
        documents: notionPages.map((p) => ({
          externalId: p.id,
          // notion.go's version: "2|", the page's last edit in RFC 3339
          // (Go's, no trailing zeros) and the week of its weekly re-read,
          // offset by its id so pages aren't all read again in one sync. A
          // page read again unchanged is updated in place.
          version: `2|${p.editedAt.replace(/\.?0+Z$/, "Z")}|${Math.floor(
            (Math.floor(Date.parse(syncedAt) / 1000) +
              Number(
                createHash("sha256").update(p.id).digest().readBigUInt64BE() %
                  BigInt(NOTION_REREAD),
              )) /
              NOTION_REREAD,
          )}`,
          at: p.editedAt,
          title: p.title,
          url: p.url,
          // Its title and id, then its body; its last edit is its time.
          text: [
            `# ${p.title}`,
            "",
            `Page id: ${p.id}`,
            "",
            "## Overview",
            "",
            `${p.owner} keeps this page up to date.`,
            "",
            "- Status: in use",
            `- Owner: ${p.owner}`,
            "",
          ].join("\n"),
        })),
      },
    ];
  if ("google" in s)
    return [
      {
        stream: googleStream,
        documents: googleDocs.map((d) => ({
          externalId: d.id,
          // google.go's version: "1|", the Doc's modified time as Drive
          // gives it, Drive's version, then 16 hex digits of a hash of what
          // the document shows beside the export (its name, owner and link).
          version: `1|${d.modifiedAt}|${d.driveVersion}|${sha(`${d.name}\0${d.owner}\0${d.link}`, 16)}`,
          at: d.modifiedAt,
          title: d.name,
          url: d.link,
          text: [
            `# ${d.name}`,
            "",
            `Owner: ${d.owner}`,
            `Last modified: ${d.modifiedAt.slice(0, 16).replace("T", " ")} UTC`,
            `File id: ${d.id}`,
            "",
            "## Summary",
            "",
            "Written for the team, to read before the next review.",
            "",
            "## Next steps",
            "",
            "- Review by Friday",
            "- Share with the account owners",
            "",
          ].join("\n"),
        })),
      },
    ];
  if (!("granola" in s)) return [];
  const transcripts = !!s.granola.transcripts;
  return granolaStreams.map((stream) => ({
    stream,
    documents:
      stream.name === "transcripts" && !transcripts
        ? null
        : granolaNotes.map((n) => {
            const updated = n.title.includes("edited") ? syncedAt : n.updatedAt;
            const [[a], [b]] = n.attendees;
            const head = [
              `# ${n.title}`,
              "",
              `Date: ${n.createdAt.slice(0, 16).replace("T", " ")} UTC`,
              `Attendees: ${n.attendees.map(([name, email]) => `${name} <${email}>`).join(", ")}`,
              `Note id: ${n.id}`,
              "",
            ];
            const body =
              stream.name === "notes"
                ? [
                    "## Summary",
                    "",
                    `- ${a} walked through where ${n.title.toLowerCase()} stands.`,
                    `- ${b} owns the follow-ups, due within the week.`,
                    ...(n.title.includes("edited")
                      ? [`- Edited at ${updated}.`]
                      : []),
                  ]
                : [
                    `${a}: Let's start with ${n.title.toLowerCase()}.`,
                    `${b}: Sure. I'll share my screen.`,
                    ...(n.title.includes("edited")
                      ? [`${a}: Edited at ${updated}.`]
                      : []),
                  ];
            return {
              externalId: n.id,
              // RFC3339Nano, as Go writes it: no trailing zeros.
              version: `1|${updated.replace(/\.?0+Z$/, "Z")}`,
              at: n.createdAt,
              title: n.title,
              url: `https://notes.granola.ai/d/${n.id}`,
              text: [...head, ...body, ""].join("\n"),
            };
          }),
  }));
}

// An unreadable object is listed by name, or "name: reason" where the
// source says why.
function apiDiscovery(
  objects: ApiObject[],
  foreignKeys: Record<string, ConnectorForeignKey[]>,
  readable: (name: string) => boolean,
  reason: (name: string) => string = () => "",
): ConnectorDiscovery {
  const tables: ConnectorTable[] = objects
    .filter((o) => readable(o.name))
    .map((o) => ({
      name: o.name,
      primaryKey: o.key,
      foreignKeys: (foreignKeys[o.name] || []).filter((fk) =>
        readable(fk.referencedTable),
      ),
    }));
  const unavailable = objects
    .filter((o) => !readable(o.name))
    .map((o) => (reason(o.name) ? `${o.name}: ${reason(o.name)}` : o.name));
  return unavailable.length ? { tables, unavailable } : { tables };
}

// ---------------------------------------------------------------- Databases

type DbTable = {
  name: string;
  columns: [name: string, postgres: string, mysql: string, nullable?: true][];
  primaryKey: string[];
  foreignKeys: [column: string, table: string, referenced: string][];
  rows: number;
};
const shop: DbTable[] = [
  {
    name: "accounts",
    columns: [
      ["id", "uuid", "char(36)"],
      ["name", "text", "varchar(255)"],
      ["plan", "text", "varchar(32)"],
      ["created_at", "timestamp with time zone", "datetime"],
    ],
    primaryKey: ["id"],
    foreignKeys: [],
    rows: 412,
  },
  {
    name: "users",
    columns: [
      ["id", "uuid", "char(36)"],
      ["account_id", "uuid", "char(36)"],
      ["email", "text", "varchar(255)"],
      ["name", "text", "varchar(255)", true],
      ["created_at", "timestamp with time zone", "datetime"],
    ],
    primaryKey: ["id"],
    foreignKeys: [["account_id", "accounts", "id"]],
    rows: 2318,
  },
  {
    name: "products",
    columns: [
      ["id", "integer", "int"],
      ["sku", "text", "varchar(64)"],
      ["name", "text", "varchar(255)"],
      ["price", "numeric(10,2)", "decimal(10,2)"],
    ],
    primaryKey: ["id"],
    foreignKeys: [],
    rows: 86,
  },
  {
    name: "orders",
    columns: [
      ["id", "bigint", "bigint"],
      ["account_id", "uuid", "char(36)"],
      ["user_id", "uuid", "char(36)", true],
      ["status", "text", "varchar(32)"],
      ["total", "numeric(12,2)", "decimal(12,2)"],
      ["placed_at", "timestamp with time zone", "datetime"],
    ],
    primaryKey: ["id"],
    foreignKeys: [
      ["account_id", "accounts", "id"],
      ["user_id", "users", "id"],
    ],
    rows: 9604,
  },
  {
    name: "order_items",
    columns: [
      ["order_id", "bigint", "bigint"],
      ["line", "integer", "int"],
      ["product_id", "integer", "int"],
      ["quantity", "integer", "int"],
      ["unit_price", "numeric(10,2)", "decimal(10,2)"],
    ],
    primaryKey: ["order_id", "line"],
    foreignKeys: [
      ["order_id", "orders", "id"],
      ["product_id", "products", "id"],
    ],
    rows: 22417,
  },
  // The sales team's contacts: the people the Sales graph enriches.
  {
    name: "people",
    columns: PEOPLE_COLUMNS,
    primaryKey: ["contact_id"],
    foreignKeys: [],
    rows: PEOPLE.length,
  },
];

// The sample company's database name, wherever it runs
// (src/web/areas/ontology/sample.ts).
const ACME_DATABASE = "acme";
// Acme Corp's tables, as local/acme/acme.sql creates and fills them:
// "  name type[ NOT NULL]" columns, its keys, and a row per value line.
const acme: DbTable[] = (() => {
  const sql = readFileSync(
    new URL("../../local/acme/acme.sql", import.meta.url),
    "utf8",
  );
  return [...sql.matchAll(/CREATE TABLE (\w+) \(\n([\s\S]*?)\n\);/g)].map(
    ([, name, body]) => {
      const lines = body!.split(",\n").map((l) => l.trim());
      const values = sql
        .slice(sql.indexOf(`INSERT INTO ${name} `))
        .split(";")[0]!;
      return {
        name: name!,
        columns: lines
          .filter((l) => !/^(PRIMARY|FOREIGN) KEY/.test(l))
          .map((l): DbTable["columns"][number] => {
            const [column, type] = l.split(" ");
            return l.endsWith("NOT NULL")
              ? [column!, type!, type!]
              : [column!, type!, type!, true];
          }),
        primaryKey: /PRIMARY KEY \(([^)]+)\)/.exec(body!)![1]!.split(", "),
        foreignKeys: [
          ...body!.matchAll(
            /FOREIGN KEY \((\w+)\) REFERENCES (\w+) \((\w+)\)/g,
          ),
        ].map(([, column, table, referenced]) => [
          column!,
          table!,
          referenced!,
        ]),
        rows: values.split("\n").filter((l) => l.startsWith("  (")).length,
      };
    },
  );
})();

function dbTable(t: DbTable, kind: "postgres" | "mysql"): ConnectorTable {
  return {
    name: t.name,
    columns: t.columns.map(
      ([name, pg, my, nullable]): ConnectorColumn => ({
        name,
        type: kind === "postgres" ? pg : my,
        ...(nullable ? { nullable } : {}),
      }),
    ),
    primaryKey: t.primaryKey,
    foreignKeys: t.foreignKeys.map(([column, table, referenced]) => ({
      column,
      referencedTable: table,
      referencedColumn: referenced,
    })),
  };
}

// The guard refuses loopback, private, link-local, CGNAT and reserved hosts.
const PRIVATE_HOST =
  /^(localhost|.*\.localhost|127\.|10\.|192\.168\.|169\.254\.|172\.(1[6-9]|2\d|3[01])\.|100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.|0\.|\[?::1\]?$|\[?f[cd])/i;

function prepareDatabase(
  kind: "postgres" | "mysql",
  c: DatabaseConnectorConfig,
): DatabaseConnectorConfig | SourceError {
  const host = (c.host || "").trim(),
    database = (c.database || "").trim(),
    user = (c.user || "").trim();
  if (!host || /[/\s]/.test(host))
    return invalid("enter the database server's host name or IP address");
  if (!database) return invalid("enter the database name");
  if (!user) return invalid("enter the database user");
  if (!c.password) return invalid("enter the database password");
  const port = c.port || (kind === "postgres" ? 5432 : 3306);
  if (port < 1 || port > 65535)
    return invalid("the database port must be between 1 and 65535");
  const schema = (c.schema || "").trim();
  if (kind === "mysql" && schema)
    return invalid(
      "this database has no schema setting; use the database name",
    );
  const tables = [
    ...new Set((c.tables || []).map((t) => t.trim()).filter(Boolean)),
  ];
  if (c.ca && !c.ca.includes("-----BEGIN CERTIFICATE-----"))
    return invalid("the CA certificate is not a PEM certificate");
  return {
    host,
    port,
    database,
    user,
    ...(kind === "postgres" ? { schema: schema || "public" } : {}),
    ...(tables.length ? { tables } : {}),
    ...(c.disableTls ? { disableTls: true } : {}),
    ...(c.ca ? { ca: c.ca } : {}),
  };
}

function discoverDatabase(
  kind: "postgres" | "mysql",
  s: DatabaseConnectorConfig,
  password: string,
): ConnectorDiscovery | SourceError {
  if (PRIVATE_HOST.test(s.host!))
    return invalid(
      "that host is not on the public internet; Helix can only reach public database hosts",
    );
  if (s.host!.endsWith(".invalid"))
    return refused(
      "could not connect; check the host, port and connection settings, then test again",
    );
  if (password === "wrong")
    return refused(
      "authentication failed; check the username, password and read permissions, then test again",
    );
  const all =
    kind === "mysql" || s.schema === "public"
      ? (s.database === ACME_DATABASE ? acme : shop)
          .map((t) => dbTable(t, kind))
          .sort((a, b) => a.name.localeCompare(b.name))
      : [];
  const missing = (s.tables || []).find((t) => !all.some((x) => x.name === t));
  if (missing)
    return invalid(
      `table "${missing}" is not in this database, or the user cannot read it`,
    );
  return { tables: all };
}

// ---------------------------------------------------------------- S3

// The mock stamp's foundry-connector role: the one principal customers'
// roles trust. It sits in the mock's own account.
export const S3_PRINCIPAL =
  "arn:aws:iam::000000000000:role/helix-mock-foundry-connector";
const S3_ACCOUNT = "000000000000";
// Helix's own buckets, refused as sources.
const S3_RESERVED = ["helix-mock-foundry-data"];
// Where the fixture bucket lives, as a test detects it.
const S3_REGION = "eu-west-2";
// foundry.sync.max_connector_bytes: 20 GiB.
const S3_MAX_BYTES = 20 * 2 ** 30;

const knobs = config as typeof config & { s3RoleAccess?: boolean };
onReset(() => {
  delete knobs.s3RoleAccess;
});

/** What a customer's role must trust; undefined where role access is off. */
export function s3Setup(workspaceId: string): S3ConnectorSetup | undefined {
  return knobs.s3RoleAccess === false
    ? undefined
    : {
        principalArn: S3_PRINCIPAL,
        externalId: `helix-foundry-${workspaceId}`,
      };
}

const BUCKET = /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/;
const REGION = /^[a-z]{2}(-[a-z]+)+-\d{1,2}$/;
const ROLE_ARN = /^arn:aws:iam::(\d{12}):role\/[\w+=,.@/-]{1,512}$/;
const M9 =
  "Helix can only reach public storage outside Helix; check the endpoint";

// S3's Prepare: settings checked and normalised, the secret split off.
function prepareS3(
  raw: Record<string, any>,
  workspaceId: string,
): Source | SourceError {
  const bucket = String(raw.bucket || "").trim();
  if (
    !BUCKET.test(bucket) ||
    bucket.includes("..") ||
    /^\d+\.\d+\.\d+\.\d+$/.test(bucket)
  )
    return invalid("enter the bucket name, like acme-exports");
  if (S3_RESERVED.includes(bucket))
    return invalid(`${bucket} is a Helix bucket`);
  const trimmed = String(raw.prefix || "")
      .trim()
      .replace(/^\//, ""),
    prefix = trimmed && !trimmed.endsWith("/") ? trimmed + "/" : trimmed;
  const region = String(raw.region || "").trim();
  if (region && (!REGION.test(region) || /^(cn-|us-gov-|us-iso)/.test(region)))
    return invalid("enter the bucket's region, like eu-west-2");
  const tables = [
    ...new Set(
      ((raw.tables || []) as unknown[])
        .map((t) => String(t).trim())
        .filter(Boolean),
    ),
  ];
  if (!raw.role === !raw.keys)
    return invalid("choose IAM role or access key access");
  const base = { bucket, prefix, region, tables };
  if (raw.role) {
    if (!s3Setup(workspaceId))
      return invalid(
        "IAM role access is not available here; use an access key",
      );
    const roleArn = String(raw.role.roleArn || "").trim(),
      account = ROLE_ARN.exec(roleArn)?.[1];
    if (!account || account === S3_ACCOUNT)
      return invalid(
        "enter the role ARN from your AWS account, like arn:aws:iam::123456789012:role/helix-foundry",
      );
    return {
      kind: "s3",
      settings: { s3: { ...base, role: { roleArn } } },
      secret: "",
      workspaceId,
    };
  }
  const accessKeyId = String(raw.keys.accessKeyId || "").trim(),
    secret = String(raw.keys.secretAccessKey || "").trim(),
    typed = String(raw.keys.endpoint || "").trim();
  if (
    accessKeyId.length < 3 ||
    accessKeyId.length > 128 ||
    /\s/.test(accessKeyId)
  )
    return invalid("enter the access key ID");
  let endpoint = "";
  if (typed) {
    let url: URL | undefined;
    try {
      url = new URL(typed);
    } catch {
      url = undefined;
    }
    if (
      !url ||
      url.protocol !== "https:" ||
      !url.hostname ||
      url.username ||
      url.password ||
      url.pathname !== "/" ||
      /[?#]/.test(typed)
    )
      return invalid(
        "enter the HTTPS address of your S3-compatible storage, without a path",
      );
    const host = url.hostname.toLowerCase();
    if (host.endsWith("amazonaws.com") || host.endsWith("amazonaws.com.cn"))
      return invalid("for AWS, leave the endpoint empty");
    if (host === "helix-db.com" || host.endsWith(".helix-db.com"))
      return invalid(M9);
    endpoint = url.origin;
  }
  if (!endpoint && accessKeyId.startsWith("ASIA"))
    return invalid(
      "temporary keys (ASIA…) expire; use an IAM user's key or a role",
    );
  if (!secret) return invalid("enter the secret access key");
  return {
    kind: "s3",
    settings: { s3: { ...base, keys: { accessKeyId, endpoint } } },
    secret,
    workspaceId,
  };
}

// The plan's V1 fixture folder, as S3 lists it: keys relative to the
// connector's folder, in key order.
const s3Fixture: Listed[] = [
  { key: "_SUCCESS", size: 0 },
  { key: "api/invoices.json", size: 47_312 },
  { key: "archive/old.csv", size: 2_048, archived: true },
  { key: "customers.csv", size: 86_016 },
  { key: "events/2026-10-01.jsonl.gz", size: 3_250_585 },
  { key: "events/2026-10-02.jsonl.zst", size: 3_041_870 },
  { key: "notes.xlsx", size: 12_288 },
  {
    key: "orders/dt=2026-10-01/part-00000-3f1e9c2a-7b4d-4e1f-9a2b-1c3d5e7f9a0b-c000.snappy.parquet",
    size: 644_245_094,
  },
  {
    key: "orders/dt=2026-10-02/part-00000-8c2d4e6f-1a3b-4c5d-8e7f-9a0b1c2d3e4f-c000.zstd.parquet",
    size: 644_245_095,
  },
  { key: "products.json", size: 18_944 },
];
// The bad-records bucket's malformed sample (filesource.Error's words).
const BAD_RECORD = "events/2026-10-02.jsonl.zst record 12: not a JSON object";

type FileTable = {
  // Sampled at a test; exact after a sync.
  columns: [name: string, type: string, nullable?: true][];
  // The first sampled rows; Parquet samples read the footer only.
  preview: object[];
  // The key a test guesses from its sample, then the one a sync measures.
  guess: string[];
  key: string[];
  rows: number;
};
const invoice = (n: number, status: string) => ({
  id: `in_${1000 + n}`,
  object: "invoice",
  customer: `cus_${200 + n}`,
  amount_due: 4900 * n,
  currency: "gbp",
  status,
  created: 1759309200 + n * 86400,
  lines: { object: "list", data: [{ price: "price_team", quantity: n }] },
});
const s3Tables: Record<string, FileTable> = {
  api: {
    columns: [
      ["id", "string"],
      ["object", "string"],
      ["customer", "string"],
      ["amount_due", "integer"],
      ["currency", "string"],
      ["status", "string"],
      ["created", "integer"],
      ["lines", "json"],
    ],
    preview: [1, 2, 3, 4, 5].map((n) => invoice(n, n === 4 ? "open" : "paid")),
    guess: ["id"],
    key: ["id"],
    rows: 120,
  },
  customers: {
    columns: [
      ["id", "integer"],
      ["name", "string"],
      ["email", "string"],
      ["country", "string"],
      ["signed_up", "string"],
    ],
    preview: [
      ["Ada Lovelace", "GB"],
      ["Grace Hopper", "US"],
      ["Alan Turing", "GB"],
      ["Katherine Johnson", "US"],
      ["Hedy Lamarr", "AT"],
    ].map(([name, country], i) => ({
      id: i + 1,
      name,
      email: `${name!.split(" ")[0]!.toLowerCase()}@example.com`,
      country,
      signed_up: `2026-0${i + 1}-04`,
    })),
    guess: ["id"],
    key: ["id"],
    rows: 1000,
  },
  events: {
    columns: [
      ["event_id", "string"],
      ["type", "string"],
      ["customer_id", "integer"],
      ["at", "string"],
      ["properties", "json"],
      // Only the newer file has it.
      ["session_id", "string", true],
    ],
    preview: [1, 2, 3, 4, 5].map((n) => ({
      event_id: `evt_${9000 + n}`,
      type: n % 2 ? "page_viewed" : "order_placed",
      customer_id: n,
      at: `2026-10-02T09:0${n}:00Z`,
      properties: { path: `/shop/${n}`, referrer: null },
      session_id: `ses_${n}`,
    })),
    guess: [],
    key: [],
    rows: 4812,
  },
  orders: {
    columns: [
      ["id", "integer"],
      ["customer_id", "integer"],
      ["total", "decimal"],
      ["currency", "string"],
      ["placed_at", "timestamptz"],
      ["items", "json"],
      ["shipping", "json", true],
      // The Hive partition, last.
      ["dt", "string"],
    ],
    preview: [],
    guess: [],
    key: ["id"],
    rows: 24_310,
  },
  products: {
    columns: [
      ["id", "integer"],
      ["name", "string"],
      ["price", "float"],
      ["tags", "json"],
      ["active", "boolean"],
    ],
    preview: ["Notebook", "Pen", "Desk lamp", "Mug", "Backpack"].map(
      (name, i) => ({
        id: i + 1,
        name,
        price: 4.5 + i * 10,
        tags: ["office"],
        active: i !== 3,
      }),
    ),
    guess: ["id"],
    key: ["id"],
    rows: 40,
  },
};
const fileColumns = (t: FileTable): ConnectorColumn[] =>
  t.columns.map(([name, type, nullable]) => ({
    name,
    type,
    ...(nullable ? { nullable } : {}),
  }));

// S3's Discover at a test: credentials, the bucket, its listing laid out
// into tables, then each table's sample. The region it detects is stored.
function discoverS3(
  src: Source,
  s: S3ConnectorConfig,
): { discovery: ConnectorDiscovery; settings: ConnectorConfig } | SourceError {
  const where = `s3://${s.bucket}/${s.prefix || ""}`;
  if ("role" in s) {
    const setup = s3Setup(src.workspaceId),
      arn = s.role.roleArn || "";
    if (!setup)
      return invalid(
        "IAM role access is not available here; use an access key",
      );
    if (arn.includes("untrusted"))
      return refused(
        `Helix could not assume the role; check its trust policy names ${setup.principalArn} and requires external ID ${setup.externalId}`,
      );
    if (arn.includes("open"))
      return invalid(
        `your role's trust policy must require external ID ${setup.externalId}; Helix won't use a role that doesn't`,
      );
  } else {
    if (s.keys.endpoint && PRIVATE_HOST.test(new URL(s.keys.endpoint).hostname))
      return invalid(M9);
    if (src.secret.includes("bad"))
      return refused("S3 rejected the access key; check the key ID and secret");
  }
  if (s.bucket === "missing-bucket")
    return invalid(`bucket ${s.bucket} does not exist`);
  if (s.bucket === "denied-bucket")
    return refused(
      `Helix cannot list ${where}; allow s3:ListBucket on this folder`,
    );
  if (s.bucket === "empty-bucket")
    return invalid(
      `no files Helix can read under ${where}; Helix reads Parquet, JSON, JSON Lines and CSV`,
    );
  const fallback =
    (s.prefix || "").split("/").filter(Boolean).at(-1) || s.bucket!;
  const { datasets, skipped } = layout(fallback, s3Fixture);
  const missing = (s.tables || []).find(
    (t) => !datasets.some((d) => d.name === t),
  );
  if (missing)
    return invalid(`table "${missing}" is not in this folder any more`);
  const tables = datasets.map((d): ConnectorTable => {
    const t = s3Tables[d.name]!,
      problem = s.bucket === "bad-records" && d.name === "events";
    return {
      name: d.name,
      columns: fileColumns(t),
      primaryKey: t.guess,
      foreignKeys: [],
      format: d.format,
      fileCount: d.files.length,
      bytes: String(d.files.reduce((n, f) => n + f.size, 0)),
      preview: t.preview.map((row) => JSON.stringify(row)),
      ...(problem ? { problem: BAD_RECORD } : {}),
    };
  });
  return {
    discovery: {
      tables,
      unavailable: skipped.map((x) => `${x.name}: ${x.reason}`),
      maxBytes: String(S3_MAX_BYTES),
    },
    // AWS answers with the bucket's region; a store at an endpoint keeps the
    // one given.
    settings: {
      s3: {
        ...s,
        region: "keys" in s && s.keys.endpoint ? s.region : S3_REGION,
      },
    },
  };
}

// S3's Sync: every chosen table, two passes over every file, exact columns
// and the key pass 1 measures; a malformed record fails the sync.
function syncS3(s: S3ConnectorConfig, found: ConnectorDiscovery) {
  const chosen = s.tables?.length ? new Set(s.tables) : undefined;
  const tables = (found.tables || []).filter(
    (t) => !chosen || chosen.has(t.name),
  );
  if (tables.some((t) => t.problem)) return BAD_RECORD;
  return tables.map((t): SyncedSnapshot => {
    const fixture = s3Tables[t.name]!;
    return {
      name: t.name,
      columns: fileColumns(fixture),
      primaryKey: fixture.key,
      foreignKeys: [],
      rows: fixture.rows,
    };
  });
}

// ---------------------------------------------------------------- Kinds

/**
 * A Google service account's JSON key, or undefined for anything else: an
 * object of type service_account with its client_email and private_key.
 */
function serviceAccount(
  key: string,
): { client_email: string; private_key_id?: string } | undefined {
  try {
    const k = JSON.parse(key) as Record<string, unknown>;
    return k?.type === "service_account" &&
      typeof k.client_email === "string" &&
      k.client_email &&
      typeof k.private_key === "string" &&
      k.private_key
      ? {
          client_email: k.client_email,
          ...(typeof k.private_key_id === "string"
            ? { private_key_id: k.private_key_id }
            : {}),
        }
      : undefined;
  } catch {
    return undefined;
  }
}

/** keyHint: the key's type prefix and last four characters, "sk_live…4f2a". */
export function keyHint(key: string, prefixParts: number) {
  if (key.length < 12) return "…";
  // Only parts an underscore ends: "cal_abc…" keeps "cal", never the key.
  const parts = key.split("_"),
    prefix = parts.slice(0, Math.min(prefixParts, parts.length - 1)).join("_");
  return prefix + "…" + key.slice(-4);
}

/**
 * Splits a request's config into kind, settings and credential, then checks
 * the settings as the kind's Prepare does. API keys and S3 secrets are
 * trimmed; a database password is kept as sent.
 */
export function prepare(
  config: unknown,
  workspaceId: string,
): Source | SourceError {
  const c = (config || {}) as Partial<Record<ConnectorKindName, any>>;
  const set = kinds.map((k) => k.name).filter((k) => c[k] !== undefined);
  if (set.length !== 1) return invalid("config is required");
  const kind = set[0]!,
    raw = c[kind] || {};
  switch (kind) {
    case "stripe": {
      const key = String(raw.apiKey || "").trim();
      if (!/^(sk|rk)_(test|live)_[A-Za-z0-9]+$/.test(key))
        return invalid(
          "enter a Stripe secret or restricted key (sk_… or rk_…)",
        );
      return { kind, settings: { stripe: {} }, secret: key, workspaceId };
    }
    case "workos": {
      const key = String(raw.apiKey || "").trim();
      if (!/^sk_[A-Za-z0-9_]+$/.test(key))
        return invalid("enter a WorkOS API key (sk_…)");
      return { kind, settings: { workos: {} }, secret: key, workspaceId };
    }
    case "posthog": {
      const key = String(raw.apiKey || "").trim(),
        p = raw as PostHogConnectorConfig;
      if (!/^phx_[A-Za-z0-9_]+$/.test(key))
        return invalid("enter a PostHog personal API key (phx_…)");
      const region = p.region || "us";
      let host = "";
      if (region === "custom") {
        let url: URL | undefined;
        try {
          url = new URL((p.host || "").trim());
        } catch {
          url = undefined;
        }
        if (!url || url.protocol !== "https:" || !url.host || url.username)
          return invalid(
            "enter the HTTPS address of your PostHog, without credentials",
          );
        host = url.origin;
      } else if (region === "us" || region === "eu") {
        if (p.host)
          return invalid("a PostHog host is only used with the custom region");
      } else return invalid("PostHog region must be us, eu or custom");
      const projectId = (p.projectId || "").trim();
      if (projectId && !/^\d+$/.test(projectId))
        return invalid(
          "the PostHog project ID is a number, found in project settings",
        );
      const eventDays = p.eventDays || 30;
      if (eventDays < 1 || eventDays > 365)
        return invalid("PostHog event days must be between 1 and 365");
      return {
        kind,
        settings: {
          posthog: {
            region,
            ...(host ? { host } : {}),
            ...(projectId ? { projectId } : {}),
            eventDays,
          },
        },
        secret: key,
        workspaceId,
      };
    }
    case "github": {
      const token = String(raw.token || "").trim(),
        g = raw as GitHubConnectorConfig;
      if (
        !/^(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})$/.test(
          token,
        )
      )
        return invalid(
          "enter a GitHub personal access token (github_pat_… or ghp_…)",
        );
      const repos: string[] = [];
      for (const r of (g.repos || []).map((r) => r.trim())) {
        if (!/^[A-Za-z0-9-]+\/[A-Za-z0-9._-]+$/.test(r))
          return invalid(`"${r}" is not a repository; write it as owner/name`);
        if (!repos.some((seen) => seen.toLowerCase() === r.toLowerCase()))
          repos.push(r);
      }
      if (repos.length > maxGitHubRepos)
        return invalid(`choose at most ${maxGitHubRepos} repositories`);
      const reviewDays = g.reviewDays || 90;
      if (reviewDays < 1 || reviewDays > 365)
        return invalid("review days must be between 1 and 365");
      return {
        kind,
        settings: {
          github: { ...(repos.length ? { repos } : {}), reviewDays },
        },
        secret: token,
        workspaceId,
      };
    }
    case "linear": {
      const key = String(raw.apiKey || "").trim();
      if (!/^lin_api_[A-Za-z0-9]+$/.test(key))
        return invalid("enter a Linear personal API key (lin_api_…)");
      return { kind, settings: { linear: {} }, secret: key, workspaceId };
    }
    case "calcom": {
      const key = String(raw.apiKey || "").trim();
      if (!/^cal_[A-Za-z0-9_]+$/.test(key))
        return invalid("enter a Cal.com API key (cal_live_… or cal_…)");
      return { kind, settings: { calcom: {} }, secret: key, workspaceId };
    }
    case "granola": {
      const key = String(raw.apiKey || "").trim();
      if (!/^grn_[A-Za-z0-9_-]+$/.test(key))
        return invalid("enter a Granola API key (grn_…)");
      return {
        kind,
        settings: { granola: { transcripts: raw.transcripts === true } },
        secret: key,
        workspaceId,
      };
    }
    case "slack": {
      const token = String(raw.botToken || "").trim();
      if (!/^xoxb-[A-Za-z0-9-]+$/.test(token))
        return invalid("enter a Slack bot token (xoxb-…)");
      const historyDays = raw.historyDays || 90;
      if (historyDays < 1 || historyDays > 365)
        return invalid("keep 1 to 365 days of history");
      return {
        kind,
        settings: { slack: { historyDays } },
        secret: token,
        workspaceId,
      };
    }
    case "notion": {
      const token = String(raw.token || "").trim();
      if (!/^(ntn|secret)_[A-Za-z0-9]+$/.test(token))
        return invalid("enter a Notion integration secret (ntn_…)");
      return { kind, settings: { notion: {} }, secret: token, workspaceId };
    }
    case "google": {
      const key = String(raw.serviceAccountKey || "").trim();
      if (!serviceAccount(key))
        return invalid("paste the service account's JSON key");
      const subject = String(raw.subject || "")
        .trim()
        .toLowerCase();
      if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(subject))
        return invalid("enter the email of the Workspace user Foundry acts as");
      const eventDays = raw.eventDays || 90;
      if (eventDays < 1 || eventDays > 365)
        return invalid("keep 1 to 365 days of past events");
      return {
        kind,
        settings: { google: { subject, eventDays } },
        secret: key,
        workspaceId,
      };
    }
    case "postgres":
    case "mysql": {
      const s = prepareDatabase(kind, raw);
      if ("code" in s) return s;
      const settings = kind === "postgres" ? { postgres: s } : { mysql: s };
      return {
        kind,
        settings,
        secret: String(raw.password || ""),
        workspaceId,
      };
    }
    case "s3":
      return prepareS3(raw, workspaceId);
    case "events": {
      // A description and a text field, each only when sent (null is not
      // sent, as protojson reads it), so an update that leaves one out
      // keeps it. An API source's fields are learned from what is sent to
      // it, never taken from a request; ./data.ts locks the text field.
      const events: EventsConnectorConfig = {};
      if (raw.description != null) {
        events.description = String(raw.description).trim();
        const n = [...events.description].length;
        if (n > MAX_EVENT_DESCRIPTION)
          return invalid(
            `a description holds at most ${MAX_EVENT_DESCRIPTION} characters; this one holds ${n}`,
          );
      }
      if (raw.textField != null) {
        events.textField = String(raw.textField);
        // "" clears it.
        const problem = events.textField && textFieldProblem(events.textField);
        if (problem) return invalid(problem);
      }
      return { kind, settings: { events }, secret: "", workspaceId };
    }
  }
}

/**
 * Proves the credential and lists what it reads, as the kind's Discover
 * does. It may complete the settings (the PostHog project, the S3 region).
 */
export function discover(
  src: Source,
): { discovery: ConnectorDiscovery; settings: ConnectorConfig } | SourceError {
  const s = src.settings;
  if ("s3" in s) return discoverS3(src, s.s3);
  // Nothing to dial: one table, events, keyed by the pieces' ids, whose
  // fields come with the first pieces.
  if ("events" in s)
    return {
      discovery: {
        tables: [
          {
            name: "events",
            columns: columnsOf(new Map()),
            primaryKey: ["id"],
          },
        ],
      },
      settings: s,
    };
  if ("postgres" in s || "mysql" in s) {
    const kind = "postgres" in s ? "postgres" : "mysql",
      d = discoverDatabase(
        kind,
        "postgres" in s ? s.postgres : s.mysql,
        src.secret,
      );
    return "code" in d ? d : { discovery: d, settings: s };
  }
  if (src.secret.includes("bad"))
    return refused(
      (
        {
          stripe: "Stripe rejected this key; check it and test again",
          workos: "WorkOS rejected this key; check it and test again",
          posthog:
            "PostHog rejected this key; check it and the region, then test again",
          github: "GitHub rejected this token; check it and enter it again",
          linear: "Linear rejected this key; check it and enter it again",
          calcom: "Cal.com rejected this key; check it and enter it again",
          granola: "Granola rejected this key; check it and enter it again",
          slack: "Slack rejected this token; check it and enter it again",
          notion: "Notion rejected this secret; check it and enter it again",
          google:
            "Google rejected this service account key; check it and paste it again",
        } as Partial<Record<ConnectorKindName, string>>
      )[src.kind]!,
    );
  if ("stripe" in s)
    return {
      discovery: apiDiscovery(
        stripeObjects,
        stripeForeignKeys,
        (name) => !src.secret.startsWith("rk_") || !stripeRestricted.has(name),
      ),
      settings: s,
    };
  if ("workos" in s)
    return {
      discovery: apiDiscovery(workosObjects, workosForeignKeys, () => true),
      settings: s,
    };
  if ("github" in s) {
    const missing = (s.github.repos || []).find((r) => /\/missing$/i.test(r));
    if (missing)
      return invalid(
        `GitHub has no repository ${missing} that this token can read`,
      );
    return {
      discovery: apiDiscovery(
        githubObjects,
        githubForeignKeys,
        (name) => !src.secret.includes("nopr") || !githubPullRequests.has(name),
      ),
      settings: s,
    };
  }
  if ("linear" in s)
    return {
      discovery: apiDiscovery(linearObjects, linearForeignKeys, () => true),
      settings: s,
    };
  if ("calcom" in s) {
    const discovery = apiDiscovery(
      calcomObjects,
      calcomForeignKeys,
      (name) => !src.secret.includes("solo") || !calcomTeams.has(name),
    );
    // calcom.go's Discover: a test (not a sync) warns when the key's user
    // administers none of its teams.
    if (src.secret.includes("member"))
      discovery.warnings = [
        "this key's user is not an admin of its Cal.com teams, so team_memberships and users " +
          "leave out the other members; use a team admin's key to read them",
      ];
    return { discovery, settings: s };
  }
  if ("granola" in s)
    return {
      discovery: apiDiscovery(
        granolaObjects,
        granolaForeignKeys,
        (name) => !src.secret.includes("personal") || name !== "spaces",
      ),
      settings: s,
    };
  if ("slack" in s) {
    // slack.go's Discover reads the token's scopes from auth.test: without
    // the history scope of the channels the app is in (the mock's are
    // public), messages and days are unavailable, saying which to add;
    // channel_members needs only the channel lists.
    const noHistory = (name: string) =>
      src.secret.includes("noscope") &&
      slackScopes[name] === "channels:history";
    const discovery = apiDiscovery(
      slackObjects,
      slackForeignKeys,
      (name) => !noHistory(name),
      (name) =>
        `add the ${slackScopes[name]} scope to the app and reinstall it`,
    );
    // A test (not a sync) warns when the app is in no channel, so reads none.
    if (src.secret.includes("lonely"))
      discovery.warnings = [
        `The app is in no channels yet: invite it to each channel to sync with /invite @${SLACK_BOT}, then test again`,
      ];
    return { discovery, settings: s };
  }
  if ("notion" in s) {
    if (src.secret.includes("empty"))
      return refused(
        "nothing is shared with this integration yet: in Notion, add it to a page or database from its ••• menu › Connections, then test again",
      );
    // Every table is typed; each shared database shows its first rows.
    const users = !src.secret.includes("nousers");
    const tables = notionTables
      .filter((t) => users || t.name !== "users")
      .map(
        (t): ConnectorTable => ({
          name: t.name,
          columns: t.columns.map(([name, type, nullable]) => ({
            name,
            type,
            ...(nullable ? { nullable } : {}),
          })),
          primaryKey: [t.name === "users" ? "user_id" : "page_id"],
          foreignKeys: (users && notionForeignKeys[t.name]) || [],
          ...(t.preview.length
            ? { preview: t.preview.map((row) => JSON.stringify(row)) }
            : {}),
        }),
      );
    return {
      discovery: users
        ? { tables }
        : {
            tables,
            unavailable: [
              "users: turn on Read user information in the integration's capabilities",
            ],
          },
      settings: s,
    };
  }
  if ("google" in s) {
    const subject = s.google.subject || "",
      email = serviceAccount(src.secret)?.client_email || "";
    if (subject.includes("nobody"))
      return refused(
        `Google has no user ${subject}, or the service account may not act as them; check the email`,
      );
    const notAdmin = (name: string) =>
        subject.includes("member") && name === "users",
      undelegated = (name: string) =>
        email.includes("nocalendar") &&
        googleScopes[name] === "calendar.readonly";
    return {
      discovery: apiDiscovery(
        googleObjects,
        googleForeignKeys,
        (name) => !notAdmin(name) && !undelegated(name),
        (name) =>
          notAdmin(name)
            ? `${subject} is not an admin, so the Directory's users can't be read`
            : `allow the scope https://www.googleapis.com/auth/${googleScopes[name]} for the service account's client ID in the Admin console's domain-wide delegation`,
      ),
      settings: s,
    };
  }
  const p = (s as { posthog: PostHogConnectorConfig }).posthog;
  let projectId = p.projectId;
  if (!projectId) {
    if (src.secret.includes("multi"))
      return invalid("enter a PostHog project ID: Web (12345), Mobile (67890)");
    projectId = "12345";
  }
  return {
    discovery: apiDiscovery(posthogObjects, posthogForeignKeys, () => true),
    settings: { posthog: { ...p, projectId } },
  };
}

export function hint(src: Source) {
  if (src.kind === "events") return "";
  // The key's service account and its key id's last four characters.
  if (src.kind === "google") {
    const k = serviceAccount(src.secret);
    return k ? `${k.client_email}…${(k.private_key_id || "").slice(-4)}` : "…";
  }
  if (src.kind === "slack")
    return src.secret.length < 12 ? "…" : "xoxb…" + src.secret.slice(-4);
  if (src.kind === "s3")
    return src.secret ? "secret …" + src.secret.slice(-4) : "IAM role";
  return src.kind === "postgres" || src.kind === "mysql"
    ? "password set"
    : keyHint(
        src.secret,
        src.kind === "posthog" ||
          src.kind === "granola" ||
          src.kind === "notion" ||
          (src.kind === "github" && !src.secret.startsWith("github_pat_"))
          ? 1
          : 2,
      );
}

/** One table a sync wrote: its schema and row count. */
export type SyncedSnapshot = ConnectorTable & { rows: number };

/**
 * What a sync of `src` writes, or why it fails. API tables gain their column
 * names from the rows; database tables keep their discovered columns; file
 * tables get the columns and key the sync measures.
 */
export function syncTables(src: Source): SyncedSnapshot[] | string {
  const found = discover(src);
  if ("code" in found) return found.message;
  const s = src.settings;
  if ("s3" in s) return syncS3(s.s3, found.discovery);
  if ("posthog" in s && (s.posthog.eventDays || 30) > posthogRowLimitDays)
    return "table events has more than 2000000 rows; choose fewer tables";
  const db = "postgres" in s ? s.postgres : "mysql" in s ? s.mysql : undefined;
  const chosen = db?.tables?.length ? new Set(db.tables) : undefined;
  const objects = db
    ? db.database === ACME_DATABASE
      ? acme
      : shop
    : (
        {
          stripe: stripeObjects,
          workos: workosObjects,
          posthog: posthogObjects,
          github: githubObjects,
          linear: linearObjects,
          calcom: calcomObjects,
          granola: granolaObjects,
          slack: slackObjects,
          notion: notionTables.map((t) => object(t.name, [], [], t.rows)),
          google: googleObjects,
        } as Partial<Record<ConnectorKindName, ApiObject[]>>
      )[src.kind]!;
  // Slack's history is what its days hold: two messages each, as history
  // lists them (a thread's reply is no row), and none (nor members) where
  // the app is in no channel.
  const days = "slack" in s ? slackDays(src, new Date().toISOString()) : [],
    counted: Record<string, number> =
      "slack" in s
        ? {
            messages: days.length * 2,
            channel_days: days.length,
            ...(src.secret.includes("lonely") ? { channel_members: 0 } : {}),
          }
        : {};
  return (found.discovery.tables || [])
    .filter((t) => !chosen || chosen.has(t.name))
    .map((t) => {
      const fake = objects.find((o) => o.name === t.name)!;
      return {
        ...t,
        columns: t.columns?.length
          ? t.columns
          : (fake as ApiObject).columns.map((name) => ({ name })),
        rows: counted[t.name] ?? fake.rows,
      };
    })
    .sort((a, b) => a.name.localeCompare(b.name));
}
