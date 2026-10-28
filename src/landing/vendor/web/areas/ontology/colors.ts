// Categorical hues in a fixed order (the dataviz reference palette, validated
// for both themes). Types take them in alphabetical order of the type names a
// view passes in, and a ninth type and beyond share a neutral grey. Hiding a
// type in the graph or choosing one in the table does not change that list, so
// the colours stay put. The views pass different lists, though: the graph
// passes the types of the records it has loaded, which differ between the full
// graph and the graph focused on one record, and the table every type that has
// records. So a type's colour can differ between those three views.
const HUES = {
  light: [
    "#2a78d6",
    "#eb6834",
    "#1baf7a",
    "#eda100",
    "#e87ba4",
    "#008300",
    "#4a3aa7",
    "#e34948",
  ],
  dark: [
    "#3987e5",
    "#d95926",
    "#199e70",
    "#c98500",
    "#d55181",
    "#008300",
    "#9085e9",
    "#e66767",
  ],
};
export const isDark = () => {
  const theme = document.documentElement.dataset.theme;
  return theme
    ? theme === "dark"
    : matchMedia("(prefers-color-scheme: dark)").matches;
};
// `types` is every type name, sorted.
export function typeHue(types: string[], type: string) {
  const i = types.indexOf(type),
    hues = isDark() ? HUES.dark : HUES.light;
  return i >= 0 && i < hues.length ? hues[i] : "";
}
