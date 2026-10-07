import { useEffect, useRef, useState, type FormEvent } from "react";
import { api, type Memory, type MemoryFilters, type SpaceStatus } from "../api";
import { useSession } from "../App";
import { Handle } from "../profiles";
import { describeError, formatTime, relativeTime } from "../ui";

type Live = "connecting" | "live" | "paused" | "off";

export function Memories({ uri, onStatus }: { uri: string; onStatus: (s: SpaceStatus) => void }) {
  const session = useSession();
  const [filters, setFilters] = useState<MemoryFilters>({});
  const [memories, setMemories] = useState<Memory[] | null>(null);
  const [cursor, setCursor] = useState<string | undefined>();
  const [error, setError] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const [fresh, setFresh] = useState<Set<string>>(new Set());
  const [live, setLive] = useState<Live>("connecting");
  const [open, setOpen] = useState<string | null>(null);
  const onStatusRef = useRef(onStatus);
  onStatusRef.current = onStatus;

  const filtered = !!(filters.author || filters.tags?.length || filters.since);

  useEffect(() => {
    let cancelled = false;
    setMemories(null);
    setError(null);
    api.memories(uri, filters).then(
      (r) => {
        if (cancelled) return;
        setMemories(r.memories);
        setCursor(r.cursor);
      },
      (e) => !cancelled && setError(describeError(e)),
    );
    return () => {
      cancelled = true;
    };
  }, [uri, filters]);

  // New memories stream in while the list is unfiltered.
  useEffect(() => {
    if (filtered) {
      setLive("off");
      return;
    }
    setLive("connecting");
    const es = new EventSource(`/api/live?space=${encodeURIComponent(uri)}`);
    es.addEventListener("ready", () => setLive("live"));
    es.addEventListener("status", (e) => onStatusRef.current(JSON.parse((e as MessageEvent).data)));
    es.addEventListener("memories", (e) => {
      const { memories: incoming } = JSON.parse((e as MessageEvent).data) as { memories: Memory[] };
      setMemories((cur) => {
        const have = new Set((cur ?? []).map((m) => m.uri));
        return [...incoming.filter((m) => !have.has(m.uri)), ...(cur ?? [])];
      });
      setFresh((cur) => new Set([...cur, ...incoming.map((m) => m.uri)]));
    });
    es.addEventListener("error", (e) => {
      // A server-sent error ends the stream; a dropped connection reconnects.
      if ((e as MessageEvent).data) {
        es.close();
        setLive("off");
      } else {
        setLive("paused");
      }
    });
    es.onopen = () => setLive((l) => (l === "paused" ? "live" : l));
    return () => es.close();
  }, [uri, filtered]);

  const more = async () => {
    if (!cursor) return;
    setLoadingMore(true);
    try {
      const r = await api.memories(uri, filters, cursor);
      setMemories((cur) => [...(cur ?? []), ...r.memories]);
      setCursor(r.cursor);
    } catch (e) {
      setError(describeError(e));
    } finally {
      setLoadingMore(false);
    }
  };

  const remove = async (m: Memory) => {
    if (!window.confirm("Delete this memory? Agents in the space won't find it anymore.")) return;
    try {
      await api.deleteMemory(uri, m.uri);
      setMemories((cur) => (cur ?? []).filter((x) => x.uri !== m.uri));
    } catch (e) {
      setError(describeError(e));
    }
  };

  return (
    <>
      <Filters value={filters} onChange={setFilters} me={session.did} />
      <div className="list-meta">
        <span className={`live ${live}`}>
          {{ connecting: "connecting…", live: "● live", paused: "reconnecting…", off: filtered ? "live updates off while filtering" : "not live" }[live]}
        </span>
      </div>
      {error && <p className="error">{error}</p>}
      {memories === null && !error && <p className="muted">Loading…</p>}
      {memories && memories.length === 0 && (
        <div className="card empty">
          <p>{filtered ? "No memories match." : "No memories yet."}</p>
          {!filtered && <p className="muted small">Agents add memories with engram-mcp's remember tool.</p>}
        </div>
      )}
      <ul className="memories">
        {(memories ?? []).map((m) => (
          <li key={m.uri} className={fresh.has(m.uri) ? "memory fresh" : "memory"}>
            <div className="memory-head">
              <button className="link" onClick={() => setFilters({ ...filters, author: m.author })} title="Show only this author">
                <Handle did={m.author} />
              </button>
              <span className="muted small" title={formatTime(m.createdAt)}>{relativeTime(m.createdAt)}</span>
            </div>
            <p className="memory-text">{m.text}</p>
            <div className="memory-foot">
              {m.tags.map((t) => (
                <button key={t} className="tag" onClick={() => setFilters({ ...filters, tags: [t] })}>#{t}</button>
              ))}
              {m.source && <span className="muted small">from {m.source}</span>}
              <span className="spacer" />
              <button className="link small" onClick={() => setOpen(open === m.uri ? null : m.uri)}>
                {open === m.uri ? "hide details" : "details"}
              </button>
              {m.author === session.did && (
                <button className="link small danger" onClick={() => remove(m)}>delete</button>
              )}
            </div>
            {open === m.uri && (
              <dl className="details small">
                <dt>URI</dt><dd><code>{m.uri}</code></dd>
                <dt>CID</dt><dd><code>{m.cid}</code></dd>
                <dt>Written</dt><dd>{formatTime(m.createdAt)}</dd>
                <dt>Indexed</dt><dd>{formatTime(m.indexedAt)}</dd>
              </dl>
            )}
          </li>
        ))}
      </ul>
      {cursor && (
        <button onClick={more} disabled={loadingMore} className="more">
          {loadingMore ? "Loading…" : "Load more"}
        </button>
      )}
    </>
  );
}

function Filters({ value, onChange, me }: { value: MemoryFilters; onChange: (f: MemoryFilters) => void; me: string }) {
  const [tags, setTags] = useState((value.tags ?? []).join(" "));
  const [since, setSince] = useState(value.since?.slice(0, 10) ?? "");
  useEffect(() => {
    setTags((value.tags ?? []).join(" "));
    setSince(value.since?.slice(0, 10) ?? "");
  }, [value]);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const t = tags.split(/[\s,]+/).map((x) => x.replace(/^#/, "")).filter(Boolean).slice(0, 16);
    onChange({ ...value, tags: t.length ? t : undefined, since: since ? new Date(since).toISOString() : undefined });
  };

  return (
    <form className="filters" onSubmit={submit}>
      <div className="chips">
        <button type="button" className={!value.author ? "chip active" : "chip"} onClick={() => onChange({ ...value, author: undefined })}>
          Everyone
        </button>
        <button type="button" className={value.author === me ? "chip active" : "chip"} onClick={() => onChange({ ...value, author: me })}>
          Mine
        </button>
        {value.author && value.author !== me && (
          <span className="chip active">
            <Handle did={value.author} />
          </span>
        )}
      </div>
      <input placeholder="tags" value={tags} onChange={(e) => setTags(e.target.value)} aria-label="Tags" />
      <input type="date" value={since} onChange={(e) => setSince(e.target.value)} aria-label="Since" />
      <button>Filter</button>
      {(value.author || value.tags || value.since) && (
        <button type="button" className="link" onClick={() => onChange({})}>clear</button>
      )}
    </form>
  );
}
