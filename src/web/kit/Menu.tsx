import {
  createContext,
  useContext,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
} from "react";
import { Link } from "react-router-dom";
import { Check, ChevronDown, type LucideIcon } from "lucide-react";
import { cx } from "./util";

const MenuClose = createContext<() => void>(() => {});

const GAP = 6,
  EDGE = 8;
// Shows `pop` in the top layer at a fixed position next to `anchor`, flipping
// to the other side when it would leave the viewport, and follows the anchor
// while the page scrolls or resizes.
function usePlacement(
  open: boolean,
  anchor: RefObject<HTMLElement | null>,
  pop: RefObject<HTMLElement | null>,
  align: "start" | "end",
  side: "bottom" | "top",
) {
  useLayoutEffect(() => {
    if (!open) return;
    // A Select in a FilterSelect drops from the whole pill.
    const el = pop.current!,
      trigger =
        anchor.current!.closest<HTMLElement>(".hf-filter-select") ??
        anchor.current!;
    try {
      el.showPopover?.();
    } catch {
      /* already shown, or no Popover API: position: fixed still applies */
    }
    const place = () => {
      const r = trigger.getBoundingClientRect(),
        vw = window.innerWidth,
        vh = window.innerHeight;
      el.style.setProperty("--hf-menu-trigger-width", `${r.width}px`);
      const w = el.offsetWidth,
        h = el.offsetHeight;
      const above = r.top - GAP - h,
        below = r.bottom + GAP;
      let top = side === "top" ? above : below;
      if (side === "bottom" && below + h > vh - EDGE && above >= EDGE)
        top = above;
      if (side === "top" && above < EDGE && below + h <= vh - EDGE) top = below;
      const left = align === "end" ? r.right - w : r.left;
      el.style.top = `${Math.max(EDGE, Math.min(top, vh - h - EDGE))}px`;
      el.style.left = `${Math.max(EDGE, Math.min(left, vw - w - EDGE))}px`;
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
      try {
        el.hidePopover?.();
      } catch {
        /* already hidden */
      }
    };
  }, [open, anchor, pop, align, side]);
}

// Button that opens a menu; closes on outside click, Escape, Tab or selection.
// The menu sits in the top layer at a fixed position next to its trigger, so
// overflow on any ancestor (sidebar, table, dialog) cannot clip it.
export function DropdownMenu({
  label,
  trigger,
  children,
  align = "start",
  side = "bottom",
  className,
  triggerClassName,
}: {
  // Accessible name of the trigger button.
  label: string;
  trigger: ReactNode;
  children: ReactNode;
  align?: "start" | "end";
  side?: "bottom" | "top";
  className?: string;
  triggerClassName?: string;
}) {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null),
    button = useRef<HTMLButtonElement>(null),
    menu = useRef<HTMLDivElement>(null);
  const id = useId();
  const items = () => [
    ...(menu.current?.querySelectorAll<HTMLElement>(
      '[role^="menuitem"]:not([aria-disabled="true"])',
    ) || []),
  ];
  usePlacement(open, button, menu, align, side);
  useEffect(() => {
    if (!open) return;
    items()[0]?.focus();
    const outside = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", outside);
    return () => document.removeEventListener("pointerdown", outside);
  }, [open]);
  const close = (refocus = true) => {
    setOpen(false);
    if (refocus) button.current?.focus();
  };
  const onKey = (e: KeyboardEvent) => {
    const list = items(),
      i = list.indexOf(document.activeElement as HTMLElement);
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      close();
    } else if (e.key === "Tab") close(false);
    else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const step = e.key === "ArrowDown" ? 1 : -1;
      list[(i + step + list.length) % list.length]?.focus();
    } else if (e.key === "Home" || e.key === "End") {
      e.preventDefault();
      (e.key === "Home" ? list[0] : list[list.length - 1])?.focus();
    }
  };
  return (
    <div ref={root} className={cx("hf-menu", className)}>
      <button
        ref={button}
        type="button"
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        className={cx("hf-menu-trigger", triggerClassName)}
        onClick={() => setOpen((v) => !v)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown" && !open) {
            e.preventDefault();
            setOpen(true);
          }
        }}
      >
        {trigger}
      </button>
      {open && (
        <div
          ref={menu}
          id={id}
          role="menu"
          popover="manual"
          aria-label={label}
          className={cx("hf-menu-popover", `is-${align}`, `is-${side}`)}
          onKeyDown={onKey}
        >
          <MenuClose.Provider value={() => close()}>
            {children}
          </MenuClose.Provider>
        </div>
      )}
    </div>
  );
}

