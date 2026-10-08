import { useState, type FormEvent } from "react";
import { api } from "../api";
import { Logo } from "../Logo";
import { Landing } from "./Landing";

export function SignIn() {
  const params = new URLSearchParams(window.location.search);
  const [handle, setHandle] = useState("");
  const [error, setError] = useState<string | null>(params.get("signin_error"));
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const { redirect } = await api.login(handle);
      window.location.assign(redirect);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setBusy(false);
    }
  };

  return (
    <div className="landing">
      <header className="landing-top">
        <span className="brand">
          <Logo /> Engram Garden
        </span>
        <a href="https://github.com/haileyok/engram-garden">GitHub</a>
      </header>

      <section className="hero">
        <div className="hero-copy">
          <h1>A memory your AI agents share</h1>
          <p className="lede">
            Agents start every session knowing nothing, and can't see what the agent next to them learned. Engram
            Garden gives them a shared place to write down decisions, gotchas and how things are done, and to find
            those notes again by meaning, from any machine.
          </p>
          <p>
            <a href="#how">See how it works</a>
          </p>
        </div>
        <form onSubmit={submit} className="card hero-signin" id="signin">
          <h2>Sign in</h2>
          <p className="muted small">
            Use your Bluesky handle, or any other ATProto account, to browse the memory spaces you belong to and set
            up ones you run.
          </p>
          <label htmlFor="handle">Your handle</label>
          <input
            id="handle"
            autoFocus={error !== null}
            autoComplete="username"
            placeholder="alice.bsky.social"
            value={handle}
            onChange={(e) => setHandle(e.target.value)}
          />
          <button className="primary" disabled={busy || !handle.trim()}>
            {busy ? "Redirecting…" : "Sign in"}
          </button>
          {error && <p className="error">{error}</p>}
        </form>
      </section>

      <Landing />
    </div>
  );
}
