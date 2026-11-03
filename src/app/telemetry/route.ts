import { telemetryEnabled } from "@/server/telemetry";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// GET /telemetry: { enabled }, whether this install reports how it is used
// (src/server/telemetry.ts). A local build's tracker loads only once this
// says yes; the hosted site doesn't ask.
export function GET() {
  return Response.json(
    { enabled: telemetryEnabled(process.env) },
    { headers: { "Cache-Control": "no-store" } },
  );
}