export function MenuItem({
  icon: Icon,
  children,
  hint,
  to,
  href,
  onSelect,
  danger,
  disabled,
  checked,
  external,
}: {
  icon?: LucideIcon;
  children: ReactNode;
  // Trailing text, e.g. a shortcut or role.
  hint?: ReactNode;
  // Router path; renders a Link.
  to?: string;
  // Plain URL (docs, downloads).
  href?: string;
  onSelect?: () => void;
  danger?: boolean;
  disabled?: boolean;
  // Renders a checkable item (menuitemradio) with a check mark.
  checked?: boolean;
  external?: boolean;
}) {
  const close = useContext(MenuClose);
  const role = checked === undefined ? "menuitem" : "menuitemradio";
  const content = (
    <>
      {checked !== undefined && (
        <Check
          size={14}
          aria-hidden
          className={cx("hf-menu-check", !checked && "is-hidden")}
        />
      )}
      {Icon && <Icon size={15} aria-hidden />}
      <span className="hf-menu-label">{children}</span>
      {hint && <span className="hf-menu-hint">{hint}</span>}
    </>
  );
  const props = {
    role,
    tabIndex: -1,
    "aria-disabled": disabled || undefined,
    "aria-checked": checked,
    className: cx("hf-menu-item", danger && "is-danger"),
  } as const;
  if (to && !disabled)
    return (
      <Link {...props} to={to} onClick={() => (onSelect?.(), close())}>
        {content}
      </Link>
    );
  if (href && !disabled)
    return (
      <a
        {...props}
        href={href}
        target={external ? "_blank" : undefined}
        rel={external ? "noreferrer" : undefined}
        onClick={() => close()}
      >
        {content}
      </a>
    );
  return (
    <button
      {...props}
      type="button"
      disabled={disabled}
      onClick={() => {
        onSelect?.();
        close();
      }}
    >
      {content}
    </button>
  );
}
export const MenuSeparator = () => (
  <div role="separator" className="hf-menu-separator" />
);
export const MenuLabel = ({ children }: { children: ReactNode }) => (
  <div className="hf-menu-group-label" role="presentation">
    {children}
  </div>
);
// A heading row in normal case, e.g. the signed-in user and their role.
export const MenuHeader = ({
  title,
  subtitle,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
}) => (
  <div className="hf-menu-header" role="presentation">
    <strong>{title}</strong>
    {subtitle && <small>{subtitle}</small>}
  </div>
);

export type SelectOption<V extends string = string> = {
  value: V;
  label: string;
  // Trailing text in the list, e.g. what a role can do.
  hint?: string;
  disabled?: boolean;
};

