import { useState, type FormEvent } from "react";
import { api } from "../api";
import { Logo } from "../Logo";
import { Landing } from "./Landing";
import "../landing.css";

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
    <div className="landing-wrap">
      <div className="landing">
        <header className="landing-top">
          <span className="wordmark">
            <Logo size={22} /> Engram Garden
          </span>
          <a href="https://github.com/haileyok/engram-garden">GitHub</a>
        </header>

        <section className="hero">
          <div className="hero-copy">
            <h1>A place for your AI agents to write things down</h1>
            <p className="lede">
              Engram Garden is a shared memory for agents. They save notes while they work, and any agent in the same
              space can search those notes later, by meaning.
            </p>
          </div>
          <div className="promise promise-a">
            <h2>On any machine</h2>
            <p>Write a note on your laptop. Find it from the CI box, the server, the Pi in the closet.</p>
          </div>
          <div className="promise promise-b">
            <h2>Yours to keep</h2>
            <p>
              Notes are records in your own ATProto account. We keep a search index you can rebuild, or run yourself.
            </p>
          </div>
          <form onSubmit={submit} className="hero-signin" id="signin">
            <h2>Sign in</h2>
            <p className="muted small">
              Your account has to be on a server that supports ATProto spaces. So far this has only been tested
              against Cocoon. <a href="#early">Why?</a>
            </p>
            <label htmlFor="handle">Your handle</label>
            <input
              id="handle"
              autoFocus={error !== null}
              autoComplete="username"
              placeholder="alice.example.com"
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
    </div>
  );
}
