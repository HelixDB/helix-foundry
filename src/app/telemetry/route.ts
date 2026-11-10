import { localConfig } from "@/server/config";
import { forwardUsage, telemetryEnabled } from "@/server/telemetry";
import { LOCAL } from "@/shared/deployment";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// The "Foundry Local" site's, in a local install's image (next.config.ts).
const CLIENT_ID = process.env.NEXT_PUBLIC_DATABUDDY_CLIENT_ID || "";

// GET /telemetry: { enabled }, whether this install reports how it is used
// (src/server/telemetry.ts). A local build's page sends nothing until this
// says yes; the hosted site doesn't ask.
export function GET() {
  return Response.json(
    { enabled: telemetryEnabled(process.env) },
    { headers: { "Cache-Control": "no-store" } },
  );
}

// POST /telemetry: a local page's usage events, forwarded to Databuddy
// (forwardUsage). Only a local build with a client id has it.
export function POST(request: Request) {
  const local = LOCAL && CLIENT_ID ? localConfig() : null;
  if (!local) return new Response(null, { status: 404 });
  return forwardUsage(
    request,
    {
      origin: local.origin,
      clientId: CLIENT_ID,
      enabled: telemetryEnabled(process.env),
    },
    fetch,
  );
}
