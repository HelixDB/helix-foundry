import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { Markdown } from "@/web/areas/documents/markdown";

// A Markdown document, rendered: GitHub's flavor, no raw HTML, no images
// loaded, and only http, https and mailto links, each in a new tab.

const html = (text: string) =>
  renderToStaticMarkup(createElement(Markdown, { text }));

describe("Markdown documents", () => {
  it("renders GitHub's flavor, headings one level under the page's", () => {
    const out = html(
      [
        "# Q4",
        "## Orders",
        "| SKU | Product |",
        "|---|---|",
        "| A1 | **Widget** |",
        "",
        "- [x] done",
        "- [ ] ~~dropped~~",
        "",
        "```",
        "let a = 1;",
        "```",
      ].join("\n"),
    );
    expect(out).toContain("<h2>Q4</h2>");
    expect(out).toContain("<h3>Orders</h3>");
    expect(out).not.toContain("<h1");
    expect(out).toContain('<div class="dc-md-table"><table>');
    expect(out).toContain("<strong>Widget</strong>");
    expect(out).toContain('type="checkbox"');
    expect(out).toContain("<del>dropped</del>");
    expect(out).toContain("<pre><code>let a = 1;\n</code></pre>");
  });

  it("shows raw HTML as text and never loads an image", () => {
    const out = html(
      '<img src="https://x.test/p.png">\n\n![Q4 chart](https://x.test/c.png) ![](rel.png)',
    );
    expect(out).not.toContain("<img");
    expect(out).toContain("&lt;img");
    expect(out).toContain("Image: Q4 chart");
    expect(out).not.toContain("x.test/c.png");
    expect(out).not.toContain("rel.png");
  });

  it("links only http, https and mailto, in a new tab", () => {
    const out = html(
      "[a](https://a.test) [b](mailto:b@b.test) [c](javascript:alert(1)) [d](./d.md) [e](#e) www.f.test",
    );
    for (const href of [
      "https://a.test",
      "mailto:b@b.test",
      "http://www.f.test",
    ])
      expect(out).toContain(
        `<a href="${href}" target="_blank" rel="noreferrer noopener">`,
      );
    expect(out.match(/<a /g)).toHaveLength(3);
    expect(out).toContain("<span>c</span>");
    expect(out).toContain("<span>d</span>");
    expect(out).toContain("<span>e</span>");
  });
});

// An ontology's extraction over a Markdown document: runs of the source shown
// as <mark>s in the rendered document (rehypeMarks).

const marked = (text: string, runs: [number, number, string?][]) =>
  renderToStaticMarkup(
    createElement(Markdown, {
      text,
      marks: runs.map(([from, to, id]) => ({
        from,
        to,
        properties: id ? { id } : {},
      })),
    }),
  );

