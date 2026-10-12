import {
  useLayoutEffect,
  useState,
  useId,
  useRef,
  type RefObject,
  type TextareaHTMLAttributes,
} from "react";

// A textarea that grows with its text between minRows and maxRows, then
// scrolls. Enter submits; Shift+Enter (or Enter while composing text with an
// input method) adds a new line.
export function PromptInput({
  value,
  onValueChange,
  onSubmit,
  minRows = 1,
  maxRows = 6,
  inputRef,
  className,
  completion,
  completionKey,
  ...rest
}: {
  value: string;
  onValueChange: (value: string) => void;
  onSubmit: () => void;
  minRows?: number;
  maxRows?: number;
  inputRef?: RefObject<HTMLTextAreaElement | null>;
  completion?: string;
  completionKey?: string;
} & Omit<
  TextareaHTMLAttributes<HTMLTextAreaElement>,
  "value" | "onChange" | "onSubmit" | "rows"
>) {
  const own = useRef<HTMLTextAreaElement>(null),
    ref = inputRef || own;
  const suggestionId = useId();
  const ghost = useRef<HTMLSpanElement>(null);
  const [focused, setFocused] = useState(false),
    [atEnd, setAtEnd] = useState(true),
    [composing, setComposing] = useState(false),
    [dismissed, setDismissed] = useState<string | null>(null);
  const stamp = JSON.stringify([completionKey, completion, value]);
  const visible =
    !!completion &&
    !composing &&
    atEnd &&
    !value.includes("\n") &&
    focused &&
    // One typed letter is too little to guess from.
    value.length !== 1 &&
    completion.toLowerCase().startsWith(value.toLowerCase()) &&
    completion.length > value.length &&
    dismissed !== stamp;
  const caret = () => {
    const el = ref.current;
    setAtEnd(
      !!el &&
        el.selectionStart === el.value.length &&
        el.selectionEnd === el.value.length &&
        el.scrollTop === 0 &&
        el.scrollLeft === 0,
    );
  };
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const fit = () => {
      const style = getComputedStyle(el),
        line = parseFloat(style.lineHeight) || 20,
        pad = parseFloat(style.paddingTop) + parseFloat(style.paddingBottom);
      const min = line * minRows + pad,
        max = line * maxRows + pad;
      el.style.height = "auto";
      const full = Math.max(
        el.scrollHeight,
        visible ? (ghost.current?.scrollHeight || 0) + pad : 0,
      );
      el.style.height = Math.max(min, Math.min(full, max)) + "px";
      el.style.overflowY = full > max + 1 ? "auto" : "hidden";
    };
    fit();
    if (typeof ResizeObserver === "undefined") return;
    let width = el.clientWidth;
    const observer = new ResizeObserver((entries) => {
      if (
        width === el.clientWidth &&
        !entries.some((entry) => entry.target === ghost.current)
      )
        return;
      width = el.clientWidth;
      fit();
    });
    observer.observe(el);
    if (ghost.current) observer.observe(ghost.current);
    return () => observer.disconnect();
  }, [value, minRows, maxRows, visible, completion]);
  const field = (
    <textarea
      {...rest}
      ref={ref}
      rows={minRows}
      className={className}
      aria-autocomplete={completion ? "inline" : rest["aria-autocomplete"]}
      aria-describedby={
        [rest["aria-describedby"], visible ? suggestionId : null]
          .filter(Boolean)
          .join(" ") || undefined
      }
      placeholder={visible ? "" : rest.placeholder}
      onFocus={(e) => {
        setFocused(true);
        caret();
        rest.onFocus?.(e);
      }}
      onBlur={(e) => {
        setFocused(false);
        rest.onBlur?.(e);
      }}
      onSelect={(e) => {
        caret();
        rest.onSelect?.(e);
      }}
      onScroll={(e) => {
        caret();
        rest.onScroll?.(e);
      }}
      onCompositionStart={(e) => {
        setComposing(true);
        rest.onCompositionStart?.(e);
      }}
      onCompositionEnd={(e) => {
        setComposing(false);
        caret();
        rest.onCompositionEnd?.(e);
      }}
      value={value}
      onChange={(e) => {
        setDismissed(null);
        onValueChange(e.target.value);
        caret();
      }}
      onKeyDown={(e) => {
        rest.onKeyDown?.(e);
        if (e.defaultPrevented || e.nativeEvent.isComposing || composing)
          return;
        if (
          visible &&
          e.key === "Tab" &&
          !e.shiftKey &&
          !e.altKey &&
          !e.ctrlKey &&
          !e.metaKey
        ) {
          e.preventDefault();
          onValueChange(completion!);
          requestAnimationFrame(() =>
            ref.current?.setSelectionRange(
              completion!.length,
              completion!.length,
            ),
          );
          return;
        }
        if (visible && e.key === "Escape") {
          e.preventDefault();
          setDismissed(stamp);
          return;
        }
        if (
          e.key === "Enter" &&
          !e.shiftKey &&
          !e.altKey &&
          !e.nativeEvent.isComposing
        ) {
          e.preventDefault();
          onSubmit();
        }
      }}
    />
  );
  if (completion === undefined && completionKey === undefined) return field;
  return (
    <div className="an-completion-field">
      {field}
      {visible && (
        <>
          <span className="an-completion" ref={ghost} aria-hidden>
            <span>{value}</span>
            {completion!.slice(value.length)}
          </span>
          <span id={suggestionId} className="hf-sr-only">
            Suggestion: {completion}. Tab to accept; Enter to send.
          </span>
        </>
      )}
    </div>
  );
}
