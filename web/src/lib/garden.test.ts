import { describe, expect, it } from "vitest";
import { AUTHORS, CLUSTERS, NOTES, SUGGESTIONS, keysOf, neighbors, search, stem } from "./garden";

describe("garden example data", () => {
  it("has valid authors and clusters, and positions inside the unit square", () => {
    for (const n of NOTES) {
      expect(AUTHORS[n.author]).toBeDefined();
      expect(CLUSTERS[n.cluster]).toBeDefined();
      expect(n.x).toBeGreaterThan(0.02);
      expect(n.x).toBeLessThan(0.98);
      expect(n.y).toBeGreaterThan(0.05);
      expect(n.y).toBeLessThan(0.95);
    }
  });

  it("keeps notes from landing on top of each other", () => {
    for (const a of NOTES) {
      for (const b of NOTES) {
        if (a.id >= b.id) continue;
        // In a 900 by 480 field, 28px apart at least.
        const d = Math.hypot((a.x - b.x) * 900, (a.y - b.y) * 480);
        expect(d, `${a.id} and ${b.id}`).toBeGreaterThan(28);
      }
    }
  });

  it("has all three kinds of writer, so the demo can say it isn't only agents", () => {
    const kinds = new Set(NOTES.map((n) => AUTHORS[n.author].kind));
    expect(kinds).toEqual(new Set(["agent", "person", "script"]));
  });

  it("plants some notes late", () => {
    expect(NOTES.some((n) => n.late)).toBe(true);
    expect(NOTES.filter((n) => !n.late).length).toBeGreaterThan(25);
  });
});

describe("stem and keys", () => {
  it("matches plural, -ing and -ed forms", () => {
    expect(stem("Deploys")).toBe("deploy");
    expect(stem("editing")).toBe("edit");
    expect(stem("slowed")).toBe("slow");
    expect(stem("migrations")).toBe("migration");
  });

  it("gives words that mean about the same thing one key", () => {
    expect(keysOf("ship")).toEqual(keysOf("deploys"));
    expect(keysOf("how do we ship a change?").size).toBe(1);
  });
});

describe("search", () => {
  const top = (q: string) => NOTES[search(q)[0].id].text;

  it("finds notes that don't use the question's words", () => {
    const hits = search("how do we ship a change?").map((h) => NOTES[h.id].text.toLowerCase());
    // The deploy repo note is among them, and none of them say "ship".
    expect(hits.some((t) => t.includes("deploys go through the deploy repo's workflow"))).toBe(true);
    for (const t of hits) expect(t).not.toMatch(/\bship\b/);
  });

  const clusters = (q: string) => search(q).map((h) => NOTES[h.id].cluster);

  it("lights up the whole deploys patch for a question about shipping", () => {
    const c = clusters("how do we ship a change?");
    expect(c.length).toBeGreaterThanOrEqual(4);
    expect(new Set(c)).toEqual(new Set([0]));
  });

  it("answers a login question with the login notes and not with unrelated ones", () => {
    const hits = search("why did login break?");
    expect(NOTES[hits[0].id].text).toContain("Login breaks on Safari");
    expect(hits.length).toBeLessThanOrEqual(3);
    for (const h of hits) expect(NOTES[h.id].text).not.toContain("node:fs");
  });

  it("answers a precise question with a precise note", () => {
    expect(top("can I edit an old migration?")).toContain("append-only");
    expect(top("something is slow after a restart")).toContain("cold after a restart");
  });

  it("sends a newcomer to the onboarding patch", () => {
    const c = clusters("I'm new, where do I start?");
    expect(c.length).toBeGreaterThanOrEqual(4);
    expect(new Set(c)).toEqual(new Set([5]));
  });

  it("ignores words that fit any note", () => {
    expect(keysOf("an old new change").size).toBe(0);
  });

  it("scores from 1 downward and returns at most the limit", () => {
    for (const q of SUGGESTIONS) {
      const hits = search(q);
      expect(hits.length).toBeGreaterThan(0);
      expect(hits.length).toBeLessThanOrEqual(5);
      expect(hits[0].score).toBe(1);
      for (const h of hits) expect(h.score).toBeGreaterThan(0);
      expect([...hits].sort((a, b) => b.score - a.score)).toEqual(hits);
    }
  });

  it("returns nothing for a question with no overlap", () => {
    expect(search("xylophone zebra")).toEqual([]);
    expect(search("   ")).toEqual([]);
    expect(search("the and of")).toEqual([]);
  });

  it("only searches the notes it is given", () => {
    const planted = NOTES.filter((n) => !n.late);
    for (const h of search("deploy", planted)) expect(planted.some((n) => n.id === h.id)).toBe(true);
  });
});

describe("neighbors", () => {
  it("links notes only within their own patch, once each", () => {
    const edges = neighbors();
    expect(edges.length).toBeGreaterThan(20);
    const seen = new Set<string>();
    for (const [a, b] of edges) {
      expect(a).toBeLessThan(b);
      expect(NOTES[a].cluster).toBe(NOTES[b].cluster);
      const key = `${a}-${b}`;
      expect(seen.has(key)).toBe(false);
      seen.add(key);
    }
  });
});
