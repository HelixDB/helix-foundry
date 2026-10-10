import { config, requireSettings } from "./config.js";
import { store } from "./store.js";
import { createApp } from "./app.js";
import { startWorker } from "./jobs.js";
requireSettings();
if (
  !["127.0.0.1", "localhost", "::1"].includes(config.host) &&
  !config.allowNetwork
)
  throw new Error(
    `Helix Foundry has no sign-in, so it only listens on this computer. Use HOST=127.0.0.1, or set ALLOW_NETWORK_ACCESS=true when something in front of it keeps it private (HOST is ${config.host}).`,
  );
await store.init();
const app = await createApp(store);
await app.listen({ port: config.port, host: config.host });
const stop = config.inlineWorker ? startWorker(store) : () => {};
for (const signal of ["SIGINT", "SIGTERM"])
  process.on(signal, async () => {
    await stop();
    await app.close();
    process.exit(0);
  });