// Single-choice field in place of a native <select>, whose OS popup ignores
// the theme: the trigger shows the chosen option and opens a listbox in the
// menu popover. onChange fires only for a different, enabled option. Inside
// a <label> the label's text opens it too; options are buttons, so a click
// on one never reaches the label's control.
export function Select<V extends string>({
  label,
  value,
  onChange,
  options,
  placeholder = "Choose…",
  disabled,
  autoFocus,
  className,
}: {
  // Accessible name of the trigger and the list.
  label: string;
  value: V;
  onChange: (value: V) => void;
  options: readonly SelectOption<V>[];
  // Shown when no option has `value`.
  placeholder?: string;
  disabled?: boolean;
  // Focused first when its Dialog opens.
  autoFocus?: boolean;
  className?: string;
}) {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null),
    button = useRef<HTMLButtonElement>(null),
    list = useRef<HTMLDivElement>(null),
    typed = useRef({ text: "", at: 0 });
  const id = useId();
  const chosen = options.find((o) => o.value === value);
  // Enabled options, in the order of `enabled` below.
  const items = () => [
    ...(list.current?.querySelectorAll<HTMLElement>(
      '[role="option"]:not([aria-disabled="true"])',
    ) || []),
  ];
  const enabled = options.filter((o) => !o.disabled);
  usePlacement(open, button, list, "start", "bottom");
  useEffect(() => {
    if (!open) return;
    (
      list.current?.querySelector<HTMLElement>(
        '[aria-selected="true"]:not([aria-disabled="true"])',
      ) ?? items()[0]
    )?.focus();
    // A wrapping label is inside: its click toggles the trigger itself.
    const outside = (e: PointerEvent) => {
      const at = e.target as Node;
      if (
        !root.current?.contains(at) &&
        ![...(button.current?.labels ?? [])].some((l) => l.contains(at))
      )
        setOpen(false);
    };
    document.addEventListener("pointerdown", outside);
    return () => document.removeEventListener("pointerdown", outside);
  }, [open]);
  const close = (refocus = true) => {
    setOpen(false);
    if (refocus) button.current?.focus();
  };
  const onKey = (e: KeyboardEvent) => {
    const all = items(),
      i = all.indexOf(document.activeElement as HTMLElement);
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      close();
    } else if (e.key === "Tab") close(false);
    else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const step = e.key === "ArrowDown" ? 1 : -1;
      all[(i + step + all.length) % all.length]?.focus();
    } else if (e.key === "Home" || e.key === "End") {
      e.preventDefault();
      (e.key === "Home" ? all[0] : all[all.length - 1])?.focus();
    } else if (
      e.key.length === 1 &&
      e.key !== " " &&
      !e.ctrlKey &&
      !e.metaKey &&
      !e.altKey
    ) {
      // Typeahead: a run of keys finds the option starting with them; one
      // key again moves on to the next option with that letter.
      const prev = typed.current,
        text =
          (e.timeStamp - prev.at < 700 ? prev.text : "") + e.key.toLowerCase();
      typed.current = { text, at: e.timeStamp };
      const from = text.length === 1 ? i + 1 : Math.max(i, 0),
        order = [...enabled.slice(from), ...enabled.slice(0, from)],
        hit = order.find((o) => o.label.toLowerCase().startsWith(text));
      if (hit) all[enabled.indexOf(hit)]?.focus();
    }
  };
  return (
    <div ref={root} className={cx("hf-select", className)}>
      <button
        ref={button}
        type="button"
        role="combobox"
        aria-label={label}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        disabled={disabled}
        data-autofocus={autoFocus ? "" : undefined}
        className="hf-select-trigger"
        onClick={() => setOpen((v) => !v)}
        onKeyDown={(e) => {
          if ((e.key === "ArrowDown" || e.key === "ArrowUp") && !open) {
            e.preventDefault();
            setOpen(true);
          }
        }}
      >
        <span className={cx("hf-select-value", !chosen && "is-placeholder")}>
          {chosen?.label ?? placeholder}
        </span>
        <ChevronDown size={15} aria-hidden />
      </button>
      {open && (
        <div
          ref={list}
          id={id}
          role="listbox"
          popover="manual"
          aria-label={label}
          className="hf-menu-popover hf-select-popover is-start is-bottom"
          onKeyDown={onKey}
        >
          {options.map((o) => (
            <button
              key={o.value}
              type="button"
              role="option"
              tabIndex={-1}
              aria-selected={o.value === value}
              aria-disabled={o.disabled || undefined}
              className="hf-menu-item"
              onClick={() => {
                if (o.disabled) return;
                if (o.value !== value) onChange(o.value);
                close();
              }}
            >
              <Check
                size={14}
                aria-hidden
                className={cx(
                  "hf-menu-check",
                  o.value !== value && "is-hidden",
                )}
              />
              <span className="hf-menu-label">{o.label}</span>
              {o.hint && <span className="hf-menu-hint">{o.hint}</span>}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
