import {
  StrictMode,
  Suspense,
  lazy,
  useEffect,
  useState,
  type ComponentType,
  type ReactNode,
} from "react";
import {
  BrowserRouter,
  Navigate,
  Route,
  Routes,
  matchPath,
  useLocation,
  useNavigate,
} from "react-router-dom";
import {
  QueryClient,
  QueryClientProvider,
  useQuery,
} from "@tanstack/react-query";
import { X } from "lucide-react";
import { LOCAL } from "../shared/deployment";
import type { WhoamiResponse } from "../shared/wfe";
import { signInPath } from "../shared/return-path";
import {
  identifyUser,
  signedIn,
  trackEvent,
  workspaceOpened,
} from "./analytics";
import { ApiError } from "./api";
import { Button, SkeletonText } from "./kit";
import { paths } from "./paths";
import { Shell } from "./shell/Shell";
import {
  ChromeBoundary,
  NotFound,
  RouteErrorBoundary,
} from "./shell/ErrorBoundary";
import { applyTheme, readTheme } from "./shell/theme";
import { SignInPage, SignedInAs } from "./sign-in";
import { WorkspacePicker } from "./areas/workspace/picker";
import { myAccessQuery } from "./areas/workspace/access-api";
import { granteeRedirect } from "./areas/workspace/access-model";
import { ontologyQueries } from "./areas/ontology/queries";
import { captureCallback, tabStorage } from "./areas/data/sign-ins";
import {
  Loading,
  WorkspaceContext,
  type ShellUser,
  type ShellWorkspace,
} from "./ui";
import {
  chooseWorkspace,
  chosenWorkspace,
  shellUser,
  shellWorkspaces,
  whoamiQuery,
  workspacesQuery,
} from "./workspaces";

// The Foundry single-page app, mounted client-only by src/app/(app)/[...slug].
// Signed out: the sign-in page. Signed in: the workspace picker until a
// workspace is chosen, then the shell and each area's routes, or, while the
// workspace has no ontology, only the onboarding's. A local build
// (src/shared/deployment.ts) is always signed in and has no sign-in page.

// A provider's return from connecting a source by signing in brings a
// single-use code in the URL: it moves into this tab's sign-in, and the URL
// becomes the bare callback, before any call (./areas/data/sign-ins.ts).
if (typeof window !== "undefined") {
  applyTheme(readTheme());
  captureCallback(window.location, tabStorage(), (path) =>
    window.history.replaceState(window.history.state, "", path),
  );
}

// Each area's index.tsx exports its route components; main.tsx owns the table.
function area<M>(load: () => Promise<M>, name: keyof M) {
  return lazy(() =>
    load().then((m) => ({ default: m[name] as ComponentType<any> })),
  );
}
const data = () => import("./areas/data"),
  ontology = () => import("./areas/ontology"),
  workspace = () => import("./areas/workspace");
const SettingsRoutes = area(workspace, "SettingsRoutes");
// Data and Ontology own their whole URL prefix, and the Ontology area also
// the Explorer's and Ask's: each *Routes component holds its nested route
// table.
const DataRoutes = area(data, "DataRoutes"),
  OntologyRoutes = area(ontology, "OntologyRoutes"),
  ExploreRoutes = area(ontology, "ExploreRoutes"),
  AskRoutes = area(ontology, "AskRoutes");
// Where a provider returns from connecting a source by signing in, and the
// shell's notice of connectors that need signing in again: the Data area's.
const ConnectCallback = area(data, "ConnectCallback"),
  AccountBreaks = lazy(() =>
    import("./areas/data/account-breaks").then((m) => ({
      default: m.AccountBreaks,
    })),
  );

