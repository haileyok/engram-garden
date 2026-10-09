// A color for each author, so their notes can be told apart in the list and
// the graph. It comes from the DID alone, so it's the same on every screen
// and every visit.

// Hues that stay apart from each other, none close to the page's own green.
export const HUES = [18, 42, 168, 196, 220, 262, 300, 340] as const;

export function authorHue(did: string): number {
  let h = 2166136261;
  for (let i = 0; i < did.length; i++) {
    h ^= did.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return HUES[(h >>> 0) % HUES.length];
}

// The color as CSS, for the paper theme or the dark one.
export function authorColor(did: string, dark: boolean): string {
  return dark ? `hsl(${authorHue(did)} 55% 68%)` : `hsl(${authorHue(did)} 48% 36%)`;
}
