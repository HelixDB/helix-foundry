import { Dialog } from "../../../kit";

import { KBD } from "./ui";

const GROUPS: { title: string; rows: [string[], string][] }[] = [
  {
    title: "Explorer",
    rows: [
      [["/"], "Jump to a node"],
      [["["], "Show or hide labels"],
      [["]"], "Show or hide the inspector"],
      [["?"], "These shortcuts"],
      [["Esc"], "Close menus and search, then clear the selection"],
    ],
  },
  {
    title: "Selection",
    rows: [
      [["E"], "Expand the selected node"],
      [["H"], "Hide the selected node"],
      [["P"], "Pin or unpin the selected node"],
      [["Alt", "←"], "Back to the previous selection"],
    ],
  },
  {
    title: "Canvas (when focused)",
    rows: [
      [["←", "↑", "→", "↓"], "Move between nodes"],
      [["↵"], "Expand the selected node"],
      [["+", "−"], "Zoom"],
      [["0"], "Reset zoom"],
      [["F"], "Fit to view"],
      [["R"], "Re-run the layout"],
    ],
  },
];

/** Lists the keyboard shortcuts, in the kit's dialog (it shows in fullscreen too). */
export function ShortcutsDialog({
  open,
  onOpenChange,
  primaryModifier,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  primaryModifier: string;
}) {
  if (!open) return null;
  return (
    <Dialog
      title="Keyboard shortcuts"
      description={`Single keys work when you aren’t typing in a field. ${primaryModifier}K stays Foundry’s search.`}
      onClose={() => onOpenChange(false)}
      size="md"
    >
      <div className="ex-shortcuts">
        {GROUPS.map((group) => (
          <section key={group.title}>
            <h3 className="ex-section-title">{group.title}</h3>
            <dl>
              {group.rows.map(([keys, action]) => (
                <div key={action}>
                  <dt>{action}</dt>
                  <dd>
                    {keys.map((key) => (
                      <kbd key={key} className={KBD}>
                        {key}
                      </kbd>
                    ))}
                  </dd>
                </div>
              ))}
            </dl>
          </section>
        ))}
      </div>
    </Dialog>
  );
}
