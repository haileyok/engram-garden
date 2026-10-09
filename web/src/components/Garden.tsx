import { useCallback, useEffect, useRef, useState, type CSSProperties, type PointerEvent as ReactPointerEvent } from "react";
import { AUTHORS, CENTER_OF, CLUSTERS, NOTES, SUGGESTIONS, neighbors, recall, search, type Hit } from "../lib/garden";
import { Prompt } from "./Terminal";

// The landing page's centerpiece: a garden of example notes. Each note is a
// sprout, colored by who wrote it. Ask a question and a ripple spreads out from
// the ground; the notes that mean the same thing bloom. The drawing is a canvas,
// but everything it shows is also in the list beside it, which is real text.

const EDGES = neighbors();
const SPEED = 0.5; // how fast the ripple travels, in px per ms
const PLANT_AT = [5200, 11800, 18400, 25000]; // when the late notes are written, ms after start
const ANSWER_MS = 6800; // how long autoplay lingers on an answer
const TYPE_MS = 34;

type Ask = { id: number; t0: number; ox: number; oy: number; score: Map<number, number> };

// What the canvas keeps between frames. It lives in a ref: React doesn't need
// to render 60 times a second.
type Live = {
  w: number;
  h: number;
  px: number[];
  py: number[];
  planted: number[]; // when each note appeared; Infinity until it's written
  lit: number[]; // how bloomed it is, 0 to 1
  dim: number[]; // how visible it is, 0 to 1
  tl: number[]; // where lit is heading
  td: number[]; // where dim is heading
  applied: number[]; // the last ask this note has reacted to
  flash: number[]; // a spark as the ripple passes
  q: Ask | null;
  hover: number | null;
};

function newLive(reduced: boolean): Live {
  const n = NOTES.length;
  const planted = NOTES.map((note, i) => {
    if (reduced) return -1e9;
    if (note.late) return Infinity;
    return performance.now() + 250 + note.cluster * 150 + (i % 7) * 75;
  });
  return {
    w: 0,
    h: 0,
    px: new Array(n).fill(0),
    py: new Array(n).fill(0),
    planted,
    lit: new Array(n).fill(0),
    dim: new Array(n).fill(1),
    tl: new Array(n).fill(0),
    td: new Array(n).fill(1),
    applied: new Array(n).fill(0),
    flash: new Array(n).fill(0),
    q: null,
    hover: null,
  };
}

const clamp01 = (x: number) => Math.min(1, Math.max(0, x));
const easeOutBack = (x: number) => 1 + 2.70158 * Math.pow(x - 1, 3) + 1.70158 * Math.pow(x - 1, 2);

function layout(live: Live, w: number, h: number) {
  live.w = w;
  live.h = h;
  const padX = 34;
  const top = 40;
  const bottom = 64;
  NOTES.forEach((n, i) => {
    live.px[i] = padX + n.x * (w - 2 * padX);
    live.py[i] = top + n.y * (h - top - bottom);
  });
}

// step moves the bloom and the ripple along by dt milliseconds.
function step(live: Live, t: number, dt: number, still: boolean) {
  const q = live.q;
  const k = still ? 1 : 1 - Math.exp(-dt / 220);
  for (let i = 0; i < NOTES.length; i++) {
    if (live.planted[i] > t) continue;
    if (q && live.applied[i] !== q.id) {
      const d = Math.hypot(live.px[i] - q.ox, live.py[i] - q.oy);
      if (still || t >= q.t0 + d / SPEED) {
        const s = q.score.get(i);
        live.tl[i] = s === undefined ? 0 : 0.4 + 0.6 * s;
        live.td[i] = s === undefined ? 0.28 : 1;
        live.applied[i] = q.id;
        live.flash[i] = still ? 0 : 1;
      }
    }
    live.lit[i] += (live.tl[i] - live.lit[i]) * k;
    live.dim[i] += (live.td[i] - live.dim[i]) * k;
    live.flash[i] *= Math.exp(-dt / 280);
  }
}

