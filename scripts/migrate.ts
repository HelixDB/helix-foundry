import { store, SCHEMA_VERSION } from "../apps/api/src/store.js";
import { requireSecrets } from "../apps/api/src/config.js";
requireSecrets();
await store.init();
console.log(`Helix Foundry schema version ${SCHEMA_VERSION} is ready.`);
