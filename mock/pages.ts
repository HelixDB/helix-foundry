import type { ServerResponse } from "node:http";

// The plain HTML pages the mock serves in place of hosted ones a link
// leaves the site for: Stripe Checkout and the billing portal
// (./routes/billing.ts), and a provider's consent page for connecting a
// source by signing in (./routes/sign-ins.ts).

export const escape = (s: string) =>
  s.replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ]!,
  );

export const page = (title: string, body: string) =>
  `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>${escape(title)}</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#f8f7f3;color:#292a26;font:14px Inter,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
main{display:grid;gap:12px;width:360px}form{display:grid;gap:12px}h1{margin:0 0 4px;font:400 26px Georgia,serif}p{margin:0 0 8px;color:#65675e}
label{display:grid;gap:6px}input{height:36px;padding:0 10px;border:1px solid #d6d3c8;border-radius:8px;font:inherit}
button,a{display:grid;place-items:center;height:40px;border:0;border-radius:8px;background:#343932;color:#fbfaf6;font:500 14px inherit;text-decoration:none;cursor:pointer}
a,button.secondary{background:#eceae2;color:#292a26}</style></head><body><main>${body}</main></body></html>`;

export function send(res: ServerResponse, status: number, html: string) {
  res.writeHead(status, {
    "Content-Type": "text/html; charset=utf-8",
    "Cache-Control": "no-store",
  });
  res.end(html);
}
