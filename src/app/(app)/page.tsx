import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { authConfig } from "@/server/config";
import { LOCAL } from "@/shared/deployment";

/** Foundry's landing page, on helix-website (its home). */
const LANDING_URL = "https://www.helix-db.com/";

type Props = {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
};

// `/` has no page of its own. A visitor without a session goes to the
// landing page, keeping the link's query (its `utm_*`). Anyone with one, and
// every local build (no sign-in), opens the app on Ask, as the app's own `/`
// does, keeping the query but `via` (an old reminder's or README's link); a
// session that has run out is the app's to send to sign-in.
export default async function Home({ searchParams }: Props) {
  const all = Object.entries(await searchParams).flatMap(([key, value]) =>
    [value ?? []].flat().map((v) => [key, v]),
  );
  const session = authConfig()?.sessionCookie;
  if (!LOCAL && !(session && (await cookies()).has(session))) {
    const query = new URLSearchParams(all).toString();
    redirect(query ? `${LANDING_URL}?${query}` : LANDING_URL);
  }
  const query = new URLSearchParams(
    all.filter(([key]) => key !== "via"),
  ).toString();
  redirect(query ? `/ask?${query}` : "/ask");
}
