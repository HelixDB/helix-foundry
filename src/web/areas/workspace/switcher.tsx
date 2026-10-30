import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { ChevronsUpDown, LayoutGrid, Plus } from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import {
  Button,
  Dialog,
  DropdownMenu,
  MenuItem,
  MenuLabel,
  MenuSeparator,
} from "../../kit";
import { trackEvent } from "../../analytics";
import { paths } from "../../paths";
import type { ShellWorkspace } from "../../ui";
import { createWorkspace, slugify } from "../../workspaces";
import { initials } from "./people";

// Creates a workspace in the stamp's default region and opens it.
export function CreateWorkspaceDialog({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (id: string) => void;
}) {
  const [name, setName] = useState(""),
    [error, setError] = useState(""),
    [pending, setPending] = useState(false);
  const qc = useQueryClient(),
    slug = slugify(name);
  return (
    <Dialog
      title="Create a workspace"
      description={
        LOCAL
          ? "A new workspace starts with its own data and ontology."
          : "A new workspace starts with its own data, ontology and team."
      }
      size="sm"
      onClose={onClose}
    >
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          setPending(true);
          setError("");
          try {
            const w = await createWorkspace(name);
            trackEvent("workspace_created");
            await qc.invalidateQueries({ queryKey: ["whoami"] });
            await qc.invalidateQueries({ queryKey: ["workspaces"] });
            onCreated(w.id);
          } catch (err) {
            setError((err as Error).message);
          } finally {
            setPending(false);
          }
        }}
      >
        <label className="hf-field">
          <span>Workspace name</span>
          <input
            required
            data-autofocus
            maxLength={100}
            placeholder="e.g. Acme"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          {name.trim() && (
            <small>
              {slug ? (
                <>
                  Its address will be <code>{slug}</code>, with a short suffix
                  if that’s taken.
                </>
              ) : (
                "Use at least one letter or number."
              )}
            </small>
          )}
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
            disabled={!slug}
          >
            Create workspace
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

// The sidebar's workspace menu: switch, see them all, or create one.
export function WorkspaceSwitcher({
  workspaces,
  current,
  onSwitch,
}: {
  workspaces: ShellWorkspace[];
  current: ShellWorkspace;
  onSwitch: (id: string) => void;
}) {
  const [creating, setCreating] = useState(false),
    navigate = useNavigate();
  return (
    <>
      <DropdownMenu
        label={`Workspace: ${current.name}`}
        className="hf-switcher"
        triggerClassName="hf-switcher-trigger"
        trigger={
          <>
            <span className="hf-ws-mark" aria-hidden>
              {initials(current.name)}
            </span>
            <span className="hf-switcher-text">
              <strong>{current.name}</strong>
            </span>
            <ChevronsUpDown
              size={14}
              className="hf-switcher-chevron"
              aria-hidden
            />
          </>
        }
      >
        <MenuLabel>Workspaces</MenuLabel>
        {workspaces.map((w) => (
          <MenuItem
            key={w.id}
            checked={w.id === current.id}
            onSelect={() => {
              if (w.id === current.id) return;
              onSwitch(w.id);
              navigate(paths.home());
            }}
          >
            {w.name}
          </MenuItem>
        ))}
        <MenuSeparator />
        <MenuItem icon={LayoutGrid} to={paths.workspaces()}>
          All workspaces
        </MenuItem>
        <MenuItem icon={Plus} onSelect={() => setCreating(true)}>
          Create workspace
        </MenuItem>
      </DropdownMenu>
      {creating && (
        <CreateWorkspaceDialog
          onClose={() => setCreating(false)}
          onCreated={(id) => {
            setCreating(false);
            onSwitch(id);
            navigate(paths.home());
          }}
        />
      )}
    </>
  );
}
