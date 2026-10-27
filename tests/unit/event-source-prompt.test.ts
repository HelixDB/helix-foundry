import { execFileSync } from "node:child_process";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  eventSourceCurl,
  eventSourcePiecesExample,
  eventSourcePrompt,
  type EventSourcePromptInput,
} from "@/web/areas/data/event-source-prompt";

const input = (
  over: Partial<EventSourcePromptInput> = {},
): EventSourcePromptInput => ({
  origin: "https://foundry.example.com",
  mcpUrl: "https://mcp.example.com/mcp",
  workspace: { id: "ws1", name: "Acme" },
  source: {
    id: "c1",
    name: "Order lines",
    description: "One line of an order.",
  },
  ...over,
});
const source = (over: Partial<EventSourcePromptInput["source"]>) =>
  input({ source: { ...input().source, ...over } });
// The fenced blocks of a language, in order.
const blocks = (text: string, lang: string) =>
  [...text.matchAll(/^```(\w+)\n([\s\S]*?)\n```$/gm)]
    .filter((m) => m[1] === lang)
    .map((m) => m[2]);
// The section titles, in order.
const headings = (text: string) =>
  [...text.matchAll(/^#+ (.+)$/gm)].map((m) => m[1]);

describe("eventSourcePrompt", () => {
  const text = eventSourcePrompt(input());

  it("keeps its example", () => {
    expect(
      eventSourcePrompt({
        origin: "https://foundry.helix-db.com",
        mcpUrl: "https://mcp.helix-db.com/mcp",
        workspace: { id: "ws1", name: "Acme" },
        source: { id: "c1", name: "Order lines" },
      }).split("\n", 1)[0],
    ).toBe("# Send pieces of data to “Order lines” in Helix Foundry");
  });

  it("has its sections in order, and ends with a newline", () => {
    expect(headings(text)).toEqual([
      "Send pieces of data to “Order lines” in Helix Foundry",
      "Connect",
      "An app or agent that sends on its own",
      "An agent I drive",
      "Send",
      "On errors",
      "Which mode fits",
    ]);
    expect(text.endsWith("\n")).toBe(true);
  });

  // Byte for byte: a stamp without the HTTP endpoint (prod, local mode)
  // copies this text. `vitest -u` rewrites it after a deliberate change.
  it("keeps its text", async () => {
    await expect(text).toMatchFileSnapshot(
      "./__fixtures__/event-source-prompt.txt",
    );
  });

  it("says nothing of HTTP without an endpoint, as next.config.ts's empty one", () => {
    expect(eventSourcePrompt(input({ eventsUrl: "" }))).toBe(text);
    expect(eventSourcePrompt(input({ eventsUrl: undefined }))).toBe(text);
    for (const word of ["HTTP", "curl", "pieces.json", "server-side"])
      expect(text).not.toContain(word);
  });

  it("says what the source is: its name, id, workspace and how its fields behave", () => {
    expect(text).toContain(
      "my API source “Order lines” in Helix Foundry, in my workspace “Acme” (`ws1`). Its id is `c1`; the Helix MCP calls it an event source, a connector of kind `events`.",
    );
    expect(text).toContain("Foundry keeps every piece");
    expect(text).toContain(
      "What each piece is, in my words:\n\n> One line of an order.\n",
    );
    expect(text).toContain(
      "Its fields are the top-level keys your pieces carry. Each keeps the JSON type it first had; a send that changes one is refused and names both types.",
    );
  });

  it.each([
    {
      name: "no description",
      description: undefined,
      shown:
        "It has no description yet: ask me what a piece is before you send any.\n",
    },
    {
      name: "an empty one",
      description: "",
      shown:
        "It has no description yet: ask me what a piece is before you send any.\n",
    },
    {
      name: "only space",
      description: " \n\t ",
      shown:
        "It has no description yet: ask me what a piece is before you send any.\n",
    },
    {
      name: "several lines, quoted line by line and trimmed",
      description: "  One order line.\n\n# Not a heading  \n",
      shown: "\n> One order line.\n>\n> # Not a heading\n\nIts fields",
    },
  ])("quotes its description: $name", ({ description, shown }) => {
    const out = eventSourcePrompt(source({ description }));
    expect(out).toContain(shown);
    expect(headings(out)).not.toContain("Not a heading");
    expect(out.includes("What each piece is")).toBe(shown.includes(">"));
  });

  it("connects a headless sender with the token from HELIX_EVENT_TOKEN, as Claude Code's .mcp.json", () => {
    // The config, then the example call.
    expect(blocks(text, "json")).toHaveLength(2);
    expect(JSON.parse(blocks(text, "json")[0])).toEqual({
      mcpServers: {
        "helix-events": {
          type: "http",
          url: "https://mcp.example.com/mcp",
          headers: { Authorization: "Bearer ${HELIX_EVENT_TOKEN}" },
        },
      },
    });
    expect(text).toContain(
      "For Claude Code, add `helix-events` to the project’s `.mcp.json`:",
    );
    expect(text).toContain(
      "Claude Code expands `${HELIX_EVENT_TOKEN}` when it starts the server, so the file holds no secret.",
    );
    expect(text).toContain(
      "Never add the server with `claude mcp add --header`: the shell expands the variable there",
    );
    expect(text).toContain(
      "put it in the sender’s environment as `HELIX_EVENT_TOKEN`",
    );
    expect(text).toContain("Never print, log or commit the token.");
    expect(text).toContain("never ask me for the token in the chat");
  });

  it("connects Codex with bearer_token_env_var", () => {
    expect(blocks(text, "toml")).toEqual([
      '[mcp_servers.helix-events]\nurl = "https://mcp.example.com/mcp"\nbearer_token_env_var = "HELIX_EVENT_TOKEN"',
    ]);
    expect(text).toContain("For Codex, add to `~/.codex/config.toml`:");
  });

  it("connects an agent the user drives through its own sign-in", () => {
    expect(text).toContain(
      "- Claude Code: `claude mcp add --transport http helix https://mcp.example.com/mcp`, then `/mcp` to sign in.",
    );
    expect(text).toContain(
      "- Codex: `codex mcp add helix --url https://mcp.example.com/mcp && codex mcp login helix`.",
    );
    expect(text).toContain(
      "can send as me through its signed-in Helix MCP, if I’m an owner or admin of the workspace.",
    );
  });

  it("links the source's page with the token dialog open, with the workspace", () => {
    const link =
      "https://foundry.example.com/data/connections/c1?workspace=ws1&token=new";
    expect(text.split(link).length - 1).toBe(2);
    expect(
      eventSourcePrompt({
        ...source({ id: "c/1 ?" }),
        workspace: { id: "w&1", name: "Acme" },
      }),
    ).toContain(
      "https://foundry.example.com/data/connections/c%2F1%20%3F?workspace=w%261&token=new",
    );
  });

  it("sends with helix_foundry_send_pieces, naming this source and workspace", () => {
    expect(text).toContain(
      "Send with `helix_foundry_send_pieces`, in calls like this one:",
    );
    const call = JSON.parse(blocks(text, "json")[1]);
    expect(call).toEqual({
      workspace_id: "ws1",
      event_source_id: "c1",
      client_token: "<a new UUID>",
      pieces: [
        {
          id: "<the piece’s id>",
          occurred_at: "<when it happened, RFC 3339; optional>",
          data_json: "<the piece: one JSON object, as text>",
        },
      ],
    });
    // Ids that need escaping stay valid JSON.
    const odd = eventSourcePrompt({
      ...source({ id: 'c"1\\' }),
      workspace: { id: 'w"s', name: "Acme" },
    });
    expect(JSON.parse(blocks(odd, "json")[1])).toMatchObject({
      workspace_id: 'w"s',
      event_source_id: 'c"1\\',
    });
  });

  it.each([
    [
      "the call's size",
      "Up to 500 pieces a call, with at most 1 MiB of data together",
    ],
    ["the escaped size", "keep a call under about 800 KB"],
    [
      "a call is whole or nothing",
      "One bad piece refuses the whole call, and none of it is stored.",
    ],
    [
      "a client token per call",
      "`client_token`: a new random value (a UUID) for each call, 16 to 64 printable ASCII characters.",
    ],
    [
      "replays",
      "Keep it only to retry that call with the same pieces: the retry stores nothing new, and its answer has `replayed: true`.",
    ],
    [
      "ids",
      "`id`: 1 to 128 printable ASCII characters with no spaces, unique within the call.",
    ],
    ["times", "`occurred_at`: when it happened, if you know: an RFC 3339 time"],
    ["a piece's size", "one JSON object as text, at most 256 KiB"],
    ["big integers", "send integers above 2^53 (9007199254740992) as strings"],
    ["depth", "Its JSON nests at most 64 deep"],
    ["finite numbers", "every number in it is finite (not `1e400`)"],
    [
      "surrogates",
      "every `\\uD800`-`\\uDBFF` escape is followed by a `\\uDC00`-`\\uDFFF` one",
    ],
    [
      "keys",
      "1 to 64 bytes each, with no control characters and no backticks, and none twice",
    ],
    ["the field cap", "at most 200 in the source"],
    [
      "reserved keys",
      "`id`, `received_at` and `occurred_at` are not data keys",
    ],
    [
      "the type lock",
      "A field keeps the JSON type it first had: string, number (integers and decimals alike), boolean, object or array. `null` is always allowed.",
    ],
    [
      "replace by id",
      "A piece with an id already in the source replaces it, whoever sent it. If several apps or agents send, give each its own source.",
    ],
    [
      "capacity",
      "Watch `capacity` and `warning` in each answer, and tell me at 80%.",
    ],
    ["when pieces arrive", "Pieces reach the source’s table at its next sync"],
  ])("gives the send rule: %s", (_, rule) => {
    expect(text).toContain(rule);
  });

  it.each([
    [
      "invalid_arguments",
      "fix the piece it names. If it says the `client_token` was already used, send with a new one.",
    ],
    [
      "resource_exhausted",
      "wait as it says, then retry with the same `client_token`.",
    ],
    [
      "failed_precondition",
      "the source is full or paused, or has all the fields it may have. Stop and tell me what it says.",
    ],
    ["backend_unavailable", "retry with the same `client_token`."],
    [
      "not_found",
      "the workspace or source id is wrong, or isn’t the send token’s own. Stop and tell me.",
    ],
  ])("says what to do on %s", (code, what) => {
    expect(text).toContain(`- \`${code}\`: ${what}`);
  });

  it.each([
    [
      "the choice",
      "## Which mode fits\n\nAn ontology reads the source in one of two ways, and the pieces’ shape decides which fits:\n",
    ],
    [
      "structured JSON suits mappings",
      "- Structured mappings suit strictly structured JSON: ids, amounts, statuses, times. Its fields map to types and properties, and every piece is kept.",
    ],
    [
      "text suits extraction",
      "- Entity extraction suits a piece whose body is text, such as an agent’s reply or a note: Foundry’s agent reads the text and drafts the entities and facts it describes.",
    ],
    [
      "the text field stays a string",
      "The text goes in one top-level field, the source’s text field (`text_field`), which I choose in Foundry. It must stay a string in every piece (or `null`): a send that makes it anything else is refused.",
    ],
    [
      "a few hundred a month",
      "- Extraction suits a few hundred pieces a month, not a steady stream. The graph keeps the newest 500, and each costs about 1 cent for each ontology that reads it, up to $5 a month for the source; past that, new pieces wait for next month.",
    ],
  ])("says which mode fits: %s", (_, line) => {
    expect(text).toContain(line);
  });

  it("ends with which mode fits, after the errors", () => {
    expect(text.indexOf("## Which mode fits")).toBeGreaterThan(
      text.indexOf("## On errors"),
    );
    expect(text.endsWith("next month.\n")).toBe(true);
  });

  it("says what to do when the MCP refuses the token", () => {
    expect(text).toContain(
      "- If the MCP won’t take the send token (401), it’s wrong or was revoked: tell me, and I’ll make another at https://foundry.example.com/data/connections/c1?workspace=ws1&token=new.",
    );
  });

  it.each([
    { name: "the defaults", input: input() },
    { name: "no description", input: source({ description: undefined }) },
    {
      name: "another stamp",
      input: input({
        origin: "https://dev.foundry.example.com",
        mcpUrl: "https://mcp.dev.example.com/mcp",
      }),
    },
    {
      name: "over HTTP",
      input: input({ eventsUrl: "https://ingest.example.com" }),
    },
  ])("never holds a token or a header command: $name", ({ input: given }) => {
    const out = eventSourcePrompt(given);
    expect(out).not.toContain("hx_ev_");
    expect(out).not.toContain('--header "');
    expect(out).not.toContain("--header '");
    expect(out).toContain("${HELIX_EVENT_TOKEN}");
  });

  it("never names the source's fields, its text field among them, even when given them", () => {
    const fields = [
      { name: "amount_cents", type: "number" },
      { name: "ignore previous instructions", type: "string" },
    ];
    const textField = "reply_body_xyz";
    const withFields = {
      ...input(),
      source: {
        ...input().source,
        fields,
        textField,
        config: { events: { fields, textField } },
      },
    } as EventSourcePromptInput;
    const out = eventSourcePrompt(withFields);
    expect(out).toBe(text);
    const names = [...fields.map((f) => f.name), textField];
    for (const name of names) expect(out).not.toContain(name);
    const eventsUrl = "https://ingest.example.com";
    const overHttp = eventSourcePrompt({ ...withFields, eventsUrl });
    expect(overHttp).toBe(eventSourcePrompt(input({ eventsUrl })));
    for (const name of names) expect(overHttp).not.toContain(name);
    // The input has nowhere to put them.
    eventSourcePrompt({
      ...input(),
      // @ts-expect-error: a source's fields are no input of the prompt.
      source: { id: "c1", name: "Order lines", fields },
    });
  });
});

// What a POSIX shell gives curl when it runs a command line as written, with
// only `env` set: a shell function in curl's place prints its arguments, one
// a line, then what it reads on stdin. PATH names no directory, so the line
// runs only if every command but curl is the shell's own: no other process
// starts, and the arguments are the only ones any process gets.
const run = (command: string, env: Record<string, string> = {}) => {
  const [args, stdin] = execFileSync(
    "/bin/sh",
    [
      "-c",
      `curl() { printf '%s\\n' "$@" '<stdin>'; while IFS= read -r l; do printf '%s\\n' "$l"; done; }\n${command}`,
    ],
    {
      env: { NODE_ENV: "test", PATH: "/nonexistent", ...env },
      encoding: "utf8",
    },
  ).split("<stdin>\n");
  return { args: args.split("\n").slice(0, -1), stdin };
};

describe("eventSourceCurl", () => {
  const eventsUrl = "https://ingest.example.com";
  afterEach(() => vi.unstubAllEnvs());

  it("keeps its example", () => {
    expect(
      eventSourceCurl({
        eventsUrl: "https://ingest.helix-db.com",
        sourceId: "c1",
      }),
    ).toBe(
      `printf 'Authorization: Bearer %s\\n' "$HELIX_EVENT_TOKEN" | curl -sS -X POST "https://ingest.helix-db.com/v1/sources/c1/pieces" -H @- -H "Content-Type: application/json" --data-binary @pieces.json`,
    );
  });

  it.each([
    { name: "a plain token", token: "hx_ev_abc123" },
    { name: "one a shell would split or expand", token: 'hx_ev_a b"$c`d`%s\\' },
    { name: "none set", token: undefined },
  ])(
    "runs as written, handing curl the token from HELIX_EVENT_TOKEN on stdin only: $name",
    ({ token }) => {
      const curl = eventSourceCurl({ eventsUrl, sourceId: "c1" });
      expect(curl).not.toContain("<");
      const { args, stdin } = run(
        curl,
        token === undefined ? {} : { HELIX_EVENT_TOKEN: token },
      );
      expect(args).toEqual([
        "-sS",
        "-X",
        "POST",
        "https://ingest.example.com/v1/sources/c1/pieces",
        "-H",
        "@-",
        "-H",
        "Content-Type: application/json",
        "--data-binary",
        "@pieces.json",
      ]);
      // One header line; without the variable, an empty bearer: the door's 401.
      expect(stdin).toBe(`Authorization: Bearer ${token ?? ""}\n`);
    },
  );

  it.each([
    { name: "a plain id", sourceId: "c1", segment: "c1" },
    {
      name: "a UUID",
      sourceId: "0b5e6c2a-7d1f-4f7e-9a3c-2b1d8e6f4a10",
      segment: "0b5e6c2a-7d1f-4f7e-9a3c-2b1d8e6f4a10",
    },
    {
      name: "a slash, space and query",
      sourceId: "c/1 ?",
      segment: "c%2F1%20%3F",
    },
    {
      name: "what a shell would expand",
      sourceId: 'c$HOME"`id`\\',
      segment: "c%24HOME%22%60id%60%5C",
    },
    { name: "non-ASCII", sourceId: "ç", segment: "%C3%A7" },
  ])(
    "escapes the source id as one path segment: $name",
    ({ sourceId, segment }) => {
      const url = run(eventSourceCurl({ eventsUrl, sourceId }), {
        HOME: "/root",
      }).args[3];
      expect(url).toBe(
        `https://ingest.example.com/v1/sources/${segment}/pieces`,
      );
      expect(new URL(url).pathname.split("/")).toEqual([
        "",
        "v1",
        "sources",
        segment,
        "pieces",
      ]);
    },
  );

  it("is pure: it never inlines the token, even when the environment has it", () => {
    vi.stubEnv("HELIX_EVENT_TOKEN", "hx_ev_secret");
    const curl = eventSourceCurl({ eventsUrl, sourceId: "c1" });
    expect(curl).toBe(eventSourceCurl({ eventsUrl, sourceId: "c1" }));
    expect(curl).not.toContain("hx_ev_");
    expect(curl).not.toContain("--header");
    expect(eventSourcePrompt(input({ eventsUrl }))).not.toContain("hx_ev_");
  });
});

describe("eventSourcePiecesExample", () => {
  it("keeps its example", () => {
    expect(JSON.parse(eventSourcePiecesExample()).pieces[0].data).toEqual({
      "<field>": "<value>",
    });
  });

  it("is a body to fill in: a client token and pieces whose data is an object", () => {
    const body = JSON.parse(eventSourcePiecesExample());
    expect(body).toEqual({
      client_token: "<a new UUID for each send; the same one to retry it>",
      pieces: [
        {
          id: "<the piece’s id>",
          occurred_at: "2026-10-23T09:30:00Z",
          data: { "<field>": "<value>" },
        },
      ],
    });
    expect(eventSourcePiecesExample()).toBe(eventSourcePiecesExample());
    expect(eventSourcePiecesExample()).toBe(JSON.stringify(body, null, 2));
  });
});

describe("eventSourcePrompt over HTTP", () => {
  const eventsUrl = "https://ingest.example.com",
    text = eventSourcePrompt(input({ eventsUrl }));

  it("adds its section after Connect, and leaves the rest as it was", () => {
    expect(headings(text)).toEqual([
      "Send pieces of data to “Order lines” in Helix Foundry",
      "Connect",
      "An app or agent that sends on its own",
      "An agent I drive",
      "Or send over HTTP",
      "Send",
      "On errors",
      "Which mode fits",
    ]);
    const [before, rest] = text.split("## Or send over HTTP\n\n");
    const after = rest.slice(rest.indexOf("## Send\n"));
    expect(before + after).toBe(eventSourcePrompt(input()));
    expect(text.endsWith("\n")).toBe(true);
  });

  it("shows the curl for this source, then the pieces.json to fill in", () => {
    expect(blocks(text, "sh")).toEqual([
      eventSourceCurl({ eventsUrl, sourceId: "c1" }),
    ]);
    expect(text).toContain(
      `printf 'Authorization: Bearer %s\\n' "$HELIX_EVENT_TOKEN" | curl -sS -X POST "https://ingest.example.com/v1/sources/c1/pieces" -H @-`,
    );
    expect(text).toContain("--data-binary @pieces.json");
    // The MCP config, pieces.json, then the MCP call.
    const json = blocks(text, "json");
    expect(json).toHaveLength(3);
    expect(json[1]).toBe(eventSourcePiecesExample());
    const body = JSON.parse(json[1]);
    expect(typeof body.client_token).toBe("string");
    expect(body.pieces[0].data).toBeTypeOf("object");
    expect(body.pieces[0]).not.toHaveProperty("data_json");
    expect(JSON.parse(json[2])).toHaveProperty("event_source_id", "c1");
    expect(text).toContain(
      "`pieces.json` is a file to fill in with real pieces, never one to send as it is",
    );
    // An odd id reaches the curl escaped.
    expect(
      blocks(eventSourcePrompt({ ...source({ id: "c/1" }), eventsUrl }), "sh"),
    ).toEqual([eventSourceCurl({ eventsUrl, sourceId: "c/1" })]);
  });

  it.each([
    [
      "who it is for",
      "An app or service without an MCP client can POST pieces",
    ],
    ["server-side only", "server-side only: never from a browser"],
    [
      "the same token",
      "the same send token, from `HELIX_EVENT_TOKEN`, as its bearer",
    ],
    [
      "the token stays unwritten and out of curl's arguments",
      "The shell reads `$HELIX_EVENT_TOKEN` as it runs, and its own `printf` hands curl the header on stdin (`-H @-`), so the token is never written down, nor in curl’s arguments, where other users of the machine could see it. Keep it on stdin if you change the command.",
    ],
    [
      "the same rules",
      "The rules under Send hold, with `data` for `data_json`: the piece itself as a JSON object, not as text.",
    ],
    [
      "client_token required",
      "`client_token` is required: a send without one is refused.",
    ],
    [
      "the size",
      "At most about 1 MiB of data a call, counted without the whitespace between its JSON tokens, in a body of at most 2 MiB.",
    ],
    ["the answer", "A 200 answers what `helix_foundry_send_pieces` would"],
    [
      "the error shape",
      'Foundry’s other answers are `{"error": {"code": …, "message": …}}`, with `piece` or `reason` beside `message` where said below.',
    ],
    [
      "how to see a status",
      "What to do on each status (`curl -i` shows it, with the headers):",
    ],
  ])("says %s", (_, line) => {
    expect(text).toContain(line);
  });

  it.each([
    [
      "400",
      "fix what its message says, or send with a new `client_token` if it says that one was already used. Its `piece`, when there is one, is the refused piece’s place in `pieces`, 1 being the first.",
    ],
    [
      "401",
      "the send token is missing, wrong or was revoked. Tell me, and give me the link to make another: https://foundry.example.com/data/connections/c1?workspace=ws1&token=new.",
    ],
    ["404", "the URL’s source isn’t the send token’s own. Stop and tell me."],
    [
      "408",
      "the body took over 15 s to arrive. Send it again with the same `client_token`, or send fewer pieces a call if it keeps happening.",
    ],
    [
      "409",
      "the source is full or paused, or has all the fields it may have, as its `reason` says. Stop and tell me.",
    ],
    ["413", "the body is over 2 MiB. Send fewer pieces a call."],
    [
      "415",
      "send JSON in UTF-8, uncompressed, with `Content-Type: application/json`.",
    ],
    [
      "429",
      "wait as many seconds as its `Retry-After` header says, then retry with the same `client_token`.",
    ],
    [
      "500, 502, 503 or 504",
      "wait a second, or as its `Retry-After` says, then retry with the same `client_token`.",
    ],
  ])("says what to do on %s", (status, what) => {
    expect(text).toContain(`\n- ${status}: ${what}\n`);
  });

  // Every status the foundry-events door answers (its handler's checks,
  // bodyFailure and backendFailure) and the ALB in front of it, once each.
  it("lists every status the door answers, in order", () => {
    const list = text.slice(text.indexOf("What to do on each status"));
    const statuses = [
      ...list.slice(0, list.indexOf("\n\n## ")).matchAll(/^- ([\d, or]+):/gm),
    ].flatMap((m) => m[1].split(/, | or /));
    expect(statuses).toEqual([
      "400",
      "401",
      "404",
      "408",
      "409",
      "413",
      "415",
      "429",
      "500",
      "502",
      "503",
      "504",
    ]);
  });

  it("links the token page once more, for a 401", () => {
    const link =
      "https://foundry.example.com/data/connections/c1?workspace=ws1&token=new";
    expect(text.split(link).length - 1).toBe(3);
  });
});
