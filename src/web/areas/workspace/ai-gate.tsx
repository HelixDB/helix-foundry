import "./workspace.css";
import { useEffect, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Bot } from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import { ButtonLink, cx } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { PLAN_PRICE } from "./plan";
import { ProGate, isProRequired } from "./pro-gate";
import {
  aiKey,
  aiQuery,
  inForce,
  isAiNotConfigured,
  splitSettingsLink,
} from "./ai-api";

export { isAiNotConfigured };

/**
 * Whether the workspace's model work has a provider: always hosted (Helix's
 * Claude is the default, so nothing is read); locally, false while the
 * setting resolves to none, undefined while it is first read. A setting
 * that can't be read counts as one: the WFE still refuses the work without
 * a provider (FOUNDRY_AI_NOT_CONFIGURED), and nothing waits on it for good.
 */
export function useAiConfigured(): boolean | undefined {
  const { id } = useWorkspace();
  const ai = useQuery({ ...aiQuery(id), enabled: LOCAL });
  if (!LOCAL) return true;
  if (ai.data) return inForce(ai.data).source !== "none";
  return ai.isPending ? undefined : true;
}

/**
 * Where model work needs an AI provider and none is set up (a local install
 * without one): say so and where to choose one. Show it in place of the
 * action while useAiConfigured() is false, or of an error for which
 * isAiNotConfigured(error) is true, as <ProGate/> is for Foundry Pro.
 *
 * @example
 * <AiGate action="Generating an ontology" />
 */
export function AiGate({
  action,
  className,
}: {
  // What needs it, as a sentence's subject: "Generating an ontology".
  action: string;
  className?: string;
}) {
  const { role, name } = useWorkspace();
  const owner = role === "owner";
  return (
    <div role="note" className={cx("wk-pro-gate", className)}>
      <Bot size={16} className="wk-pro-gate-icon" aria-hidden />
      <div className="wk-pro-gate-text">
        <strong>{action} needs an AI provider</strong>
        <p>
          {owner
            ? "Choose where Foundry’s AI runs in Settings › AI models."
            : `Ask an owner of ${name} to choose one in Settings › AI models.`}
        </p>
      </div>
      {owner && (
        <ButtonLink
          size="sm"
          variant="primary"
          to={paths.settings("ai")}
          className="wk-pro-gate-action"
        >
          Choose a provider
        </ButtonLink>
      )}
    </div>
  );
}

/**
 * Hosted, off Pro and Max: the workspace's own model key needs a plan, to
 * be saved and to be run on (the WFE refuses its work with
 * FOUNDRY_PRO_REQUIRED once the workspace leaves Pro).
 *
 * @example
 * <OwnKeyGate />
 */
export function OwnKeyGate() {
  const { name } = useWorkspace();
  return (
    <ProGate
      feature="Your own model keys"
      title="Bringing your own model key needs Foundry Pro"
      pitch={`Pro runs ${name}’s drafts, document reads and questions on your own key, for ${PLAN_PRICE.pro} a month. Default stays available on every plan.`}
    />
  );
}

/**
 * Where model work shows its error (generating, asking): <AiGate/> while no
 * AI provider is set up, as useAiConfigured() (`ready`) says or as the WFE
 * said in refusing it (`error`); <OwnKeyGate/> when the WFE refused the
 * workspace's own key off Pro; else `children`, the error as the page
 * shows it. A refusal for want of a provider reads the setting again, so
 * `ready` follows it. The page clears `error` when it tries again, and the
 * gate goes with it.
 *
 * @example
 * <AiRefusal action="Generating an ontology" error={error} ready={aiReady}>
 *   {!!error && <p role="alert">{(error as Error).message}</p>}
 * </AiRefusal>
 */
export function AiRefusal({
  action,
  error,
  ready,
  children,
}: {
  // As <AiGate/>'s.
  action: string;
  error: unknown;
  ready: boolean | undefined;
  children?: ReactNode;
}) {
  const { id } = useWorkspace(),
    qc = useQueryClient();
  const refused = isAiNotConfigured(error);
  useEffect(() => {
    if (refused) void qc.invalidateQueries({ queryKey: aiKey(id) });
  }, [error]);
  if (ready === false || refused) return <AiGate action={action} />;
  if (isProRequired(error)) return <OwnKeyGate />;
  return children;
}

/**
 * A failure's text with "Settings › AI models", where the WFE's copy says
 * an owner can fix it, linked to the AI settings.
 *
 * @example
 * <SettingsLinked text="Claude refused the API key. An owner can enter a new one in Settings › AI models." />
 */
export function SettingsLinked({ text }: { text: string }) {
  return (
    <>
      {splitSettingsLink(text).map((part, i) =>
        i % 2 ? (
          <Link key={i} to={paths.settings("ai")}>
            {part}
          </Link>
        ) : (
          part
        ),
      )}
    </>
  );
}
