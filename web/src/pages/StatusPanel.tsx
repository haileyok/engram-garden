import type { ModelInfo, SpaceStatus } from "../api";
import { Handle } from "../profiles";

function Model({ m }: { m: ModelInfo }) {
  return (
    <span>
      <strong>{m.model}</strong> <span className="muted small">· {m.dims} dimensions · <code>{m.modelDigest.slice(0, 19)}…</code></span>
    </span>
  );
}

export function StatusPanel({ status }: { status: SpaceStatus }) {
  const declared = status.config;
  return (
    <div className="stack">
      <section className="card">
        <h2>Index</h2>
        <dl className="details">
          <dt>Memories</dt>
          <dd>{status.memories.toLocaleString()}</dd>
          <dt>Model</dt>
          <dd>{declared ? <Model m={declared} /> : <span className="muted">none declared yet; nothing is indexed until the authority declares one</span>}</dd>
          {status.building && (
            <>
              <dt>Moving to</dt>
              <dd>
                <Model m={status.building} />
                <br />
                <span className="muted small">
                  {(status.buildingMemories ?? 0).toLocaleString()} of {status.memories.toLocaleString()} memories have a vector for it
                  {status.memories > 0 && ` (${Math.round(((status.buildingMemories ?? 0) / status.memories) * 100)}%)`}
                </span>
              </dd>
            </>
          )}
        </dl>
      </section>
      {status.skipped.length > 0 && (
        <section className="card">
          <h2>Memories not indexed</h2>
          <p className="muted small">
            These authors wrote memories whose vectors don't come from the space's model, so search can't use
            them. Their agents need to embed with the declared model.
          </p>
          <ul className="plain">
            {status.skipped.map((s) => (
              <li key={s.author}>
                <Handle did={s.author} /> <span className="muted">· {s.count} skipped</span>
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}
