import { createContext, useContext, useEffect, useState } from "react";
import { api, ApiError, type Session } from "./api";
import { Link, useRouter } from "./router";
import { SignIn } from "./pages/SignIn";
import { Home } from "./pages/Home";
import { NewSpace } from "./pages/NewSpace";
import { SpacePage } from "./pages/SpacePage";
import { spaceUri } from "./lib/uri";
import { Logo } from "./Logo";
import "./app.css";

const SessionContext = createContext<Session | null>(null);

export function useSession(): Session {
  const s = useContext(SessionContext);
  if (!s) throw new Error("not signed in");
  return s;
}

export function App() {
  const [session, setSession] = useState<Session | null | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .session()
      .then(setSession)
      .catch((e: unknown) => {
        if (e instanceof ApiError && e.status === 401) setSession(null);
        else setError(e instanceof Error ? e.message : String(e));
      });
  }, []);

  // A request that finds the session gone sends the user back to sign in.
  useEffect(() => {
    const onExpired = () => setSession(null);
    window.addEventListener("engram:signed-out", onExpired);
    return () => window.removeEventListener("engram:signed-out", onExpired);
  }, []);

  if (error) return <div className="app"><main className="app-main"><p className="error">{error}</p></main></div>;
  if (session === undefined) return <div className="app"><main className="app-main"><p className="muted">Loading…</p></main></div>;
  if (session === null) return <SignIn />;

  const signOut = async () => {
    await api.logout().catch(() => {});
    setSession(null);
    window.history.replaceState(null, "", "/");
  };

  return (
    <SessionContext.Provider value={session}>
      <div className="app">
        <header className="app-top">
          <Link to="/" className="wordmark">
            <Logo size={22} /> Engram Garden
          </Link>
          <div className="who">
            <span title={session.did}>{session.handle ? `@${session.handle}` : session.did}</span>
            <button className="link" onClick={signOut}>Sign out</button>
          </div>
        </header>
        <main className="app-main">
          <Routes />
        </main>
      </div>
    </SessionContext.Provider>
  );
}

function Routes() {
  const { loc } = useRouter();
  const m = /^\/space\/([^/]+)\/([^/]+)\/?$/.exec(loc.path);
  if (m) {
    let uri: string;
    try {
      uri = spaceUri(decodeURIComponent(m[1]), decodeURIComponent(m[2]));
    } catch {
      return <p className="error">That isn't a valid space address.</p>;
    }
    return <SpacePage key={uri} uri={uri} />;
  }
  if (loc.path === "/new") return <NewSpace />;
  return <Home />;
}
