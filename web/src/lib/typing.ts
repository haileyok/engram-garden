// The timeline behind the typed terminal demo on the landing page.
//
// A demo is a list of steps spread over one or more terminal windows. They
// play in order: a command is typed one character per tick, its output
// appears a moment after it's done, and the next step starts right after.
// reveal says what's showing at a given tick. It has no clock of its own, so
// it's easy to test and to jump to the end for people who prefer less motion.

export type Step =
  | { win: number; kind: "cmd"; text: string } // a prompt and a typed command
  | { win: number; kind: "out"; text: string } // what the command printed
  | { win: number; kind: "prompt" }; // a fresh, empty prompt

export type Shown = {
  visible: boolean;
  // Characters of a command that are typed so far. Always 0 for other kinds.
  chars: number;
};

// A pause before typing starts, and before output or a new prompt appears.
export const PRE = 8;
export const OUT_PAUSE = 14;
export const PROMPT_PAUSE = 10;

type Timed = { begin: number; typeFrom: number };

function timeline(steps: Step[]): { timed: Timed[]; end: number } {
  let t = 0;
  const timed = steps.map((s): Timed => {
    switch (s.kind) {
      case "cmd": {
        const begin = t;
        const typeFrom = t + PRE;
        t = typeFrom + s.text.length;
        return { begin, typeFrom };
      }
      case "out":
        t += OUT_PAUSE;
        return { begin: t, typeFrom: t };
      case "prompt":
        t += PROMPT_PAUSE;
        return { begin: t, typeFrom: t };
    }
  });
  return { timed, end: t };
}

// totalTicks is when the last step has finished.
export function totalTicks(steps: Step[]): number {
  return timeline(steps).end;
}

export function reveal(steps: Step[], tick: number): Shown[] {
  const { timed } = timeline(steps);
  return steps.map((s, i) => {
    const { begin, typeFrom } = timed[i];
    const visible = tick >= begin;
    if (s.kind !== "cmd") return { visible, chars: 0 };
    return { visible, chars: Math.max(0, Math.min(s.text.length, tick - typeFrom)) };
  });
}
