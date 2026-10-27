import { Component, type ReactNode } from "react";
import { Compass, RefreshCw, TriangleAlert } from "lucide-react";
import { Button, ButtonLink, EmptyState, Page } from "../kit";
import { useShell } from "./context";

// A lazy chunk from an older deploy is gone; reloading fetches the new build.
const staleChunk = (e: Error) =>
  /dynamically imported module|Importing a module script failed|ChunkLoadError/i.test(
    e.message,
  );

// Catches render errors in one route so the shell stays usable.
export class RouteErrorBoundary extends Component<
  { resetKey: string; children: ReactNode },
  { error: Error | null }
> {
  state = { error: null as Error | null };
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  componentDidUpdate(previous: { resetKey: string }) {
    if (previous.resetKey !== this.props.resetKey && this.state.error)
      this.setState({ error: null });
  }
  componentDidCatch(error: Error) {
    console.error(error);
  }
  render() {
    const { error } = this.state;
    if (!error) return this.props.children;
    const stale = staleChunk(error);
    return (
      <Page width="narrow">
        <div role="alert">
          <EmptyState
            size="lg"
            icon={TriangleAlert}
            title={
              stale ? "A new version is available" : "This page hit a problem"
            }
            text={
              stale
                ? "Reload to get the latest version of Helix Foundry."
                : error.message ||
                  "Something went wrong while showing this page."
            }
            action={
              <>
                <Button
                  variant="primary"
                  icon={RefreshCw}
                  onClick={() =>
                    stale
                      ? window.location.reload()
                      : this.setState({ error: null })
                  }
                >
                  {stale ? "Reload" : "Try again"}
                </Button>
                <ButtonLink to="/">Go home</ButtonLink>
              </>
            }
          />
        </div>
      </Page>
    );
  }
}

// Keeps one piece of shell chrome (sidebar, palette, tray, Ask panel) from
// blanking the whole app when it throws; the fallback replaces only that piece.
export class ChromeBoundary extends Component<
  {
    name: string;
    fallback?: ReactNode;
    onError?: (error: Error) => void;
    children: ReactNode;
  },
  { failed: boolean }
> {
  state = { failed: false };
  static getDerivedStateFromError() {
    return { failed: true };
  }
  componentDidCatch(error: Error) {
    console.error(`${this.props.name} failed:`, error);
    this.props.onError?.(error);
  }
  render() {
    return this.state.failed
      ? (this.props.fallback ?? null)
      : this.props.children;
  }
}

export function NotFound() {
  const shell = useShell();
  return (
    <Page width="narrow">
      <h1 className="hf-sr-only">Page not found</h1>
      <EmptyState
        size="lg"
        icon={Compass}
        title="We couldn’t find that page"
        text={
          <>
            Nothing lives at <code>{window.location.pathname}</code>. It may
            have moved, or the link may be out of date.
          </>
        }
        action={
          <>
            <ButtonLink to="/" variant="primary">
              Go home
            </ButtonLink>
            <Button onClick={() => shell.openPalette()}>Search</Button>
          </>
        }
      />
    </Page>
  );
}
