import "./workspace.css";
import { Sparkles } from "lucide-react";
import { ButtonLink, cx } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { PLAN_PRICE, canManage } from "./plan";

export { isProRequired } from "./plan";

/**
 * Where the WFE enforces Foundry Pro: say what needs it and how to get it.
 * Show it beside a Pro-only choice (a connector kind with `available: false`)
 * or in place of an error for which isProRequired(error) is true. The WFE
 * passes Max wherever it passes Pro, so a Max workspace never sees it.
 *
 * @example
 * <ProGate feature="PostgreSQL connectors" />
 * <ProGate feature="People enrichment" title="People enrichment needs Foundry Pro" pitch="Pro looks up …" />
 */
export function ProGate({
  feature,
  title,
  pitch,
  compact,
  className,
}: {
  // What needs Pro, plural: "PostgreSQL connectors".
  feature: string;
  // In place of "<feature> need Foundry Pro", and of the pitch to owners
  // and admins (which names the connectors Pro adds).
  title?: string;
  pitch?: string;
  // One line, for inside a form or a card.
  compact?: boolean;
  className?: string;
}) {
  const { role, name } = useWorkspace();
  return (
    <div
      role="note"
      className={cx("wk-pro-gate", compact && "is-compact", className)}
    >
      <Sparkles size={16} className="wk-pro-gate-icon" aria-hidden />
      <div className="wk-pro-gate-text">
        <strong>{title ?? `${feature} need Foundry Pro`}</strong>
        {!compact && (
          <p>
            {canManage(role)
              ? (pitch ??
                `Pro adds PostgreSQL, MySQL and S3 to ${name} for ${PLAN_PRICE.pro} a month.`)
              : `Ask an owner or admin of ${name} to upgrade it to Pro.`}
          </p>
        )}
      </div>
      {canManage(role) && (
        <ButtonLink
          size="sm"
          variant="primary"
          to={paths.settings("billing")}
          className="wk-pro-gate-action"
        >
          Upgrade to Pro
        </ButtonLink>
      )}
    </div>
  );
}
