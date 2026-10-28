import type {
  AnchorHTMLAttributes,
  ButtonHTMLAttributes,
  ReactNode,
} from "react";
import { Link, type LinkProps } from "react-router-dom";
import { LoaderCircle, type LucideIcon } from "lucide-react";
import { cx } from "./util";

export type ButtonVariant =
  | "primary"
  | "secondary"
  | "ghost"
  | "accent"
  | "danger";
export type ButtonSize = "sm" | "md" | "lg";
type Common = {
  variant?: ButtonVariant;
  size?: ButtonSize;
  icon?: LucideIcon;
  iconRight?: LucideIcon;
  children?: ReactNode;
};
const classes = (
  variant: ButtonVariant,
  size: ButtonSize,
  extra?: string,
  iconOnly?: boolean,
) =>
  cx(
    "hf-btn",
    `hf-btn--${variant}`,
    `hf-btn--${size}`,
    iconOnly && "hf-btn--icon",
    extra,
  );
const iconSize = (size: ButtonSize) => (size === "sm" ? 14 : 16);

export function Button({
  variant = "secondary",
  size = "md",
  icon: Icon,
  iconRight: IconRight,
  pending,
  className,
  children,
  type = "button",
  disabled,
  ...rest
}: Common & ButtonHTMLAttributes<HTMLButtonElement> & { pending?: boolean }) {
  return (
    <button
      type={type}
      className={classes(variant, size, className, !children)}
      disabled={disabled || pending}
      aria-busy={pending || undefined}
      {...rest}
    >
      {pending ? (
        <LoaderCircle size={iconSize(size)} className="hf-spin" aria-hidden />
      ) : (
        Icon && <Icon size={iconSize(size)} aria-hidden />
      )}
      {children}
      {IconRight && <IconRight size={iconSize(size)} aria-hidden />}
    </button>
  );
}
// A router link styled as a button.
export function ButtonLink({
  variant = "secondary",
  size = "md",
  icon: Icon,
  iconRight: IconRight,
  className,
  children,
  ...rest
}: Common & LinkProps) {
  return (
    <Link className={classes(variant, size, className, !children)} {...rest}>
      {Icon && <Icon size={iconSize(size)} aria-hidden />}
      {children}
      {IconRight && <IconRight size={iconSize(size)} aria-hidden />}
    </Link>
  );
}
// An external or download link styled as a button.
export function ButtonAnchor({
  variant = "secondary",
  size = "md",
  icon: Icon,
  className,
  children,
  ...rest
}: Common & AnchorHTMLAttributes<HTMLAnchorElement>) {
  return (
    <a className={classes(variant, size, className, !children)} {...rest}>
      {Icon && <Icon size={iconSize(size)} aria-hidden />}
      {children}
    </a>
  );
}
// Icon-only button; `label` is its accessible name and tooltip.
export function IconButton({
  label,
  icon: Icon,
  variant = "ghost",
  size = "md",
  className,
  type = "button",
  ...rest
}: {
  label: string;
  icon: LucideIcon;
  variant?: ButtonVariant;
  size?: ButtonSize;
} & Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children">) {
  return (
    <button
      type={type}
      aria-label={label}
      title={label}
      className={classes(variant, size, className, true)}
      {...rest}
    >
      <Icon size={iconSize(size)} aria-hidden />
    </button>
  );
}
