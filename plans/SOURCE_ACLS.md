# Source permissions (mirrored ACLs)

What someone reads in Foundry is what their grants allow **and** what the
source itself shares with them. Grants stay the admin's coarse layer (a
connector, a table, a set); a source's own permissions narrow it item by item,
and follow the source as it changes, with nothing to maintain by hand.

This first layer mirrors Slack. The model is the same for every source; a
kind only differs in what its adapter can report.

## Who it applies to

- **Grantees** (restricted members, members of teams with grants, agent keys)
  are narrowed by source permissions on every surface the grants path already
  guards (typed reads, Records, entity and document pages, Ask, the MCP,
  Founder threads, `/me`).
- **Full readers** are everyone else: owners, admins, members with raw access,
  and plain members who aren't restricted. They read as today.
  Narrowing plain members by default needs the Host-only paths closed first
  (the WFE's `databases:query-read`, document listing and bytes, other
  people's questions, dashboards' tiles, generations, workflows), and is a
  later layer.

## The model

- **Item**: the unit a source shares. Slack: a channel.
- **Audience** of an item: `everyone` (any reader the source doesn't mark as
  a guest: a Slack public channel) or `members` (the people the source lists:
  a private channel's members, by email).
- **Source people**: the source's accounts, by lowercased email, with `guest`
  (Slack `is_restricted` or `is_ultra_restricted`).
- **Identity**: a reader is matched to source people by their verified email
  (`ports.Member.Email`, lowercased and trimmed). An agent key, or a person
  with no email or no match, is no one in the source: it reads `everyone`
  items only. A reader matched to a guest reads only the `members` items that
  list them.
- **Documents** belong to an item by the kind's rule (Slack: the external id
  `<channel>:<day>` before the colon). A document whose item is unknown is
  read by full readers only.
- **Tables**: until rows can be filtered, a table of a kind that reports
  audiences is readable only by a reader who reads every item of the
  connector. A grant of the table doesn't widen that.
- **Freshness**: audiences are read from the source at the end of every
  successful sync, in full, whatever tables the connector syncs, and replace
  the stored ones in one transaction. A connector whose audiences were never
  read, or were last read more than 7 days ago, reads as items unknown:
  its documents and tables go to full readers only, and the Access page
  says why.

So effective visibility of a connector's content for a grantee is:
`granted (connector, table or set) ∧ visible(item)`; partial whenever any item
the load holds isn't visible.

## Where it lives

- `connector.Audienced` (optional kind interface): `Audiences(ctx, src)`
  returns the source people and items; `ItemOfDocument(stream, externalID)`
  maps a document to its item. Slack implements both (users.list,
  conversations.list, conversations.members for the channels the app is in).
- The syncer calls it after a sync publishes and stores the result through
  `DB.ReplaceSourceAudiences`. Failing to read audiences doesn't fail the
  sync; it records the problem, and the 7-day rule decides when that hides
  content.
- MySQL (new migration): `foundry_source_audiences` (per connector: read at,
  problem), `foundry_source_people`, `foundry_source_items`,
  `foundry_source_item_members`.
- `access.Sharing`: what one reader reads of each audienced connector,
  built per call from the store and the reader's email. `access.Grants`
  takes it: `CoversSet`, `CoversTable`, `Part` and `Holds` apply it, so
  Covers/Reaches/Partial, `/me`, Read, Ask and Seen stay one code path.
- `GET /v1/workspaces/{workspaceId}/foundry/access/sources`
  (owners and admins): per audienced connector, when audiences were read, the
  problem, items (private and public counts), and the source people's emails
  and guest flags, which the site matches against the members list.

## The site

- Settings › Access › **Source permissions**: one row per audienced
  connection: "Follows Slack channel membership", when it was last read,
  N of M members matched, who isn't matched, and the problem if any.
- The grants dialog says, on a connection that follows its source's
  permissions, that a grant gives only what the source shares.
- A Slack connection's Details: "Permissions: follows channel membership".
- The mock serves the same: Slack users with emails and a guest, a private
  channel, audiences at each sync, and per-document narrowing for grantees.

## Later layers

1. Drive, GitHub, Linear, Granola, Calendar, Cal.com audiences.
2. Row filtering (needs the engine's visibility contract to filter rows by
   item, and possibly what loads write: approval first).
3. Narrowing every non-full reader, once the Host-only paths are closed.
4. Source-centric policies replacing per-principal grants.