function leaf(ctx: CanvasRenderingContext2D, x: number, y: number, angle: number, len: number) {
  ctx.save();
  ctx.translate(x, y);
  ctx.rotate(angle);
  ctx.beginPath();
  ctx.moveTo(0, 0);
  ctx.quadraticCurveTo(len * 0.5, -len * 0.42, len, 0);
  ctx.quadraticCurveTo(len * 0.5, len * 0.42, 0, 0);
  ctx.fill();
  ctx.restore();
}

function draw(ctx: CanvasRenderingContext2D, live: Live, t: number) {
  const { w, h } = live;
  ctx.clearRect(0, 0, w, h);

  // Night ground, with the light coming up from the soil.
  const bg = ctx.createLinearGradient(0, 0, 0, h);
  bg.addColorStop(0, "#282c34");
  bg.addColorStop(0.6, "#252c30");
  bg.addColorStop(1, "#1f3128");
  ctx.fillStyle = bg;
  ctx.fillRect(0, 0, w, h);
  const horizon = ctx.createRadialGradient(w / 2, h + 40, 10, w / 2, h + 40, w * 0.7);
  horizon.addColorStop(0, "rgb(143 203 155 / 0.22)");
  horizon.addColorStop(1, "rgb(143 203 155 / 0)");
  ctx.fillStyle = horizon;
  ctx.fillRect(0, 0, w, h);

  // Topic names, quiet, above each patch.
  ctx.font = "600 10.5px ui-monospace, 'JetBrains Mono', Menlo, monospace";
  ctx.textAlign = "center";
  ctx.fillStyle = "#b4cdb9";
  CLUSTERS.forEach((name, c) => {
    const [cx, cy] = CENTER_OF[c];
    const x = 34 + cx * (w - 68);
    const y = 40 + Math.max(0.02, cy - 0.205) * (h - 104);
    ctx.globalAlpha = 0.34;
    ctx.fillText(name.toUpperCase().split("").join("\u200a"), x, y);
  });
  ctx.globalAlpha = 1;

  // Roots between neighbors in a patch.
  ctx.lineWidth = 1;
  for (const [a, b] of EDGES) {
    if (live.planted[a] > t || live.planted[b] > t) continue;
    const ax = live.px[a];
    const ay = live.py[a];
    const bx = live.px[b];
    const by = live.py[b];
    const glow = Math.min(live.lit[a], live.lit[b]);
    const vis = Math.min(live.dim[a], live.dim[b]);
    ctx.globalAlpha = (0.1 + 0.5 * glow) * (0.4 + 0.6 * vis);
    ctx.strokeStyle = glow > 0.3 ? "#e6dcc0" : "#8fcb9b";
    ctx.beginPath();
    ctx.moveTo(ax, ay + 18);
    const mx = (ax + bx) / 2;
    const my = (ay + by) / 2 + 22;
    ctx.quadraticCurveTo(mx, my, bx, by + 18);
    ctx.stroke();
  }
  ctx.globalAlpha = 1;

  // The ripple, and the roots it grows to the notes it finds.
  const q = live.q;
  if (q) {
    const r = Math.max(0, (t - q.t0) * SPEED);
    const reach = Math.hypot(Math.max(q.ox, live.w - q.ox), q.oy) + 40;
    if (r < reach) {
      const fade = 1 - r / reach;
      for (const [scale, a, lw] of [
        [1, 0.5, 2],
        [0.8, 0.28, 1.2],
        [0.6, 0.14, 1],
      ] as const) {
        ctx.globalAlpha = fade * a;
        ctx.strokeStyle = "#e6eadf";
        ctx.lineWidth = lw;
        ctx.beginPath();
        ctx.arc(q.ox, q.oy, r * scale, Math.PI, 2 * Math.PI);
        ctx.stroke();
      }
    }
    ctx.lineWidth = 1.6;
    for (const [id, s] of q.score) {
      if (live.planted[id] > t) continue;
      const dist = Math.hypot(live.px[id] - q.ox, live.py[id] - q.oy);
      const p = clamp01((t - (q.t0 + dist / SPEED)) / 600);
      if (p <= 0) continue;
      const c = AUTHORS[NOTES[id].author].color;
      const len = dist * 1.15;
      ctx.save();
      ctx.setLineDash([len, len]);
      ctx.lineDashOffset = len * (1 - p);
      ctx.globalAlpha = 0.5 * s;
      ctx.strokeStyle = c;
      ctx.beginPath();
      ctx.moveTo(q.ox, q.oy);
      const mx = (q.ox + live.px[id]) / 2 + (live.px[id] - q.ox) * 0.12;
      const my = (q.oy + live.py[id]) / 2 + 30;
      ctx.quadraticCurveTo(mx, my, live.px[id], live.py[id] + 18);
      ctx.stroke();
      ctx.restore();
    }
    ctx.globalAlpha = 1;
    // The question's seed, where the ripple starts.
    const pulse = 0.5 + 0.5 * Math.sin(t / 380);
    const seed = ctx.createRadialGradient(q.ox, q.oy, 0, q.ox, q.oy, 22);
    seed.addColorStop(0, `rgb(251 248 239 / ${0.7 + 0.2 * pulse})`);
    seed.addColorStop(1, "rgb(251 248 239 / 0)");
    ctx.fillStyle = seed;
    ctx.beginPath();
    ctx.arc(q.ox, q.oy, 22, 0, 6.283);
    ctx.fill();
  }

  // The sprouts.
  for (let i = 0; i < NOTES.length; i++) {
    const age = (t - live.planted[i]) / 1100;
    if (age <= 0) continue;
    const g = easeOutBack(clamp01(age));
    const lit = live.lit[i];
    const alpha = live.dim[i] * (0.72 + 0.28 * lit);
    const color = AUTHORS[NOTES[i].author].color;
    const sway = Math.sin(t / 950 + i * 1.7) * 1.6 * (1 - 0.6 * lit);
    const x = live.px[i];
    const y = live.py[i];
    const hx = x + sway;

    // stem
    ctx.globalAlpha = alpha * 0.8;
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    ctx.moveTo(x, y + 20 * g);
    ctx.quadraticCurveTo(x + sway * 1.8, y + 11 * g, hx, y);
    ctx.stroke();

    // leaves
    ctx.fillStyle = color;
    ctx.globalAlpha = alpha * 0.7;
    const ly = y + 11 * g;
    leaf(ctx, x + sway * 0.8, ly, -0.55 - 0.2 * lit, (5.5 + 2.5 * lit) * g);
    leaf(ctx, x + sway * 0.8, ly + 3, Math.PI + 0.55 + 0.2 * lit, (5.5 + 2.5 * lit) * g);

    // bloom
    if (lit > 0.04) {
      const gr = 14 + 34 * lit;
      const glow = ctx.createRadialGradient(hx, y, 0, hx, y, gr);
      glow.addColorStop(0, color);
      glow.addColorStop(1, "rgb(0 0 0 / 0)");
      ctx.globalAlpha = alpha * 0.34 * lit;
      ctx.fillStyle = glow;
      ctx.beginPath();
      ctx.arc(hx, y, gr, 0, 6.283);
      ctx.fill();
      ctx.globalAlpha = alpha * (0.35 + 0.6 * lit);
      ctx.fillStyle = color;
      const pr = 2.4 + 4.6 * lit;
      const pd = 4 + 5.5 * lit;
      for (let p = 0; p < 6; p++) {
        const a = (p / 6) * 6.283 + i + t / 5200;
        ctx.beginPath();
        ctx.arc(hx + Math.cos(a) * pd, y + Math.sin(a) * pd, pr, 0, 6.283);
        ctx.fill();
      }
    }

    // bud, or the middle of the bloom
    ctx.globalAlpha = alpha;
    ctx.fillStyle = lit > 0.3 ? "#fff6d8" : color;
    ctx.beginPath();
    ctx.arc(hx, y, (3 + 1.5 * lit) * Math.min(1, g), 0, 6.283);
    ctx.fill();

    // a spark as the ripple passes
    if (live.flash[i] > 0.02) {
      ctx.globalAlpha = live.flash[i] * 0.7;
      ctx.strokeStyle = "#fbf8ef";
      ctx.lineWidth = 1.2;
      ctx.beginPath();
      ctx.arc(hx, y, 6 + 16 * (1 - live.flash[i]), 0, 6.283);
      ctx.stroke();
    }
  }

  if (live.hover !== null && live.planted[live.hover] <= t) {
    const i = live.hover;
    ctx.globalAlpha = 0.9;
    ctx.strokeStyle = AUTHORS[NOTES[i].author].color;
    ctx.lineWidth = 1.5;
    ctx.setLineDash([3, 3]);
    ctx.beginPath();
    ctx.arc(live.px[i], live.py[i], 17, 0, 6.283);
    ctx.stroke();
    ctx.setLineDash([]);
  }
  ctx.globalAlpha = 1;
}

