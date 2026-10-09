import { describe, expect, it } from "vitest";
import { fitTo, Layout, type Link, WIDTH, HEIGHT } from "./graphLayout";

function clique(ids: number[], w: number): Link[] {
  const out: Link[] = [];
  for (const a of ids) for (const b of ids) if (a < b) out.push({ a, b, w });
  return out;
}

function dist(p: { x: number; y: number }, q: { x: number; y: number }) {
  return Math.hypot(p.x - q.x, p.y - q.y);
}

describe("Layout", () => {
  const twoGroups = [...clique([0, 1, 2, 3, 4], 0.8), ...clique([5, 6, 7, 8, 9], 0.8), { a: 4, b: 5, w: 0.4 }];

  it("is the same every time for the same seed", () => {
    const a = new Layout(10, twoGroups, 7);
    const b = new Layout(10, twoGroups, 7);
    a.settle();
    b.settle();
    expect(a.points).toEqual(b.points);
    const c = new Layout(10, twoGroups, 8);
    c.settle();
    expect(c.points).not.toEqual(a.points);
  });

  it("keeps every note inside the picture", () => {
    const l = new Layout(60, clique([0, 1, 2], 0.9), 3);
    l.settle();
    for (const p of l.points) {
      expect(Number.isFinite(p.x) && Number.isFinite(p.y)).toBe(true);
      expect(p.x).toBeGreaterThanOrEqual(0);
      expect(p.x).toBeLessThanOrEqual(WIDTH);
      expect(p.y).toBeGreaterThanOrEqual(0);
      expect(p.y).toBeLessThanOrEqual(HEIGHT);
    }
  });

  it("puts linked notes closer together than unlinked ones", () => {
    const l = new Layout(10, twoGroups, 1);
    l.settle();
    const mean = (pairs: [number, number][]) =>
      pairs.reduce((s, [a, b]) => s + dist(l.points[a], l.points[b]), 0) / pairs.length;
    const within: [number, number][] = [];
    const across: [number, number][] = [];
    for (let a = 0; a < 10; a++)
      for (let b = a + 1; b < 10; b++) ((a < 5) === (b < 5) ? within : across).push([a, b]);
    expect(mean(within)).toBeLessThan(mean(across) * 0.7);
  });

  it("doesn't stack notes that have no links", () => {
    const l = new Layout(30, [], 5);
    l.settle();
    let nearest = Infinity;
    for (let a = 0; a < 30; a++) for (let b = a + 1; b < 30; b++) nearest = Math.min(nearest, dist(l.points[a], l.points[b]));
    expect(nearest).toBeGreaterThan(8);
  });

  it("starts from the positions it is given, and stays still once settled", () => {
    const from = Array.from({ length: 4 }, (_, i) => ({ x: 100 + i * 50, y: 200 }));
    const l = new Layout(4, [], 1, from);
    expect(l.points).toEqual(from);
    const settled = new Layout(10, twoGroups, 1);
    settled.settle(600);
    expect(settled.step()).toBeLessThan(0.5);
  });

  it("handles no notes and one note", () => {
    const none = new Layout(0, [], 1);
    none.settle();
    expect(none.points).toEqual([]);
    const one = new Layout(1, [], 1);
    one.settle();
    expect(one.points).toHaveLength(1);
  });
});

describe("fitTo", () => {
  it("scales and centers the notes in a box, keeping their shape", () => {
    const pts = [
      { x: 0, y: 0 },
      { x: 100, y: 0 },
      { x: 100, y: 50 },
    ];
    const f = fitTo(pts, 400, 400, 20);
    const at = (p: { x: number; y: number }) => ({ x: p.x * f.scale + f.ox, y: p.y * f.scale + f.oy });
    const a = at(pts[0]);
    const b = at(pts[1]);
    expect(a.x).toBeGreaterThanOrEqual(20 - 1e-9);
    expect(b.x).toBeLessThanOrEqual(380 + 1e-9);
    expect((b.x - a.x) / (at(pts[2]).y - b.y)).toBeCloseTo(2, 5);
    // Centered on both axes.
    const ys = pts.map((p) => at(p).y);
    expect((Math.min(...ys) + Math.max(...ys)) / 2).toBeCloseTo(200, 5);
  });

  it("copes with a single point", () => {
    const f = fitTo([{ x: 5, y: 5 }], 300, 200, 10);
    expect(f.scale).toBeGreaterThan(0);
    expect(5 * f.scale + f.ox).toBeCloseTo(150, 5);
    expect(5 * f.scale + f.oy).toBeCloseTo(100, 5);
  });
});
