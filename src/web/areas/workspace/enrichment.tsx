import { useState } from "react";
import {
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { LoaderCircle } from "lucide-react";
import type { FoundryEnrichment } from "../../../shared/wfe";
import {
  Button,
  ConfirmDialog,
  Panel,
  SkeletonText,
  formatNumber,
  plural,
} from "../../kit";
import { useWorkspace } from "../../ui";
import {
  CAP_MAX,
  DEFAULT_CAP,
  ENRICHMENT_NOTICE,
  ENRICHMENT_NOTICE_VERSION,
  enrichmentCredits,
  enrichmentKey,
  enrichmentQuery,
  enrichmentStatusText,
  planUnknown,
  updateEnrichment,
} from "./enrichment-api";
import { membersQuery } from "./members";
import { PLAN_PRICE } from "./plan";
import { ProGate } from "./pro-gate";

// Settings › People enrichment: the owner allows
// Helix to look up the public work profiles of the people in the
// workspace's tables on context.dev, under a monthly credit cap, after
// reading what is sent and stored. Admins and members see the setting.
// Turning it off deletes the stored profiles, and Helix reloads each
// ontology that applied some, without them.

export function EnrichmentTab() {
  const { id, role, name } = useWorkspace();
  const query = useQuery(enrichmentQuery(id));
  // Who allowed it, by name: the first page of Settings › Members.
  const members = useInfiniteQuery(membersQuery(id));
  const e = query.data,
    owner = role === "owner";
  if (query.isPending)
    return (
      <Panel title="People enrichment">
        <SkeletonText lines={3} />
      </Panel>
    );
  if (!e)
    return (
      <Panel title="People enrichment">
        <p role="alert" className="hf-form-error">
          Couldn’t read the setting. {query.error?.message}
        </p>
        <Button size="sm" onClick={() => void query.refetch()}>
          Retry
        </Button>
      </Panel>
    );
  const allowedBy = members.data?.pages
    .flatMap((p) => p.members || [])
    .find((m) => m.user?.id === e.enabledBy)?.user;
  const proRequired =
    e.availability === "FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED";
  return (
    <>
      {proRequired && (
        <ProGate
          feature="People enrichment"
          title="People enrichment needs Foundry Pro"
          pitch={`Pro looks up the public work profiles of the people in ${name}’s tables, for ${PLAN_PRICE.pro} a month.`}
        />
      )}
      <Panel
        title="People enrichment"
        subtitle="Look up public work profiles of the people in your tables on context.dev: their current title, location and work history."
        padded={false}
      >
        <div className="wk-pad">
          {e.availability === "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED" && (
            <p className="wk-note">
              Helix has paused lookups for now. Loads still apply profiles found
              before.
            </p>
          )}
          {owner && planUnknown(e) && (
            <p className="wk-note">
              Helix can’t read this workspace’s plan just now. Try again later
              to allow People enrichment or change its cap; turning it off still
              works.
            </p>
          )}
          {owner ? (
            // Keyed by what was saved: a save starts the form afresh.
            <OwnerForm
              key={`${e.enabled}:${e.monthlyCreditCap}`}
              enrichment={e}
              allowedBy={allowedBy?.displayName || allowedBy?.email}
              // Off Pro, while Helix has lookups off or can't read the
              // plan, the WFE refuses turning it on and a new cap; turning
              // it off still works.
              locked={
                e.availability !== "FOUNDRY_ENRICHMENT_AVAILABILITY_AVAILABLE"
              }
            />
          ) : (
            <p className="wk-note">{enrichmentStatusText(e, false)}</p>
          )}
          {!!e.purgesPending && (
            <p role="status" className="wk-plan-confirming">
              <LoaderCircle size={14} className="hf-spin" aria-hidden />
              Removing enriched values:{" "}
              {plural(e.purgesPending, "ontology", "ontologies")} left
            </p>
          )}
        </div>
      </Panel>
    </>
  );
}

/** The owner's switch, notice, cap and save. */
function OwnerForm({
  enrichment: e,
  allowedBy,
  locked,
}: {
  enrichment: FoundryEnrichment;
  allowedBy?: string;
  locked: boolean;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const credits = enrichmentCredits(e);
  const [on, setOn] = useState(!!e.enabled),
    [lawful, setLawful] = useState(false),
    // The default only before an owner set a cap: 0 while on is a cap.
    [cap, setCap] = useState(
      String(e.enabled || credits.cap ? credits.cap : DEFAULT_CAP),
    ),
    [confirmOff, setConfirmOff] = useState(false),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const capNumber = Number(cap),
    capValid =
      cap.trim() !== "" &&
      Number.isInteger(capNumber) &&
      capNumber >= 0 &&
      capNumber <= CAP_MAX;
  const save = async (enabled: boolean) => {
    setPending(true);
    setError("");
    try {
      // A new cap keeps it on, so the setting is read first: a page that
      // still shows it on can't turn it back on after another owner turned
      // it off, without the notice and the lawful basis.
      if (enabled && e.enabled) {
        const now = await enrichmentQuery(id).queryFn();
        if (!now.enabled) {
          qc.setQueryData(enrichmentKey(id), now);
          notify(
            "People enrichment was turned off meanwhile, so the cap wasn’t saved.",
            true,
          );
          return;
        }
      }
      const next = await updateEnrichment(id, {
        enabled,
        monthlyCreditCap: String(enabled ? capNumber : credits.cap),
        // The WFE keeps every change with the notice shown, off included.
        noticeVersion: ENRICHMENT_NOTICE_VERSION,
      });
      qc.setQueryData(enrichmentKey(id), next);
      notify(
        !enabled
          ? "People enrichment is off"
          : e.enabled
            ? `The cap is ${formatNumber(capNumber)} credits a month`
            : "People enrichment is on",
      );
      setConfirmOff(false);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setPending(false);
    }
  };
  const status = enrichmentStatusText(e, true, allowedBy);
  return (
    <>
      <label className="checkbox">
        <input
          type="checkbox"
          role="switch"
          checked={on}
          disabled={pending || (locked && !e.enabled)}
          onChange={(ev) => {
            setError("");
            if (ev.target.checked) setOn(true);
            else if (e.enabled) setConfirmOff(true);
            else setOn(false);
          }}
        />
        Allow People enrichment
      </label>
      {status && <p className="wk-note">{status}</p>}
      {on && !e.enabled && !locked && (
        <>
          <p className="wk-note">{ENRICHMENT_NOTICE}</p>
          <label className="checkbox">
            <input
              type="checkbox"
              checked={lawful}
              disabled={pending}
              onChange={(ev) => setLawful(ev.target.checked)}
            />
            I confirm we have a lawful basis to do this.
          </label>
        </>
      )}
      {on && !locked && (
        <>
          <label className="hf-field">
            <span>Monthly credit cap</span>
            <input
              type="number"
              inputMode="numeric"
              min={0}
              max={CAP_MAX}
              step={1}
              value={cap}
              disabled={pending}
              aria-invalid={!capValid}
              onChange={(ev) => setCap(ev.target.value)}
            />
            <small>
              {capValid
                ? `At most ${formatNumber(CAP_MAX)}. A profile found costs 20 credits.`
                : `Enter a whole number from 0 to ${formatNumber(CAP_MAX)}.`}
            </small>
          </label>
          <div className="hf-form-actions">
            <Button
              variant="primary"
              pending={pending}
              disabled={
                !capValid ||
                (!e.enabled && !lawful) ||
                (!!e.enabled && capNumber === credits.cap)
              }
              onClick={() => void save(true)}
            >
              {e.enabled ? "Save cap" : "Allow People enrichment"}
            </Button>
          </div>
        </>
      )}
      {/* While turning it off, the dialog says why it couldn't. */}
      {error && !confirmOff && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
      {confirmOff && (
        <ConfirmDialog
          title="Turn off People enrichment?"
          description="Lookups stop and stored profiles are deleted. Enriched values are removed as each ontology reloads; Helix starts those loads."
          confirmLabel="Turn off"
          tone="danger"
          pending={pending}
          onConfirm={() => save(false)}
          onCancel={() => {
            setConfirmOff(false);
            setError("");
          }}
        >
          {error && (
            <p role="alert" className="hf-form-error">
              {error}
            </p>
          )}
        </ConfirmDialog>
      )}
    </>
  );
}
