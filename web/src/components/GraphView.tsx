import { useCallback, useEffect, useMemo, useRef, useState, type CSSProperties, type KeyboardEvent } from "react";
import { api, type MemoryGraph } from "../api";
import { useSession } from "../App";
import { authorColor, authorHue } from "../lib/colors";
import { fitTo, Layout, type Link } from "../lib/graphLayout";
import type { Recall } from "../lib/useRecall";
import { Handle, useHandle } from "../profiles";
import { describeError } from "../ui";
import { Note } from "./Note";

// How much of the space to draw, and how many links each memory may have. The
// slider only hides links from what was fetched.
const NODE_LIMIT = 300;
const NEIGHBORS = 5;
const FETCH_MIN = 0.3;
const DEFAULT_PERCENT = 45;

type Hover = { i: number; x: number; y: number };

// What the drawing needs, kept in a ref so the animation always sees the latest.
type Scene = {
  graph: MemoryGraph | null;
  links: Link[];
  degree: number[];
  selected: number | null;
  hover: number | null;
  author: string | null;
  hits: Map<string, number> | null; // uri -> rank in the search, from 1
};

const dark = () => window.matchMedia("(prefers-color-scheme: dark)").matches;

// GraphView draws the space's newest memories as a graph: each memory is a dot
// in its author's color, and memories that mean similar things are linked, so
// they gather into groups by topic. A search lights up the memories it found.
export function GraphView({ uri, total, recall }: { uri: string; total?: number; recall: Recall }) {
  const session = useSession();
  const [graph, setGraph] = useState<MemoryGraph | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [percent, setPercent] = useState(DEFAULT_PERCENT);
  const [selected, setSelected] = useState<number | null>(null);
  const [hover, setHover] = useState<Hover | null>(null);
  const [author, setAuthor] = useState<string | null>(null);

  const stage = useRef<HTMLDivElement>(null);
  const canvas = useRef<HTMLCanvasElement>(null);
  const layout = useRef<Layout | null>(null);
  const size = useRef({ w: 0, h: 0, dpr: 1 });
  const fit = useRef({ scale: 1, ox: 0, oy: 0 });
  const frame = useRef(0);

  useEffect(() => {
    setGraph(null);
    setError(null);
    setSelected(null);
    layout.current = null;
    api.graph(uri, { limit: NODE_LIMIT, neighbors: NEIGHBORS, minSimilarity: FETCH_MIN }).then(setGraph, (e) => setError(describeError(e)));
  }, [uri]);

  const links = useMemo<Link[]>(
    () => (graph?.edges ?? []).filter((e) => e.similarity >= percent * 10).map((e) => ({ a: e.a, b: e.b, w: e.similarity / 1000 })),
    [graph, percent],
  );
  const degree = useMemo(() => {
    const d = new Array<number>(graph?.nodes.length ?? 0).fill(0);
    for (const l of links) {
      d[l.a]++;
      d[l.b]++;
    }
    return d;
  }, [graph, links]);
  const hits = useMemo(
    () => (recall.state.phase === "done" ? new Map(recall.state.hits.map((h, i) => [h.uri, i + 1] as const)) : null),
    [recall.state],
  );
  const authors = useMemo(() => {
    const counts = new Map<string, number>();
    for (const n of graph?.nodes ?? []) counts.set(n.author, (counts.get(n.author) ?? 0) + 1);
    return [...counts].sort((a, b) => b[1] - a[1]);
  }, [graph]);

  const scene = useRef<Scene>({ graph, links, degree, selected, hover: null, author, hits });
  scene.current = { graph, links, degree, selected, hover: hover?.i ?? null, author, hits };

  const draw = useCallback(() => {
    const c = canvas.current;
    const lay = layout.current;
    const s = scene.current;
    const { w, h, dpr } = size.current;
    if (!c || !lay || !s.graph || !w) return;
    const ctx = c.getContext("2d");
    if (!ctx) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    const css = getComputedStyle(c);
    const ink = css.getPropertyValue("--ink").trim() || "#222";
    const accent = css.getPropertyValue("--accent").trim() || "#2f6b45";
    const bloom = css.getPropertyValue("--bloom").trim() || "#a94826";
    const line = css.getPropertyValue("--muted").trim() || "#777";
    const isDark = dark();

    const f = fitTo(lay.points, w, h, 30);
    fit.current = f;
    const px = (i: number) => lay.points[i].x * f.scale + f.ox;
    const py = (i: number) => lay.points[i].y * f.scale + f.oy;
    const nodes = s.graph.nodes;
    const faded = (i: number) =>
      (s.hits !== null && !s.hits.has(nodes[i].uri)) || (s.author !== null && nodes[i].author !== s.author);
    const focus = s.hover ?? s.selected;

    ctx.lineCap = "round";
    for (const l of s.links) {
      const near = focus !== null && (l.a === focus || l.b === focus);
      ctx.globalAlpha = near ? 0.95 : (0.1 + 0.4 * l.w) * (faded(l.a) || faded(l.b) ? 0.25 : 1);
      ctx.strokeStyle = near ? accent : line;
      ctx.lineWidth = near ? 1.8 : 0.6 + 1.4 * l.w;
      ctx.beginPath();
      ctx.moveTo(px(l.a), py(l.a));
      ctx.lineTo(px(l.b), py(l.b));
      ctx.stroke();
    }

    for (let i = 0; i < nodes.length; i++) {
      const r = 4 + Math.min(4, Math.sqrt(s.degree[i] ?? 0));
      ctx.globalAlpha = faded(i) ? 0.2 : 1;
      ctx.fillStyle = authorColor(nodes[i].author, isDark);
      ctx.beginPath();
      ctx.arc(px(i), py(i), r, 0, Math.PI * 2);
      ctx.fill();
      const rank = s.hits?.get(nodes[i].uri);
      if (rank !== undefined) {
        ctx.globalAlpha = 1;
        ctx.strokeStyle = bloom;
        ctx.lineWidth = 2;
        ctx.beginPath();
        ctx.arc(px(i), py(i), r + 4, 0, Math.PI * 2);
        ctx.stroke();
        if (rank <= 9) {
          ctx.fillStyle = bloom;
          ctx.font = "600 11px ui-monospace, Menlo, monospace";
          ctx.textAlign = "center";
          ctx.fillText(String(rank), px(i), py(i) - r - 8);
        }
      }
      if (i === s.selected) {
        ctx.globalAlpha = 1;
        ctx.strokeStyle = ink;
        ctx.lineWidth = 2;
        ctx.beginPath();
        ctx.arc(px(i), py(i), r + 3, 0, Math.PI * 2);
        ctx.stroke();
      }
    }
    ctx.globalAlpha = 1;
  }, []);

  // Lay the memories out again when the graph or the links change, starting
  // from where they were, and let them settle.
  useEffect(() => {
    if (!graph) return;
    const prev = layout.current?.points;
    const lay = new Layout(graph.nodes.length, links, 1, prev && prev.length === graph.nodes.length ? prev : undefined);
    layout.current = lay;
    cancelAnimationFrame(frame.current);
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      lay.settle();
      draw();
      return;
    }
    const tick = () => {
      for (let i = 0; i < 3; i++) lay.step();
      draw();
      if (!lay.cool) frame.current = requestAnimationFrame(tick);
    };
    frame.current = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame.current);
  }, [graph, links, draw]);

  useEffect(draw, [selected, hover, author, hits, draw]);

  // Keep the canvas as sharp as the screen.
  useEffect(() => {
    const el = stage.current;
    const c = canvas.current;
    if (!el || !c) return;
    const measure = () => {
      const dpr = window.devicePixelRatio || 1;
      const w = el.clientWidth;
      const h = el.clientHeight;
      size.current = { w, h, dpr };
      c.width = Math.round(w * dpr);
      c.height = Math.round(h * dpr);
      draw();
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, [draw, graph]);

  const nodeAt = (clientX: number, clientY: number): Hover | null => {
    const c = canvas.current;
    const lay = layout.current;
    if (!c || !lay || !graph) return null;
    const rect = c.getBoundingClientRect();
    const x = clientX - rect.left;
    const y = clientY - rect.top;
    const f = fit.current;
    let best: Hover | null = null;
    let bestD = Infinity;
    for (let i = 0; i < lay.points.length; i++) {
      const nx = lay.points[i].x * f.scale + f.ox;
      const ny = lay.points[i].y * f.scale + f.oy;
      const d = (nx - x) ** 2 + (ny - y) ** 2;
      const reach = 4 + Math.min(4, Math.sqrt(degree[i] ?? 0)) + 6;
      if (d <= reach * reach && d < bestD) {
        best = { i, x: nx, y: ny };
        bestD = d;
      }
    }
    return best;
  };

  const onKey = (e: KeyboardEvent) => {
    const n = graph?.nodes.length ?? 0;
    if (!n) return;
    if (e.key === "ArrowRight" || e.key === "ArrowDown") setSelected(((selected ?? -1) + 1) % n);
    else if (e.key === "ArrowLeft" || e.key === "ArrowUp") setSelected(((selected ?? 0) - 1 + n) % n);
    else if (e.key === "Escape") setSelected(null);
    else return;
    e.preventDefault();
  };

  const neighbors = useMemo(() => {
    if (selected === null) return [];
    const out: { j: number; w: number }[] = [];
    for (const l of links) {
      if (l.a === selected) out.push({ j: l.b, w: l.w });
      else if (l.b === selected) out.push({ j: l.a, w: l.w });
    }
    return out.sort((a, b) => b.w - a.w);
  }, [links, selected]);

  if (error) return <p className="error">{error}</p>;
  if (!graph) return <p className="muted">Loading…</p>;
  const n = graph.nodes.length;
  const shown = hits ? graph.nodes.filter((m) => hits.has(m.uri)).length : 0;
  const older = hits ? hits.size - shown : 0;
  const sel = selected !== null ? graph.nodes[selected] : null;

  return (
    <div className="graph">
      <p className="graph-count muted">
        {n === 0
          ? "Nothing to draw yet."
          : `The newest ${n.toLocaleString()}${total && total > n ? ` of ${total.toLocaleString()}` : ""} ${n === 1 ? "memory" : "memories"}, with ${links.length.toLocaleString()} ${links.length === 1 ? "link" : "links"} between the ones that are alike.`}
      </p>
      {n > 0 && (
        <>
          <div className="graph-bar">
            <label className="graph-slider">
              <span>Linked when at least {percent}% alike</span>
              <input type="range" min={30} max={90} step={5} value={percent} onChange={(e) => setPercent(Number(e.target.value))} />
            </label>
            {hits && (
              <span className="graph-hits muted small">
                {shown} of the {hits.size} matches {shown === 1 ? "is" : "are"} drawn
                {older > 0 && `; ${older} ${older === 1 ? "is" : "are"} older than what's shown`}
              </span>
            )}
          </div>
          <div className="graph-stage" ref={stage}>
            <canvas
              ref={canvas}
              className="graph-canvas"
              tabIndex={0}
              role="img"
              aria-label={`A graph of ${n} memories. Use the arrow keys to move between them; the selected one is shown below.`}
              onPointerMove={(e) => {
                const h = nodeAt(e.clientX, e.clientY);
                setHover((cur) => (cur?.i === h?.i ? cur : h));
              }}
              onPointerLeave={() => setHover(null)}
              onClick={(e) => {
                const h = nodeAt(e.clientX, e.clientY);
                setSelected(h ? (h.i === selected ? null : h.i) : null);
              }}
              onKeyDown={onKey}
            />
            {hover && <Tip memory={graph.nodes[hover.i]} x={hover.x} y={hover.y} />}
          </div>
          <ul className="graph-legend">
            {authors.slice(0, 8).map(([did, count]) => (
              <li key={did}>
                <button
                  type="button"
                  className={author === did ? "legend-item on" : "legend-item"}
                  style={{ "--hue": authorHue(did) } as CSSProperties}
                  onClick={() => setAuthor(author === did ? null : did)}
                  aria-pressed={author === did}
                >
                  <i aria-hidden />
                  <Handle did={did} /> <span className="muted">{count}</span>
                </button>
              </li>
            ))}
            {authors.length > 8 && <li className="muted small">and {authors.length - 8} more</li>}
          </ul>
          {sel && (
            <div className="graph-pick">
              <ol className="notes">
                <Note memory={sel} me={session.did} />
              </ol>
              {neighbors.length > 0 && (
                <div className="graph-links">
                  <h3>Most alike</h3>
                  <ul>
                    {neighbors.slice(0, 5).map(({ j, w }) => (
                      <li key={j}>
                        <button type="button" className="link" onClick={() => setSelected(j)}>
                          {snippet(graph.nodes[j].text, 90)}
                        </button>
                        <span className="note-sim">{w.toFixed(2)}</span>
                      </li>
                    ))}
                  </ul>
                </div>
              )}
            </div>
          )}
        </>
      )}
    </div>
  );
}

function snippet(text: string, max: number): string {
  const t = text.replace(/\s+/g, " ").trim();
  return t.length > max ? t.slice(0, max - 1).trimEnd() + "…" : t;
}

function Tip({ memory, x, y }: { memory: { author: string; text: string }; x: number; y: number }) {
  const handle = useHandle(memory.author);
  return (
    <div className="graph-tip" style={{ left: x, top: y }}>
      <b style={{ color: authorColor(memory.author, dark()) }}>{handle ? `@${handle}` : memory.author.slice(0, 18)}</b>
      <span>{snippet(memory.text, 140)}</span>
    </div>
  );
}
