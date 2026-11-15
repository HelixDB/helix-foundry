/** The helper's version, as package.json has it (a test holds them equal). */
export const VERSION = "0.1.0";

/** The pinned package, as npx runs it. */
export const PACKAGE = `@helixdb/foundry-agent-host@${VERSION}`;

/**
 * How the person runs one of the helper's commands, as they first ran it:
 * through npx, at this version (no foundry-agent-host is on their PATH).
 */
export const invoke = (args: string) => `npx -y ${PACKAGE} ${args}`;
