import { describe, expect, it } from "vitest";
import { authorColor, authorHue, HUES } from "./colors";

describe("authorHue", () => {
  it("gives an author the same hue every time", () => {
    expect(authorHue("did:plc:alice")).toBe(authorHue("did:plc:alice"));
  });

  it("picks from the palette", () => {
    for (const d of ["did:plc:a", "did:plc:b", "did:web:example.com", ""]) expect(HUES).toContain(authorHue(d));
  });

  it("spreads authors over the palette", () => {
    const seen = new Set<number>();
    for (let i = 0; i < 60; i++) seen.add(authorHue(`did:plc:author${i}`));
    expect(seen.size).toBeGreaterThanOrEqual(6);
  });
});

describe("authorColor", () => {
  it("is darker on paper and lighter on the dark theme", () => {
    const lightness = (c: string) => Number(/(\d+)%\)$/.exec(c)?.[1]);
    expect(authorColor("did:plc:alice", false)).toMatch(/^hsl\(\d+ \d+% \d+%\)$/);
    expect(lightness(authorColor("did:plc:alice", false))).toBeLessThan(lightness(authorColor("did:plc:alice", true)));
  });
});