// The onboarding's wizard, in place of a page it doesn't offer. The Helix
// MCP's connector links (/data?add=<kind>, with the sign-in an agent
// started: &sign_in=<id>) open their form on it, and its API source token
// links (/data/connections/<id>?token=new) the source's send panel
// (?event_source=<id>&token=new); a link's workspace is kept.
function ToOnboarding() {
  const { pathname, search } = useLocation(),
    params = new URLSearchParams(search),
    add = pathname === "/data" && params.get("add"),
    signIn = add && params.get("sign_in"),
    source =
      params.get("token") === "new" &&
      matchPath("/data/connections/:connectorId", pathname)?.params.connectorId;
  const q = new URLSearchParams();
  if (add) q.set("add", add);
  if (signIn) q.set("sign_in", signIn);
  if (source) {
    q.set("event_source", source);
    q.set("token", "new");
  }
  const workspace = params.get("workspace");
  if (workspace) q.set("workspace", workspace);
  return (
    <Navigate to={paths.newOntology() + (q.size ? "?" + q : "")} replace />
  );
}
// / is Ask, on the ontology last opened (else the newest); a link's query
// goes with it. While the workspace has no ontology (`onboarding`), it
// offers only the new-ontology wizard, which connects data and uploads
// documents in place, the pages of the ontology it creates (they open
// before the list has it), Settings, for the plan a database connection
// needs, and the callback of a sign-in started from the wizard. One table
// either way, so a page open as the first ontology arrives stays mounted. A
// grantee (restricted to what source grants give) is never onboarding, and
// stays on Ask and its ontologies' overviews (granteeRedirect).
function WorkspaceRoutes({
  onboarding,
  grantee,
}: {
  onboarding: boolean;
  grantee: boolean;
}) {
  const { pathname, search } = useLocation();
  const offered = (page: ReactNode) => (onboarding ? <ToOnboarding /> : page);
  const away = grantee ? granteeRedirect(pathname) : undefined;
  if (away) return <Navigate to={away} replace />;
  return (
    <Routes>
      <Route
        path="/"
        element={offered(
          <Navigate to={{ pathname: "/ask", search }} replace />,
        )}
      />
      <Route path="/data/*" element={offered(<DataRoutes />)} />
      {onboarding && <Route path="/ontology" element={<ToOnboarding />} />}
      <Route path="/ontology/*" element={<OntologyRoutes />} />
      <Route path="/explore/*" element={offered(<ExploreRoutes />)} />
      <Route path="/ask/*" element={offered(<AskRoutes />)} />
      <Route path="/settings/*" element={<SettingsRoutes />} />
      <Route path="/connect/callback" element={<ConnectCallback />} />
      <Route path="*" element={offered(<NotFound />)} />
    </Routes>
  );
}
function RouteFallback() {
  return (
    <div className="hf-page hf-page--default" aria-busy="true">
      <SkeletonText lines={5} />
    </div>
  );
}
function Centered({ children }: { children: ReactNode }) {
  return <div className="hf-app hf-center">{children}</div>;
}
function GateError({
  text,
  retry,
  email,
}: {
  text: string;
  retry: () => void;
  // The signed-in user's, when known; Sign out is offered either way.
  email?: string;
}) {
  return (
    <Centered>
      <div className="hf-gate-error">
        <p role="alert">{text}</p>
        <Button onClick={retry}>Retry</Button>
        <SignedInAs email={email} />
      </div>
    </Centered>
  );
}

// The chosen workspace's shell and pages, once its ontologies are read: with
// none, it is onboarding. A list that couldn't be read leaves its pages to
// say so. Hosted, what the user reads through source grants is read first:
// a grantee sees only Ask and the ontologies its grants cover, never the
// onboarding. If it can't be read, the user is taken as no grantee: the WFE
// still refuses what a grantee can't do.
function WorkspaceApp({
  workspaces,
  current,
  user,
  onSwitch,
  notify,
}: {
  workspaces: ShellWorkspace[];
  current: ShellWorkspace;
  user: ShellUser;
  onSwitch: (id: string) => void;
  notify: (text: string, error?: boolean) => void;
}) {
  const location = useLocation();
  const list = useQuery(ontologyQueries.list(current.id)),
    access = useQuery({ ...myAccessQuery(current.id), enabled: !LOCAL });
  const grantee = !LOCAL && !!access.data?.grantee,
    onboarding = !grantee && !!list.data && !list.data.ontologies?.length;
  useEffect(
    () => workspaceOpened(current),
    [current.id, current.slug, current.name, current.role],
  );
  useEffect(() => {
    if (onboarding) trackEvent("onboarding_shown");
  }, [onboarding, current.id]);
  if (list.isPending || (!LOCAL && access.isPending))
    return (
      <div className="hf-center">
        <Loading />
      </div>
    );
  return (
    <WorkspaceContext.Provider
      value={{
        ...current,
        user,
        onboarding,
        grantee,
        granted: access.data?.ontologyIds || [],
        partial: access.data?.partialOntologyIds || [],
        notify,
      }}
    >
      <Shell
        workspaces={workspaces}
        current={current}
        user={user}
        onSwitch={onSwitch}
      >
        {/* A local build has no accounts to break, and a grantee reads no
            connectors. */}
        {!LOCAL && !grantee && (
          <ChromeBoundary name="Connector notices" fallback={null}>
            <Suspense fallback={null}>
              <AccountBreaks key={current.id} />
            </Suspense>
          </ChromeBoundary>
        )}
        <RouteErrorBoundary resetKey={location.pathname}>
          <Suspense fallback={<RouteFallback />}>
            {/* Keyed by workspace: switching starts every page afresh. */}
            <div key={current.id} className="hf-route">
              <WorkspaceRoutes onboarding={onboarding} grantee={grantee} />
            </div>
          </Suspense>
        </RouteErrorBoundary>
      </Shell>
    </WorkspaceContext.Provider>
  );
}

