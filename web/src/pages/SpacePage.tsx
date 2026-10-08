import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type SpaceStatus } from "../api";
import { goToGrant, IndexingNotice, useService } from "../components/Indexing";
import { ConnectAgent } from "../components/ConnectAgent";
import { useSession } from "../App";
import { useRouter } from "../router";
import { Handle } from "../profiles";
import { parseSpaceUri, spacePath } from "../lib/uri";
import { describeError } from "../ui";
import { Memories } from "./Memories";
import { StatusPanel } from "./StatusPanel";
import { Manage } from "./Manage";
import { rememberSpace } from "./Home";

type Tab = "memories" | "status" | "manage" | "agents";

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

  const tabs: [Tab, string][] = [["memories", "Memories"], ["status", "Status"], ["agents", "Connect an agent"]];
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
      <GrantOutcome />
      {unindexed ? (
        <NotIndexed uri={uri} isAuthority={isAuthority} />
      ) : statusError ? (
        <p className="error">{describeError(statusError)}</p>
      ) : (
        <IndexingNotice uri={uri} access={status?.access} isAuthority={isAuthority} />
      )}
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
      {tab === "agents" && (
        <ConnectAgent uri={uri} status={status} isAuthority={isAuthority} onManage={() => navigate(spacePath(uri) + "?tab=manage")} />
      )}
    </>
  );
}

// The outcome of a grant or stop, when the appview sends the browser back.
function GrantOutcome() {
  const { loc } = useRouter();
  const outcome = loc.query.get("indexing");
  const error = loc.query.get("indexing_error");
  if (error) return <p className="error">{error}</p>;
  if (outcome === "granted") return <div className="notice inline">The appview can read this space now. It's indexing it.</div>;
  if (outcome === "stopped") return <div className="notice inline">The appview has stopped indexing this space.</div>;
  return null;
}

// The appview doesn't index the space yet.
function NotIndexed({ uri, isAuthority }: { uri: string; isAuthority: boolean }) {
  const service = useService();
  return (
    <div className="card notice">
      <h2>Not indexed yet</h2>
      {service?.registration === "closed" ? (
        <p>This appview indexes only the spaces its operator adds. Ask them to add this one.</p>
      ) : isAuthority ? (
        <>
          <p>
            The appview needs your permission to read this space before it can index it. You'll confirm on your
            account's sign-in page; it gets read-only access to the memory spaces you run.
          </p>
          {service?.grantUrl ? (
            <button className="primary" onClick={() => goToGrant(service, uri, "grant")}>
              Let the appview index this space
            </button>
          ) : (
            service !== undefined && <p className="muted">This appview isn't set up to index spaces you grant it.</p>
          )}
        </>
      ) : (
        <p>The appview hasn't been allowed to index this space. Its authority can allow it from the space's Manage tab.</p>
      )}
    </div>
  );
}
