#!/usr/bin/env node
// foundry-agent-host: runs a local Helix Foundry install's Assistant on the
// Claude Code you are signed in to on this computer.
//
//   pair <code> [--foundry URL] [--name NAME]   pair with the code Foundry showed
//   run [--claude PATH] [--concurrency N]       run the Assistant's turns
//   install [--claude PATH] [--dry-run]         keep it running (launchd, systemd)
//   doctor [--claude PATH]                      say what stands in the way

import { hostname } from "node:os";
import { parseArgs } from "node:util";

import { configPath, loadPaired, savePaired } from "./config.ts";
import { doctor } from "./doctor.ts";
import { findClaude } from "./find.ts";
import { Foundry } from "./foundry.ts";
import { install, service } from "./install.ts";
import { serve } from "./run.ts";
import { VERSION } from "./version.ts";

const USAGE = `foundry-agent-host ${VERSION}: Helix Foundry's Assistant on your Claude Code subscription

  pair <code> [--foundry URL] [--name NAME]   pair with the code Foundry showed (default http://127.0.0.1:8081)
  run [--claude PATH] [--concurrency N]       run the Assistant's turns until stopped
  install [--claude PATH] [--dry-run]         keep it running at login (launchd on macOS, systemd on Linux)
  doctor [--claude PATH]                      say what stands in the way
`;

async function main(argv: string[]): Promise<number> {
  const { values, positionals } = parseArgs({
    args: argv,
    allowPositionals: true,
    options: {
      foundry: { type: "string" },
      name: { type: "string" },
      claude: { type: "string" },
      concurrency: { type: "string" },
      "dry-run": { type: "boolean" },
      version: { type: "boolean" },
      help: { type: "boolean" },
    },
  });
  const [command, ...rest] = positionals;
  if (values.version) {
    console.log(VERSION);
    return 0;
  }
  switch (command) {
    case "pair": {
      const code = rest[0];
      if (!code?.startsWith("hxp_")) {
        console.error("pair takes the code Foundry showed: hxp_…");
        return 2;
      }
      const base = values.foundry ?? "http://127.0.0.1:8081";
      const name = values.name ?? hostname();
      const paired = await new Foundry(base).pair({
        code,
        name,
        runtimes: ["claude_code"],
        version: VERSION,
        platform: `${process.platform}-${process.arch}`,
      });
      await savePaired({
        foundry: base,
        hostId: paired.host_id,
        name,
        token: paired.token,
      });
      console.log(
        `Paired with Foundry at ${base} as ${name}. Run foundry-agent-host install to keep it running, or foundry-agent-host run.`,
      );
      return 0;
    }
    case "run": {
      const paired = await loadPaired();
      if (!paired) {
        console.error(
          `Not paired (${configPath()}): run foundry-agent-host pair <code> first.`,
        );
        return 1;
      }
      const claude = await findClaude(values.claude);
      if (!claude) {
        console.error(
          "Claude Code isn't installed here, or isn't on PATH: install it, or name it with --claude <path>.",
        );
        return 1;
      }
      const concurrency = Math.min(
        4,
        Math.max(1, Number(values.concurrency ?? "1") || 1),
      );
      const stop = new AbortController();
      for (const signal of ["SIGINT", "SIGTERM"] as const)
        process.once(signal, () => stop.abort());
      await serve({
        foundry: new Foundry(paired.foundry, paired.token),
        claude,
        version: VERSION,
        concurrency,
        signal: stop.signal,
        log: (line) => console.log(`${new Date().toISOString()} ${line}`),
      });
      return 0;
    }
    case "install": {
      const claude = await findClaude(values.claude);
      if (!claude) {
        console.error(
          "Claude Code isn't installed here, or isn't on PATH: install it, or name it with --claude <path>.",
        );
        return 1;
      }
      if (!values["dry-run"] && !(await loadPaired())) {
        console.error("Pair first: foundry-agent-host pair <code>.");
        return 1;
      }
      const done = await install(service(claude), {
        dryRun: values["dry-run"],
      });
      console.log(
        values["dry-run"]
          ? `${done.file}\n\n${done.body}`
          : `Installed ${done.file}: the helper runs now, and at every login.`,
      );
      return 0;
    }
    case "doctor": {
      const checks = await doctor({ claude: values.claude });
      for (const c of checks)
        console.log(
          `${c.ok ? "ok     " : "problem"}  ${c.what}${!c.ok && c.fix ? `\n         ${c.fix}` : ""}`,
        );
      return checks.every((c) => c.ok) ? 0 : 1;
    }
    default:
      (command && !values.help ? console.error : console.log)(USAGE);
      return command && !values.help ? 2 : 0;
  }
}

// The SDK warns that allowedTools shadows canUseTool: the helper keeps
// canUseTool on purpose, a second guard that denies what allowedTools
// doesn't name (claude.ts), so that warning is dropped.
process.removeAllListeners("warning");
process.on("warning", (w) => {
  if ((w as { code?: string }).code !== "CLAUDE_SDK_CAN_USE_TOOL_SHADOWED")
    console.warn(`${w.name}: ${w.message}`);
});

main(process.argv.slice(2)).then(
  (code) => process.exit(code),
  (e) => {
    console.error((e as Error).message);
    process.exit(1);
  },
);
