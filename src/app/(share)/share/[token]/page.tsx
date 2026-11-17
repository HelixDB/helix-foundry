import { notFound } from "next/navigation";
import { connection } from "next/server";
import { shareDoor } from "@/server/share";
import { SharedDashboard } from "./client";

// /share/<token>: a published dashboard, for anyone who has the link. The
// door's origin is read at request time (HELIX_INGEST_ORIGIN, else the
// stamp's events URL: src/server/share.ts) and the browser asks the door
// for the snapshot itself. A build without public links has no such page.
export default async function SharePage({
  params,
}: {
  params: Promise<{ token: string }>;
}) {
  await connection();
  const door = shareDoor();
  if (!door) notFound();
  const { token } = await params;
  return <SharedDashboard door={door} token={token} />;
}
