import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { Landing, type Via } from "@/landing/landing";
import { authConfig } from "@/server/config";
import { remindersConfig } from "@/server/reminders";
import { LOCAL } from "@/shared/deployment";

type Props = {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
};

// `/`: the hosted site's landing page, for a visitor who isn't signed in.
// Anyone with a session, and every local build (which has no sign-in and no
// landing page), opens the app on Ask, as the app's own `/` does, keeping the
// link's query but its `via`. A session that has run out is the app's to
// send to sign-in. `?via=email` (a reminder's link) or `?via=github` (the
// README's) greets a visitor coming back on a computer (src/landing).
export default async function Home({ searchParams }: Props) {
  const params = await searchParams;
  const session = authConfig()?.sessionCookie;
  if (LOCAL || (session && (await cookies()).has(session))) {
    const query = new URLSearchParams(
      Object.entries(params)
        .filter(([key]) => key !== "via")
        .flatMap(([key, value]) => [value ?? []].flat().map((v) => [key, v])),
    ).toString();
    redirect(query ? `/ask?${query}` : "/ask");
  }
  const via = [params.via].flat()[0];
  return (
    <Landing
      via={via === "email" || via === "github" ? (via as Via) : undefined}
      remind={remindersConfig() !== null}
    />
  );
}
