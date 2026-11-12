// A WebVTT transcript (.vtt), as the WFE reads one: the backend's
// transcript package (backend/transcript), ported line for line, so the
// website shows a transcript as ontologies read it and the mock's loads read
// it as the WFE's do. Both read the backend's goldens
// (backend/transcript/testdata, tests/unit/transcript.test.ts): a change
// there fails here until this follows.

/** One speaker's words, from when they started saying them. */
export type Turn = {
  // The cue's voice (<v Ada Lovelace>); "" when it names none, and then the
  // turn is that one cue.
  speaker: string;
  // Milliseconds from the start of the recording.
  startMs: number;
  text: string;
};

/** What a WebVTT file holds that ontologies read. */
export type Transcript = {
  // The text of its WEBVTT line, as in "WEBVTT - Weekly sync", without the
  // dash; "" when it has none.
  title: string;
  // The lines of its NOTE blocks before the first cue.
  notes: string[];
  // Its cues with text, in the file's order, each run of cues one speaker
  // said in a row merged into one turn.
  turns: Turn[];
};

// The white space collapse folds: ASCII's, and the no-break space &nbsp; is.
const SPACE = /[ \t\n\v\f\r\u00a0]+/;

/** `s` trimmed of white space, each run of it one space. */
const collapse = (s: string) =>
  s
    .split(SPACE)
    .filter((w) => w !== "")
    .join(" ");

// A file's text without a byte order mark, its lines ended by LF.
const normalize = (src: string) =>
  src
    .replace(/^\uFEFF/, "")
    .replace(/\r\n/g, "\n")
    .replace(/\r/g, "\n");

// A line that is `word`, alone or followed by white space.
function keyword(line: string, word: string) {
  if (!line.startsWith(word)) return false;
  const rest = line.slice(word.length);
  return rest === "" || rest[0] === " " || rest[0] === "\t";
}

// The text after WEBVTT on a file's first line, or undefined when that line
// isn't WEBVTT, alone or followed by white space.
const signature = (line: string) =>
  keyword(line, "WEBVTT") ? line.slice("WEBVTT".length) : undefined;

// The named character references WebVTT tools write.
const REFERENCES: Record<string, string> = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  apos: "'",
  nbsp: "\u00a0",
  lrm: "",
  rlm: "",
};

// The character of a numeric reference's name (#38, #x26), or undefined
// for NUL, a surrogate, past the last character, or no number.
function numeric(name: string): string | undefined {
  if (!name.startsWith("#")) return undefined;
  let digits = name.slice(1).toLowerCase(),
    hex = false;
  if (digits.startsWith("x")) {
    digits = digits.slice(1);
    hex = true;
  }
  if (!(hex ? /^[0-9a-f]+$/ : /^[0-9]+$/).test(digits)) return undefined;
  const n = parseInt(digits, hex ? 16 : 10);
  if (!(n > 0) || n > 0x10ffff || (n >= 0xd800 && n <= 0xdfff))
    return undefined;
  return String.fromCodePoint(n);
}

// `s` with its character references decoded: the named ones of REFERENCES,
// each closed by ;, and numeric ones. Any other & is text.
function decode(s: string) {
  let out = "";
  for (;;) {
    const i = s.indexOf("&");
    if (i < 0) return out + s;
    out += s.slice(0, i);
    s = s.slice(i);
    const end = s.indexOf(";");
    const name = end >= 0 && end <= 10 ? s.slice(1, end) : undefined;
    const r =
      name === undefined
        ? undefined
        : Object.hasOwn(REFERENCES, name)
          ? REFERENCES[name]
          : numeric(name);
    if (r === undefined) {
      out += "&";
      s = s.slice(1);
    } else {
      out += r;
      s = s.slice(end + 1);
    }
  }
}

// A cue's speaker, from its first voice tag, and its words: its tags taken
// out, its references decoded and its white space collapsed. A < that
// nothing closes is text.
function cueText(payload: string): { speaker: string; words: string } {
  let speaker = "",
    out = "";
  for (let i = 0; i < payload.length; ) {
    if (payload[i] !== "<") {
      let j = payload.indexOf("<", i);
      if (j < 0) j = payload.length;
      out += decode(payload.slice(i, j));
      i = j;
      continue;
    }
    const end = payload.indexOf(">", i);
    if (end < 0) {
      out += decode(payload.slice(i));
      break;
    }
    const tag = payload.slice(i + 1, end);
    if (
      speaker === "" &&
      (tag === "v" || tag.startsWith("v.") || keyword(tag, "v"))
    ) {
      const k = tag.search(/[ \t\n]/);
      if (k >= 0) speaker = collapse(decode(tag.slice(k)));
    }
    i = end + 1;
  }
  return { speaker, words: collapse(out) };
}

