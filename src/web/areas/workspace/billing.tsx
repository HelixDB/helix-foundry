import { useEffect, useId, useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, CreditCard, Gem, LoaderCircle, Sparkles } from "lucide-react";
import type { FoundryPlan } from "../../../shared/wfe";
import {
  Button,
  ConfirmDialog,
  Dialog,
  Panel,
  SkeletonText,
  Tag,
  cx,
  formatDate,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { connectorKindsQuery } from "../data/data-api";
import {
  PAID_TIERS,
  PLAN_PRICE,
  TIER_NAME,
  billingPortalUrl,
  canManage,
  cancelPlan,
  checkoutFailure,
  forgetCheckout,
  pendingCheckout,
  planKey,
  planQuery,
  planSummary,
  rememberCheckout,
  resumePlan,
  startCheckout,
  type PaidTier,
  type PlanSummary,
  type Tier,
} from "./plan";

// Settings → Plan & billing: the workspace's Foundry plan (Free, Pro or Max)
// as Autumn reports it, buying Pro or Max through Stripe Checkout, switching
// between them, cancelling and resuming at the period end, and the hosted
// billing portal. Checkout returns here (the WFE's return URL is
// /settings/billing), and the page keeps reading the plan until the plan it
// bought shows.

// How long the page keeps reading the plan after a checkout.
const CONFIRM_MS = 60_000;

// Every plan has every connector; paid plans sync more often (the WFE's
// plan rules: Free daily, Pro hourly reading only what changed, Max every 15
// minutes).
const features: Record<Tier, string[]> = {
  free: [
    "Every connector, PostgreSQL, MySQL, S3 and API sources included",
    "Daily syncs and synced tables",
    "Ontology generation and versions",
  ],
  pro: ["Everything in Free", "Hourly syncs that read only what changed"],
  max: ["Everything in Free", "Syncs every 15 minutes"],
};
const MAX_NOTE = "Max adds more soon.";
const tierIcon = { pro: Sparkles, max: Gem };

// After a checkout of `tier`: "checking" reads the plan every 2 s until it
// shows; "slow" when it has not after CONFIRM_MS (Back on the Stripe page, or
// a slow payment), with a way to check again.
type Confirm = { step: "idle" } | { step: "checking" | "slow"; tier: PaidTier };

// What a checkout left the page to do: read the plan, which a card on file
// was charged for (a plan) or may have been (an error message).
type Outcome = { tier: PaidTier } & (
  | { plan: FoundryPlan }
  | { unsure: string }
);

const day = (d: Date | null) => (d ? formatDate(d.toISOString(), false) : "");

/**
 * One purchase of `tier`, or a switch to it, with a new client token (callers
 * disable their button meanwhile). Null when the browser is leaving for
 * Stripe Checkout; throws when the BFF or the WFE refused, so nothing was
 * charged.
 */
async function purchase(
  workspaceId: string,
  tier: PaidTier,
  promoCode?: string,
): Promise<Outcome | null> {
  try {
    const result = await startCheckout({
      workspaceId,
      clientToken: crypto.randomUUID(),
      promoCode,
      tier,
    });
    rememberCheckout(workspaceId, tier);
    if (result.kind === "redirect") {
      window.location.assign(result.url);
      return null;
    }
    return { tier, plan: result.plan };
  } catch (err) {
    if (checkoutFailure(err) === "refused") throw err;
    rememberCheckout(workspaceId, tier);
    return { tier, unsure: (err as Error).message };
  }
}

function planText(s: PlanSummary): string {
  if (s.tier === "free")
    return "Connect any source, sync it daily and generate an ontology. Upgrade for faster syncs.";
  const end = day(s.periodEnd),
    name = TIER_NAME[s.tier];
  switch (s.state) {
    case "renewing":
      return end ? `Renews on ${end}.` : "Renews every month.";
    case "ending":
      return end
        ? `${name} stays on until ${end}, then this workspace moves to Free.`
        : `${name} ends at the end of this period, then this workspace moves to Free.`;
    case "pastDue":
      return `The last invoice wasn’t paid. Update the payment method to keep ${name}.`;
  }
}

function PlanTag({ s }: { s: PlanSummary }) {
  if (s.tier === "free") return <Tag>Free</Tag>;
  return s.state === "renewing" ? (
    <Tag tone="success">Active</Tag>
  ) : s.state === "ending" ? (
    <Tag tone="warning">Ending</Tag>
  ) : (
    <Tag tone="danger">Payment due</Tag>
  );
}

// From Free: pick Pro or Max (helix-foundry's ChoiceCard radios), add a
// promotion code, and pay on Stripe Checkout.
function UpgradeDialog({
  initial,
  onClose,
  onDone,
}: {
  initial: PaidTier;
  onClose: () => void;
  onDone: (outcome: Outcome) => void;
}) {
  const { id, name } = useWorkspace();
  const ids = useId();
  const [tier, setTier] = useState(initial),
    [promo, setPromo] = useState(""),
    [error, setError] = useState(""),
    [pending, setPending] = useState(false);
  return (
    <Dialog
      title="Upgrade Foundry"
      description={`Pick a plan for ${name}. You’ll pay on a secure Stripe page.`}
      onClose={onClose}
    >
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          setPending(true);
          setError("");
          try {
            const outcome = await purchase(id, tier, promo);
            // Stays pending while the browser leaves for Stripe.
            if (outcome) onDone(outcome);
          } catch (err) {
            setError((err as Error).message);
            setPending(false);
          }
        }}
      >
        <fieldset className="plain-fieldset">
          <legend className="hf-sr-only">Plan</legend>
          <div className="choice-options wk-tier-options">
            {PAID_TIERS.map((t) => {
              const Icon = tierIcon[t];
              return (
                <label
                  key={t}
                  className={cx("choice-card", tier === t && "selected")}
                >
                  <span className="choice-card-icon" aria-hidden="true">
                    <Icon strokeWidth={1.5} />
                  </span>
                  <span className="choice-card-heading">
                    <input
                      type="radio"
                      name="tier"
                      value={t}
                      checked={tier === t}
                      disabled={pending}
                      onChange={() => setTier(t)}
                      aria-label={TIER_NAME[t]}
                      aria-describedby={`${ids}-${t}`}
                    />
                    <strong>{TIER_NAME[t]}</strong>
                  </span>
                  <span id={`${ids}-${t}`} className="choice-card-description">
                    {PLAN_PRICE[t]} a month
                  </span>
                </label>
              );
            })}
          </div>
        </fieldset>
        <ul className="wk-plan-features">
          {features[tier].slice(1).map((f) => (
            <li key={f}>
              <Check size={14} aria-hidden />
              {f}
            </li>
          ))}
        </ul>
        {tier === "max" && <p className="wk-note wk-tier-note">{MAX_NOTE}</p>}
        <label className="hf-field">
          <span>Promotion code</span>
          <input
            autoComplete="off"
            placeholder="Optional"
            value={promo}
            onChange={(e) => setPromo(e.target.value)}
          />
        </label>
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
        <div className="hf-form-actions">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" pending={pending}>
            Continue to payment
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

// From one paid plan to the other: the WFE attaches the other plan, which
// replaces the held one (one Autumn group).
function SwitchDialog({
  from,
  to,
  onClose,
  onDone,
}: {
  from: PaidTier;
  to: PaidTier;
  onClose: () => void;
  onDone: (outcome: Outcome) => void;
}) {
  const { id, name } = useWorkspace();
  const [error, setError] = useState(""),
    [pending, setPending] = useState(false);
  return (
    <ConfirmDialog
      title={`Switch to Foundry ${TIER_NAME[to]}?`}
      description={`${name} moves from ${TIER_NAME[from]} at ${PLAN_PRICE[from]} a month to ${TIER_NAME[to]} at ${PLAN_PRICE[to]} a month, starting now. PostgreSQL, MySQL and S3 stay on.`}
      confirmLabel={`Switch to ${TIER_NAME[to]}`}
      cancelLabel={`Keep ${TIER_NAME[from]}`}
      pending={pending}
      onConfirm={async () => {
        setPending(true);
        setError("");
        try {
          const outcome = await purchase(id, to);
          if (outcome) onDone(outcome);
        } catch (err) {
          setError((err as Error).message);
          setPending(false);
        }
      }}
      onCancel={onClose}
    >
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
    </ConfirmDialog>
  );
}

function PlanCard({
  tier,
  current,
  action,
}: {
  tier: Tier;
  current: boolean;
  action: ReactNode;
}) {
  return (
    <section
      className={cx("wk-plan-card", current && "is-current")}
      aria-label={`Foundry ${TIER_NAME[tier]}`}
    >
      <header>
        <h3>{TIER_NAME[tier]}</h3>
        {current && <Tag tone="accent">Current plan</Tag>}
      </header>
      <p className="wk-plan-price">
        <strong className="hf-num">{PLAN_PRICE[tier]}</strong>
        <span>per month</span>
      </p>
      <ul className="wk-plan-features">
        {features[tier].map((f) => (
          <li key={f}>
            <Check size={14} aria-hidden />
            {f}
          </li>
        ))}
      </ul>
      {tier === "max" && <p className="wk-note">{MAX_NOTE}</p>}
      {action && <div className="wk-plan-card-action">{action}</div>}
    </section>
  );
}

export function BillingTab() {
  const { id, name, role, notify } = useWorkspace();
  const qc = useQueryClient(),
    manager = canManage(role);
  const [confirm, setConfirm] = useState<Confirm>(() => {
      const tier = pendingCheckout(id);
      return tier ? { step: "checking", tier } : { step: "idle" };
    }),
    [upgrading, setUpgrading] = useState<PaidTier | null>(null),
    [switching, setSwitching] = useState<PaidTier | null>(null),
    [cancelling, setCancelling] = useState(false),
    [busy, setBusy] = useState<"cancel" | "resume" | "portal" | null>(null);
  const checking = confirm.step === "checking";
  const plan = useQuery({
    ...planQuery(id),
    ...(checking && { staleTime: 0, refetchInterval: 2000 }),
  });
  const s = planSummary(plan.data),
    known = !plan.isPending && !plan.error,
    held = TIER_NAME[s.tier];

  // Runs per state change, not per render, so the timer runs once.
  useEffect(() => {
    if (confirm.step === "idle") return;
    if (s.tier === confirm.tier) {
      forgetCheckout();
      setConfirm({ step: "idle" });
      notify(`${name} is on Foundry ${TIER_NAME[confirm.tier]}`);
      // A paid plan opens the database connectors: Data's picker reads them
      // afresh.
      void qc.invalidateQueries({ queryKey: connectorKindsQuery(id).queryKey });
      return;
    }
    if (confirm.step !== "checking") return;
    const timer = setTimeout(() => {
      forgetCheckout();
      setConfirm({ step: "slow", tier: confirm.tier });
    }, CONFIRM_MS);
    return () => clearTimeout(timer);
  }, [confirm, s.tier]);

  // Back from Stripe can restore this page from the back-forward cache, with
  // a checkout dialog still waiting: close it and read the plan.
  useEffect(() => {
    const restored = (e: PageTransitionEvent) => {
      if (!e.persisted) return;
      setUpgrading(null);
      setSwitching(null);
      const tier = pendingCheckout(id);
      if (tier) setConfirm({ step: "checking", tier });
    };
    window.addEventListener("pageshow", restored);
    return () => window.removeEventListener("pageshow", restored);
  }, [id]);

  const checkedOut = (outcome: Outcome) => {
    setUpgrading(null);
    setSwitching(null);
    if ("plan" in outcome) qc.setQueryData(planKey(id), outcome.plan);
    else notify(outcome.unsure, true);
    setConfirm({ step: "checking", tier: outcome.tier });
  };
  const change = async (
    which: "cancel" | "resume",
    message: (plan: FoundryPlan) => string,
  ) => {
    setBusy(which);
    try {
      const next = await (which === "cancel" ? cancelPlan : resumePlan)(id);
      qc.setQueryData(planKey(id), next);
      notify(message(next));
      setCancelling(false);
    } catch (e) {
      notify((e as Error).message, true);
    } finally {
      setBusy(null);
    }
  };
  const openPortal = async () => {
    setBusy("portal");
    try {
      window.location.assign(await billingPortalUrl(id));
    } catch (e) {
      notify((e as Error).message, true);
      setBusy(null);
    }
  };

  const action =
    !manager || !known ? null : s.tier === "free" ? (
      <Button
        variant="primary"
        icon={Sparkles}
        disabled={checking}
        onClick={() => setUpgrading("pro")}
      >
        Upgrade
      </Button>
    ) : s.state === "ending" ? (
      <Button
        variant="primary"
        pending={busy === "resume"}
        onClick={() =>
          void change("resume", () => `${name} will stay on Foundry ${held}`)
        }
      >
        Resume {held}
      </Button>
    ) : s.state === "pastDue" ? (
      <Button
        variant="primary"
        icon={CreditCard}
        pending={busy === "portal"}
        onClick={() => void openPortal()}
      >
        Update payment method
      </Button>
    ) : (
      <Button variant="ghost" onClick={() => setCancelling(true)}>
        Cancel {held}
      </Button>
    );

  // Each paid plan's card offers itself: bought from Free, switched to from
  // the other paid plan (not while an invoice is unpaid).
  const offer = (tier: Tier) =>
    !manager ||
    !known ||
    tier === "free" ||
    tier === s.tier ||
    (s.tier !== "free" && s.state === "pastDue") ? null : (
      <Button
        size="sm"
        disabled={checking}
        onClick={() =>
          s.tier === "free" ? setUpgrading(tier) : setSwitching(tier)
        }
      >
        {s.tier === "free" ? "Choose" : "Switch to"} {TIER_NAME[tier]}
      </Button>
    );

  return (
    <>
      <Panel
        title="Plan"
        subtitle="Billed per workspace, separately from Helix Cloud."
        actions={
          manager && (
            <Button
              size="sm"
              icon={CreditCard}
              pending={busy === "portal"}
              onClick={() => void openPortal()}
            >
              Manage billing
            </Button>
          )
        }
      >
        {plan.isPending ? (
          <SkeletonText lines={2} />
        ) : plan.error ? (
          <div className="wk-plan-error">
            <p role="alert" className="hf-form-error">
              Couldn’t read the plan. {plan.error.message}
            </p>
            <Button size="sm" onClick={() => void plan.refetch()}>
              Retry
            </Button>
          </div>
        ) : (
          <div className="wk-plan-current">
            <div>
              <h2 className="wk-plan-name">
                Foundry {held} <PlanTag s={s} />
              </h2>
              <p>{planText(s)}</p>
              {checking && (
                <p role="status" className="wk-plan-confirming">
                  <LoaderCircle size={14} className="hf-spin" aria-hidden />
                  Checking your payment…
                </p>
              )}
              {confirm.step === "slow" && s.tier !== confirm.tier && (
                <p role="status" className="wk-plan-confirming">
                  {TIER_NAME[confirm.tier]} hasn’t shown up yet. If you finished
                  paying, it can take a few minutes.
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() =>
                      setConfirm({ step: "checking", tier: confirm.tier })
                    }
                  >
                    Check again
                  </Button>
                </p>
              )}
            </div>
            {action}
          </div>
        )}
      </Panel>
      <div className="wk-plans">
        {(["free", ...PAID_TIERS] as const).map((t) => (
          <PlanCard
            key={t}
            tier={t}
            current={known && s.tier === t}
            action={offer(t)}
          />
        ))}
      </div>
      {!manager && (
        <p className="wk-note">Only owners and admins can change the plan.</p>
      )}
      {upgrading && (
        <UpgradeDialog
          initial={upgrading}
          onClose={() => setUpgrading(null)}
          onDone={checkedOut}
        />
      )}
      {switching && s.tier !== "free" && (
        <SwitchDialog
          from={s.tier}
          to={switching}
          onClose={() => setSwitching(null)}
          onDone={checkedOut}
        />
      )}
      {cancelling && s.tier !== "free" && (
        <ConfirmDialog
          title={`Cancel Foundry ${held}?`}
          description={
            s.periodEnd
              ? `${held} stays on until ${day(s.periodEnd)}. After that, connectors sync once a day, as on Free.`
              : `${held} stays on until the end of this period. After that, connectors sync once a day, as on Free.`
          }
          confirmLabel={`Cancel ${held}`}
          cancelLabel={`Keep ${held}`}
          tone="danger"
          pending={busy === "cancel"}
          onConfirm={() =>
            change("cancel", (next) => {
              const end = planSummary(next);
              return end.tier !== "free" && end.periodEnd
                ? `Foundry ${TIER_NAME[end.tier]} ends on ${day(end.periodEnd)}`
                : `Foundry ${held} will end at the end of this period`;
            })
          }
          onCancel={() => setCancelling(false)}
        />
      )}
    </>
  );
}
