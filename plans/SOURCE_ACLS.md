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
- **Identity**: a person is matched to source people by their verified email
  (`ports.Member.Email`, lowercased and trimmed; the host must send only an
  email it has verified). A person the source has no account for by that
  email (none, an address it doesn't know, an offboarded account), or one
  matched to a guest, reads only the `members` items that list them: in
  practice an unmatched person reads none. An agent key, or a team, is no one
  in the source: it reads `everyone` items only. Where nothing tells emails
  (`ports.People` missing), a person's items are unknown.
- **Documents** belong to an item by the kind's rule (Slack: the external id
  `<channel>:<day>` before the colon). A document whose item is unknown is
  read by full readers only.
- **Never whole**: a load may hold documents of items the last read didn't
  list (the app left a channel, a channel was deleted or turned private, or
  was joined after the last read), so no grantee covers a set of an
  audienced connector whole: a load holding one is always read in part,
  document by document.
- **Tables** are read by their rows (contract 4.32.0 `visibility.rows`):
  the kind says which row-key field holds a table's item, exactly or as a
  prefix (`Audienced.TableRows`). Slack: channels and channel_members by
  `channel_id`, messages and channel_days by `<channel>:`; users is the
  directory, whole for a matched non-guest person only. A table with no
  rule, a mapping whose `row_key` lacks the field, unknown audiences, or
  over 10,000 items: full readers only. Nothing loads write changes.
- **Freshness**: audiences are read from the source at the end of a
  successful sync, in full, whatever tables the connector syncs, at most once
  an hour (`syncer.AudienceEvery`, unless a read since failed), after the
  sync owes its refreshes, and replace the stored ones in one transaction; a
  read whose digest matches the stored one only moves `read_at`. A connector
  whose audiences were never read, were last read more than 7 days ago, or
  more than a day ago with a failed read since, reads as items unknown: its
  documents go to full readers only, and the Access page says why.
- **What was read stays with it**: a grantee's answer records the items its
  documents were of, and a restricted member's Founder thread the items of
  what its calls read; either is theirs only while every one is still shared
  with them. A thread's call records, once it returns: for a graph read, the
  items of the documents and table files of its load that the reader reads
  (`access.Grants.Part`, the part the API narrows the read to: a result names
  only its quotes' documents, not those its rows, entities and counts came
  from), as the load held them before the call and after (a sync may trim a
  document out of it meanwhile); for an ask or get_question, its question's
  items; for a list_questions, every listed question's. Its other tools
  read no document's content and record nothing, so a thread that read
  nothing of a channel is never hidden by losing it. Items are read on both
  sides of each call (a helper's too): one the call drew on and the reader
  lost during it fails the turn for good, before the result is delivered
  (a helper's turn ends there, as the worker's does).

So effective visibility of a connector's content for a grantee is:
`granted (connector or set) ∧ visible(item)`, always partial.

## Where it lives

- `connector.Audienced` (optional kind interface): `Audiences(ctx, src)`
  returns the source people and items; `ItemOfDocument(stream, externalID)`
  maps a document to its item. Slack implements both (users.list,
  conversations.list, conversations.members for the channels the app is in),
  through a token or through Helix's Slack app's install alike.
- The syncer calls it after a sync publishes and stores the result through
  `DB.ReplaceSourceAudiences`. Failing to read audiences doesn't fail the
  sync; it records the problem, and the 7-day rule decides when that hides
  content.
- MySQL (new migration): `foundry_source_audiences` (per connector: read at,
  digest, problem), `foundry_source_people`, `foundry_source_items`,
  `foundry_source_item_members`, and `foundry_question_source_items`,
  `foundry_agent_thread_source_items` (the items an answer or a thread read).
- `access.Sharing`: what one reader reads of each audienced connector,
  built per call from the store and the reader's email. `access.Grants`
  takes it: `CoversSet`, `CoversTable`, `Part` and `Holds` apply it, so
  Covers/Reaches/Partial, `/me`, Read, Ask and Seen stay one code path.
- `GET /v1/workspaces/{workspaceId}/foundry/access/sources`
  (owners and admins): per audienced connector, when audiences were read, the
  problem, items (private and public counts), and, matched in the API, how
  many people it narrows and which of them it has no account for or knows
  as guests, by user id. The source's directory never leaves the API.
- `GetMyFoundryAccess` tells a grantee the items shared with it
  (`source_items`), which Founder's worker reads around each call; it
  records on the thread only those of what the call read (`agent.drew`).

## The site

- Settings › Access › **Source permissions**: one row per audienced
  connection: "Follows Slack channel membership", when it was last read,
  N of M restricted people matched, who isn't matched (five names, then a
  count), and the problem if any.
- The grants dialog says, on a connection that follows its source's
  permissions, that a grant gives only what the source shares.
- A Slack connection's Details: "Permissions: follows channel membership".
- The mock serves the same: Slack users with emails and a guest, a private
  channel, audiences at each sync, and per-document narrowing for grantees.

## Later layers

1. Drive, GitHub, Linear, Granola, Calendar, Cal.com audiences.
2. Row filtering for other kinds: GitHub issues and pull requests (prefix
   `<repoID>#`) and Google (exact) by their keys as they are; GitHub
   comments and reviews and Linear need composite keys, a new version and a
   reload.
3. Narrowing every non-full reader, once the Host-only paths are closed.
4. Source-centric policies replacing per-principal grants.
