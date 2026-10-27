import type { ReactNode } from "react";

// The agent writes its summary in a little Markdown: paragraphs, **bold**,
// `code` and "- " bullets, nested by two spaces. This renders that much as
// elements (never as HTML), and anything else as the text it is.

function inline(text: string): ReactNode[] {
  return text
    .split(/(\*\*[^*]+\*\*|`[^`]+`)/g)
    .map((part, i) =>
      part.startsWith("**") && part.endsWith("**") && part.length > 4 ? (
        <strong key={i}>{part.slice(2, -2)}</strong>
      ) : part.startsWith("`") && part.endsWith("`") && part.length > 2 ? (
        <code key={i}>{part.slice(1, -1)}</code>
      ) : (
        part
      ),
    );
}

type Item = { text: string; children: Item[] };
function list(lines: string[]): Item[] {
  const root: Item[] = [],
    stack: { depth: number; items: Item[] }[] = [{ depth: -1, items: root }];
  let last: Item | undefined;
  for (const line of lines) {
    // A wrapped line continues the bullet before it.
    if (!/^\s*- /.test(line)) {
      if (last) last.text += " " + line.trim();
      continue;
    }
    const depth = line.search(/\S/),
      item: Item = { text: line.trim().slice(2), children: [] };
    while (stack.length > 1 && stack.at(-1)!.depth >= depth) stack.pop();
    stack.at(-1)!.items.push(item);
    stack.push({ depth, items: item.children });
    last = item;
  }
  return root;
}
function Bullets({ items }: { items: Item[] }) {
  return (
    <ul>
      {items.map((item, i) => (
        <li key={i}>
          {inline(item.text)}
          {item.children.length > 0 && <Bullets items={item.children} />}
        </li>
      ))}
    </ul>
  );
}

export function Prose({
  text,
  className,
}: {
  text: string;
  className?: string;
}) {
  const blocks = text.trim().split(/\n\s*\n/);
  return (
    <div className={className}>
      {blocks.map((block, i) => {
        const lines = block.split("\n");
        const first = lines.findIndex((l) => /^\s*- /.test(l));
        if (first < 0) return <p key={i}>{inline(lines.join(" "))}</p>;
        // A heading line (or a few) may come before the bullets.
        return (
          <div key={i}>
            {first > 0 && <p>{inline(lines.slice(0, first).join(" "))}</p>}
            <Bullets items={list(lines.slice(first))} />
          </div>
        );
      })}
    </div>
  );
}
