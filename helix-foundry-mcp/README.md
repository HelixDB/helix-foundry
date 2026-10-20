# Helix Foundry MCP server (read-only)

Lets Claude Desktop ask questions of your local Helix Foundry and browse its datasets and ontology.

## Setup

1. **Get a token.** Open http://localhost:3001 → Settings → Developer tools and create a **read-only** token. Copy it.
2. **Install.** Put this folder somewhere permanent (e.g. `~/helix-foundry-mcp`), then run:
   ```bash
   cd ~/helix-foundry-mcp
   npm install
   ```
   You need Node.js 18 or later (`node -v`).
3. **Add it to Claude Desktop.** Settings → Developer → Edit Config. This opens `claude_desktop_config.json`. Add:
   ```json
   {
     "mcpServers": {
       "helix-foundry": {
         "command": "node",
         "args": ["/ABSOLUTE/PATH/TO/helix-foundry-mcp/server.mjs"],
         "env": {
           "FOUNDRY_TOKEN": "paste-your-token",
           "FOUNDRY_URL": "http://localhost:3001"
         }
       }
     }
   }
   ```
   If the file already has an `mcpServers` block, add the `helix-foundry` entry inside it.
   On Windows, write the path as `C:\\Users\\you\\helix-foundry-mcp\\server.mjs`.
   If Claude can't find `node`, use the full path from `which node` (or `where node` on Windows) as the `command`.
4. **Restart Claude Desktop** completely (quit it, then reopen). `helix-foundry` should show up in the tools / connectors menu.

If you changed Foundry's port (`PUBLIC_PORT`), change `FOUNDRY_URL` to match. The workspace is detected from the token. Set `FOUNDRY_WORKSPACE` only if you need to override it.

## Tools

| Tool | What it does |
| --- | --- |
| `ask_question` | Ask the Foundry Analyst a question. You get back a summary, the SQL it ran, rows and citations |
| `get_run` | Check on an Analyst run that took longer than the timeout |
| `workspace_home` | Workspace overview: sources, ontology, findings, dashboards |
| `list_resources` / `get_resource` | Browse datasets, object types, objects, relations, dashboards and more |
| `get_record` / `get_neighbors` | Look up an ontology record and the objects linked to it |
| `preview_dataset` | Sample rows and the column profile of a dataset |
| `run_sql` | Read-only DuckDB SQL over the datasets you pass in |

Nothing here writes, publishes or deletes. Using a read-only token makes sure of that on the Foundry side too.

## Troubleshooting

- **`fetch failed`**: Foundry isn't running, or it's on a different port. Check with `curl http://localhost:3001/api/health`.
- **401**: The token is wrong or has been revoked.
- **403 Origin / Host**: Use `localhost` or `127.0.0.1` in `FOUNDRY_URL`, not your machine's network IP.
- **Logs**: Claude Desktop writes them to `~/Library/Logs/Claude/mcp-server-helix-foundry.log` on macOS and `%APPDATA%\Claude\logs` on Windows.
