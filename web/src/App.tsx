import { createContext, useContext, useEffect, useState } from "react";
import { api, ApiError, type Session } from "./api";
import { Link, useRouter } from "./router";
import { SignIn } from "./pages/SignIn";
import { Home } from "./pages/Home";
import { NewSpace } from "./pages/NewSpace";
import { SpacePage } from "./pages/SpacePage";
import { spaceUri } from "./lib/uri";
import { Logo } from "./Logo";

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

  if (error) return <div className="page"><p className="error">{error}</p></div>;
  if (session === undefined) return <div className="page"><p className="muted">Loading…</p></div>;
  if (session === null) return <SignIn />;

  const signOut = async () => {
    await api.logout().catch(() => {});
    setSession(null);
    window.history.replaceState(null, "", "/");
  };

  return (
    <SessionContext.Provider value={session}>
      <header className="topbar">
        <Link to="/" className="brand">
          <Logo /> Engram Garden
        </Link>
        <div className="who">
          <span title={session.did}>{session.handle ? `@${session.handle}` : session.did}</span>
          <button className="link" onClick={signOut}>Sign out</button>
        </div>
      </header>
      <main className="page">
        <Routes />
      </main>
    </SessionContext.Provider>
  );
}

function Routes() {
  const { loc } = useRouter();
  const m = /^\/space\/([^/]+)\/([^/]+)\/?$/.exec(loc.path);
  if (m) {
    const uri = spaceUri(decodeURIComponent(m[1]), decodeURIComponent(m[2]));
    return <SpacePage key={uri} uri={uri} />;
  }
  if (loc.path === "/new") return <NewSpace />;
  return <Home />;
}
