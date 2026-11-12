import { checkBotId } from "botid/server";
import type { NextRequest } from "next/server";
import { handleRemind, remindersConfig } from "@/server/reminders";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// POST /remind from the landing page's reminder form (src/landing/remind.tsx):
// src/server/reminders.ts checks it and asks Foundry's door to remind the
// address. On the hosted stamps Vercel BotID runs first: the page's
// src/instrumentation-client.ts marks the form's request, and checkBotId
// reads the mark. Other builds (the mock, e2e, pnpm dev) have no BotID.
const BOTID = process.env.NEXT_PUBLIC_BOTID === "1";

export async function POST(request: NextRequest) {
  return handleRemind(request, {
    config: remindersConfig(),
    isBot: BOTID ? async () => (await checkBotId()).isBot : undefined,
  });
}
