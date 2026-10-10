import { execute } from "./engine.js";
let body = "";
for await (const chunk of process.stdin) body += chunk;
try {
  process.stdout.write(
    JSON.stringify(
      await execute(JSON.parse(body), undefined, process.env.EXECUTOR_SCRATCH),
    ),
  );
} catch (e) {
  process.stderr.write(e instanceof Error ? e.message : "Execution failed");
  process.exitCode = 1;
}
