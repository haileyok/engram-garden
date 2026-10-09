import { useEffect, useState } from "react";
import { api, type Access, type Service } from "../api";
import { grantLink, spacePath } from "../lib/uri";

// The appview reads a space with its authority's permission, granted on
// the authority's own sign-in page. These send the authority there and
// show where things stand.

export function useService(): Service | null | undefined {
  const [service, setService] = useState<Service | null | undefined>(undefined);
  useEffect(() => {
    api.service().then(setService, () => setService(null));
  }, []);
  return service;
}

// goToGrant leaves the app for the appview's grant page, which comes back
// to the space's page (or one of its tabs).
export function goToGrant(service: Service, uri: string, mode: "grant" | "stop", tab?: string) {
  if (!service.grantUrl) return;
  const back = window.location.origin + spacePath(uri) + (tab ? `?tab=${tab}` : "");
  window.location.assign(grantLink(service.grantUrl, uri, mode, back));
}

function when(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "" : d.toLocaleDateString(undefined, { dateStyle: "medium" });
}

// IndexingCard is the authority's control over indexing, under Manage.
export function IndexingCard({ uri, access }: { uri: string; access?: Access }) {
  const service = useService();
  const state = access?.state ?? "missing";

  const stop = () => {
    if (!service) return;
    if (!window.confirm("Stop the appview indexing this space? Search keeps the memories indexed so far, but new ones won't appear until you let it index the space again.")) return;
    goToGrant(service, uri, "stop", "manage");
  };

  return (
    <section className="panel">
      <h2>Indexing</h2>
      {service === undefined ? (
        <p className="muted">Loading…</p>
      ) : !service?.grantUrl ? (
        <p className="muted">This appview isn't set up to index spaces you grant it.</p>
      ) : state === "granted" ? (
        <>
          <p>
            The appview reads this space with your permission{access?.grantedAt && <>, given {when(access.grantedAt)}</>}, and keeps
            its search index up to date. It can only read; it can't write or change members.
          </p>
          <button className="danger" onClick={stop}>Stop indexing</button>
        </>
      ) : state === "lapsed" ? (
        <>
          <p>
            Your permission stopped working, so the appview can't update the index. This happens when the app's access
            is revoked at your account's server, or after a long time.
          </p>
          {access?.error && <p className="muted small"><code>{access.error}</code></p>}
          <button className="primary" onClick={() => goToGrant(service, uri, "grant", "manage")}>Grant access again</button>
        </>
      ) : service.registration === "closed" ? (
        <p className="muted">This appview indexes only the spaces its operator adds.</p>
      ) : (
        <>
          <p>
            The appview needs your permission to read this space before it can index it. You'll confirm on your
            account's sign-in page; it gets read-only access to the memory spaces you run.
          </p>
          <button className="primary" onClick={() => goToGrant(service, uri, "grant", "manage")}>
            Let the appview index this space
          </button>
        </>
      )}
    </section>
  );
}

// IndexingNotice says, on the space's page, when the appview can't keep
// the space current.
export function IndexingNotice({ uri, access, isAuthority }: { uri: string; access?: Access; isAuthority: boolean }) {
  const service = useService();
  if (!access || access.state === "granted") return null;
  const lapsed = access.state === "lapsed";
  return (
    <div className="notice inline">
      {lapsed ? "The appview's permission to read this space stopped working" : "The appview isn't allowed to read this space"}, so
      new memories won't show up.{" "}
      {isAuthority && service?.grantUrl ? (
        <button className="primary small" onClick={() => goToGrant(service, uri, "grant")}>
          {lapsed ? "Grant access again" : "Let it index the space"}
        </button>
      ) : (
        !isAuthority && "The space's authority can let it index the space again."
      )}
    </div>
  );
}
