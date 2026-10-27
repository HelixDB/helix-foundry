import { describe, expect, it } from "vitest";
import { blankMdx } from "../../mock/mdx";

// The mock's MDX blanker, as the WFE's reads MDX: code overwritten with
// spaces of the same UTF-8 length, newlines kept, code blocks and inline
// code left as written. The website's MDX page relies on the lengths.

const bytes = (s: string) => Buffer.byteLength(s, "utf8");

describe("blankMdx", () => {
  it("blanks import and export blocks to the next blank line", () => {
    const text = [
      'import { Callout } from "./callout";',
      "export const meta = {",
      '  title: "Q4",',
      "};",
      "",
      "# Q4 imports",
      "Exports grew.",
    ].join("\n");
    const out = blankMdx(text);
    expect(out.split("\n").slice(0, 4)).toEqual(
      text
        .split("\n")
        .slice(0, 4)
        .map((l) => " ".repeat(l.length)),
    );
    expect(out.split("\n").slice(4)).toEqual(text.split("\n").slice(4));
  });

  it("blanks expressions, nested and across lines, with their strings", () => {
    const text = 'Pay {fmt({ n: 1200, cur: "}" })} by {\n  props.day\n}.';
    const out = blankMdx(text);
    expect(out).toBe(
      "Pay " +
        " ".repeat('{fmt({ n: 1200, cur: "}" })}'.length) +
        " by  \n" +
        " ".repeat("  props.day".length) +
        "\n .",
    );
  });

  it("leaves code blocks, inline code and an unbalanced brace as written", () => {
    const text = [
      "Use `{a}` here, or ``{`b`}``.",
      "",
      "```js",
      "const x = { a: 1 };",
      "```",
      "",
      "An open { never closes.",
    ].join("\n");
    expect(blankMdx(text)).toBe(text);
  });

  it("keeps the byte length, the newlines and valid UTF-8 of any text", () => {
    const text = [
      "---",
      "title: “Größe”",
      "---",
      "import X from 'x' // €",
      "",
      "<Note>Café {“€” + '😀'} 😀</Note>",
      "Plain {",
    ].join("\n");
    const out = blankMdx(text);
    expect(bytes(out)).toBe(bytes(text));
    // Newlines sit at the same byte offsets.
    const lines = (s: string) => s.split("\n").map(bytes);
    expect(lines(out)).toEqual(lines(text));
    expect(Buffer.from(out, "utf8").toString("utf8")).toBe(out);
    expect(out).toContain("<Note>Café ");
    expect(out).toContain("😀</Note>");
    expect(out).not.toContain("€");
  });
});
