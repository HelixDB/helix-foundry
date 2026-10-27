// Categorical hues in a fixed order: the --hf-type-1 to --hf-type-8 tokens
// (src/web/workspace.css, the dataviz reference palette validated for both
// themes; the explorer's canvas draws the same values, explorer/lib/theme.ts).
// A hue is a CSS variable, so it follows a theme switch without a render.
// Types take them in alphabetical order of the type names a view passes in,
// and a ninth type and beyond share a neutral grey. Hiding a type in the graph
// or choosing one in the table does not change that list, so the colours stay
// put. The views pass different lists, though: the graph passes the types of
// the records it has loaded, which differ between the full graph and the
// graph focused on one record, and the table every type that has records. So
// a type's colour can differ between those three views.
export const TYPE_HUES = 8;

// `types` is every type name, sorted. "" past the palette: the caller's grey.
export function typeHue(types: string[], type: string) {
  const i = types.indexOf(type);
  return i >= 0 && i < TYPE_HUES ? `var(--hf-type-${i + 1})` : "";
}
