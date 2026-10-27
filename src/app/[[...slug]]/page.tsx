import { ClientOnly } from "./client";

// Every path that is not a route handler (/auth, /login/callback, /papi) is
// the single-page app; react-router owns the URL from here.
export function generateStaticParams() {
  return [{ slug: [""] }];
}

export default function Page() {
  return <ClientOnly />;
}
