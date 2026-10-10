import { request as httpRequest } from "node:http";
import { request as httpsRequest } from "node:https";
// POSTs a JSON body and waits for the reply for as long as `signal` allows.
// fetch gives up on a server that sends no headers for five minutes, and both
// Ollama reading a long prompt on a CPU and the executor running a whole-table
// job send none until they are done.
export function postJson(
  url: string,
  body: string,
  signal: AbortSignal,
  headers: Record<string, string> = {},
) {
  return new Promise<{ status: number; text: string }>((resolve, reject) => {
    const target = new URL(url);
    const req = (target.protocol === "https:" ? httpsRequest : httpRequest)(
      target,
      {
        method: "POST",
        headers: { ...headers, "content-type": "application/json" },
        signal,
      },
      (res) => {
        let text = "";
        res.setEncoding("utf8");
        res.on("data", (chunk) => (text += chunk));
        res.on("end", () => resolve({ status: res.statusCode || 0, text }));
        res.on("error", (error) =>
          reject(signal.aborted ? signal.reason : error),
        );
      },
    );
    // Surface the abort reason (a timeout or "Run canceled"), as fetch does.
    req.on("error", (error) => reject(signal.aborted ? signal.reason : error));
    req.end(body);
  });
}
