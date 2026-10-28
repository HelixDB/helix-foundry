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
} from "react";
import { Link } from "react-router-dom";
import { Check, type LucideIcon } from "lucide-react";
import { cx } from "./util";

const MenuClose = createContext<() => void>(() => {});

const GAP = 6,
  EDGE = 8;
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
  useLayoutEffect(() => {
    if (!open) return;
    const el = menu.current!,
      trigger = button.current!;
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
  }, [open, align, side]);
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
