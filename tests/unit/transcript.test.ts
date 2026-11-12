import { readFileSync, readdirSync } from "node:fs";
import { describe, expect, it } from "vitest";
import {
  parseTranscript,
  renderTranscript,
  transcriptText,
  turnStart,
} from "../../src/shared/transcript";

// The website's port of the backend's transcript package reads the
// backend's own goldens: each testdata/*.vtt reads as its .txt, byte for
// byte, so what the Data page shows and the mock's loads read is what the
// WFE's loads read.
const testdata = new URL("../../backend/transcript/testdata/", import.meta.url);
const goldens = readdirSync(testdata).filter((f) => f.endsWith(".vtt"));

describe("transcriptText", () => {
  it("has the backend's goldens to read", () => {
    expect(goldens).toEqual(
      expect.arrayContaining(["teams.vtt", "zoom.vtt", "header.vtt"]),
    );
  });

  it.each(goldens)("reads %s as the backend does", (name) => {
    const src = readFileSync(new URL(name, testdata), "utf8"),
      want = readFileSync(new URL(name.replace(/\.vtt$/, ".txt"), testdata));
    expect(Buffer.from(transcriptText(src), "utf8").equals(want)).toBe(true);
    expect(transcriptText(src)).toBe(want.toString("utf8"));
  });

  it("reads a file that isn't WebVTT, or has no cue with text, as it is", () => {
    for (const src of ["", "webvtt\n", "WEBVTTX", " WEBVTT\n", "WEBVTT\n"])
      expect(transcriptText(src)).toBe(src);
    expect(parseTranscript("WEBVTT\tTitle")?.title).toBe("Title");
    expect(parseTranscript("\uFEFFWEBVTT\r\n")).toBeDefined();
  });
});

describe("parseTranscript", () => {
  it("merges a speaker's cues in a row into one turn", () => {
    const t = parseTranscript(
      readFileSync(new URL("teams.vtt", testdata), "utf8"),
    );
    expect(t?.turns.map((turn) => [turn.speaker, turn.startMs])).toEqual([
      ["Ada Lovelace", 3120],
      ["Grace Hopper", 9880],
      ["Ada Lovelace", 14360],
      ["Grace Hopper", 62500],
    ]);
  });

  it("reads the title and the notes before the first cue", () => {
    const t = parseTranscript(
      readFileSync(new URL("header.vtt", testdata), "utf8"),
    );
    expect(t?.title).toBe("Weekly sync");
    expect(t?.notes).toEqual([
      "Recorded 2026-11-10",
      "Attendees: Ada Lovelace, Grace Hopper",
      "Organizer: ada@example.com",
    ]);
  });

  it("decodes references as the backend does", () => {
    const words = (payload: string) =>
      parseTranscript(`WEBVTT\n\n00:01.000 --> 00:02.000\n${payload}`)?.turns[0]
        ?.text;
    expect(words("&#38;&#x26;&#X26; &lt;&gt;&quot;&apos;")).toBe(`&&& <>"'`);
    expect(words("&#0; &#xD800; &#x110000; &copy; &")).toBe(
      "&#0; &#xD800; &#x110000; &copy; &",
    );
    expect(words("x&lrm;y&rlm;z&nbsp;w")).toBe("xyz w");
  });
});

describe("renderTranscript", () => {
  it("writes each turn's start in hours, minutes and seconds", () => {
    expect(turnStart(3 * 3600_000 + 4 * 60_000 + 5_900)).toBe("03:04:05");
    expect(
      renderTranscript({
        title: "Sync",
        notes: [],
        turns: [
          { speaker: "Ada", startMs: 0, text: "Hi." },
          { speaker: "", startMs: 61_000, text: "No speaker." },
        ],
      }),
    ).toBe("Sync\n\n[00:00:00] Ada: Hi.\n[00:01:01] No speaker.\n");
  });
});
