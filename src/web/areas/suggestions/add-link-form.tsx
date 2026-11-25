// The version page's Edit tab, opened from a finding's Add this link
// (?fill=<fingerprint>&link=<name>&set=<set id>&describe=<text>): a form for
// the link's ends and description, whose save stages the link and its route
// (addLinkEdits) and saves them as a new version of this one; making that
// version current with the finding moves the current load to it in place
// (AddLinkSaved).
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { Link2 } from "lucide-react";
import type * as wfe from "../../../shared/wfe";
import { Button, Select } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import type { AgentBundle } from "../ontology/model";
import { setCurrentVersion } from "../ontology/queries";
import { useOntology } from "../ontology/scope";
import { addLinkEdits } from "./add-link";

/** What a card asks the Edit tab to add, read from its URL. */
export type AddLinkAsk = {
  fingerprint: string;
  link: string;
  setId: string;
  describe: string;
};

/**
 * The ask a URL's search holds, or none: it needs a fingerprint, a link and
 * a set.
 *
 * @example
 * addLinkAsk(new URLSearchParams("fill=ab&link=ceo_of&set=set_1")); // { fingerprint: "ab", link: "ceo_of", setId: "set_1", describe: "" }
 */
export function addLinkAsk(params: URLSearchParams): AddLinkAsk | undefined {
  const fingerprint = params.get("fill") || "",
    link = params.get("link") || "",
    setId = params.get("set") || "";
  if (!fingerprint || !link || !setId) return undefined;
  return { fingerprint, link, setId, describe: params.get("describe") || "" };
}

/** The form: the link's ends and description, then Save as a new version. */
export function AddLinkForm({
  bundle,
  ask,
  setName,
  busy,
  onSave,
}: {
  bundle: AgentBundle;
  ask: AddLinkAsk;
  // The set's name, as the workspace has it.
  setName: string;
  busy: boolean;
  onSave: (edits: wfe.OntologyEdit[]) => void;
}) {
  const types = bundle.ontology.ok
    ? bundle.ontology.value.types.map((t) => t.name)
    : [];
  const [from, setFrom] = useState(""),
    [to, setTo] = useState(""),
    [description, setDescription] = useState(ask.describe);
  const [refused, setRefused] = useState("");
  const save = () => {
    const got = addLinkEdits(bundle, {
      link: ask.link,
      setId: ask.setId,
      from,
      to,
      description,
    });
    if (got.kind === "refused") return setRefused(got.why);
    setRefused("");
    onSave(got.edits);
  };
  const options = types.map((t) => ({ value: t, label: t }));
  return (
    <section className="on-stack" aria-label="Add this link">
      <h3>Add the link “{ask.link}”</h3>
      <p>
        Documents of {setName || "this set"} state it. Saving makes a new
        version with the link and the route that reads them; making it current
        updates the current load in place, with no new load.
      </p>
      <label className="hf-field">
        <span>From</span>
        <Select
          label="From"
          value={from}
          onChange={setFrom}
          options={options}
        />
      </label>
      <label className="hf-field">
        <span>To</span>
        <Select label="To" value={to} onChange={setTo} options={options} />
      </label>
      <label className="hf-field">
        <span>Description</span>
        <textarea
          rows={3}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
      </label>
      {refused && (
        <p role="alert" className="hf-form-error">
          {refused}
        </p>
      )}
      <div className="hf-form-actions">
        <Button variant="primary" icon={Link2} disabled={busy} onClick={save}>
          Save as a new version
        </Button>
      </div>
    </section>
  );
}

/**
 * A saved version that adds a finding's link: Make current and update the
 * load, which makes it current with the finding (fillFinding) so the current
 * load moves to it in place, then shows the finding's card.
 */
export function AddLinkSaved({
  versionId,
  version,
  fingerprint,
}: {
  versionId: string;
  version?: number;
  fingerprint: string;
}) {
  const { id: ws, notify } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    qc = useQueryClient(),
    navigate = useNavigate();
  const [pending, setPending] = useState(false);
  const makeCurrent = async () => {
    setPending(true);
    try {
      await setCurrentVersion(ws, o, {
        versionId,
        expectedCurrentVersionId: ontology.currentVersionId || "",
        fillFinding: fingerprint,
      });
      await qc.invalidateQueries({ queryKey: [ws, "ontology", o] });
      navigate(paths.review(o, "ontology"));
    } catch (e) {
      const message = (e as Error).message;
      notify(message.charAt(0).toUpperCase() + message.slice(1), true);
      await qc.invalidateQueries({ queryKey: [ws, "ontology", o] });
    } finally {
      setPending(false);
    }
  };
  return (
    <div className="on-stack">
      <p role="status">Version {version ?? "?"} saved with the link.</p>
      <div className="hf-form-actions">
        <Button
          variant="primary"
          pending={pending}
          onClick={() => void makeCurrent()}
        >
          Make current and update the load
        </Button>
      </div>
    </div>
  );
}
