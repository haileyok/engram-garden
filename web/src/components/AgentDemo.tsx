import { useEffect, useRef, useState } from "react";
import { reveal, totalTicks, type Step } from "../lib/typing";
import { Prompt, TermFrame } from "./Terminal";

const NOTE = "Deploys go through the deploy repo's workflow, one SHA for every service.";
const AUTHOR = "did:plc:u4kxm7qvh2o3zfd5ryw6bnae";
const URI = "at://did:plc:7w3y…/garden.engram.memory/3mxdzk2fq7s22";

// What the two agents type and see. The output is what engram prints.
const STEPS: Step[] = [
  { win: 0, kind: "cmd", text: `engram remember "${NOTE}" -t infra` },
  { win: 0, kind: "out", text: `Remembered: ${URI}` },
  { win: 0, kind: "prompt" },
  { win: 1, kind: "cmd", text: `engram recall "how do we ship a change?"` },
  {
    win: 1,
    kind: "out",
    text: `[702] 2026-10-08 08:01  ${AUTHOR}  in memory\ntags: infra\n${NOTE}`,
  },
  { win: 1, kind: "prompt" },
];

const WINDOWS = [
  { title: "scout@laptop", cwd: "~/api" },
  { title: "archivist@ci", cwd: "~" },
];

const TICK_MS = 24;

// useTicker counts ticks up to `total`, starting when the element scrolls into
// view. With reduced motion it jumps to the end.
function useTicker(total: number) {
  const ref = useRef<HTMLDivElement>(null);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) {
      setTick(total);
      return;
    }
    let timer: number | undefined;
    const play = () => {
      let t = 0;
      timer = window.setInterval(() => {
        t += 1;
        setTick(t);
        if (t >= total) window.clearInterval(timer);
      }, TICK_MS);
    };
    const el = ref.current;
    if (!el || typeof IntersectionObserver === "undefined") {
      play();
      return () => window.clearInterval(timer);
    }
    const seen = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          seen.disconnect();
          play();
        }
      },
      { threshold: 0.3 },
    );
    seen.observe(el);
    return () => {
      seen.disconnect();
      window.clearInterval(timer);
    };
  }, [total]);
  return { ref, tick };
}

// The same text for people using a screen reader, which would otherwise hear
// the animation one character at a time.
const TRANSCRIPT = STEPS.flatMap((s) =>
  s.kind === "cmd" ? [`${WINDOWS[s.win].title}: ${s.text}`] : s.kind === "out" ? [s.text] : [],
).join("\n");

// AgentDemo is two terminal windows: an agent saves a note and another agent
// finds it. Every line takes its space from the start, so the page doesn't move
// while it types.
export function AgentDemo() {
  const total = totalTicks(STEPS);
  const { ref, tick } = useTicker(total);
  const shown = reveal(STEPS, tick);

  // The window of the latest step that has started is the focused one. Ghostty
  // draws an unfocused window's cursor hollow.
  let focused = STEPS[0].win;
  STEPS.forEach((s, i) => {
    if (shown[i].visible) focused = s.win;
  });

  return (
    <div className="stage" ref={ref}>
      <p className="sr-only">Example, as a transcript: {TRANSCRIPT}</p>
      <div className="stage-bed" aria-hidden>
        {WINDOWS.map((w, win) => {
          const mine = STEPS.map((s, i) => ({ s, i })).filter(({ s }) => s.win === win);
          // A window always shows its first prompt, so it never looks empty
          // while it waits for its turn. The cursor sits on the latest prompt
          // or command, until output follows it.
          let cursor = -1;
          mine.forEach(({ s, i }, n) => {
            if (!(shown[i].visible || n === 0)) return;
            cursor = s.kind === "out" ? -1 : i;
          });
          return (
            <TermFrame key={w.title} title={w.title} className={win === 1 ? "term-b" : ""}>
              {mine.map(({ s, i }, n) => {
                const on = shown[i].visible || n === 0;
                const off = on ? "" : " t-off";
                const caret =
                  cursor === i ? <span className={focused === win ? "t-caret" : "t-caret hollow"} /> : null;
                if (s.kind === "out") {
                  return (
                    <div key={i} className={`t-out${off}`}>
                      {s.text}
                    </div>
                  );
                }
                if (s.kind === "prompt") {
                  return (
                    <div key={i} className={`t-line${off}`}>
                      <Prompt cwd={w.cwd} />
                      {caret}
                    </div>
                  );
                }
                const typed = shown[i].chars;
                return (
                  <div key={i} className={`t-line${off}`}>
                    <Prompt cwd={w.cwd} />
                    <span>{s.text.slice(0, typed)}</span>
                    {caret}
                    <span className="t-ghost">{s.text.slice(typed)}</span>
                  </div>
                );
              })}
            </TermFrame>
          );
        })}
      </div>
    </div>
  );
}
