import type { Element, ElementContent, Root, Text } from "hast";

/** A range of a Markdown source, in UTF-16 offsets, to show as a <mark>. */
export type MarkRun = {
  from: number;
  to: number;
  // The <mark>'s hast properties (className, id, dataAnchors…).
  properties: Element["properties"];
};

type Position = NonNullable<Text["position"]>;

/**
 * A rehype plugin that wraps each run's rendered text in a <mark>. Each text
 * node is aligned with its slice of `source` (its own position, else its
 * element's, as for code), so the Markdown syntax between a run's characters
 * (`**`, a backtick, an escape) stays out of it. A run that crosses elements
 * becomes one <mark> per text node, and only the first keeps the run's `id`.
 * Runs are sorted and don't overlap, as `segments` makes them; whitespace
 * between blocks is never marked. A <mark> inside a link (an `a`, even one
 * shown as text) gets `dataInLink`, so it never takes a click of its own.
 * Raw HTML, which the page shows as text (MDX's JSX, `<Callout>…`), becomes
 * text here and is marked as text is.
 *
 * @example
 * // "a **b** c", with a run over "b** c" (4 to 9):
 * // <p>a <strong><mark>b</mark></strong><mark> c</mark></p>
 * // "[b](https://b.test) c", with a run over "b" (1 to 2):
 * // <p><a …><mark data-in-link="true">b</mark></a> c</p>
 */
export function rehypeMarks({
  source,
  runs,
}: {
  source: string;
  runs: MarkRun[];
}) {
  // The run holding a source offset, by binary search.
  const runAt = (offset: number) => {
    let lo = 0,
      hi = runs.length - 1;
    while (lo <= hi) {
      const mid = (lo + hi) >> 1,
        r = runs[mid]!;
      if (offset < r.from) hi = mid - 1;
      else if (offset >= r.to) lo = mid + 1;
      else return r;
    }
    return undefined;
  };
  const placed = new Set<MarkRun>();
  // A text node as pieces: its characters found in order in its slice of the
  // source, cut where the run under them changes.
  const split = (
    text: Text,
    at: Position,
    inLink: boolean,
  ): ElementContent[] => {
    const end = at.end.offset ?? 0,
      value = text.value,
      pieces: ElementContent[] = [];
    let j = at.start.offset ?? end,
      from = 0,
      current: MarkRun | undefined;
    const flush = (to: number) => {
      if (to <= from) return;
      const piece: Text = { type: "text", value: value.slice(from, to) };
      from = to;
      if (!current) return void pieces.push(piece);
      const { id, ...rest } = current.properties;
      const properties = inLink ? { ...rest, dataInLink: true } : rest;
      pieces.push({
        type: "element",
        tagName: "mark",
        properties: placed.has(current) ? properties : { ...properties, id },
        children: [piece],
      });
      placed.add(current);
    };
    for (let i = 0; i < value.length; i++) {
      while (j < end && source[j] !== value[i]) j++;
      const run = j < end ? runAt(j) : undefined;
      if (run !== current) {
        flush(i);
        current = run;
      }
      j++;
    }
    flush(value.length);
    return pieces;
  };
  const walk = (node: Root | Element, at?: Position, inLink = false) => {
    (node as Element).children = node.children.flatMap(
      (child): ElementContent[] => {
        if (child.type === "element") {
          walk(child, child.position ?? at, inLink || child.tagName === "a");
          return [child];
        }
        // Raw HTML (JSX in MDX, which Markdown reads as HTML) is shown as
        // its text, so it is marked as text is.
        const raw = child as { type: string; value?: unknown };
        if (child.type !== "text" && raw.type !== "raw")
          return [child as ElementContent];
        const text: Text = {
          type: "text",
          value: String(raw.value ?? ""),
          position: child.position,
        };
        const position = text.position ?? (text.value.trim() ? at : undefined);
        return position ? split(text, position, inLink) : [text];
      },
    );
  };
  return (tree: Root) => {
    if (runs.length) walk(tree);
  };
}
