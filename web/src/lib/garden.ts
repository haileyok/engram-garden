// Example notes and a tiny matcher for the landing page's garden.
//
// None of this is real data and the matcher isn't a model. It groups words that
// mean about the same thing ("ship", "deploy", "release") and scores a note by
// the groups it shares with the question, so the demo can show a search finding
// a note that doesn't use the question's words. Real search embeds both with the
// space's embedding model. The page says so under the garden.

export type Kind = "agent" | "person" | "script";

export type Author = { name: string; kind: Kind; color: string };

// Who wrote the notes. The colors read on the garden's dark ground.
export const AUTHORS: Author[] = [
  { name: "scout", kind: "agent", color: "#8fcb9b" },
  { name: "archivist", kind: "agent", color: "#7fb8d6" },
  { name: "maya", kind: "person", color: "#e29468" },
  { name: "omar", kind: "person", color: "#c49ad9" },
  { name: "merge-bot", kind: "script", color: "#e6d28f" },
];

export const CLUSTERS = ["deploys", "auth", "database", "frontend", "incidents", "onboarding"] as const;

// Where each topic's patch sits, in a unit square, and how far it spreads.
const CENTERS: [number, number][] = [
  [0.17, 0.34],
  [0.5, 0.26],
  [0.83, 0.34],
  [0.17, 0.7],
  [0.5, 0.74],
  [0.83, 0.7],
];
const SPREAD: [number, number] = [0.1, 0.15];

type Seed = {
  cluster: number;
  author: number;
  text: string;
  // Extra words the note is about, when its own text doesn't say them.
  about?: string;
  // Planted a few seconds in, as if just written.
  late?: boolean;
};

const SEEDS: Seed[] = [
  // deploys
  { cluster: 0, author: 0, text: "Deploys go through the deploy repo's workflow, one SHA for every service." },
  { cluster: 0, author: 2, text: "Staging deploys on every merge to main. Prod waits for a manual approval in the workflow." },
  { cluster: 0, author: 1, text: "To roll back, re-run the deploy workflow with the previous SHA. Don't revert on main first." },
  { cluster: 0, author: 4, text: "merged #482: move the worker to its own deployment" },
  { cluster: 0, author: 3, text: "The canary takes 5% of traffic for ten minutes before the full rollout." },
  { cluster: 0, author: 0, text: "A cancelled deploy can leave the lock behind. Delete the lock object in the bucket." },
  { cluster: 0, author: 4, text: "merged #496: deploys now post the SHA to the team channel", late: true },
  // auth
  { cluster: 1, author: 1, text: "Sessions refresh hourly. A 400 with use_dpop_nonce is the normal retry, not a failure." },
  { cluster: 1, author: 0, text: "Refresh tokens are single-use, so never restore a session from a backup." },
  { cluster: 1, author: 2, text: "Login breaks on Safari when the redirect URI ends in a slash." },
  { cluster: 1, author: 4, text: "merged #471: rotate the signing key every 90 days" },
  { cluster: 1, author: 3, text: "CI tokens live in the shared vault under ci-tokens, never in the repo." },
  { cluster: 1, author: 1, text: "The CI account can only read. Anything that writes goes through the app." },
  { cluster: 1, author: 4, text: "merged #501: sign-in errors now say which account server failed", late: true },
  // database
  { cluster: 2, author: 2, text: "Migrations are append-only. Once a numbered file is in use, add a new one instead of editing it." },
  { cluster: 2, author: 1, text: "Run migrations under the advisory lock, or two instances racing leave half a schema." },
  { cluster: 2, author: 3, text: "The backup timer runs daily at 03:10 and keeps 14 dumps." },
  { cluster: 2, author: 0, text: "The migration needed a second pass because the backfill timed out on the big table." },
  { cluster: 2, author: 4, text: "merged #490: add an index on space and author", about: "database" },
  { cluster: 2, author: 2, text: "After the Postgres upgrade, rebuild indexes that use the old collation." },
  // frontend
  { cluster: 3, author: 3, text: "The web app's CSP allows no external fonts, so the card is drawn with local ones." },
  { cluster: 3, author: 0, text: "Run tsc straight from node_modules. pnpm isn't installed on the dev box." },
  { cluster: 3, author: 1, text: "A test that imports node:fs breaks the frontend build. Check repo files from a Go test." },
  { cluster: 3, author: 2, text: "Link previews are cached by URL, so a new card needs a new filename." },
  { cluster: 3, author: 4, text: "merged #477: dark mode for the settings page" },
  { cluster: 3, author: 3, text: "Keep the landing page under 200 kB of JavaScript." },
  // incidents
  { cluster: 4, author: 1, text: "Oct 2: search was slow because the cache was cold after a restart. Warm it before moving traffic." },
  { cluster: 4, author: 0, text: "The alert fires when the indexer falls five minutes behind on a member's repo." },
  { cluster: 4, author: 2, text: "If indexing stalls, check whether the member's account server is down before restarting anything." },
  { cluster: 4, author: 3, text: "The fencing relies on the bucket honoring conditional writes. Wasabi does." },
  { cluster: 4, author: 4, text: "merged #485: retry the bucket probe with backoff" },
  { cluster: 4, author: 1, text: "Postmortem: one corrupt commit got indexed. Now nothing is indexed until it verifies." },
  { cluster: 4, author: 4, text: "merged #503: page when the indexer falls behind, not just when it dies", late: true },
  // onboarding
  { cluster: 5, author: 0, text: "A new machine needs Ollama with the same embedding model as the space.", about: "start" },
  { cluster: 5, author: 2, text: "Ask in the help channel before touching the production bucket.", about: "start" },
  { cluster: 5, author: 3, text: "Run engram login once per machine. Sign-ins last two weeks.", about: "start" },
  { cluster: 5, author: 1, text: "Tests use the sandbox account server, never a real handle.", about: "start" },
  { cluster: 5, author: 0, text: "Each space declares its own embedding model, so check engram spaces first.", about: "start" },
  { cluster: 5, author: 2, text: "Standup notes go in the team space, not in the repo.", about: "start" },
];

