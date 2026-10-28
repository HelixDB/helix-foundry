import type { InputHTMLAttributes, ReactNode } from "react";
import { Search, X } from "lucide-react";
import { cx } from "./util";

// A horizontal row of controls; wraps on small screens.
export function Toolbar({
  children,
  className,
  sticky,
}: {
  children: ReactNode;
  className?: string;
  // Sticks to the top of the scrolling page.
  sticky?: boolean;
}) {
  return (
    <div className={cx("hf-toolbar", sticky && "is-sticky", className)}>
      {children}
    </div>
  );
}
export const ToolbarSpacer = () => <span className="hf-toolbar-spacer" />;

export function SearchInput({
  value,
  onChange,
  label = "Search",
  placeholder = "Search",
  className,
  ...rest
}: {
  value: string;
  onChange: (value: string) => void;
  label?: string;
  placeholder?: string;
  className?: string;
} & Omit<
  InputHTMLAttributes<HTMLInputElement>,
  "value" | "onChange" | "className"
>) {
  return (
    <div className={cx("hf-search", className)}>
      <Search size={15} aria-hidden />
      <input
        type="search"
        aria-label={label}
        placeholder={placeholder}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        {...rest}
      />
      {value && (
        <button
          type="button"
          aria-label="Clear search"
          onClick={() => onChange("")}
        >
          <X size={14} aria-hidden />
        </button>
      )}
    </div>
  );
}

// A labelled native select for filters.
export function FilterSelect({
  label,
  value,
  onChange,
  options,
  className,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  options: { value: string; label: string }[];
  className?: string;
}) {
  return (
    <label className={cx("hf-filter-select", className)}>
      <span>{label}</span>
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </label>
  );
}

// Single-choice segmented control (radio group).
export function Segmented({
  label,
  value,
  onChange,
  options,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  options: { value: string; label: ReactNode; count?: number }[];
}) {
  return (
    <div className="hf-segmented" role="radiogroup" aria-label={label}>
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="radio"
          aria-checked={o.value === value}
          className={o.value === value ? "is-active" : undefined}
          onClick={() => onChange(o.value)}
        >
          {o.label}
          {o.count !== undefined && <span>{o.count}</span>}
        </button>
      ))}
    </div>
  );
}

// Search plus filters plus trailing actions, with an optional result count.
export function FilterBar({
  search,
  children,
  actions,
  count,
  className,
}: {
  search?: {
    value: string;
    onChange: (value: string) => void;
    placeholder?: string;
    label?: string;
  };
  // Filter controls: <FilterSelect/>, <Segmented/>, chips...
  children?: ReactNode;
  actions?: ReactNode;
  // e.g. "24 of 120 datasets"
  count?: ReactNode;
  className?: string;
}) {
  return (
    <Toolbar className={cx("hf-filterbar", className)}>
      {search && (
        <SearchInput
          value={search.value}
          onChange={search.onChange}
          placeholder={search.placeholder}
          label={search.label || search.placeholder || "Search"}
        />
      )}
      {children}
      <ToolbarSpacer />
      {count !== undefined && (
        <span className="hf-filterbar-count">{count}</span>
      )}
      {actions}
    </Toolbar>
  );
}
