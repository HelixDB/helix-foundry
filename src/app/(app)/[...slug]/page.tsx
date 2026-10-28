import { ClientOnly } from "./client";

// Every path but / (the landing page) that is not a route handler (/auth,
// /login/callback, /papi) is the single-page app; react-router owns the URL
// from here. Each is rendered once, the first time it is asked for.
export function generateStaticParams() {
  return [];
}

export default function Page() {
  return <ClientOnly />;
}
