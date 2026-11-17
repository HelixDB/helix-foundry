import { useId, type ReactNode } from "react";
import { cx } from "../../kit";

/**
 * helix-foundry's ChoiceCard (apps/web/src/choice-card.tsx): one radio of
 * the group `name`, with its icon and what choosing it means. A disabled
 * one says why in its description.
 */
export function ChoiceCard({
  name,
  title,
  description,
  icon,
  checked,
  disabled,
  onChange,
}: {
  name: string;
  title: string;
  description: string;
  icon: ReactNode;
  checked: boolean;
  disabled?: boolean;
  onChange: () => void;
}) {
  const descriptionId = useId();
  return (
    <label className={cx("choice-card", checked && "selected")}>
      <span className="choice-card-icon" aria-hidden="true">
        {icon}
      </span>
      <span className="choice-card-heading">
        <input
          type="radio"
          name={name}
          checked={checked}
          disabled={disabled}
          onChange={onChange}
          aria-label={title}
          aria-describedby={descriptionId}
        />
        <strong>{title}</strong>
      </span>
      <span id={descriptionId} className="choice-card-description">
        {description}
      </span>
    </label>
  );
}
