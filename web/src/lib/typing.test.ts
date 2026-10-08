import { describe, expect, it } from "vitest";
import { OUT_PAUSE, PRE, reveal, totalTicks, type Step } from "./typing";

// Two windows: window 0 types a command and prints a line, then window 1 does.
const steps: Step[] = [
  { win: 0, kind: "cmd", text: "abcde" },
  { win: 0, kind: "out", text: "done" },
  { win: 1, kind: "cmd", text: "xyz" },
  { win: 1, kind: "out", text: "found" },
  { win: 1, kind: "prompt" },
];

describe("reveal", () => {
  it("starts with the first command showing nothing typed", () => {
    const s = reveal(steps, 0);
    expect(s[0]).toEqual({ visible: true, chars: 0 });
    expect(s.slice(1).every((x) => !x.visible)).toBe(true);
  });

  it("types one character per tick once the pause before typing is over", () => {
    expect(reveal(steps, PRE)[0].chars).toBe(0);
    expect(reveal(steps, PRE + 3)[0].chars).toBe(3);
    expect(reveal(steps, PRE + 5)[0].chars).toBe(5);
    expect(reveal(steps, PRE + 99)[0].chars).toBe(5);
  });

  it("prints output only after its command is typed and a pause has passed", () => {
    const typed = PRE + 5;
    expect(reveal(steps, typed)[1].visible).toBe(false);
    expect(reveal(steps, typed + OUT_PAUSE - 1)[1].visible).toBe(false);
    expect(reveal(steps, typed + OUT_PAUSE)[1].visible).toBe(true);
  });

  it("runs the second window only after the first window's output", () => {
    const outAt = PRE + 5 + OUT_PAUSE;
    expect(reveal(steps, outAt)[2].visible).toBe(true); // it begins right after
    expect(reveal(steps, outAt - 1)[2].visible).toBe(false);
    expect(reveal(steps, outAt)[2].chars).toBe(0);
  });

  it("shows everything, fully typed, at the end", () => {
    const s = reveal(steps, totalTicks(steps));
    expect(s.every((x) => x.visible)).toBe(true);
    expect(s[0].chars).toBe(5);
    expect(s[2].chars).toBe(3);
  });

  it("never takes anything back as time passes", () => {
    let before = reveal(steps, 0);
    for (let t = 1; t <= totalTicks(steps) + 5; t++) {
      const now = reveal(steps, t);
      now.forEach((x, i) => {
        expect(x.chars).toBeGreaterThanOrEqual(before[i].chars);
        if (before[i].visible) expect(x.visible).toBe(true);
      });
      before = now;
    }
  });

  it("copes with an empty script", () => {
    expect(reveal([], 10)).toEqual([]);
    expect(totalTicks([])).toBe(0);
  });
});