export function Garden() {
  const wrapRef = useRef<HTMLElement>(null);
  const fieldRef = useRef<HTMLDivElement>(null);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const live = useRef<Live | null>(null);
  const reduced = useRef(false);
  const auto = useRef(true);
  const askCount = useRef(0);
  const redraw = useRef<() => void>(() => {});

  const [text, setText] = useState("");
  const [asked, setAsked] = useState("");
  const [hits, setHits] = useState<Hit[]>([]);
  const [tip, setTip] = useState<number | null>(null);
  const [size, setSize] = useState({ w: 0, h: 0 });

  // ask runs a question: the ripple starts from the ground and the matching
  // notes bloom as it reaches them.
  const ask = useCallback((question: string) => {
    const lv = live.current;
    if (!lv) return;
    const now = performance.now();
    const found = NOTES.filter((n) => lv.planted[n.id] <= now);
    const result = search(question, found);
    askCount.current += 1;
    lv.q = {
      id: askCount.current,
      t0: reduced.current ? -1e9 : now,
      ox: lv.w / 2,
      oy: lv.h - 22,
      score: new Map(result.map((h) => [h.id, h.score])),
    };
    setAsked(question);
    setHits(result);
    redraw.current();
  }, []);

  // The drawing loop, and keeping the canvas the size of its box.
  useEffect(() => {
    const canvas = canvasRef.current;
    const field = fieldRef.current;
    if (!canvas || !field) return;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    reduced.current = !!window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
    const lv = newLive(reduced.current);
    live.current = lv;

    let raf = 0;
    let last = performance.now();
    let onscreen = true;

    const resize = () => {
      const w = field.clientWidth;
      const h = field.clientHeight;
      const dpr = Math.min(2, window.devicePixelRatio || 1);
      canvas.width = Math.round(w * dpr);
      canvas.height = Math.round(h * dpr);
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      layout(lv, w, h);
      if (lv.q) {
        lv.q.ox = w / 2;
        lv.q.oy = h - 22;
      }
      setSize({ w, h });
      frame(performance.now(), true);
    };

    const frame = (t: number, once = false) => {
      const dt = Math.min(64, t - last);
      last = t;
      step(lv, t, dt, reduced.current);
      draw(ctx, lv, t);
      if (!once && !reduced.current && onscreen && !document.hidden) raf = requestAnimationFrame(loop);
    };
    const loop = (t: number) => frame(t);
    redraw.current = () => {
      if (reduced.current) frame(performance.now(), true);
    };

    const ro = new ResizeObserver(resize);
    ro.observe(field);
    resize();
    if (!reduced.current) raf = requestAnimationFrame(loop);

    const io = new IntersectionObserver((entries) => {
      onscreen = entries.some((e) => e.isIntersecting);
      if (onscreen && !reduced.current) {
        cancelAnimationFrame(raf);
        last = performance.now();
        raf = requestAnimationFrame(loop);
      }
    });
    io.observe(field);
    const vis = () => {
      if (!document.hidden && onscreen && !reduced.current) {
        cancelAnimationFrame(raf);
        last = performance.now();
        raf = requestAnimationFrame(loop);
      }
    };
    document.addEventListener("visibilitychange", vis);

    return () => {
      cancelAnimationFrame(raf);
      ro.disconnect();
      io.disconnect();
      document.removeEventListener("visibilitychange", vis);
      live.current = null;
    };
  }, []);

  // The show: type a question, let the ripple answer it, move to the next, and
  // plant a few notes as if a teammate's script just wrote them. It stops the
  // moment someone uses the garden themselves.
  useEffect(() => {
    const wrap = wrapRef.current;
    if (!wrap) return;
    let cancelled = false;
    const timers: number[] = [];
    const sleep = (ms: number) => new Promise<void>((res) => timers.push(window.setTimeout(res, ms)));
    const alive = () => !cancelled && auto.current;

    const whenSeen = () =>
      new Promise<void>((res) => {
        if (typeof IntersectionObserver === "undefined") return res();
        const io = new IntersectionObserver(
          (entries) => {
            if (entries.some((e) => e.isIntersecting)) {
              io.disconnect();
              res();
            }
          },
          { threshold: 0.35 },
        );
        io.observe(wrap);
        timers.push(window.setTimeout(() => io.disconnect(), 120000));
      });

    (async () => {
      await sleep(0);
      if (window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) {
        // No motion: show one answer and stop.
        setText(SUGGESTIONS[0]);
        ask(SUGGESTIONS[0]);
        return;
      }
      await whenSeen();
      if (cancelled) return;

      // The late notes, planted on a schedule, whatever the show is doing.
      const late = NOTES.filter((n) => n.late);
      late.forEach((n, k) => {
        timers.push(
          window.setTimeout(() => {
            const lv = live.current;
            if (!lv || cancelled) return;
            lv.planted[n.id] = performance.now();
            if (lv.q) lv.applied[n.id] = lv.q.id;
          }, PLANT_AT[k % PLANT_AT.length]),
        );
      });

      await sleep(900);
      let i = 0;
      while (alive()) {
        const q = SUGGESTIONS[i % SUGGESTIONS.length];
        setText("");
        for (let c = 1; c <= q.length; c++) {
          if (!alive()) return;
          setText(q.slice(0, c));
          await sleep(TYPE_MS);
        }
        await sleep(280);
        if (!alive()) return;
        ask(q);
        await sleep(ANSWER_MS);
        i += 1;
      }
    })();

    return () => {
      cancelled = true;
      timers.forEach(clearTimeout);
    };
  }, [ask]);

  const stopAuto = () => {
    auto.current = false;
  };
  const submit = (q: string) => {
    stopAuto();
    setText(q);
    if (q.trim()) ask(q);
  };

  // Hovering finds the nearest sprout within reach.
  const onMove = (e: ReactPointerEvent<HTMLCanvasElement>) => {
    const lv = live.current;
    const canvas = canvasRef.current;
    if (!lv || !canvas) return;
    const rect = canvas.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const y = e.clientY - rect.top;
    const now = performance.now();
    let best: number | null = null;
    let bestD = 22;
    for (let i = 0; i < NOTES.length; i++) {
      if (lv.planted[i] > now) continue;
      const d = Math.hypot(lv.px[i] - x, lv.py[i] + 6 - y);
      if (d < bestD) {
        best = i;
        bestD = d;
      }
    }
    lv.hover = best;
    setTip(best);
    redraw.current();
  };
  const onLeave = () => {
    if (live.current) live.current.hover = null;
    setTip(null);
    redraw.current();
  };
  const markHover = (id: number | null) => {
    if (live.current) live.current.hover = id;
    setTip(id);
    redraw.current();
  };

  const lv = live.current;
  const tipAt = tip !== null && lv ? { x: Math.min(Math.max(lv.px[tip], 130), size.w - 130), y: lv.py[tip] } : null;

  return (
    <section className="term garden" ref={wrapRef} aria-labelledby="garden-h">
      <div className="term-bar">
        <span className="term-dots" aria-hidden>
          <i />
          <i />
          <i />
        </span>
        <span className="term-title">scout@laptop</span>
        <span className="term-end" />
      </div>
      <h2 id="garden-h" className="sr-only">
        Try a search on some example notes
      </h2>
      <div className="garden-body">
        <div className="garden-main">
          <div className="garden-head">
            <form
              className="garden-ask"
              onSubmit={(e) => {
                e.preventDefault();
                submit(text);
              }}
            >
              <label htmlFor="garden-q" className="sr-only">
                Ask a question about the example notes
              </label>
              <span aria-hidden>
                <Prompt cwd="~/api" />
              </span>
              <span className="cmd" aria-hidden>
                engram recall{" "}
              </span>
              <span aria-hidden>"</span>
              <input
                id="garden-q"
                ref={inputRef}
                value={text}
                autoComplete="off"
                spellCheck={false}
                placeholder="how do we ship a change?"
                onFocus={stopAuto}
                onChange={(e) => {
                  stopAuto();
                  setText(e.target.value);
                }}
              />
              <span aria-hidden>"</span>
              <button type="submit">run &#8629;</button>
            </form>
            <div className="garden-chips">
              {SUGGESTIONS.map((s) => (
                <button type="button" key={s} onClick={() => submit(s)} className={s === asked ? "on" : ""}>
                  {s}
                </button>
              ))}
            </div>
          </div>

          <div className="garden-field" ref={fieldRef}>
            <canvas ref={canvasRef} aria-hidden onPointerMove={onMove} onPointerLeave={onLeave} />
            {tipAt && tip !== null && (
              <div className="garden-tip" style={{ left: tipAt.x, top: tipAt.y }} aria-hidden>
                <b style={{ color: AUTHORS[NOTES[tip].author].color }}>{AUTHORS[NOTES[tip].author].name}</b>
                <i>{AUTHORS[NOTES[tip].author].kind}</i>
                <span>{NOTES[tip].text}</span>
              </div>
            )}
          </div>
        </div>

        <aside className="garden-side" aria-live="polite" aria-label="Command output">
          <div className="garden-out">
            {asked ? (
              <div>
                <Prompt cwd="~/api" />
                <span className="t-fg">{`engram recall "${asked}"`}</span>
              </div>
            ) : (
              <div className="t-dim"># pick a question, or type your own</div>
            )}
            {asked && hits.length === 0 && <div>No memories found.</div>}
            {hits.map((h) => {
              const n = NOTES[h.id];
              const a = AUTHORS[n.author];
              const r = recall(n, h.score);
              return (
                <div
                  key={h.id}
                  className="garden-hit"
                  onPointerEnter={() => markHover(h.id)}
                  onPointerLeave={() => markHover(null)}
                  style={{ "--c": a.color } as CSSProperties}
                >
                  <div>
                    <span className="t-dim">{`[${r.similarity}] ${r.when}  `}</span>
                    <span className="did" title={`${a.name}, ${a.kind}`}>
                      {r.did}
                    </span>
                    <span className="t-dim">{`  in ${r.space}`}</span>
                  </div>
                  <div className="t-dim">{`tags: ${r.tags.join(", ")}`}</div>
                  <div className="t-fg">{r.text}</div>
                  <div className="t-dim">{r.uri}</div>
                </div>
              );
            })}
          </div>
        </aside>
      </div>
    </section>
  );
}
