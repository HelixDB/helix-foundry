export async function api<T = any>(
  path: string,
  body?: unknown,
  method?: string,
  extraHeaders?: Record<string, string>,
  options?: Pick<RequestInit, "keepalive">,
): Promise<T> {
  const res = await fetch("/api/v1" + path, {
    ...options,
    credentials: "same-origin",
    method: method || (body === undefined ? "GET" : "POST"),
    headers: {
      ...(body instanceof FormData
        ? {}
        : { "content-type": "application/json" }),
      ...extraHeaders,
    },
    body:
      body === undefined
        ? undefined
        : body instanceof FormData
          ? body
          : JSON.stringify(body),
  });
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || "Request failed");
  return data;
}
export const number = (n: number | undefined) =>
  new Intl.NumberFormat("en-US").format(n || 0);
export const relative = (date: string) => {
  const min = Math.floor((Date.now() - Date.parse(date)) / 60000);
  return min < 1
    ? "just now"
    : min < 60
      ? `${min}m ago`
      : min < 1440
        ? `${Math.floor(min / 60)}h ago`
        : new Date(date).toLocaleDateString();
};
