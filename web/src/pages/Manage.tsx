import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api, type Member, type ModelInfo, type Service, type SpaceConfig, type SpaceStatus } from "../api";
import { useSession } from "../App";
import { Handle } from "../profiles";
import { describeError } from "../ui";
import { ModelForm } from "../components/ModelForm";

export function Manage({ uri, status, onChanged }: { uri: string; status: SpaceStatus | null; onChanged: () => void }) {
  return (
    <div className="stack">
      <Members uri={uri} onChanged={onChanged} />
      <ModelConfig uri={uri} status={status} onChanged={onChanged} />
    </div>
  );
}

function Members({ uri, onChanged }: { uri: string; onChanged: () => void }) {
  const session = useSession();
  const [members, setMembers] = useState<Member[] | null>(null);
  const [service, setService] = useState<Service | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [who, setWho] = useState("");
  const [write, setWrite] = useState(true);
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    api.members(uri).then((r) => setMembers(r.members), (e) => setError(describeError(e)));
  }, [uri]);
  useEffect(load, [load]);
  useEffect(() => {
    api.service().then(setService, () => {});
  }, []);

  const add = async (member: string, w: boolean) => {
    setBusy(true);
    setError(null);
    try {
      await api.putMember(uri, member, true, w);
      setWho("");
      load();
      onChanged();
    } catch (e) {
      setError(describeError(e));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (m: Member) => {
    const label = m.did === service?.account ? "the appview (it will stop indexing this space)" : m.did;
    if (!window.confirm(`Remove ${label} from the space? They'll lose access, but memories already in their repo stay there.`)) return;
    try {
      await api.removeMember(uri, m.did);
      load();
    } catch (e) {
      setError(describeError(e));
    }
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    add(who, write);
  };

  const appviewMissing = service?.account && members && !members.some((m) => m.did === service.account);

  return (
    <section className="card">
      <h2>Members</h2>
      <p className="muted small">
        Members can read every memory in the space; writers can add their own. Give each agent its own account.
        You're always a member.
      </p>
      {appviewMissing && (
        <div className="notice inline">
          The appview can't index this space until its account is a member.{" "}
          <button className="primary small" disabled={busy} onClick={() => add(service!.account!, false)}>
            Add the appview
          </button>
        </div>
      )}
      {error && <p className="error">{error}</p>}
      {members === null && !error && <p className="muted">Loading…</p>}
      <ul className="members">
        {(members ?? []).map((m) => (
          <li key={m.did}>
            <Handle did={m.did} />
            {m.did === service?.account && <span className="badge">appview</span>}
            {m.did === session.did && <span className="badge">you</span>}
            <span className="muted small">{m.write ? "reads and writes" : m.read ? "reads" : "no access"}</span>
            <span className="spacer" />
            <button className="link small danger" onClick={() => remove(m)}>remove</button>
          </li>
        ))}
      </ul>
      <form onSubmit={submit} className="row">
        <input placeholder="handle or DID" value={who} onChange={(e) => setWho(e.target.value)} aria-label="New member" />
        <label className="check">
          <input type="checkbox" checked={write} onChange={(e) => setWrite(e.target.checked)} /> can write
        </label>
        <button disabled={busy || !who.trim()}>Add member</button>
      </form>
    </section>
  );
}

function ModelConfig({ uri, status, onChanged }: { uri: string; status: SpaceStatus | null; onChanged: () => void }) {
  const [config, setConfig] = useState<SpaceConfig | null | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState<ModelInfo | null>(null);
  const [prefixes, setPrefixes] = useState({ documentPrefix: "", queryPrefix: "" });
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState(false);

  useEffect(() => {
    api.config(uri).then((r) => setConfig(r.config), (e) => setError(describeError(e)));
  }, [uri]);

  const act = async (action: "declare" | "next" | "promote" | "cancel", confirmText?: string) => {
    if (confirmText && !window.confirm(confirmText)) return;
    setBusy(true);
    setError(null);
    try {
      const r = await api.putConfig(uri, action, action === "declare" || action === "next" ? draft ?? undefined : undefined, prefixes);
      setConfig(r.config);
      setEditing(false);
      onChanged();
    } catch (e) {
      setError(describeError(e));
    } finally {
      setBusy(false);
    }
  };

  if (config === undefined && !error) return <section className="card"><p className="muted">Loading…</p></section>;

  const coverage =
    status?.building && status.memories > 0 ? Math.round(((status.buildingMemories ?? 0) / status.memories) * 100) : null;

  return (
    <section className="card">
      <h2>Embedding model</h2>
      {error && <p className="error">{error}</p>}
      {config ? (
        <dl className="details">
          <dt>Model</dt>
          <dd>
            <strong>{config.model}</strong> · {config.dims} dimensions
            <br />
            <code className="small">{config.modelDigest}</code>
          </dd>
          {(config.documentPrefix || config.queryPrefix) && (
            <>
              <dt>Prefixes</dt>
              <dd className="small"><code>{JSON.stringify(config.documentPrefix ?? "")}</code> / <code>{JSON.stringify(config.queryPrefix ?? "")}</code></dd>
            </>
          )}
          {config.next && (
            <>
              <dt>Moving to</dt>
              <dd>
                <strong>{config.next.model}</strong> · {config.next.dims} dimensions
                {coverage !== null && <span className="muted"> · {coverage}% of memories ready</span>}
                <p className="muted small">
                  Agents running engram-mcp re-embed their memories in the background. Switch once most are ready;
                  memories without the new vector drop out of search.
                </p>
                <div className="row">
                  <button className="primary" disabled={busy} onClick={() => act("promote", `Switch searches to ${config.next!.model}?`)}>
                    Switch to {config.next.model}
                  </button>
                  <button disabled={busy} onClick={() => act("cancel")}>Cancel the move</button>
                </div>
              </dd>
            </>
          )}
        </dl>
      ) : (
        <p className="muted">No model declared. Nothing is indexed until you declare one.</p>
      )}
      {!config?.next && (
        editing || !config ? (
          <div className="stack">
            <ModelForm value={null} onChange={(m, p) => { setDraft(m); setPrefixes(p); }} disabled={busy} />
            <div className="row">
              {config ? (
                <button className="primary" disabled={busy || !draft} onClick={() => act("next")}>
                  Start moving to this model
                </button>
              ) : (
                <button className="primary" disabled={busy || !draft} onClick={() => act("declare")}>
                  Declare this model
                </button>
              )}
              {config && <button onClick={() => setEditing(false)}>Cancel</button>}
            </div>
          </div>
        ) : (
          <button onClick={() => setEditing(true)}>Change model…</button>
        )
      )}
    </section>
  );
}
