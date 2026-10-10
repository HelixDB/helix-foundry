import { setTimeout as sleep } from "node:timers/promises";

// SaaS APIs rate-limit bulk reads; honor Retry-After and back off on 429/5xx.
export async function retryingFetch(
  url: URL,
  headers: Record<string, string>,
  init: RequestInit = {},
) {
  for (let attempt = 0; ; attempt++) {
    const response = await fetch(url, {
      ...init,
      headers,
      redirect: "error",
      signal: init.signal
        ? AbortSignal.any([init.signal, AbortSignal.timeout(30000)])
        : AbortSignal.timeout(30000),
    });
    if ((response.status === 429 || response.status >= 500) && attempt < 6) {
      const wait = Number(response.headers.get("retry-after")) * 1000;
      const delay = wait > 0 ? Math.min(wait, 30000) : 250 * 2 ** attempt;
      await (init.signal
        ? sleep(delay, undefined, { signal: init.signal })
        : sleep(delay));
      continue;
    }
    return response;
  }
}
