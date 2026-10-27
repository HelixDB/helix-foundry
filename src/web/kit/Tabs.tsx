import { useRef, type KeyboardEvent, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";
import type { LucideIcon } from "lucide-react";
import { cx, formatNumber } from "./util";

export type TabItem = {
  id: string;
  label: ReactNode;
  count?: number;
  icon?: LucideIcon;
  disabled?: boolean;
};

// The active tab lives in the URL (?tab=) so it survives reloads and links.
// Unknown or missing values fall back to `fallback`; the fallback is not written.
export function useTabParam(
  ids: readonly string[],
  fallback = ids[0],
  param = "tab",
): [string, (id: string) => void] {
  const [params, setParams] = useSearchParams();
  const raw = params.get(param);
  const value = raw && ids.includes(raw) ? raw : fallback;
  return [
    value,
    (id) =>
      setParams(
        (p) => {
          const next = new URLSearchParams(p);
          if (id === fallback) next.delete(param);
          else next.set(param, id);
          return next;
        },
        { replace: true },
      ),
  ];
}

const tabId = (prefix: string, id: string) => `${prefix}-tab-${id}`;
const panelId = (prefix: string, id: string) => `${prefix}-panel-${id}`;

// ARIA tabs. Uncontrolled use syncs with ?<param>=; pass value/onChange to control.
export function Tabs({
  items,
  label,
  value,
  onChange,
  param = "tab",
  idPrefix = param,
  className,
}: {
  items: TabItem[];
  // Accessible name of the tab list.
  label: string;
  value?: string;
  onChange?: (id: string) => void;
  param?: string;
  // Must match the idPrefix given to <TabPanel/>.
  idPrefix?: string;
  className?: string;
}) {
  const ids = items.map((t) => t.id);
  const [urlValue, setUrlValue] = useTabParam(ids, ids[0], param);
  const current = value ?? urlValue,
    select = onChange ?? setUrlValue;
  const refs = useRef<Record<string, HTMLButtonElement | null>>({});
  const enabled = items.filter((t) => !t.disabled);
  const move = (e: KeyboardEvent, id: string) => {
    const i = enabled.findIndex((t) => t.id === id);
    const next =
      e.key === "ArrowRight"
        ? enabled[(i + 1) % enabled.length]
        : e.key === "ArrowLeft"
          ? enabled[(i - 1 + enabled.length) % enabled.length]
          : e.key === "Home"
            ? enabled[0]
            : e.key === "End"
              ? enabled[enabled.length - 1]
              : undefined;
    if (!next) return;
    e.preventDefault();
    select(next.id);
    refs.current[next.id]?.focus();
  };
  return (
    <div role="tablist" aria-label={label} className={cx("hf-tabs", className)}>
      {items.map((t) => {
        const selected = t.id === current;
        return (
          <button
            key={t.id}
            ref={(el) => {
              refs.current[t.id] = el;
            }}
            type="button"
            role="tab"
            id={tabId(idPrefix, t.id)}
            aria-selected={selected}
            aria-controls={panelId(idPrefix, t.id)}
            tabIndex={selected ? 0 : -1}
            disabled={t.disabled}
            className={cx("hf-tab", selected && "is-active")}
            onClick={() => select(t.id)}
            onKeyDown={(e) => move(e, t.id)}
          >
            {t.icon && <t.icon size={15} aria-hidden />}
            <span>{t.label}</span>
            {t.count !== undefined && (
              <span className="hf-tab-count">{formatNumber(t.count)}</span>
            )}
          </button>
        );
      })}
    </div>
  );
}

// Renders children only while its tab is active.
export function TabPanel({
  id,
  value,
  idPrefix = "tab",
  className,
  children,
}: {
  id: string;
  // The active tab id (from useTabParam or your own state).
  value: string;
  idPrefix?: string;
  className?: string;
  children: ReactNode;
}) {
  if (id !== value) return null;
  return (
    <div
      role="tabpanel"
      id={panelId(idPrefix, id)}
      aria-labelledby={tabId(idPrefix, id)}
      tabIndex={0}
      className={cx("hf-tab-panel", className)}
    >
      {children}
    </div>
  );
}
