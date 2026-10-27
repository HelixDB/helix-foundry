import type { CSSProperties, ReactNode } from "react";
import { Link } from "react-router-dom";
import { ArrowUpRight, Inbox, type LucideIcon } from "lucide-react";
import { cx } from "./util";
import { StatusDot, type Status } from "./StatusBadge";

export function EmptyState({
  icon: Icon = Inbox,
  title,
  text,
  action,
  size = "md",
  className,
}: {
  icon?: LucideIcon;
  title: ReactNode;
  text?: ReactNode;
  // Buttons or links; several are laid out in a row.
  action?: ReactNode;
  size?: "sm" | "md" | "lg";
  className?: string;
}) {
  return (
    <div className={cx("hf-empty", `hf-empty--${size}`, className)}>
      <div className="hf-empty-icon" aria-hidden>
        <Icon size={size === "sm" ? 18 : 22} />
      </div>
      <h3>{title}</h3>
      {text && <p>{text}</p>}
      {action && <div className="hf-empty-action">{action}</div>}
    </div>
  );
}

export function StatCard({
  label,
  value,
  hint,
  delta,
  to,
  icon: Icon,
  status,
  children,
  loading,
  className,
}: {
  label: ReactNode;
  value: ReactNode;
  hint?: ReactNode;
  delta?: { value: ReactNode; trend: "up" | "down" | "flat"; good?: boolean };
  // Makes the whole card a link.
  to?: string;
  icon?: LucideIcon;
  // Shows a status dot beside the label.
  status?: Status;
  // Extra content under the value, e.g. a <SourceStack/>.
  children?: ReactNode;
  loading?: boolean;
  className?: string;
}) {
  const body = (
    <>
      <div className="hf-stat-label">
        {Icon && <Icon size={15} aria-hidden />}
        <span>{label}</span>
        {status && <StatusDot status={status} />}
        {to && <ArrowUpRight size={14} className="hf-stat-go" aria-hidden />}
      </div>
      <div className="hf-stat-value hf-num">
        {loading ? <span className="hf-skeleton hf-stat-skeleton" /> : value}
      </div>
      {delta && (
        <div
          className={cx(
            "hf-stat-delta",
            delta.good === undefined
              ? "is-flat"
              : delta.good
                ? "is-good"
                : "is-bad",
          )}
        >
          {delta.trend === "up" ? "▲" : delta.trend === "down" ? "▼" : "•"}{" "}
          {delta.value}
        </div>
      )}
      {hint && <div className="hf-stat-hint">{hint}</div>}
      {children && <div className="hf-stat-extra">{children}</div>}
    </>
  );
  return to ? (
    <Link to={to} className={cx("hf-stat", "is-link", className)}>
      {body}
    </Link>
  ) : (
    <div className={cx("hf-stat", className)}>{body}</div>
  );
}

// A titled region of a page, without a surface.
export function Section({
  title,
  description,
  actions,
  id,
  className,
  children,
}: {
  title?: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  id?: string;
  className?: string;
  children: ReactNode;
}) {
  return (
    <section
      className={cx("hf-section", className)}
      aria-labelledby={title && id ? id + "-title" : undefined}
    >
      {(title || actions) && (
        <div className="hf-section-head">
          <div>
            {title && <h2 id={id ? id + "-title" : undefined}>{title}</h2>}
            {description && <p>{description}</p>}
          </div>
          {actions && <div className="hf-section-actions">{actions}</div>}
        </div>
      )}
      {children}
    </section>
  );
}

// A raised card surface with an optional header and footer.
export function Panel({
  title,
  subtitle,
  actions,
  footer,
  padded = true,
  className,
  style,
  children,
}: {
  title?: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
  footer?: ReactNode;
  padded?: boolean;
  className?: string;
  style?: CSSProperties;
  children: ReactNode;
}) {
  return (
    <div className={cx("hf-panel", className)} style={style}>
      {(title || actions) && (
        <div className="hf-panel-head">
          <div>
            {title && <h3>{title}</h3>}
            {subtitle && <p>{subtitle}</p>}
          </div>
          {actions && <div className="hf-panel-actions">{actions}</div>}
        </div>
      )}
      <div className={cx("hf-panel-body", padded && "is-padded")}>
        {children}
      </div>
      {footer && <div className="hf-panel-foot">{footer}</div>}
    </div>
  );
}

// List on the left, detail on the right; on narrow screens shows one at a time.
export function SplitView({
  list,
  detail,
  hasSelection = false,
  listWidth = 320,
  listLabel = "Items",
  className,
}: {
  list: ReactNode;
  detail: ReactNode;
  // When true, narrow screens show the detail instead of the list.
  hasSelection?: boolean;
  listWidth?: number;
  listLabel?: string;
  className?: string;
}) {
  return (
    <div
      className={cx("hf-split", hasSelection && "has-selection", className)}
      style={{ "--hf-split-list": listWidth + "px" } as CSSProperties}
    >
      <aside className="hf-split-list" aria-label={listLabel}>
        {list}
      </aside>
      <div className="hf-split-detail">{detail}</div>
    </div>
  );
}

export type KeyValueItem = {
  label: ReactNode;
  value: ReactNode;
  mono?: boolean;
  hint?: ReactNode;
};
export function KeyValue({
  items,
  columns = 1,
  className,
}: {
  items: KeyValueItem[];
  columns?: 1 | 2 | 3;
  className?: string;
}) {
  return (
    <dl className={cx("hf-kv", `hf-kv--${columns}`, className)}>
      {items.map((item, i) => (
        <div key={i} className="hf-kv-row">
          <dt>{item.label}</dt>
          <dd className={item.mono ? "hf-mono" : undefined}>
            {item.value === null ||
            item.value === undefined ||
            item.value === "" ? (
              <span className="hf-kv-empty">—</span>
            ) : (
              item.value
            )}
            {item.hint && <small>{item.hint}</small>}
          </dd>
        </div>
      ))}
    </dl>
  );
}

export function Skeleton({
  width,
  height = 12,
  radius,
  className,
}: {
  width?: number | string;
  height?: number | string;
  radius?: number;
  className?: string;
}) {
  return (
    <span
      aria-hidden
      className={cx("hf-skeleton", className)}
      style={{ width, height, borderRadius: radius }}
    />
  );
}
export function SkeletonText({ lines = 3 }: { lines?: number }) {
  return (
    <div className="hf-skeleton-text" role="status" aria-label="Loading">
      {Array.from({ length: lines }, (_, i) => (
        <Skeleton key={i} width={i === lines - 1 ? "60%" : "100%"} />
      ))}
    </div>
  );
}
