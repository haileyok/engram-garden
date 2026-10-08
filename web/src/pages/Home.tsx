import { useEffect, useState, type FormEvent } from "react";
import { api, type SpaceSummary } from "../api";
import { Link, useRouter } from "../router";
import { Handle } from "../profiles";
import { parseSpaceUri, SPACE_TYPE, spacePath } from "../lib/uri";
import { describeError } from "../ui";

const RECENT_KEY = "engram:recent-spaces";

// Spaces opened by URI. The PDS lists only spaces a user governs or has
// written to, so a new member finds theirs by URI the first time.
export function recentSpaces(): string[] {
  try {
    const v = JSON.parse(localStorage.getItem(RECENT_KEY) ?? "[]");
    return Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
  } catch {
    return [];
  }
}

export function rememberSpace(uri: string) {
  const next = [uri, ...recentSpaces().filter((u) => u !== uri)].slice(0, 20);
  localStorage.setItem(RECENT_KEY, JSON.stringify(next));
}

function forgetSpace(uri: string) {
  localStorage.setItem(RECENT_KEY, JSON.stringify(recentSpaces().filter((u) => u !== uri)));
}

export function Home() {
  const { navigate } = useRouter();
  const [spaces, setSpaces] = useState<SpaceSummary[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [recent, setRecent] = useState(recentSpaces);
  const [open, setOpen] = useState("");
  const [openError, setOpenError] = useState<string | null>(null);

  useEffect(() => {
    api.spaces().then((r) => setSpaces(r.spaces), (e) => setError(describeError(e)));
  }, []);

  const listed = new Set((spaces ?? []).map((s) => s.uri));
  const others = recent.filter((u) => !listed.has(u) && parseSpaceUri(u));

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const ref = parseSpaceUri(open);
    if (!ref) return setOpenError("That isn't a space URI (at://did:…/space/garden.engram.space/…).");
    if (ref.type !== SPACE_TYPE) return setOpenError("That's not a memory space.");
    rememberSpace(ref.uri);
    navigate(spacePath(ref.uri));
  };

  return (
    <>
      <div className="title-row">
        <h1>Your memory spaces</h1>
        <Link to="/new" className="button primary">New space</Link>
      </div>
      {error && <p className="error">{error}</p>}
      {spaces === null && !error && <p className="muted">Loading…</p>}
      {spaces && spaces.length === 0 && others.length === 0 && (
        <div className="card">
          <h2>Set up your first memory space</h2>
          <p className="muted">
            A memory space is where your agents write down what they learn and search for it later.
          </p>
          <ol className="connect">
            <li>
              <strong>Create a space.</strong> Pick a name and the embedding model its agents will use.
            </li>
            <li>
              <strong>Let the appview index it.</strong> You approve read-only access once, on your account's own sign-in
              page. Until you do, memories in the space can't be searched.
            </li>
            <li>
              <strong>Add your agents</strong> as members, under the space's <strong>Manage</strong> tab. An agent can use
              your account or have its own.
            </li>
            <li>
              <strong>Connect an agent.</strong> The space's <strong>Connect an agent</strong> tab has the commands to copy.
            </li>
          </ol>
          <p>
            <Link to="/new" className="button primary">Create your first space</Link>
          </p>
          <p className="muted small">Someone added you to theirs? Open it by its URI below.</p>
        </div>
      )}
      <ul className="space-list">
        {(spaces ?? []).map((s) => (
          <li key={s.uri}>
            <Link to={spacePath(s.uri)} className="space-item">
              <span className="space-name">{s.name}</span>
              <span className="muted">{s.isAuthority ? "yours" : <>run by <Handle did={s.authority} /></>}</span>
            </Link>
          </li>
        ))}
        {others.map((u) => {
          const ref = parseSpaceUri(u)!;
          return (
            <li key={u}>
              <Link to={spacePath(u)} className="space-item">
                <span className="space-name">{ref.name}</span>
                <span className="muted">opened before · run by <Handle did={ref.authority} /></span>
              </Link>
              <button
                className="link small"
                onClick={() => {
                  forgetSpace(u);
                  setRecent(recentSpaces());
                }}
              >
                forget
              </button>
            </li>
          );
        })}
      </ul>
      <form onSubmit={submit} className="card open-space">
        <label htmlFor="open">Open a space by URI</label>
        <div className="row">
          <input
            id="open"
            placeholder="at://did:plc:…/space/garden.engram.space/memory"
            value={open}
            onChange={(e) => {
              setOpen(e.target.value);
              setOpenError(null);
            }}
          />
          <button disabled={!open.trim()}>Open</button>
        </div>
        {openError && <p className="error">{openError}</p>}
      </form>
    </>
  );
}
