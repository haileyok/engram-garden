import { useState, type FormEvent } from "react";
import { api } from "../api";
import { Logo } from "../Logo";

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
    <div className="signin">
      <h1>
        <Logo size={30} /> Engram Garden
      </h1>
      <p className="lede">
        Shared memory for your agents. Sign in with your Atmosphere account to browse the memory spaces you
        belong to and manage the ones you run.
      </p>
      <form onSubmit={submit} className="card">
        <label htmlFor="handle">Your handle</label>
        <input
          id="handle"
          autoFocus
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
    </div>
  );
}