// Signed in, with the user's workspaces loaded.
function SignedIn({ me }: { me: WhoamiResponse }) {
  const location = useLocation(),
    navigate = useNavigate();
  const list = useQuery(workspacesQuery);
  const [chosen, setChosen] = useState(chosenWorkspace);
  const [toast, setToast] = useState<{ text: string; error: boolean } | null>(
    null,
  );
  useEffect(() => {
    if (!toast) return;
    const timer = setTimeout(() => setToast(null), 6000);
    return () => clearTimeout(timer);
  }, [toast]);
  // A link may name its workspace (?workspace=<id>, as the Helix MCP's
  // connector links do). One the user belongs to opens and is remembered;
  // the parameter then leaves the URL either way.
  const linked = new URLSearchParams(location.search).get("workspace");
  // A sign-in this tab started ends here, once the workspaces are read.
  const signedInTo =
    list.data && (list.data.workspaces?.length ? "workspace" : "onboarding");
  useEffect(() => {
    if (signedInTo) signedIn(signedInTo);
  }, [signedInTo]);
  useEffect(
    () =>
      identifyUser(me.user.id, {
        email: me.user.email,
        name: me.user.displayName,
      }),
    [me.user.id, me.user.email, me.user.displayName],
  );
  useEffect(() => {
    if (!linked || !list.data) return;
    if (
      shellWorkspaces(me, list.data.workspaces || []).some(
        (w) => w.id === linked,
      )
    ) {
      chooseWorkspace(linked);
      setChosen(linked);
    } else
      setToast({
        text: "That link is for a workspace you aren’t a member of.",
        error: true,
      });
    const params = new URLSearchParams(location.search);
    params.delete("workspace");
    const search = params.toString();
    // What the page arrived with (a connector's sign-in notes) stays.
    navigate(
      {
        pathname: location.pathname,
        search: search ? `?${search}` : "",
        hash: location.hash,
      },
      { replace: true, state: location.state },
    );
  }, [linked, list.data, me, location, navigate]);
  if (list.isPending)
    return (
      <Centered>
        <Loading />
      </Centered>
    );
  if (list.error)
    return (
      <GateError
        text="Couldn’t load your workspaces."
        retry={() => void list.refetch()}
        email={me.user.email}
      />
    );
  const workspaces = shellWorkspaces(me, list.data.workspaces || []),
    user = shellUser(me);
  const choose = (id: string) => {
    chooseWorkspace(id);
    setChosen(id);
  };
  const selected =
    workspaces.find((w) => w.id === linked) ||
    workspaces.find((w) => w.id === chosen) ||
    (workspaces.length === 1 ? workspaces[0] : undefined);
  if (location.pathname === "/workspaces" || !selected)
    return (
      <WorkspacePicker
        email={user.email}
        workspaces={workspaces}
        onChoose={choose}
        next={
          location.pathname === "/workspaces"
            ? "/"
            : location.pathname + location.search
        }
      />
    );
  return (
    <div className="hf-app">
      <WorkspaceApp
        workspaces={workspaces}
        current={selected}
        user={user}
        onSwitch={choose}
        notify={(text, error = false) => setToast({ text, error })}
      />
      {toast && (
        <div
          role={toast.error ? "alert" : "status"}
          className={"hf-toast" + (toast.error ? " is-error" : "")}
        >
          <span>{toast.text}</span>
          <button aria-label="Dismiss" onClick={() => setToast(null)}>
            <X size={15} />
          </button>
        </div>
      )}
    </div>
  );
}

// Everything but /login needs a session: GET /v1/whoami decides.
function Authenticated() {
  const location = useLocation();
  const me = useQuery(whoamiQuery);
  if (me.isPending)
    return (
      <Centered>
        <Loading />
      </Centered>
    );
  if (me.error instanceof ApiError && me.error.status === 401 && LOCAL)
    return (
      <GateError
        text="The Foundry API refused this site’s local token. Start both from the same install, with docker compose up -d."
        retry={() => void me.refetch()}
      />
    );
  // Opened on another name for this computer (127.0.0.1 for localhost): the
  // site says which one it answers.
  if (
    me.error instanceof ApiError &&
    me.error.code === "HOST_REJECTED" &&
    LOCAL
  )
    return (
      <GateError text={me.error.message} retry={() => void me.refetch()} />
    );
  if (me.error instanceof ApiError && me.error.status === 401)
    return (
      <Navigate
        to={signInPath(location.pathname + location.search + location.hash)}
        replace
      />
    );
  if (me.error)
    return (
      <GateError
        text="Couldn’t reach Helix Foundry. Try again in a moment."
        retry={() => void me.refetch()}
      />
    );
  return <SignedIn me={me.data} />;
}

// One retry, and none for a request the WFE refused (4xx).
const client = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 5000,
      retry: (count, e) =>
        count < 1 && !(e instanceof ApiError && e.status < 500),
    },
  },
});

export default function FoundryApp() {
  return (
    <StrictMode>
      <QueryClientProvider client={client}>
        <BrowserRouter>
          <Routes>
            <Route
              path="/login"
              element={LOCAL ? <Navigate to="/" replace /> : <SignInPage />}
            />
            <Route path="*" element={<Authenticated />} />
          </Routes>
        </BrowserRouter>
      </QueryClientProvider>
    </StrictMode>
  );
}
