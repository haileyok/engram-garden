import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type Service, type SpaceStatus } from "../api";
import { useSession } from "../App";
import { useRouter } from "../router";
import { Handle } from "../profiles";
import { parseSpaceUri, spacePath } from "../lib/uri";
import { describeError } from "../ui";
import { Memories } from "./Memories";
import { StatusPanel } from "./StatusPanel";
import { Manage } from "./Manage";
import { rememberSpace } from "./Home";

type Tab = "memories" | "status" | "manage";

export function SpacePage({ uri }: { uri: string }) {
  const session = useSession();
  const { loc, navigate } = useRouter();
  const ref = parseSpaceUri(uri);
  const isAuthority = ref?.authority === session.did;
  const tab = (loc.query.get("tab") as Tab) || "memories";
  const [status, setStatus] = useState<SpaceStatus | null>(null);
  const [statusError, setStatusError] = useState<ApiError | Error | null>(null);

  const load = useCallback(() => {
    api.status(uri).then(
      (s) => {
        setStatus(s);
        setStatusError(null);
        rememberSpace(uri);
      },
      (e) => setStatusError(e),
    );
  }, [uri]);
  useEffect(load, [load]);

  if (!ref) return <p className="error">Not a space URI.</p>;
  const unindexed = statusError instanceof ApiError && statusError.code === "UnknownSpace";

  const tabs: [Tab, string][] = [["memories", "Memories"], ["status", "Status"]];
  if (isAuthority) tabs.push(["manage", "Manage"]);

  return (
    <>
      <div className="space-head">
        <h1>{ref.name}</h1>
        <p className="muted small">
          {isAuthority ? "Your space" : <>Run by <Handle did={ref.authority} /></>} ·{" "}
          <button type="button" className="link uri" title="Copy the space's URI" onClick={() => navigator.clipboard?.writeText(uri)}>
            <code>{uri}</code>
          </button>
        </p>
      </div>
      {unindexed ? (
        <NotIndexed uri={uri} isAuthority={isAuthority} onRegistered={load} onManage={() => navigate(spacePath(uri) + "?tab=manage")} />
      ) : statusError ? (
        <p className="error">{describeError(statusError)}</p>
      ) : null}
      <nav className="tabs">
        {tabs.map(([t, label]) => (
          <button key={t} className={t === tab ? "tab active" : "tab"} onClick={() => navigate(spacePath(uri) + (t === "memories" ? "" : `?tab=${t}`))}>
            {label}
          </button>
        ))}
      </nav>
      {tab === "memories" && !unindexed && !statusError && <Memories uri={uri} onStatus={setStatus} />}
      {tab === "status" && status && <StatusPanel status={status} />}
      {tab === "manage" && isAuthority && <Manage uri={uri} status={status} onChanged={load} />}
    </>
  );
}

// The appview doesn't index the space yet.
function NotIndexed({ uri, isAuthority, onRegistered, onManage }: { uri: string; isAuthority: boolean; onRegistered: () => void; onManage: () => void }) {
  const [service, setService] = useState<Service | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    api.service().then(setService, () => {});
  }, []);

  const register = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.register(uri);
      onRegistered();
    } catch (e) {
      if (e instanceof ApiError && e.code === "NotAMember") {
        setError(isAuthority ? "Add the appview's account as a member first (under Manage)." : "The space's authority needs to add the appview's account as a member first.");
      } else {
        setError(describeError(e));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card notice">
      <h2>Not indexed yet</h2>
      {service?.registration === "closed" ? (
        <p>This appview indexes only the spaces its operator adds. Ask them to add this one.</p>
      ) : (
        <>
          <p>
            The appview hasn't been asked to index this space. It needs to be a member of the space
            {service?.account && <> (its account is <Handle did={service.account} />)</>}, then any member can register the space.
          </p>
          <div className="row">
            <button className="primary" onClick={register} disabled={busy}>
              {busy ? "Registering…" : "Index this space"}
            </button>
            {isAuthority && <button onClick={onManage}>Manage members</button>}
          </div>
        </>
      )}
      {error && <p className="error">{error}</p>}
    </div>
  );
}
