import { paths } from "../../paths";

// The prompt an API source's send panel copies for whoever sends to it: the
// user's own agent, signed in to the Helix MCP as the user, or an app or
// agent that sends on its own with a send token as its bearer. It says what
// the source is, how to reach the MCP either way, how to send pieces with
// helix_foundry_send_pieces (or over HTTP, where the stamp takes them), what
// to do on each error, and which way of reading the source (structured
// mappings or entity extraction) fits which pieces. It names
// the Helix MCP's send tool and its arguments, and the piece rules and
// foundry.events defaults of the backend's events package
// (mock/event-pieces.ts ports them), so keep it in step with them. The MCP
// calls an API source an event source.
//
// It never holds a send token: it names HELIX_EVENT_TOKEN, which the user
// sets where the sender runs. Nor does it hold the source's fields, its text
// field among them: their names come from whoever sends, and this prompt may
// be pasted into an agent that holds the user's full tools, so an agent
// meets them only as data, in the tools' answers.

export type EventSourcePromptInput = {
  // Foundry's origin: "https://foundry.helix-db.com".
  origin: string;
  // The Helix MCP's URL, for the sender's MCP config.
  mcpUrl: string;
  // Where the stamp takes pieces over HTTP, an origin with no trailing slash
  // (NEXT_PUBLIC_HELIX_EVENTS_URL). With it the prompt gains "Or send over
  // HTTP"; unset or "" (prod until the endpoint ships, local mode), it is
  // the MCP-only text, byte for byte.
  eventsUrl?: string;
  workspace: { id: string; name: string };
  // As its owners and admins wrote them; no fields (see above).
  source: { id: string; name: string; description?: string };
};

/**
 * The prompt, in Markdown, in the user's voice: what the source is (its
 * name, id and description), how to connect to the Helix MCP (a send token
 * as the bearer, read from `HELIX_EVENT_TOKEN`, or the user's own sign-in),
 * how to send pieces, what to do on each error, and which mode fits which
 * pieces (foundry.events' newest 500 and $5 a month). Its only link is the
 * source's page with the send token dialog open, where the user makes a
 * token.
 *
 * ```ts
 * eventSourcePrompt({
 *   origin: "https://foundry.helix-db.com",
 *   mcpUrl: "https://mcp.helix-db.com/mcp",
 *   workspace: { id: "ws1", name: "Acme" },
 *   source: { id: "c1", name: "Order lines" },
 * }).split("\n", 1)[0];
 * // "# Send pieces of data to “Order lines” in Helix Foundry"
 * ```
 */
