import type { NextRequest } from "next/server";
import { resetLink } from "@/server/password";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// The password reset link WorkOS emails (the WFE's
// auth.applications.foundry.password_reset_url). See src/server/password.ts.
export function GET(request: NextRequest) {
  return resetLink(request);
}
