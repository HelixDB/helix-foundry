import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { ChevronRight } from "lucide-react";
import { cx, useTitle } from "./util";

export type Crumb = { label: ReactNode; to?: string };
export type PageWidth = "narrow" | "default" | "wide" | "full";

// Standard page container: consistent gutters and max width inside the shell.
export function Page({
  width = "default",
  className,
  children,
}: {
  width?: PageWidth;
  className?: string;
  children: ReactNode;
}) {
  return (
    <div className={cx("hf-page", `hf-page--${width}`, className)}>
      {children}
    </div>
  );
}

export function Breadcrumbs({ items }: { items: Crumb[] }) {
  return (
    <nav aria-label="Breadcrumb" className="hf-breadcrumbs">
      <ol>
        {items.map((c, i) => (
          <li key={i}>
            {c.to && i < items.length - 1 ? (
              <Link to={c.to}>{c.label}</Link>
            ) : (
              <span aria-current={i === items.length - 1 ? "page" : undefined}>
                {c.label}
              </span>
            )}
            {i < items.length - 1 && <ChevronRight size={12} aria-hidden />}
          </li>
        ))}
      </ol>
    </nav>
  );
}

export function PageHeader({
  title,
  subtitle,
  breadcrumbs,
  actions,
  tabs,
  eyebrow,
  icon,
  meta,
  documentTitle,
  className,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  breadcrumbs?: Crumb[];
  // Buttons on the right (wrap under the title on small screens).
  actions?: ReactNode;
  // Usually a <Tabs/>; rendered under the header with a divider.
  tabs?: ReactNode;
  eyebrow?: ReactNode;
  // Leading visual, e.g. a <SourceLogo/> or an icon tile.
  icon?: ReactNode;
  // A row of small facts under the subtitle (badges, counts, timestamps).
  meta?: ReactNode;
  // Browser tab title; defaults to `title` when it is a string.
  documentTitle?: string;
  className?: string;
}) {
  useTitle(documentTitle ?? (typeof title === "string" ? title : undefined));
  return (
    <header className={cx("hf-page-header", !!tabs && "has-tabs", className)}>
      {breadcrumbs && breadcrumbs.length > 0 && (
        <Breadcrumbs items={breadcrumbs} />
      )}
      <div className="hf-page-header-row">
        <div className="hf-page-header-title">
          {icon && <div className="hf-page-header-icon">{icon}</div>}
          <div>
            {eyebrow && <div className="hf-eyebrow">{eyebrow}</div>}
            <h1>{title}</h1>
            {subtitle && <p className="hf-page-header-subtitle">{subtitle}</p>}
            {meta && <div className="hf-page-header-meta">{meta}</div>}
          </div>
        </div>
        {actions && <div className="hf-page-header-actions">{actions}</div>}
      </div>
      {tabs && <div className="hf-page-header-tabs">{tabs}</div>}
    </header>
  );
}