export function eventSourcePrompt(input: EventSourcePromptInput): string {
  const ws = input.workspace.id,
    { id, name } = input.source,
    description = input.source.description?.trim() || "",
    tokenPage = `${input.origin}${paths.connection(id)}?${new URLSearchParams({ workspace: ws, token: "new" })}`;
  // Claude Code expands ${HELIX_EVENT_TOKEN} itself, when it starts the server.
  const mcpJson = {
    mcpServers: {
      "helix-events": {
        type: "http",
        url: input.mcpUrl,
        headers: { Authorization: "Bearer ${HELIX_EVENT_TOKEN}" },
      },
    },
  };
  const call = {
    workspace_id: ws,
    event_source_id: id,
    client_token: "<a new UUID>",
    pieces: [
      {
        id: "<the piece’s id>",
        occurred_at: "<when it happened, RFC 3339; optional>",
        data_json: "<the piece: one JSON object, as text>",
      },
    ],
  };
  // POST /v1/sources/{id}/pieces on Foundry's foundry-events role:
  // the statuses (its handler's checks, bodyFailure and backendFailure),
  // `piece`, `reason` and caps (2 MiB of body, read within 15 s, the send's
  // 1 MiB of data compacted) are its contract, so keep them in step with it.
  const http = input.eventsUrl
    ? [
        "## Or send over HTTP",
        "",
        "An app or service without an MCP client can POST pieces to this source instead, server-side only: never from a browser or an app on someone else’s device, which would hand them the token. It sends the same send token, from `HELIX_EVENT_TOKEN`, as its bearer. This curl sends `pieces.json`:",
        "",
        "```sh",
        eventSourceCurl({ eventsUrl: input.eventsUrl, sourceId: id }),
        "```",
        "",
        "The shell reads `$HELIX_EVENT_TOKEN` as it runs, and its own `printf` hands curl the header on stdin (`-H @-`), so the token is never written down, nor in curl’s arguments, where other users of the machine could see it. Keep it on stdin if you change the command. `pieces.json` is a file to fill in with real pieces, never one to send as it is: a field keeps the type of its first send, so a made-up one would stay in the source for good.",
        "",
        "```json",
        eventSourcePiecesExample(),
        "```",
        "",
        "- The rules under Send hold, with `data` for `data_json`: the piece itself as a JSON object, not as text. Its numbers are kept exactly as written.",
        "- `client_token` is required: a send without one is refused.",
        "- At most about 1 MiB of data a call, counted without the whitespace between its JSON tokens, in a body of at most 2 MiB.",
        '- A 200 answers what `helix_foundry_send_pieces` would, so watch `capacity`, `warning` and `next` the same way. Foundry’s other answers are `{"error": {"code": …, "message": …}}`, with `piece` or `reason` beside `message` where said below.',
        "",
        "What to do on each status (`curl -i` shows it, with the headers):",
        "",
        "- 400: fix what its message says, or send with a new `client_token` if it says that one was already used. Its `piece`, when there is one, is the refused piece’s place in `pieces`, 1 being the first.",
        `- 401: the send token is missing, wrong or was revoked. Tell me, and give me the link to make another: ${tokenPage}.`,
        "- 404: the URL’s source isn’t the send token’s own. Stop and tell me.",
        "- 408: the body took over 15 s to arrive. Send it again with the same `client_token`, or send fewer pieces a call if it keeps happening.",
        "- 409: the source is full or paused, or has all the fields it may have, as its `reason` says. Stop and tell me.",
        "- 413: the body is over 2 MiB. Send fewer pieces a call.",
        "- 415: send JSON in UTF-8, uncompressed, with `Content-Type: application/json`.",
        "- 429: wait as many seconds as its `Retry-After` header says, then retry with the same `client_token`.",
        "- 500, 502, 503 or 504: wait a second, or as its `Retry-After` says, then retry with the same `client_token`.",
        "",
      ]
    : [];
  return [
    `# Send pieces of data to “${name}” in Helix Foundry`,
    "",
    `I’d like you to send pieces of data to my API source “${name}” in Helix Foundry, in my workspace “${input.workspace.name}” (\`${ws}\`). Its id is \`${id}\`; the Helix MCP calls it an event source, a connector of kind \`events\`.`,
    "",
    "An API source holds pieces of data: JSON objects, each with an id. Foundry keeps every piece, and merges them by id into the source’s table, `events`, which ontologies read.",
    "",
    ...(description
      ? [
          "What each piece is, in my words:",
          "",
          ...description.split("\n").map((l) => `> ${l}`.trimEnd()),
        ]
      : [
          "It has no description yet: ask me what a piece is before you send any.",
        ]),
    "",
    "Its fields are the top-level keys your pieces carry. Each keeps the JSON type it first had; a send that changes one is refused and names both types.",
    "",
    "## Connect",
    "",
    `You send through the Helix MCP, at ${input.mcpUrl}, in one of two ways.`,
    "",
    "### An app or agent that sends on its own",
    "",
    `It uses a send token of this source as its bearer, and sees only the send tool. I make the token on the source’s page in Foundry, ${tokenPage}, and put it in the sender’s environment as \`HELIX_EVENT_TOKEN\`. If \`HELIX_EVENT_TOKEN\` isn’t set, give me that link: never ask me for the token in the chat. Never print, log or commit the token.`,
    "",
    "For Claude Code, add `helix-events` to the project’s `.mcp.json`:",
    "",
    "```json",
    JSON.stringify(mcpJson, null, 2),
    "```",
    "",
    "Claude Code expands `${HELIX_EVENT_TOKEN}` when it starts the server, so the file holds no secret. Never add the server with `claude mcp add --header`: the shell expands the variable there, and writes the token itself into Claude Code’s config.",
    "",
    "For Codex, add to `~/.codex/config.toml`:",
    "",
    "```toml",
    "[mcp_servers.helix-events]",
    `url = ${JSON.stringify(input.mcpUrl)}`,
    'bearer_token_env_var = "HELIX_EVENT_TOKEN"',
    "```",
    "",
    "### An agent I drive",
    "",
    "An agent I work with, as you do now, can send as me through its signed-in Helix MCP, if I’m an owner or admin of the workspace. If you don’t have the MCP’s tools (`helix_foundry_send_pieces` and the rest), add it, and have me sign in:",
    "",
    `- Claude Code: \`claude mcp add --transport http helix ${input.mcpUrl}\`, then \`/mcp\` to sign in.`,
    `- Codex: \`codex mcp add helix --url ${input.mcpUrl} && codex mcp login helix\`.`,
    "",
    ...http,
    "## Send",
    "",
    "Send with `helix_foundry_send_pieces`, in calls like this one:",
    "",
    "```json",
    JSON.stringify(call, null, 2),
    "```",
    "",
    "- Up to 500 pieces a call, with at most 1 MiB of data together: keep a call under about 800 KB, because `data_json` is escaped inside it. One bad piece refuses the whole call, and none of it is stored.",
    "- `client_token`: a new random value (a UUID) for each call, 16 to 64 printable ASCII characters. Keep it only to retry that call with the same pieces: the retry stores nothing new, and its answer has `replayed: true`. With other pieces, it’s refused.",
    "- `id`: 1 to 128 printable ASCII characters with no spaces, unique within the call. A piece with an id already in the source replaces it, whoever sent it. If several apps or agents send, give each its own source.",
    "- `occurred_at`: when it happened, if you know: an RFC 3339 time (`2026-10-23T09:30:00Z`) in the years 1000 to 9999.",
    "- `data_json`: the piece itself, one JSON object as text, at most 256 KiB. Write it from the original text; if your client parses JSON into doubles first, send integers above 2^53 (9007199254740992) as strings.",
    "- Its JSON nests at most 64 deep, every number in it is finite (not `1e400`), and every `\\uD800`-`\\uDBFF` escape is followed by a `\\uDC00`-`\\uDFFF` one.",
    "- Its top-level keys are the source’s fields, at most 200 in the source: 1 to 64 bytes each, with no control characters and no backticks, and none twice. `id`, `received_at` and `occurred_at` are not data keys: the piece’s id and time go beside `data_json`.",
    "- A field keeps the JSON type it first had: string, number (integers and decimals alike), boolean, object or array. `null` is always allowed.",
    "- Pieces reach the source’s table at its next sync, about a minute after they’re sent: each answer’s `next` says so.",
    "- Watch `capacity` and `warning` in each answer, and tell me at 80%. Foundry keeps every piece, so a full source refuses sends.",
    "",
    "## On errors",
    "",
    "- `invalid_arguments`: fix the piece it names. If it says the `client_token` was already used, send with a new one.",
    "- `resource_exhausted`: wait as it says, then retry with the same `client_token`.",
    "- `failed_precondition`: the source is full or paused, or has all the fields it may have. Stop and tell me what it says.",
    "- `backend_unavailable`: retry with the same `client_token`.",
    "- `not_found`: the workspace or source id is wrong, or isn’t the send token’s own. Stop and tell me.",
    `- If the MCP won’t take the send token (401), it’s wrong or was revoked: tell me, and I’ll make another at ${tokenPage}.`,
    "",
    "## Which mode fits",
    "",
    "An ontology reads the source in one of two ways, and the pieces’ shape decides which fits:",
    "",
    "- Structured mappings suit strictly structured JSON: ids, amounts, statuses, times. Its fields map to types and properties, and every piece is kept.",
    "- Entity extraction suits a piece whose body is text, such as an agent’s reply or a note: Foundry’s agent reads the text and drafts the entities and facts it describes. The text goes in one top-level field, the source’s text field (`text_field`), which I choose in Foundry. It must stay a string in every piece (or `null`): a send that makes it anything else is refused.",
    "- Extraction suits a few hundred pieces a month, not a steady stream. The graph keeps the newest 500, and each costs about 1 cent for each ontology that reads it, up to $5 a month for the source; past that, new pieces wait for next month.",
    "",
  ].join("\n");
}

