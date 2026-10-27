import { createContext, useContext, useEffect, type ReactNode } from "react";
import { createPortal } from "react-dom";

export type ShellApi = {
  openPalette: (query?: string) => void;
  // Shows or hides the open section's panel; on a phone, opens or closes
  // the drawer that holds it.
  togglePanel: () => void;
};
export const ShellContext = createContext<ShellApi>({
  openPalette: () => {},
  togglePanel: () => {},
});
// Lets any page open the command palette.
export const useShell = () => useContext(ShellContext);

/**
 * The open section's panel, while it shows, in a section whose pages fill
 * it (`SectionPanel` kind "page"): beside the sidebar, or in the phone's
 * drawer, where `onNavigate` closes the drawer (call it from anything that
 * takes the reader back to the page).
 */
export type PanelSlot = {
  element: HTMLElement;
  onNavigate?: () => void;
  // Names the panel `label` and hides its fallback until the returned
  // release runs.
  claim: (label: string) => () => void;
};
export const PanelSlotContext = createContext<PanelSlot | null>(null);
/** The open section's panel, while it shows and its pages fill it; else null. */
export const usePanelSlot = () => useContext(PanelSlotContext);

/**
 * Puts `children` in the open section's panel, named `label`, while the
 * panel shows (nothing otherwise, as when it is hidden or in a phone's
 * closed drawer). Portaled, they keep the page's state and context. One
 * per page, rendering something: the panel's fallback shows only while
 * none is mounted.
 *
 * @example
 * <PanelContent label="Labels"><LabelRail … /></PanelContent>
 */
export function PanelContent({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  const slot = usePanelSlot(),
    claim = slot?.claim;
  useEffect(() => claim?.(label), [claim, label]);
  return slot && createPortal(children, slot.element);
}