// A cue's start in milliseconds: [hours:]minutes:seconds[.fraction], with
// minutes and seconds of one or two digits under 60 and a dot or a comma
// before the fraction, of which milliseconds count.
function timestamp(s: string): number | undefined {
  const t = s.replace(/,/g, "."),
    dot = t.indexOf(".");
  const whole = dot < 0 ? t : t.slice(0, dot),
    fraction = dot < 0 ? "" : t.slice(dot + 1);
  const parts = whole.split(":");
  if (parts.length < 2 || parts.length > 3 || fraction.includes("."))
    return undefined;
  const units: number[] = [];
  for (const [i, part] of parts.entries()) {
    if (!/^[0-9]+$/.test(part) || (i > 0 && part.length > 2) || part.length > 9)
      return undefined;
    const n = Number(part);
    if (i > 0 && n >= 60) return undefined;
    units.push(n);
  }
  if (units.length === 2) units.unshift(0);
  if (fraction !== "" && !/^[0-9]+$/.test(fraction)) return undefined;
  const ms = fraction === "" ? 0 : Number((fraction + "000").slice(0, 3));
  return ((units[0] * 60 + units[1]) * 60 + units[2]) * 1000 + ms;
}

/**
 * A WebVTT file's transcript, as the WFE reads it, or undefined when it
 * doesn't start with WEBVTT. Cues are blocks with a timing line, first or
 * after an identifier; a cue's speaker is its first voice tag's name; NOTE
 * blocks before the first cue are notes; anything else is left out.
 *
 * @example
 * parseTranscript("WEBVTT\n\n00:01.000 --> 00:02.000\n<v Ada>Hi.")?.turns;
 * // [{ speaker: "Ada", startMs: 1000, text: "Hi." }]
 * parseTranscript("1\n00:00:01,000 --> 00:00:02,000\nHi."); // undefined
 */
export function parseTranscript(src: string): Transcript | undefined {
  const lines = normalize(src).split("\n");
  const first = signature(lines[0]);
  if (first === undefined) return undefined;
  const t: Transcript = {
    title: collapse(collapse(first).replace(/^-/, "")),
    notes: [],
    turns: [],
  };
  let blocks: string[][] = [],
    block: string[] = [];
  for (const line of lines.slice(1)) {
    if (collapse(line) === "") {
      if (block.length) blocks.push(block);
      block = [];
    } else block.push(line);
  }
  if (block.length) blocks.push(block);
  // The header is the lines after WEBVTT up to the first blank line, unless
  // a timing line comes first: the cue it starts is the header's last lines,
  // from its identifier, the line before it, when there is one.
  if (lines.length > 1 && collapse(lines[1]) !== "" && blocks.length) {
    const [header, ...rest] = blocks;
    blocks = rest;
    const i = header.findIndex((line) => line.includes("-->"));
    if (i >= 0) blocks.unshift(header.slice(Math.max(i - 1, 0)));
  }
  let last: Turn | undefined,
    cued = false;
  for (const block of blocks) {
    if (keyword(block[0], "NOTE")) {
      if (!cued)
        for (const [i, line] of block.entries()) {
          const note = collapse(i === 0 ? line.slice("NOTE".length) : line);
          if (note) t.notes.push(note);
        }
      continue;
    }
    if (keyword(block[0], "STYLE") || keyword(block[0], "REGION")) continue;
    let timing = 0;
    if (!block[0].includes("-->")) {
      if (block.length < 2 || !block[1].includes("-->")) continue;
      timing = 1;
    }
    cued = true;
    const startMs = timestamp(collapse(block[timing].split("-->")[0]));
    if (startMs === undefined) continue;
    const { speaker, words } = cueText(block.slice(timing + 1).join("\n"));
    if (!words) continue;
    if (last && speaker && last.speaker === speaker) {
      last.text += " " + words;
      continue;
    }
    last = { speaker, startMs, text: words };
    t.turns.push(last);
  }
  return t;
}

/**
 * A turn's start as the WFE writes it: hours, minutes and seconds.
 *
 * @example
 * turnStart(3_723_900); // "01:02:03"
 */
export function turnStart(ms: number) {
  const s = Math.floor(ms / 1000),
    two = (n: number) => String(n).padStart(2, "0");
  return `${two(Math.floor(s / 3600))}:${two(Math.floor(s / 60) % 60)}:${two(s % 60)}`;
}

/**
 * A transcript as ontologies read it: its title and notes, a line each, then
 * a blank line and a line per turn: "[00:00:03] Ada Lovelace: Hello."
 */
export function renderTranscript(t: Transcript) {
  const header = t.title ? [t.title, ...t.notes] : t.notes;
  let out = header.map((line) => line + "\n").join("");
  if (header.length) out += "\n";
  for (const turn of t.turns)
    out += `[${turnStart(turn.startMs)}] ${turn.speaker ? turn.speaker + ": " : ""}${turn.text}\n`;
  return out;
}

/**
 * A WebVTT file's text as ontologies read it (renderTranscript), or `src`
 * as it is when it isn't one or holds no cue with text.
 *
 * @example
 * transcriptText("WEBVTT\n\n00:01.000 --> 00:02.000\n<v Ada>Hi."); // "[00:00:01] Ada: Hi.\n"
 */
export function transcriptText(src: string) {
  const t = parseTranscript(src);
  return t && t.turns.length ? renderTranscript(t) : src;
}
