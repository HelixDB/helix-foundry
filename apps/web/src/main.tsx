import React, {
  Suspense,
  lazy,
  useState,
  useEffect,
  type ComponentType,
} from "react";
import { createRoot } from "react-dom/client";
import {
  BrowserRouter,
  Routes,
  Route,
  useLocation,
  useParams,
  useNavigate,
  Navigate,
} from "react-router-dom";
import {
  QueryClient,
  QueryClientProvider,
  useQuery,
} from "@tanstack/react-query";
import { X } from "lucide-react";
import { api } from "./api";
import { WorkspaceContext, Loading, type AskContext } from "./ui";
import { Button, SkeletonText, readStorage, writeStorage } from "./kit";
import { Shell } from "./shell/Shell";
import { LiveActivityProvider } from "./shell/live";
import { VoiceProvider } from "./areas/analyst/voice";
import {
  ChromeBoundary,
  NotFound,
  RouteErrorBoundary,
} from "./shell/ErrorBoundary";
import { applyTheme, readTheme } from "./shell/theme";
import { paths } from "./paths";
import "./style.css";
import "./minimal.css";
import "./workspace.css";
import "./kit/kit.css";
import "./shell/shell.css";

applyTheme(readTheme());

// Each area's index.tsx exports its route components; main.tsx owns the table.
function area<M>(load: () => Promise<M>, name: keyof M) {
  return lazy(() =>
    load().then((m) => ({ default: m[name] as ComponentType<any> })),
  );
}
const data = () => import("./areas/data"),
  ontology = () => import("./areas/ontology"),
  analyst = () => import("./areas/analyst"),
  workspace = () => import("./areas/workspace");
const Onboarding = lazy(() => import("./onboarding"));
const HomePage = area(workspace, "HomePage"),
  SettingsPage = area(workspace, "WorkspaceSettingsPage"),
  ProposalsPage = area(workspace, "ProposalsPage");
// Data, Ontology and Analyst own their whole URL prefix: each area's *Routes
// component holds its nested route table.
const DataRoutes = area(data, "DataRoutes");
const OntologyRoutes = area(ontology, "OntologyRoutes"),
  ObjectView = area(ontology, "ObjectView");
const AnalystRoutes = area(analyst, "AnalystRoutes"),
  AnalystPanel = area(analyst, "AnalystPanel");

