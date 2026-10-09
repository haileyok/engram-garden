// Where to put each memory in the graph: a force layout in which notes repel
// each other, linked notes pull together (harder the more alike they are), and
// a weak pull to the middle keeps notes with no links from drifting away. The
// same seed gives the same picture.

export const WIDTH = 1000;
export const HEIGHT = 640;
const MARGIN = 10;

export type Point = { x: number; y: number };
// A link between notes a and b; w is how alike they are, from 0 to 1.
export type Link = { a: number; b: number; w: number };

// mulberry32: a small seeded random number generator.
function random(seed: number): () => number {
  let s = seed >>> 0;
  return () => {
    s = (s + 0x6d2b79f5) >>> 0;
    let t = s;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const COOLING = 0.985;
const COLDEST = 0.04;

export class Layout {
  points: Point[];
  private links: Link[];
  private k: number;
  private temperature: number;
  private rand: () => number;

  // `from` gives where notes already are, so a changed graph moves from the
  // old picture instead of starting over.
  constructor(count: number, links: Link[], seed = 1, from?: Point[]) {
    this.rand = random(seed);
    this.links = links;
    this.k = 0.75 * Math.sqrt((WIDTH * HEIGHT) / Math.max(1, count));
    this.points = Array.from({ length: count }, (_, i) =>
      from && from[i]
        ? { x: from[i].x, y: from[i].y }
        : { x: MARGIN + this.rand() * (WIDTH - 2 * MARGIN), y: MARGIN + this.rand() * (HEIGHT - 2 * MARGIN) },
    );
    this.temperature = from && from.length >= count ? 18 : 90;
  }

  // One round of forces. It returns how far the notes moved in all.
  step(): number {
    const n = this.points.length;
    const { points, k } = this;
    const dx = new Float64Array(n);
    const dy = new Float64Array(n);
    for (let i = 0; i < n; i++) {
      for (let j = i + 1; j < n; j++) {
        let x = points[i].x - points[j].x;
        let y = points[i].y - points[j].y;
        let d = Math.hypot(x, y);
        if (d < 0.01) {
          x = this.rand() - 0.5;
          y = this.rand() - 0.5;
          d = Math.hypot(x, y);
        }
        const f = (k * k) / d / d;
        dx[i] += x * f;
        dy[i] += y * f;
        dx[j] -= x * f;
        dy[j] -= y * f;
      }
    }
    for (const { a, b, w } of this.links) {
      const x = points[a].x - points[b].x;
      const y = points[a].y - points[b].y;
      const d = Math.hypot(x, y) || 0.01;
      const f = ((0.4 + w) * d) / k;
      dx[a] -= x * f;
      dy[a] -= y * f;
      dx[b] += x * f;
      dy[b] += y * f;
    }
    let moved = 0;
    for (let i = 0; i < n; i++) {
      dx[i] -= (points[i].x - WIDTH / 2) * 0.02 * (k / 20);
      dy[i] -= (points[i].y - HEIGHT / 2) * 0.02 * (k / 20);
      const len = Math.hypot(dx[i], dy[i]);
      if (len === 0) continue;
      const m = Math.min(len, this.temperature);
      const x = Math.min(WIDTH - MARGIN, Math.max(MARGIN, points[i].x + (dx[i] / len) * m));
      const y = Math.min(HEIGHT - MARGIN, Math.max(MARGIN, points[i].y + (dy[i] / len) * m));
      moved += Math.hypot(x - points[i].x, y - points[i].y);
      points[i] = { x, y };
    }
    this.temperature = Math.max(COLDEST, this.temperature * COOLING);
    return moved;
  }

  // Run until the notes stop moving, or `rounds` rounds.
  settle(rounds = 300): void {
    for (let i = 0; i < rounds; i++) {
      if (this.step() < this.points.length * 0.02 && this.temperature < 2) return;
    }
  }

  // Whether the notes have stopped moving about.
  get cool(): boolean {
    return this.temperature < 2;
  }
}

// How to scale and shift points so they fill a box, with padding, in the
// same shape. Draw a point at (x * scale + ox, y * scale + oy).
export function fitTo(points: Point[], width: number, height: number, pad: number) {
  if (points.length === 0) return { scale: 1, ox: width / 2, oy: height / 2 };
  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  for (const p of points) {
    minX = Math.min(minX, p.x);
    maxX = Math.max(maxX, p.x);
    minY = Math.min(minY, p.y);
    maxY = Math.max(maxY, p.y);
  }
  const bw = maxX - minX;
  const bh = maxY - minY;
  const sx = bw > 0 ? (width - 2 * pad) / bw : Infinity;
  const sy = bh > 0 ? (height - 2 * pad) / bh : Infinity;
  let scale = Math.min(sx, sy);
  if (!Number.isFinite(scale) || scale <= 0) scale = 1;
  return {
    scale,
    ox: width / 2 - ((minX + maxX) / 2) * scale,
    oy: height / 2 - ((minY + maxY) / 2) * scale,
  };
}
