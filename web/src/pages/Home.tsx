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
        <div className="card empty">
          <p>No memory spaces yet.</p>
          <p className="muted">
            Create one, or if someone added you to theirs, open it by its URI below.
          </p>
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