export type Note = {
  id: number;
  text: string;
  author: number;
  cluster: number;
  late: boolean;
  // Position in the unit square.
  x: number;
  y: number;
  keys: ReadonlySet<string>;
};

// A small deterministic generator, so the garden looks the same every visit.
function rng(seed: number) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// Words that mean about the same thing share a group.
const GROUPS: string[][] = [
  ["ship", "deploy", "deployment", "release", "rollout", "publish", "launch", "canary", "roll"],
  ["edit", "modify", "update", "alter"],
  ["merge", "merged", "pr", "commit"],
  ["break", "broke", "broken", "fail", "failure", "stall", "stuck", "error", "crash", "wrong"],
  ["login", "signin", "auth", "session", "oauth", "token", "password", "credential"],
  ["database", "db", "postgres", "migration", "schema", "backfill", "table", "sql"],
  ["slow", "latency", "lag", "sluggish", "speed", "fast"],
  ["restart", "reboot", "cold", "warm", "boot"],
  ["start", "setup", "onboard", "first", "begin", "machine", "laptop"],
  ["frontend", "ui", "web", "css", "font", "browser", "javascript"],
  ["undo", "revert", "rollback", "back"],
  ["alert", "page", "pager", "monitor", "notify"],
  ["secret", "key", "vault", "credential"],
  ["backup", "dump", "restore", "snapshot"],
  ["test", "ci", "workflow", "pipeline"],
];

const STOP = new Set(
  (
    "a an and are as at be but by can do does did for from had has have how i if in into is it its just me my of on " +
    "one or so that the their them then there these they this to up us was we were what when where which who why " +
    "will with you your should would could about after before not no only same than too very also all any each " +
    // Words that fit almost any note say nothing about which one is meant.
    "old new change"
  ).split(" "),
);

// A crude stem: enough to match "deploys" with "deploy" and "editing" with "edit".
export function stem(word: string): string {
  let w = word.toLowerCase();
  if (w.length > 4 && w.endsWith("ies")) w = w.slice(0, -3) + "y";
  else if (w.length > 5 && w.endsWith("ing")) w = w.slice(0, -3);
  else if (w.length > 4 && w.endsWith("ed")) w = w.slice(0, -2);
  else if (w.length > 4 && w.endsWith("es")) w = w.slice(0, -2);
  else if (w.length > 3 && w.endsWith("s") && !w.endsWith("ss")) w = w.slice(0, -1);
  return w;
}

