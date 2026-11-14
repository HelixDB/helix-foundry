import "./dashboards.css";
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { LayoutDashboard } from "lucide-react";
import type { FoundryDashboard } from "../../../shared/wfe";
import { Button, ButtonLink, Dialog, Select, SkeletonText } from "../../kit";
import { useWorkspace } from "../../ui";
import { MAX_NAME, dashboardName, dashboardRefusal } from "./model";
import { dashboardPaths as paths } from "./paths";
import {
  addQuestionVisual,
  createDashboard,
  dashboardQueries,
  keepDashboard,
} from "./queries";

// An Analyst answer's visual pinned to a dashboard: its query, kind and
// title as a tile of its own row, last, on a dashboard the person may change
// or a new one. The tile reads whichever load is current when it is viewed,
// or, asked, the load the answer read. A dashboard changed meanwhile is
// read again and the tile added to it as it is now: adding a tile changes
// nothing else.

const NEW = "__new__";

/** The answer's question and the result its visual shows. */
type Visual = { ontologyId: string; questionId: string; evidenceId: string };

/** "Pin to dashboard", in a visual's head; nothing for someone who can't make or change one. */
export function PinVisual(visual: Visual & { title: string }) {
  const { id: ws, grantee } = useWorkspace();
  const [open, setOpen] = useState(false);
  if (grantee) return null;
  return (
    <>
      <Button
        size="sm"
        variant="ghost"
        icon={LayoutDashboard}
        onClick={() => setOpen(true)}
      >
        Pin to dashboard
      </Button>
      {open && (
        <PinDialog ws={ws} visual={visual} onClose={() => setOpen(false)} />
      )}
    </>
  );
}

function PinDialog({
  ws,
  visual,
  onClose,
}: {
  ws: string;
  visual: Visual & { title: string };
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const list = useQuery(dashboardQueries.list(ws));
  const editable = (list.data?.dashboards || []).filter((d) => d.canEdit);
  const [chosen, setChosen] = useState(""),
    [name, setName] = useState(""),
    [pinLoad, setPinLoad] = useState(false),
    [pending, setPending] = useState(false),
    [error, setError] = useState(""),
    [done, setDone] = useState<FoundryDashboard | null>(null),
    [token] = useState(() => crypto.randomUUID());
  // The newest dashboard the person may change, else a new one.
  const target = chosen || editable[0]?.id || NEW,
    making = target === NEW,
    newName = dashboardName(name);
  const add = async () => {
    setPending(true);
    setError("");
    const body = {
      ontologyId: visual.ontologyId,
      questionId: visual.questionId,
      evidenceId: visual.evidenceId,
      ...(pinLoad ? { pinLoad } : {}),
    };
    try {
      const d = making
        ? await createDashboard(
            ws,
            { name: newName || "", clientToken: token },
            "analyst",
          )
        : editable.find((x) => x.id === target)!;
      const added = await addQuestionVisual(ws, d.id, {
        ...body,
        expectedRevision: d.revision || "",
      }).catch(async (e: unknown) => {
        if (dashboardRefusal(e) !== "FOUNDRY_DASHBOARD_CHANGED") throw e;
        // Changed meanwhile: add it to the dashboard as it is now.
        const now = await qc.fetchQuery(dashboardQueries.dashboard(ws, d.id));
        return addQuestionVisual(ws, d.id, {
          ...body,
          expectedRevision: now.dashboard.revision || "",
        });
      });
      keepDashboard(qc, ws, added);
      setDone(added);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPending(false);
    }
  };
  if (done)
    return (
      <Dialog title="Pinned to a dashboard" size="sm" onClose={onClose}>
        <p className="db-share-note">
          “{visual.title}” is a tile of {done.name} now.
        </p>
        <div className="hf-form-actions">
          <Button onClick={onClose}>Done</Button>
          <ButtonLink
            variant="primary"
            to={paths.dashboard(done.id)}
            onClick={onClose}
          >
            Open {done.name}
          </ButtonLink>
        </div>
      </Dialog>
    );
  return (
    <Dialog
      title="Pin to a dashboard"
      description={`“${visual.title}” becomes a tile of its own, last on the dashboard.`}
      size="sm"
      onClose={onClose}
    >
      {list.isPending ? (
        <SkeletonText lines={3} />
      ) : (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void add();
          }}
        >
          <div className="hf-field">
            <span>Dashboard</span>
            <Select
              label="Dashboard"
              value={target}
              onChange={setChosen}
              autoFocus
              options={[
                ...editable.map((d) => ({
                  value: d.id,
                  label: d.name || d.id,
                  ...(d.pinned ? { hint: "Pinned" } : {}),
                })),
                { value: NEW, label: "New dashboard…" },
              ]}
            />
          </div>
          {making && (
            <label className="hf-field">
              <span>Name of the new dashboard</span>
              <input
                required
                maxLength={MAX_NAME}
                placeholder="e.g. Invoices"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </label>
          )}
          <label className="db-check">
            <input
              type="checkbox"
              checked={pinLoad}
              onChange={(e) => setPinLoad(e.target.checked)}
            />
            <span>
              Keep showing the load this answer read
              <small>
                Otherwise the tile reads whichever load is current when the
                dashboard is opened.
              </small>
            </span>
          </label>
          {error && (
            <p role="alert" className="hf-form-error">
              {error}
            </p>
          )}
          <div className="hf-form-actions">
            <Button onClick={onClose}>Cancel</Button>
            <Button
              type="submit"
              variant="primary"
              pending={pending}
              disabled={making && !newName}
            >
              Pin
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}
