import { useEffect, useRef, useState, type ReactNode } from "react";
import { X } from "lucide-react";
import { Button } from "./Button";
import { cx } from "./util";

// Native modal <dialog>: focus trap, Escape and backdrop close, focus restore.
// Opening focuses the first [data-autofocus] element, else the first control.
export function Dialog({
  title,
  description,
  onClose,
  footer,
  size = "md",
  variant = "center",
  className,
  hideTitle,
  children,
}: {
  title: string;
  description?: ReactNode;
  onClose: () => void;
  footer?: ReactNode;
  size?: "sm" | "md" | "lg" | "xl";
  // "drawer" slides in from the right edge.
  variant?: "center" | "drawer";
  className?: string;
  // Keeps the title for screen readers only and drops the built-in close
  // button: the content must render its own visible close control.
  hideTitle?: boolean;
  children?: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null),
    close = useRef(onClose);
  close.current = onClose;
  useEffect(() => {
    const el = ref.current!,
      previous = document.activeElement as HTMLElement | null;
    el.showModal();
    el.querySelector<HTMLElement>("[data-autofocus]")?.focus();
    const cancel = (e: Event) => {
      e.preventDefault();
      close.current();
    };
    el.addEventListener("cancel", cancel);
    return () => {
      el.removeEventListener("cancel", cancel);
      el.close();
      previous?.focus();
    };
  }, []);
  return (
    <dialog
      ref={ref}
      aria-label={title}
      className={cx(
        "hf-dialog",
        `hf-dialog--${size}`,
        `hf-dialog--${variant}`,
        className,
      )}
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className="hf-dialog-card">
        {hideTitle ? (
          <h2 className="hf-sr-only">{title}</h2>
        ) : (
          <div className="hf-dialog-head">
            <div>
              <h2>{title}</h2>
              {description && <p>{description}</p>}
            </div>
            <button
              type="button"
              className="hf-btn hf-btn--ghost hf-btn--md hf-btn--icon"
              aria-label="Close"
              onClick={onClose}
            >
              <X size={16} aria-hidden />
            </button>
          </div>
        )}
        <div className="hf-dialog-body">{children}</div>
        {footer && <div className="hf-dialog-foot">{footer}</div>}
      </div>
    </dialog>
  );
}

export function ConfirmDialog({
  title,
  description,
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  tone = "default",
  pending,
  requireText,
  onConfirm,
  onCancel,
  children,
}: {
  title: string;
  description?: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  // "danger" styles the confirm button red, for destructive actions.
  tone?: "default" | "danger";
  pending?: boolean;
  // When set, the user must type this text before confirming.
  requireText?: string;
  onConfirm: () => void | Promise<void>;
  onCancel: () => void;
  children?: ReactNode;
}) {
  const [typed, setTyped] = useState("");
  const blocked = !!requireText && typed.trim() !== requireText;
  return (
    <Dialog
      title={title}
      description={description}
      onClose={onCancel}
      size="sm"
      footer={
        <>
          <Button onClick={onCancel} autoFocus={tone === "danger"}>
            {cancelLabel}
          </Button>
          <Button
            variant={tone === "danger" ? "danger" : "primary"}
            pending={pending}
            disabled={blocked}
            onClick={() => void onConfirm()}
          >
            {confirmLabel}
          </Button>
        </>
      }
    >
      {children}
      {requireText && (
        <label className="hf-field">
          <span>
            Type <strong>{requireText}</strong> to confirm
          </span>
          <input
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            autoComplete="off"
          />
        </label>
      )}
    </Dialog>
  );
}
