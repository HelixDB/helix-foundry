import "./markdown.css";
import type { ComponentProps } from "react";
import ReactMarkdown, {
  type Components,
  type ExtraProps,
} from "react-markdown";
import remarkFrontmatter from "remark-frontmatter";
import remarkGfm from "remark-gfm";
import type { Element } from "hast";
import { ImageIcon } from "lucide-react";
import { Tag } from "../../kit";
import { rehypeMarks, type MarkRun } from "./marks";

// A Markdown document, rendered: GitHub's flavor (tables, task lists,
// strikethrough, autolinks), never its raw HTML (shown as text). Loaded on
// its own, when a Markdown document is shown.
// - Links open in a new tab, and only http, https and mailto ones: any
//   other (relative, javascript:, a footnote's #) is its text.
// - Images aren't loaded: an outside one would tell its host who reads the
//   document, and a relative one has nothing to resolve against. Each is a
//   chip with its alt text.
// - Headings sit one level under the page's own (its h1).
// - `marks` are runs of the source shown as <mark>s (./marks.ts), drawn by
//   `mark`: an ontology's extraction over the text it read.
// - `mdx`: MDX is shown as Markdown, never compiled or run. Its JSX is HTML
//   to Markdown, shown as written; its `import`s and `{…}` are text; its
//   YAML frontmatter is a code block.

const LINKABLE = new Set(["http:", "https:", "mailto:"]);

const below = (Level: "h2" | "h3" | "h4" | "h5" | "h6") =>
  function Heading({ node: _, ...props }: ComponentProps<"h1"> & ExtraProps) {
    return <Level {...props} />;
  };

const components: Components = {
  h1: below("h2"),
  h2: below("h3"),
  h3: below("h4"),
  h4: below("h5"),
  h5: below("h6"),
  h6: below("h6"),
  a({ node: _, href, children, ...props }) {
    if (!LINKABLE.has(URL.parse(href ?? "")?.protocol ?? ""))
      return <span>{children}</span>;
    return (
      <a {...props} href={href} target="_blank" rel="noreferrer noopener">
        {children}
      </a>
    );
  },
  img: ({ alt }) => (
    <Tag icon={ImageIcon} className="dc-md-image">
      {alt ? `Image: ${alt}` : "Image"}
    </Tag>
  ),
  table: ({ node: _, ...props }) => (
    <div className="dc-md-table">
      <table {...props} />
    </div>
  ),
};

// MDX's frontmatter, as remark-frontmatter reads it, as a code block where
// it was written (marks align to its position).
const frontmatter = {
  handlers: {
    yaml: (
      _: unknown,
      node: { value: string; position?: Element["position"] },
    ): Element => ({
      type: "element",
      tagName: "pre",
      properties: {},
      position: node.position,
      children: [
        {
          type: "element",
          tagName: "code",
          properties: {},
          children: [{ type: "text", value: node.value }],
        },
      ],
    }),
  },
};

export function Markdown({
  text,
  mdx = false,
  marks,
  mark,
}: {
  text: string;
  mdx?: boolean;
  marks?: MarkRun[];
  mark?: Components["mark"];
}) {
  return (
    <div className="dc-markdown" aria-label="Document text">
      <ReactMarkdown
        remarkPlugins={mdx ? [remarkGfm, remarkFrontmatter] : [remarkGfm]}
        remarkRehypeOptions={mdx ? frontmatter : undefined}
        rehypePlugins={
          marks ? [[rehypeMarks, { source: text, runs: marks }]] : undefined
        }
        components={mark ? { ...components, mark } : components}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
}
