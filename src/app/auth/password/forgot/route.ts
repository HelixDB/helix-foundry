import type { NextRequest } from "next/server";
import { handlePassword } from "@/server/password";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// See src/server/password.ts.
export async function POST(request: NextRequest) {
  return handlePassword(request, "forgot");
}
