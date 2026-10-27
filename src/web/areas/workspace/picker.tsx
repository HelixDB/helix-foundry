import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { ChevronRight, Plus } from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import { Button } from "../../kit";
import { GatePage } from "../../sign-in";
import type { ShellWorkspace } from "../../ui";
import { initials, roleLabel } from "./people";
import { CreateWorkspaceDialog } from "./switcher";

// Choose a workspace: shown at /workspaces, and whenever the browser has not
// chosen one yet among several. With none, it offers to create the first.
export function WorkspacePicker({
  workspaces,
  email,
  onChoose,
  next = "/",
}: {
  workspaces: ShellWorkspace[];
  // The signed-in user's, shown with Sign out.
  email: string;
  onChoose: (id: string) => void;
  // Where to go once chosen.
  next?: string;
}) {
  const [creating, setCreating] = useState(false),
    navigate = useNavigate();
  const choose = (id: string) => {
    onChoose(id);
    navigate(next, { replace: true });
  };
  const none = !workspaces.length;
  return (
    <GatePage
      wide
      account={email}
      title={none ? "Create your first workspace" : "Choose a workspace"}
      lede={
        none
          ? LOCAL
            ? "A workspace holds your connected data and its ontology."
            : "A workspace holds your connected data, its ontology and your team."
          : LOCAL
            ? "Each workspace keeps its own data and ontologies."
            : "Your workspaces are shared with the Helix dashboard."
      }
    >
      <div className="hf-gate-body">
        {!none && (
          <ul className="hf-ws-list" aria-label="Workspaces">
            {workspaces.map((w) => (
              <li key={w.id}>
                <button
                  type="button"
                  className="hf-ws-option"
                  onClick={() => choose(w.id)}
                >
                  <span className="hf-ws-mark" aria-hidden>
                    {initials(w.name)}
                  </span>
                  <span className="hf-ws-option-text">
                    <strong>{w.name}</strong>
                    <small>
                      {w.slug} · {roleLabel[w.role]}
                    </small>
                  </span>
                  <ChevronRight size={16} aria-hidden />
                </button>
              </li>
            ))}
          </ul>
        )}
        <Button
          size="lg"
          variant={none ? "primary" : "secondary"}
          icon={Plus}
          onClick={() => setCreating(true)}
        >
          Create workspace
        </Button>
      </div>
      {creating && (
        <CreateWorkspaceDialog
          onClose={() => setCreating(false)}
          onCreated={(id) => {
            setCreating(false);
            choose(id);
          }}
        />
      )}
    </GatePage>
  );
}