describe("Marks on a Markdown document", () => {
  it("leaves the syntax out of a run, cutting it where elements do", () => {
    // "b** c": the run starts inside the bold and ends after it; only its
    // first piece keeps its id.
    expect(marked("a **b** c", [[4, 9, "s0-4"]])).toBe(
      '<div class="dc-markdown" aria-label="Document text"><p>a <strong><mark id="s0-4">b</mark></strong><mark> c</mark></p></div>',
    );
  });

  it("marks headings, inline code, escapes, entities and code blocks", () => {
    const text = [
      "# Title",
      "",
      "Use `run()` on \\*AT&amp;T\\*.",
      "",
      "```",
      "let a = 1;",
      "```",
    ].join("\n");
    const at = (s: string) => [text.indexOf(s), text.indexOf(s) + s.length];
    const out = marked(text, [
      [at("Title")[0]!, at("Title")[1]!],
      [at("run()")[0]!, at("run()")[1]!],
      [at("\\*AT")[0]!, at("T\\*")[1]!],
      [at("a = 1")[0]!, at("a = 1")[1]!],
    ]);
    expect(out).toContain("<h2><mark>Title</mark></h2>");
    expect(out).toContain("<code><mark>run()</mark></code>");
    expect(out).toContain("<mark>*AT&amp;T*</mark>");
    expect(out).toContain("<pre><code>let <mark>a = 1</mark>;\n</code></pre>");
  });

  it("never marks the whitespace between blocks", () => {
    const text = "- one\n- two\n\n| A |\n|---|\n| b |";
    const out = marked(text, [[0, text.length]]);
    expect(out).toContain("<li><mark>one</mark></li>");
    expect(out).toContain("<li><mark>two</mark></li>");
    expect(out).toContain("<td><mark>b</mark></td>");
    expect(out).not.toMatch(/<(ul|table|thead|tbody|tr)>\s*<mark/);
    expect(out).not.toMatch(/<mark>\s+<\/mark>/);
  });

  it("flags the marks inside a link, shown as one or as text", () => {
    const text = "[Globex](https://g.test), [Initech](./i.md) and Globex";
    const at = (s: string, from = 0) => {
      const i = text.indexOf(s, from);
      return [i, i + s.length] as [number, number];
    };
    const out = marked(text, [
      [...at("Globex"), "s0-1"],
      at("Initech"),
      at("Globex", 30),
    ]);
    expect(out).toContain(
      '<a href="https://g.test" target="_blank" rel="noreferrer noopener"><mark data-in-link="true" id="s0-1">Globex</mark></a>',
    );
    expect(out).toContain(
      '<span><mark data-in-link="true">Initech</mark></span>',
    );
    expect(out).toContain(" and <mark>Globex</mark>");
    expect(out.match(/data-in-link/g)).toHaveLength(2);
  });

  it("renders as before without runs", () => {
    const text = "Some **bold** text";
    expect(marked(text, [])).toBe(
      renderToStaticMarkup(createElement(Markdown, { text })),
    );
  });
});

// MDX, shown as Markdown: never compiled or run, its JSX as written (HTML to
// Markdown, so text), its frontmatter a code block, and a load's marks over
// it, inside JSX too (raw nodes).

describe("MDX documents", () => {
  const mdx = (text: string, runs: [number, number][] = []) =>
    renderToStaticMarkup(
      createElement(Markdown, {
        text,
        mdx: true,
        marks: runs.map(([from, to]) => ({ from, to, properties: {} })),
      }),
    );
  const text = [
    "---",
    "title: Globex renewal",
    "---",
    'import { Callout } from "./callout";',
    "",
    "# Renewal",
    "",
    '<Callout type="info">Globex Corporation renews.</Callout>',
    "",
    "<Note>",
    "Initech LLC pays {props.amount} a year.",
    "</Note>",
  ].join("\n");
  const at = (s: string): [number, number] => [
    text.indexOf(s),
    text.indexOf(s) + s.length,
  ];

  it("shows the frontmatter as a code block and the JSX as written", () => {
    const out = mdx(text);
    expect(out).toContain("<pre><code>title: Globex renewal</code></pre>");
    expect(out).not.toContain("<hr");
    expect(out).toContain(
      "<p>import { Callout } from &quot;./callout&quot;;</p>",
    );
    expect(out).toContain(
      "&lt;Callout type=&quot;info&quot;&gt;Globex Corporation renews.&lt;/Callout&gt;",
    );
    expect(out).toContain("{props.amount}");
    // Markdown has no frontmatter: the same text is a rule and a heading.
    expect(html(text)).toContain("<hr/>");
  });

  it("marks text inside JSX, inline and as a block, and in the frontmatter", () => {
    const out = mdx(text, [
      at("Globex renewal"),
      at("Globex Corporation"),
      at("Initech LLC"),
    ]);
    expect(out).toContain("<code>title: <mark>Globex renewal</mark></code>");
    expect(out).toContain(
      "&lt;Callout type=&quot;info&quot;&gt;<mark>Globex Corporation</mark> renews.",
    );
    expect(out).toContain("<mark>Initech LLC</mark> pays {props.amount}");
  });
});