const GROUP_OF = new Map<string, number>();
GROUPS.forEach((g, i) => {
  for (const word of g) {
    const s = stem(word);
    // The first group to claim a word keeps it.
    if (!GROUP_OF.has(s)) GROUP_OF.set(s, i);
  }
});

// keysOf are the meanings a piece of text carries: a group for words that have
// one, the stem for the rest.
export function keysOf(text: string): Set<string> {
  const keys = new Set<string>();
  for (const raw of text.toLowerCase().split(/[^a-z0-9]+/)) {
    if (raw.length < 2 || STOP.has(raw)) continue;
    const s = stem(raw);
    if (STOP.has(s)) continue;
    const g = GROUP_OF.get(s);
    keys.add(g === undefined ? s : `g${g}`);
  }
  return keys;
}

function buildNotes(): Note[] {
  const next = rng(7);
  const counts = new Array(CENTERS.length).fill(0);
  const per = new Array(CENTERS.length).fill(0);
  for (const s of SEEDS) per[s.cluster]++;
  return SEEDS.map((s, id) => {
    const k = counts[s.cluster]++;
    const n = per[s.cluster];
    // A spiral out from the patch's center, nudged so it doesn't look drawn.
    const angle = k * 2.399963 + s.cluster * 1.3;
    const r = Math.sqrt((k + 0.55) / n);
    const [cx, cy] = CENTERS[s.cluster];
    const x = cx + Math.cos(angle) * r * SPREAD[0] + (next() - 0.5) * 0.02;
    const y = cy + Math.sin(angle) * r * SPREAD[1] + (next() - 0.5) * 0.02;
    const keys = keysOf(`${s.text} ${s.about ?? ""}`);
    return { id, text: s.text, author: s.author, cluster: s.cluster, late: !!s.late, x, y, keys };
  });
}

export const NOTES: Note[] = buildNotes();

export const SUGGESTIONS = [
  "how do we ship a change?",
  "why did login break?",
  "can I edit an old migration?",
  "something is slow after a restart",
  "I'm new, where do I start?",
];

export type Hit = { id: number; score: number };

// How close to the best note a note has to be to count.
const MIN_SCORE = 0.55;

// search ranks the given notes against a question, best first. Scores run from
// 0 to 1 relative to the best one, and only notes sharing a meaning with the
// question are returned.
export function search(question: string, notes: readonly Note[] = NOTES, limit = 5): Hit[] {
  const q = keysOf(question);
  if (q.size === 0) return [];
  // A meaning that few notes carry says more than one most of them do.
  const df = new Map<string, number>();
  for (const n of notes) for (const k of n.keys) df.set(k, (df.get(k) ?? 0) + 1);
  const weight = (k: string) => 1 / Math.sqrt(df.get(k) ?? 1);
  const raw: Hit[] = [];
  for (const n of notes) {
    let sum = 0;
    for (const k of q) if (n.keys.has(k)) sum += weight(k);
    if (sum > 0) {
      // Shorter notes that hit the same meanings are a little more on point.
      raw.push({ id: n.id, score: sum / (1 + 0.04 * n.keys.size) });
    }
  }
  raw.sort((a, b) => b.score - a.score || a.id - b.id);
  if (raw.length === 0) return [];
  const best = raw[0].score;
  // A note that only shares one stray meaning with the question isn't an answer.
  return raw
    .map((h) => ({ id: h.id, score: h.score / best }))
    .filter((h) => h.score >= MIN_SCORE)
    .slice(0, limit);
}

// neighbors lists, for each note, the nearest other notes in the same patch.
// They're the faint roots between sprouts.
export function neighbors(notes: readonly Note[] = NOTES, per = 2): [number, number][] {
  const seen = new Set<string>();
  const edges: [number, number][] = [];
  for (const a of notes) {
    const near = notes
      .filter((b) => b.id !== a.id && b.cluster === a.cluster)
      .map((b) => ({ id: b.id, d: Math.hypot((a.x - b.x) / SPREAD[0], (a.y - b.y) / SPREAD[1]) }))
      .sort((p, q) => p.d - q.d)
      .slice(0, per);
    for (const b of near) {
      const key = a.id < b.id ? `${a.id}-${b.id}` : `${b.id}-${a.id}`;
      if (seen.has(key)) continue;
      seen.add(key);
      edges.push(a.id < b.id ? [a.id, b.id] : [b.id, a.id]);
    }
  }
  return edges;
}

export const CENTER_OF = CENTERS;
