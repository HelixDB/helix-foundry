import "./data.css";
import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { KeyRound, TriangleAlert } from "lucide-react";
import { Button, ButtonLink, Dialog } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { needsSignInQuery } from "./data-api";
import { canManage } from "./model";
import { useSignInStart } from "./sign-in";
import { breakKey, brokenNotice, needsSignIn } from "./sign-ins";

// The shell's notice of connectors that stopped syncing because their
// account needs signing in again (the member who connected one left, or the
// provider revoked it), from one cheap read (needsSignInQuery). Owners and
// admins get a dialog the first time they open Foundry after a break (once
// per break, in this browser), then a banner at the top of every page until
// it is fixed; members get the banner, without Sign in again. Later hides
// the banner for the session. One broken connector is named, with Sign in
// again; several share one dialog and banner, which open Data.

// Breaks whose dialog this browser has shown (localStorage, kept to the
// newest few), and those whose banner this session hid (sessionStorage).
const SEEN = "foundry.account-broken.seen.v1",
  LATER = "foundry.account-broken.later.v1",
  KEEP = 100;
// The read waits this long after the app opens, so the page's own first
// reads go out before it: it is a notice, not what the page shows.
const AFTER_MS = 1000;

function readKeys(storage: () => Storage, key: string): string[] {
  try {
    const v: unknown = JSON.parse(storage().getItem(key) || "[]");
    return Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
  } catch {
    return [];
  }
}
function writeKeys(storage: () => Storage, key: string, keys: string[]) {
  try {
    storage().setItem(key, JSON.stringify(keys.slice(-KEEP)));
  } catch {
    // Storage blocked: the dialog may show again.
  }
}
const local = () => localStorage,
  session = () => sessionStorage;

export function AccountBreaks() {
  const { id, role } = useWorkspace(),
    manage = canManage(role),
    start = useSignInStart();
  const [armed, setArmed] = useState(false);
  useEffect(() => {
    const timer = setTimeout(() => setArmed(true), AFTER_MS);
    return () => clearTimeout(timer);
  }, []);
  const query = useQuery({ ...needsSignInQuery(id), enabled: armed });
  const [seen, setSeen] = useState(() => readKeys(local, SEEN)),
    [later, setLater] = useState(() => readKeys(session, LATER));
  const broken = (query.data?.connectors || []).filter(needsSignIn);
  if (!broken.length) return null;
  const keys = broken.map(breakKey),
    one = broken.length === 1 ? broken[0]! : undefined,
    notice = brokenNotice(broken);
  const add = (
    list: string[],
    set: (keys: string[]) => void,
    storage: () => Storage,
    key: string,
  ) => {
    const next = [...list.filter((k) => !keys.includes(k)), ...keys];
    writeKeys(storage, key, next);
    set(next);
  };
  const markSeen = () => add(seen, setSeen, local, SEEN),
    hide = () => add(later, setLater, session, LATER);
  const signInAgain = (c: typeof one) => {
    markSeen();
    if (c) void start.go({ kind: c.kind, connectorId: c.id });
  };
  const dialog = manage && keys.some((k) => !seen.includes(k)),
    banner = keys.some((k) => !later.includes(k));
  const action = (size: "sm" | "md") =>
    one ? (
      <Button
        size={size}
        variant="primary"
        icon={KeyRound}
        pending={start.pending}
        onClick={() => signInAgain(one)}
      >
        Sign in again
      </Button>
    ) : (
      <ButtonLink
        size={size}
        variant="primary"
        to={paths.data()}
        onClick={markSeen}
      >
        Open Data
      </ButtonLink>
    );
  return (
    <>
      {banner && (
        <div
          className="dc-account-banner"
          role="region"
          aria-label="Connections that stopped syncing"
        >
          <TriangleAlert
            size={16}
            className="dc-account-banner-icon"
            aria-hidden
          />
          <p className="dc-account-banner-text">
            <strong>{notice.title}.</strong> {notice.text}
            {!manage &&
              ` Ask an owner or admin to sign ${one ? "it" : "them"} in again.`}
          </p>
          <span className="dc-account-banner-actions">
            {manage && action("sm")}
            <Button size="sm" variant="ghost" onClick={hide}>
              Later
            </Button>
          </span>
          {start.error && (
            <p role="alert" className="dc-account-banner-error">
              {start.error.message}
            </p>
          )}
        </div>
      )}
      {dialog && (
        <Dialog
          title={notice.title}
          description={notice.text}
          size="sm"
          onClose={markSeen}
          footer={
            <>
              <Button onClick={markSeen}>Later</Button>
              {action("md")}
            </>
          }
        >
          <p className="dc-account-dialog-note">
            {one
              ? "Signing in again resumes it with one sync, reading with your account."
              : "Data lists each one; signing one in again resumes it with one sync."}
          </p>
        </Dialog>
      )}
    </>
  );
}
