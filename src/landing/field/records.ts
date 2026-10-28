// The Explorer's records: a slice of the Loopwell sample workspace (ten
// accounts and what links to them), laid out with the same forces and coloured
// with the same hues as the app's ObjectGraph.
import { radiusOf } from "./force";
import { typeHue } from "../vendor/web/areas/ontology/colors";
import data from "../data/loopwell-explorer.json";

export type RecordNode = {
  id: string;
  type: string;
  label: string;
  key: string;
  degree: number;
  props: string[][];
};
export const records: RecordNode[] = data.nodes;
export const links = data.edges as [number, number, string][];
export const tour: number[] = data.tour;
export const totals = { records: data.total, types: data.types };

// The graph passes the types of the records it has loaded, sorted.
export const types = [...new Set(records.map((r) => r.type))].sort();
const rgb = (hex: string) =>
  [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16)) as [
    number,
    number,
    number,
  ];
export const hue = (type: string) => typeHue(types, type) || "#6f7168";
export const hueRgb = types.map((t) => rgb(hue(t)));
export const typeIndex = records.map((r) => types.indexOf(r.type));

// Each record's links, both ways, for the panel and the highlight.
export type Neighbour = { index: number; name: string; out: boolean };
export const neighbours: Neighbour[][] = records.map(() => []);
for (const [from, to, name] of links) {
  neighbours[from].push({ index: to, name, out: true });
  neighbours[to].push({ index: from, name, out: false });
}

// The app's radius for a record with this many links.
export const radius = records.map((r) => radiusOf(r.degree));

// Positions in layout units, centred on 0, as the ObjectGraph's simulation
// settles them (force.ts, run by the export).
export const layout = () => ({
  x: Float32Array.from(data.layout.x),
  y: Float32Array.from(data.layout.y),
  w: data.layout.w,
  h: data.layout.h,
});
