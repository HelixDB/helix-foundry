import { useState } from "react";
import type { AutomationSpec, FoundryAutomation } from "../../../shared/wfe";
import { sentence as asSentence } from "../../api";
import { Button, Dialog } from "../../kit";
import { useWorkspace } from "../../ui";
import {
  automationRefusal,
  canonicalJson,
  parseSpec,
  sameSteps,
  sentence,
  type TemplateId,
} from "./model";
import { StepsPreview } from "./parts";
import {
  approveAutomation,
  createAutomation,
  updateAutomation,
  type useNames,
} from "./queries";

// What its owner approves before anything runs unattended: the review a
// save opens (the automation in a sentence, every step as it will run, as
// them) and the approval the Live switch opens for steps saved without it.
// What they read is what runs: the approval names the revision they read.

export type Draft = { name: string; description: string; spec: AutomationSpec };
type Lists = ReturnType<typeof useNames>;

/**
 * What a save sends to change `base` into `draft`: its name, description and
 * spec where they differ, the spec compared as the WFE writes it; undefined
 * when nothing differs, which sends nothing (the WFE refuses an edit that
 * changes nothing).
 */
export function changesOf(
  base: FoundryAutomation,
  draft: Draft,
): { name?: string; description?: string; specJson?: string } | undefined {
  const before = parseSpec(base.specJson || ""),
    specJson = canonicalJson(draft.spec),
    name = draft.name.trim(),
    description = draft.description.trim();
  const changes = {
    ...(name !== base.name && { name }),
    ...(description !== (base.description || "") && { description }),
    ...((!before.ok || canonicalJson(before.spec) !== specJson) && {
      specJson,
    }),
  };
  return Object.keys(changes).length ? changes : undefined;
}

/** A save's refusal in words. */
export const saveRefusal = (e: unknown) =>
  automationRefusal(e) === "FOUNDRY_AUTOMATION_CHANGED"
    ? "It was changed elsewhere since you opened it. Discard your changes to see it as it is now, then make them again."
    : automationRefusal(e) === "FOUNDRY_AUTOMATION_LIMIT"
      ? "This workspace has as many automations as it may keep (50). Delete one first."
      : asSentence((e as Error).message);

/**
 * What the owner approves: the automation in a sentence and every step as
 * it will run unattended, as them. Save and turn on approves it; Save only
 * keeps it waiting for approval. An edit that only moved steps keeps the
 * approval it had; one that changes nothing only approves.
 */
export function ReviewDialog({
  draft,
  base,
  token,
  onBase,
  template,
  lists,
  email,
  onClose,
  onSaved,
}: {
  draft: Draft;
  // The automation as the save edits it; none for a new one.
  base?: FoundryAutomation;
  // A new one's create token: a retried create is the same automation.
  token: string;
  // A step of the save went through: the automation it made or changed.
  onBase: (a: FoundryAutomation) => void;
  template: TemplateId;
  lists: Lists;
  email: boolean;
  onClose: () => void;
  onSaved: (a: FoundryAutomation) => void;
}) {
  const { id: ws } = useWorkspace();
  const [pending, setPending] = useState<"on" | "save" | null>(null),
    [error, setError] = useState("");
  const save = async (turnOn: boolean) => {
    setPending(turnOn ? "on" : "save");
    setError("");
    try {
      const changes = base && changesOf(base, draft);
      let a = !base
        ? await createAutomation(
            ws,
            {
              name: draft.name.trim(),
              description: draft.description.trim(),
              specJson: canonicalJson(draft.spec),
              clientToken: token,
            },
            template,
          )
        : changes
          ? await updateAutomation(ws, base.id, {
              expectedRevision: base.revision || "",
              ...changes,
            })
          : base;
      onBase(a);
      if (turnOn && a.status === "needs_approval")
        a = await approveAutomation(ws, a.id, a.revision || "");
      onSaved(a);
    } catch (e) {
      setError(saveRefusal(e));
    } finally {
      setPending(null);
    }
  };
  // Only moved, renamed or described again, it keeps the approval it had.
  const before = base && parseSpec(base.specJson || "");
  const approved =
    !!before?.ok &&
    sameSteps(before.spec, draft.spec) &&
    base?.status !== "needs_approval";
  return (
    <Dialog
      title="Review what runs unattended"
      size="lg"
      onClose={onClose}
      footer={
        approved ? (
          <>
            <Button onClick={onClose}>Back</Button>
            <Button
              variant="primary"
              pending={pending === "save"}
              disabled={!!pending}
              onClick={() => void save(false)}
            >
              Save
            </Button>
          </>
        ) : (
          <>
            <Button onClick={onClose}>Back</Button>
            <Button
              pending={pending === "save"}
              disabled={!!pending}
              onClick={() => void save(false)}
            >
              Save, turn on later
            </Button>
            <Button
              variant="primary"
              pending={pending === "on"}
              disabled={!!pending}
              onClick={() => void save(true)}
            >
              Save and turn on
            </Button>
          </>
        )
      }
    >
      <div className="au-review">
        <p className="au-sentence">
          {sentence(draft.spec, lists.names, email)}
        </p>
        <StepsPreview
          spec={draft.spec}
          names={lists.names}
          email={email}
          label="Steps that run unattended"
        />
        <p className="au-review-note">
          {approved
            ? "Its steps are the ones you approved: saving keeps it as it is."
            : "It runs as you, with what you may do when it runs. Turning it on approves exactly these steps; any change to them waits for your approval again. Its Assistant steps spend the workspace’s automations budget."}
        </p>
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
      </div>
    </Dialog>
  );
}

/**
 * The approval the Live switch asks for: the saved steps, as they will run
 * unattended, and Approve and go live, which names the revision read.
 */
export function ApproveDialog({
  automation,
  spec,
  lists,
  email,
  onClose,
  onApproved,
}: {
  automation: FoundryAutomation;
  spec: AutomationSpec;
  lists: Lists;
  email: boolean;
  onClose: () => void;
  onApproved: (a: FoundryAutomation) => void;
}) {
  const { id: ws } = useWorkspace();
  const [pending, setPending] = useState(false),
    [error, setError] = useState("");
  return (
    <Dialog
      title="Approve what runs unattended"
      size="lg"
      onClose={onClose}
      footer={
        <>
          <Button onClick={onClose}>Back</Button>
          <Button
            variant="primary"
            pending={pending}
            onClick={async () => {
              setPending(true);
              setError("");
              try {
                onApproved(
                  await approveAutomation(
                    ws,
                    automation.id,
                    automation.revision || "",
                  ),
                );
              } catch (e) {
                setError(saveRefusal(e));
              } finally {
                setPending(false);
              }
            }}
          >
            Approve and go live
          </Button>
        </>
      }
    >
      <div className="au-review">
        <p className="au-sentence">
          {sentence(spec, lists.names, email)}
        </p>
        <StepsPreview
          spec={spec}
          names={lists.names}
          email={email}
          label="Steps to approve"
        />
        <p className="au-review-note">
          It runs as you, with what you may do when it runs. Approving turns it
          on with exactly these steps; any change to them waits for your
          approval again.
        </p>
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
      </div>
    </Dialog>
  );
}