/**
 * The curl that POSTs `pieces.json` to an API source over HTTP, on one line.
 * It runs as written in any POSIX shell: the shell reads the send token from
 * `HELIX_EVENT_TOKEN` as it runs, and printf, a builtin of bash, dash, zsh
 * and BusyBox sh, hands curl the Authorization header on stdin (`-H @-`,
 * curl 7.55 and later). So the token is never written down, nor in any
 * process's arguments, which other users of the machine can read (ps,
 * /proc/<pid>/cmdline): curl doesn't blank its `-H` arguments. The source id
 * is escaped as one path segment. The same text is the prompt's and the send
 * panel's.
 *
 * ```ts
 * eventSourceCurl({ eventsUrl: "https://ingest.helix-db.com", sourceId: "c1" });
 * // `printf 'Authorization: Bearer %s\\n' "$HELIX_EVENT_TOKEN" | curl -sS -X POST "https://ingest.helix-db.com/v1/sources/c1/pieces" -H @- -H "Content-Type: application/json" --data-binary @pieces.json`
 * ```
 */
export function eventSourceCurl({
  eventsUrl,
  sourceId,
}: {
  eventsUrl: string;
  sourceId: string;
}): string {
  return `printf 'Authorization: Bearer %s\\n' "$HELIX_EVENT_TOKEN" | curl -sS -X POST "${eventsUrl}/v1/sources/${encodeURIComponent(sourceId)}/pieces" -H @- -H "Content-Type: application/json" --data-binary @pieces.json`;
}

/**
 * The shape of `pieces.json`, the body eventSourceCurl() sends, as indented
 * JSON: a file to fill in with real pieces, never one to send as it is. A
 * source keeps each field's type from its first send and counts it toward
 * its field cap, so a made-up field would stay in it for good.
 *
 * ```ts
 * JSON.parse(eventSourcePiecesExample()).pieces[0].data;
 * // { "<field>": "<value>" }
 * ```
 */
export function eventSourcePiecesExample(): string {
  return JSON.stringify(
    {
      client_token: "<a new UUID for each send; the same one to retry it>",
      pieces: [
        {
          id: "<the piece’s id>",
          occurred_at: "2026-10-23T09:30:00Z",
          data: { "<field>": "<value>" },
        },
      ],
    },
    null,
    2,
  );
}