// Keeps the query string when an old path moves.
function Redirect({ to }: { to: string }) {
  const { search } = useLocation();
  return <Navigate to={to + search} replace />;
}
function RunRedirect() {
  const { runId = "" } = useParams();
  return <Navigate to={paths.thread(runId)} replace />;
}
function WorkspaceRoutes() {
  return (
    <Routes>
      <Route path="/" element={<HomePage />} />
      <Route path="/data/*" element={<DataRoutes />} />
      <Route path="/sources" element={<Redirect to="/data" />} />
      <Route path="/explore" element={<Redirect to="/data" />} />
      <Route path="/pipelines/*" element={<Redirect to="/data" />} />
      <Route path="/ontology/*" element={<OntologyRoutes />} />
      <Route path="/records" element={<Redirect to="/ontology/explore" />} />
      <Route path="/records/:logicalId" element={<ObjectView />} />
      <Route path="/analyst/*" element={<AnalystRoutes />} />
      <Route path="/runs/:runId" element={<RunRedirect />} />
      {/* Dashboards were replaced by Home's key metrics. */}
      <Route path="/dashboards/*" element={<Navigate to="/" replace />} />
      <Route path="/activity" element={<Navigate to="/" replace />} />
      <Route path="/proposals" element={<ProposalsPage />} />
      <Route path="/settings" element={<SettingsPage />} />
      <Route path="/advanced" element={<Navigate to="/data" replace />} />
      <Route path="*" element={<NotFound />} />
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
function GateError({ retry }: { retry: () => void }) {
  return (
    <div className="hf-app hf-center">
      <div role="alert" className="hf-gate-error">
        <p>Couldn’t check workspace setup.</p>
        <Button onClick={retry}>Retry</Button>
      </div>
    </div>
  );
}
// Sends unfinished workspaces to setup. "Incomplete" is always checked again
// first: a publish (e.g. from a review) may have just finished setup.
function SetupGate({
  check,
}: {
  check: () => Promise<{ data?: any; error: unknown }>;
}) {
  const navigate = useNavigate(),
    [failed, setFailed] = useState(false);
  useEffect(() => {
    let live = true;
    void check().then((r) => {
      if (!live) return;
      if (r.error) setFailed(true);
      else if (!r.data?.complete)
        navigate(paths.onboarding(), { replace: true });
    });
    return () => {
      live = false;
    };
  }, []);
  return failed ? (
    <GateError retry={() => window.location.reload()} />
  ) : (
    <div className="hf-app hf-center">
      <Loading />
    </div>
  );
}
// Reachable while setup is unfinished: publishing a review is one way to
// finish setup, and setup itself links to Data (a schema change is accepted on
// the dataset page; viewers are sent to "Open data").
const ungated = /^\/(proposals|settings|data|sources|explore)(\/|$)/;

function App() {
  const location = useLocation();
  const me = useQuery({
    queryKey: ["me"],
    queryFn: () => api("/me"),
    retry: false,
  });
  const [chosen, setChosen] = useState(() => readStorage("foundry.workspace"));
  const [toast, setToast] = useState<{ text: string; error: boolean } | null>(
    null,
  );
  const [askContext, setAskContext] = useState<AskContext | null>(null);
  const selected =
    me.data?.workspaces.find((w: any) => w.id === chosen) ||
    me.data?.workspaces[0];
  const inSetup = location.pathname === "/onboarding";
  const onboarding = useQuery({
    queryKey: [selected?.id, "onboarding"],
    queryFn: () => api(`/workspaces/${selected.id}/onboarding`),
    enabled: !!selected && !inSetup,
    // Once setup is complete the shell stays open; no need to ask again.
    staleTime: (q) => ((q.state.data as any)?.complete ? Infinity : 0),
  });
  useEffect(() => {
    if (selected) writeStorage("foundry.workspace", selected.id);
    setAskContext(null);
  }, [selected?.id]);
  useEffect(() => {
    if (!toast) return;
    const timer = setTimeout(() => setToast(null), 6000);
    return () => clearTimeout(timer);
  }, [toast]);
  // No sign-in: the API sets up this computer's workspace on first run.
  if (me.isPending) return <Loading />;
  if (!me.data || !selected)
    return (
      <div className="hf-app hf-center">
        <div role="alert" className="hf-gate-error">
          <p>Couldn’t reach Helix Foundry. Check that the API is running.</p>
          <Button onClick={() => me.refetch()}>Retry</Button>
        </div>
      </div>
    );
  const gated = !inSetup && !ungated.test(location.pathname);
  if (gated && onboarding.isPending)
    return (
      <div className="hf-app hf-center">
        <Loading />
      </div>
    );
  if (gated && onboarding.error)
    return <GateError retry={() => onboarding.refetch()} />;
  if (gated && !onboarding.data?.complete)
    return (
      <SetupGate
        key={selected.id + location.pathname}
        check={onboarding.refetch}
      />
    );
  const context = {
    ...selected,
    user: me.data.user,
    notify: (text: string, error = false) => setToast({ text, error }),
    ask: (c: AskContext = {}) =>
      setAskContext({ ...c, page: location.pathname }),
  };
  if (inSetup)
    return (
      <WorkspaceContext.Provider value={context}>
        <div className="app-shell is-setup">
          <div className="main-shell">
            <main key={selected.id}>
              <Suspense fallback={<Loading />}>
                <Onboarding />
              </Suspense>
            </main>
          </div>
        </div>
        {toast && (
          <div
            role={toast.error ? "alert" : "status"}
            className={"toast " + (toast.error ? "error" : "")}
          >
            <span>{toast.text}</span>
            <button aria-label="Dismiss" onClick={() => setToast(null)}>
              <X size={16} />
            </button>
          </div>
        )}
      </WorkspaceContext.Provider>
    );
  return (
    <WorkspaceContext.Provider value={context}>
      <div className="hf-app">
        <LiveActivityProvider workspaceId={selected.id}>
          {/* Keyed by workspace: switching ends voice and its microphone. */}
          <VoiceProvider key={selected.id} workspaceId={selected.id}>
            <Shell
              workspaces={me.data.workspaces}
              current={selected}
              user={me.data.user}
              onSwitch={setChosen}
            >
              <RouteErrorBoundary resetKey={location.pathname}>
                <Suspense fallback={<RouteFallback />}>
                  <div key={selected.id} className="hf-route">
                    <WorkspaceRoutes />
                  </div>
                </Suspense>
              </RouteErrorBoundary>
            </Shell>
          </VoiceProvider>
          {askContext && (
            <ChromeBoundary
              name="Ask panel"
              onError={() => {
                setAskContext(null);
                setToast({ text: "The Ask panel hit a problem.", error: true });
              }}
            >
              <Suspense fallback={null}>
                <AnalystPanel
                  key={
                    selected.id +
                    ":" +
                    (askContext.resourceId || askContext.page)
                  }
                  context={askContext}
                  onClose={() => setAskContext(null)}
                />
              </Suspense>
            </ChromeBoundary>
          )}
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
        </LiveActivityProvider>
      </div>
    </WorkspaceContext.Provider>
  );
}
const client = new QueryClient({
  defaultOptions: { queries: { staleTime: 5000, retry: 1 } },
});
createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={client}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </React.StrictMode>,
);
