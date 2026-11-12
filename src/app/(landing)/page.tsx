import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { Landing } from "@/landing/landing";
import { jsonLd } from "@/server/agent-context";
import { authConfig } from "@/server/config";
import { LOCAL } from "@/shared/deployment";

type Props = {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
};

// `/`: the hosted site's landing page, for a visitor who isn't signed in.
// Anyone with a session, and every local build (which has no sign-in and no
// landing page), opens the app on Ask, as the app's own `/` does, keeping the
// link's query. A session that has run out is the app's to send to sign-in.
export default async function Home({ searchParams }: Props) {
  const session = authConfig()?.sessionCookie;
  if (LOCAL || (session && (await cookies()).has(session))) {
    const query = new URLSearchParams(
      Object.entries(await searchParams).flatMap(([key, value]) =>
        [value ?? []].flat().map((v) => [key, v]),
      ),
    ).toString();
    redirect(query ? `/ask?${query}` : "/ask");
  }
  return (
    <>
      {/* What Foundry is and costs, for search engines and agents
          (schema.org). "<" is escaped so no value ends the script. */}
      <script
        type="application/ld+json"
        dangerouslySetInnerHTML={{
          __html: JSON.stringify(jsonLd()).replace(/</g, "\\u003c"),
        }}
      />
      <Landing />
    </>
  );
}
