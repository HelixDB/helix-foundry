// How far under the sections the line above a section's sidebar content (Ask's
// threads) sits, in pixels. 0 is right under their links, so it never covers
// them. `room` is the height the offset and the content share.

export const SECTION_OFFSET_KEY = "foundry.sidebar.sectionOffset",
  SECTION_OFFSET_DEFAULT = 28,
  // What the content always keeps: New thread and a few threads.
  SECTION_MIN_HEIGHT = 160;

export const maxSectionOffset = (room: number) =>
  Math.max(0, room - SECTION_MIN_HEIGHT);

export const clampSectionOffset = (offset: number, room: number) =>
  Math.round(Math.min(Math.max(0, offset), maxSectionOffset(room)));

/** What localStorage holds: anything but a pixel count is the default. */
export function storedSectionOffset(stored: string) {
  const offset = Number(stored);
  return stored.trim() && Number.isFinite(offset) && offset >= 0
    ? offset
    : SECTION_OFFSET_DEFAULT;
}
