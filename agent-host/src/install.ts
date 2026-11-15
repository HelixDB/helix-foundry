// Keeping the helper running: a launchd agent on macOS, a systemd user unit
// on Linux, each running the pinned package through npx (so a cleared npx
// cache costs a download, not the helper), with the PATH, `claude` and
// `codex` the person ran install with, and their CODEX_HOME when they set
// one.

import { execFile } from "node:child_process";
import { mkdir, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { promisify } from "node:util";

import { PACKAGE, invoke } from "./version.ts";

export const LABEL = "com.helix-db.foundry-agent-host";
export const UNIT = "foundry-agent-host.service";

/** What a unit runs: npx, the pinned package, and run with its CLIs (one at least). */
export type Service = {
  npx: string;
  claude?: string;
  codex?: string;
  path: string;
  home: string;
  /** CODEX_HOME, when the person set one: where Codex's login and sessions are. */
  codexHome?: string;
};

/** The command a unit runs. */
export function command(s: Service): string[] {
  return [
    s.npx,
    "-y",
    PACKAGE,
    "run",
    ...(s.claude ? ["--claude", s.claude] : []),
    ...(s.codex ? ["--codex", s.codex] : []),
  ];
}

/** The environment a unit sets. */
const environment = (s: Service): [string, string][] => [
  ["PATH", s.path],
  ...(s.codexHome ? ([["CODEX_HOME", s.codexHome]] as [string, string][]) : []),
];

const xml = (s: string) =>
  s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");

/** The launchd agent's plist. */
export function launchdPlist(s: Service): string {
  const args = command(s)
    .map((a) => `    <string>${xml(a)}</string>`)
    .join("\n");
  const log = join(s.home, "Library", "Logs", "helix-foundry-agent-host.log");
  return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
${args}
  </array>
  <key>EnvironmentVariables</key>
  <dict>
${environment(s)
  .map(([k, v]) => `    <key>${k}</key>\n    <string>${xml(v)}</string>`)
  .join("\n")}
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>StandardOutPath</key>
  <string>${xml(log)}</string>
  <key>StandardErrorPath</key>
  <string>${xml(log)}</string>
</dict>
</plist>
`;
}

/** A systemd argument, quoted. */
const quote = (a: string) =>
  `"${a.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;

/** The systemd user unit. */
export function systemdUnit(s: Service): string {
  return `[Unit]
Description=Helix Foundry's Assistant on this computer's Claude Code or Codex
After=network-online.target

[Service]
ExecStart=${command(s).map(quote).join(" ")}
${environment(s)
  .map(([k, v]) => `Environment=${quote(`${k}=${v}`)}`)
  .join("\n")}
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`;
}

/** Where a platform's unit goes, and the commands that start it. */
export function plan(
  platform: NodeJS.Platform,
  s: Service,
  uid: number,
): { file: string; body: string; commands: string[][] } {
  if (platform === "darwin") {
    const file = join(s.home, "Library", "LaunchAgents", `${LABEL}.plist`);
    return {
      file,
      body: launchdPlist(s),
      commands: [
        ["launchctl", "bootout", `gui/${uid}/${LABEL}`],
        ["launchctl", "bootstrap", `gui/${uid}`, file],
      ],
    };
  }
  if (platform === "linux") {
    const file = join(s.home, ".config", "systemd", "user", UNIT);
    return {
      file,
      body: systemdUnit(s),
      commands: [
        ["systemctl", "--user", "daemon-reload"],
        ["systemctl", "--user", "enable", "--now", UNIT],
        ["systemctl", "--user", "restart", UNIT],
      ],
    };
  }
  throw new Error(
    `install keeps the helper running on macOS and Linux; on ${platform}, run it yourself: ${invoke("run")}`,
  );
}

const run = promisify(execFile);

/**
 * Writes the unit and starts it. A bootout of an agent that isn't loaded
 * fails, and is no error.
 */
export async function install(
  s: Service,
  opts: {
    platform?: NodeJS.Platform;
    uid?: number;
    exec?: (cmd: string, args: string[]) => Promise<unknown>;
    dryRun?: boolean;
  } = {},
): Promise<{ file: string; body: string; commands: string[][] }> {
  const p = plan(
    opts.platform ?? process.platform,
    s,
    opts.uid ?? process.getuid?.() ?? 0,
  );
  if (opts.dryRun) return p;
  await mkdir(dirname(p.file), { recursive: true });
  await writeFile(p.file, p.body, { mode: 0o644 });
  const exec = opts.exec ?? ((cmd, args) => run(cmd, args));
  for (const [cmd, ...args] of p.commands) {
    try {
      await exec(cmd!, args);
    } catch (e) {
      if (!(cmd === "launchctl" && args[0] === "bootout")) throw e;
    }
  }
  return p;
}

/** This person's service: npx beside the node running the helper. */
export function service(
  clis: { claude?: string; codex?: string },
  env: NodeJS.ProcessEnv = process.env,
): Service {
  return {
    npx: join(dirname(process.execPath), "npx"),
    ...(clis.claude && { claude: clis.claude }),
    ...(clis.codex && { codex: clis.codex }),
    path: env.PATH ?? "/usr/bin:/bin",
    home: env.HOME || homedir(),
    ...(clis.codex && env.CODEX_HOME && { codexHome: env.CODEX_HOME }),
  };
}
