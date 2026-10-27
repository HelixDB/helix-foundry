// MDX as the WFE reads it (the backend's document/mdx.go, BlankMDX): its
// code overwritten with spaces of the same UTF-8 length, newlines kept, then
// read as Markdown, so every byte offset into what loads read is the
// original's. Code is an `import` or `export` block (from a line starting
// with one to the next blank line) and a `{…}` expression (nested braces and
// the strings in it included, across lines), outside fenced and inline
// code. An unbalanced `{` is text. Simpler than the WFE's: the website
// relies only on the lengths and newlines being kept.

const ESM = /^(?:import|export)\b/;
const FENCE = /^ {0,3}(`{3,}|~{3,})/;

/** Spaces as long, in UTF-8 bytes, as `text`, with its newlines kept. */
const blank = (text: string) =>
  [...text]
    .map((ch) => (ch === "\n" ? "\n" : " ".repeat(Buffer.byteLength(ch))))
    .join("");

/** The index past the `}` closing the `{` at `from`, or -1 when none does. */
function closing(text: string, from: number): number {
  let depth = 0;
  for (let i = from; i < text.length; i++) {
    const ch = text[i]!;
    if (ch === '"' || ch === "'" || ch === "`") {
      const end = text.indexOf(ch, i + 1);
      if (end < 0) return -1;
      i = end;
    } else if (ch === "{") depth += 1;
    else if (ch === "}" && --depth === 0) return i + 1;
  }
  return -1;
}

/**
 * The text a load reads of an MDX document.
 *
 * @example
 * blankMdx('import X from "x";\n\nPay {props.amount} to `{a}`.');
 * // '                  \n\nPay                to `{a}`.'
 */
export function blankMdx(text: string): string {
  let out = "",
    fence = "",
    esm = false;
  for (let i = 0; i < text.length; ) {
    const newline = text.indexOf("\n", i),
      end = newline < 0 ? text.length : newline + 1,
      line = text.slice(i, newline < 0 ? text.length : newline);
    // Fences and import/export blocks start lines; an expression that ran
    // past a line's end carries on mid-line.
    if (i === 0 || text[i - 1] === "\n") {
      const opened = FENCE.exec(line)?.[1];
      const whole = (blanked: boolean) => {
        out += blanked ? blank(text.slice(i, end)) : text.slice(i, end);
        i = end;
      };
      if (fence) {
        if (opened?.[0] === fence[0] && opened.length >= fence.length)
          fence = "";
        whole(false);
        continue;
      }
      if (opened) {
        fence = opened;
        whole(false);
        continue;
      }
      if (!line.trim()) esm = false;
      else if (esm || ESM.test(line)) esm = true;
      if (esm) {
        whole(true);
        continue;
      }
    }
    for (let j = i; ; ) {
      if (j >= end) {
        i = j;
        break;
      }
      const ch = text[j]!;
      if (ch === "`") {
        const run = /^`+/.exec(text.slice(j))![0],
          close = text.indexOf(run, j + run.length);
        if (close >= 0 && close < end) {
          out += text.slice(j, close + run.length);
          j = close + run.length;
          continue;
        }
      }
      if (ch === "{") {
        const close = closing(text, j);
        if (close > 0) {
          out += blank(text.slice(j, close));
          j = close;
          // Past this line: the rest of the line it ends on is scanned next.
          if (j > end) {
            i = j;
            break;
          }
          continue;
        }
      }
      out += ch;
      j += 1;
    }
  }
  return out;
}
