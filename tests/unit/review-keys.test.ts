import { describe, expect, it } from "vitest";
import { keyCommand, type KeyCommand } from "@/web/areas/reviews/keys";

// The review walkthrough's keys (reviews/keys.ts keyCommand).

const press = (
  key: string,
  mods: Partial<{
    metaKey: boolean;
    ctrlKey: boolean;
    altKey: boolean;
    shiftKey: boolean;
  }> = {},
  { editable = false, choices = 3 } = {},
) =>
  keyCommand(
    {
      key,
      metaKey: false,
      ctrlKey: false,
      altKey: false,
      shiftKey: false,
      ...mods,
    },
    editable,
    choices,
  );

describe("keyCommand", () => {
  it("reads every key of the walk", () => {
    const keys: [string, KeyCommand][] = [
      ["Enter", { kind: "accept" }],
      ["1", { kind: "choose", index: 0 }],
      ["3", { kind: "choose", index: 2 }],
      ["n", { kind: "not_in_data" }],
      ["x", { kind: "reject" }],
      ["s", { kind: "skip" }],
      ["ArrowRight", { kind: "skip" }],
      ["ArrowLeft", { kind: "back" }],
      ["z", { kind: "undo" }],
      ["g", { kind: "batch" }],
      ["o", { kind: "open" }],
      ["f", { kind: "founder" }],
      // Caps Lock on.
      ["F", { kind: "founder" }],
    ];
    for (const [key, command] of keys) expect(press(key)).toEqual(command);
    expect(press("?", { shiftKey: true })).toEqual({ kind: "help" });
  });
  it("reads no digit past the card's choices, nor 0", () => {
    expect(press("4")).toBeNull();
    expect(press("9", {}, { choices: 9 })).toEqual({
      kind: "choose",
      index: 8,
    });
    expect(press("1", {}, { choices: 0 })).toBeNull();
    expect(press("0")).toBeNull();
  });
  it("reads nothing while the person types", () => {
    expect(press("Enter", {}, { editable: true })).toBeNull();
    expect(press("z", { metaKey: true }, { editable: true })).toBeNull();
  });
  it("leaves ⌘J, ⌘K and other modified keys to the shell", () => {
    expect(press("j", { metaKey: true })).toBeNull();
    expect(press("k", { metaKey: true })).toBeNull();
    expect(press("k", { ctrlKey: true })).toBeNull();
    expect(press("s", { altKey: true })).toBeNull();
    expect(press("S", { shiftKey: true })).toBeNull();
    expect(press("Z", { metaKey: true, shiftKey: true })).toBeNull();
    expect(press("q")).toBeNull();
    expect(press("Tab")).toBeNull();
  });
  it("reads ⌘Z and Ctrl+Z as Undo", () => {
    expect(press("z", { metaKey: true })).toEqual({ kind: "undo" });
    expect(press("z", { ctrlKey: true })).toEqual({ kind: "undo" });
  });
});
