import { useEffect, useState } from "react";
import type { ModelInfo } from "../api";
import { describeLocalModel, knownPrefixes, listLocalModels, OLLAMA, type LocalModel } from "../lib/ollama";

type Prefixes = { documentPrefix: string; queryPrefix: string };

// Identifies a model exactly: its name, digest and vector size. It can read
// them from the Ollama on this computer, or take them typed in.
export function ModelForm({
  value,
  onChange,
  disabled,
}: {
  value: ModelInfo | null;
  onChange: (m: ModelInfo | null, p: Prefixes) => void;
  disabled?: boolean;
}) {
  const [local, setLocal] = useState<LocalModel[] | null>(null);
  const [detectError, setDetectError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [model, setModel] = useState(value?.model ?? "");
  const [digest, setDigest] = useState(value?.modelDigest ?? "");
  const [dims, setDims] = useState(value ? String(value.dims) : "");
  const [prefixes, setPrefixes] = useState<Prefixes>(knownPrefixes(value?.model ?? ""));

  useEffect(() => {
    const n = Number(dims);
    const ok = model.trim() && digest.trim() && Number.isInteger(n) && n > 0 && n <= 16000;
    onChange(ok ? { model: model.trim(), modelDigest: digest.trim(), dims: n } : null, prefixes);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [model, digest, dims, prefixes]);

  const detect = async () => {
    setBusy(true);
    setDetectError(null);
    try {
      const models = await listLocalModels();
      setLocal(models);
      if (models.length === 0) setDetectError("Ollama has no models. Run: ollama pull nomic-embed-text");
    } catch {
      setDetectError(
        `Couldn't reach Ollama at ${OLLAMA}. Start it with OLLAMA_ORIGINS=${window.location.origin} so this page may ask it, or type the model's details below.`,
      );
    } finally {
      setBusy(false);
    }
  };

  const pick = async (m: LocalModel) => {
    setBusy(true);
    setDetectError(null);
    try {
      const info = await describeLocalModel(m);
      setModel(info.model);
      setDigest(info.modelDigest);
      setDims(String(info.dims));
      setPrefixes(knownPrefixes(info.model));
    } catch (e) {
      setDetectError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="model-form">
      <div className="row">
        <button type="button" onClick={detect} disabled={disabled || busy}>
          {busy ? "Asking Ollama…" : "Read from my Ollama"}
        </button>
        {local && local.length > 0 && (
          <select
            disabled={disabled || busy}
            defaultValue=""
            onChange={(e) => {
              const m = local.find((x) => x.name === e.target.value);
              if (m) pick(m);
            }}
          >
            <option value="" disabled>Choose a model…</option>
            {local.map((m) => (
              <option key={m.name} value={m.name}>{m.name}</option>
            ))}
          </select>
        )}
      </div>
      {detectError && <p className="error small">{detectError}</p>}
      <div className="grid-3">
        <label>
          Model
          <input value={model} onChange={(e) => { setModel(e.target.value); setPrefixes(knownPrefixes(e.target.value)); }} placeholder="nomic-embed-text" disabled={disabled} />
        </label>
        <label>
          Digest
          <input value={digest} onChange={(e) => setDigest(e.target.value)} placeholder="sha256:…" disabled={disabled} />
        </label>
        <label>
          Dimensions
          <input value={dims} onChange={(e) => setDims(e.target.value)} inputMode="numeric" placeholder="768" disabled={disabled} />
        </label>
      </div>
      <details>
        <summary className="small">Task prefixes</summary>
        <div className="grid-2">
          <label>
            Before memories
            <input value={prefixes.documentPrefix} onChange={(e) => setPrefixes({ ...prefixes, documentPrefix: e.target.value })} disabled={disabled} />
          </label>
          <label>
            Before queries
            <input value={prefixes.queryPrefix} onChange={(e) => setPrefixes({ ...prefixes, queryPrefix: e.target.value })} disabled={disabled} />
          </label>
        </div>
        <p className="muted small">Filled in for models known to need them, like nomic-embed-text.</p>
      </details>
      <p className="muted small">
        To look up a digest by hand: <code>curl -s localhost:11434/api/tags</code>. Or declare the model from a
        terminal with <code>engram model --set &lt;name&gt;</code>.
      </p>
    </div>
  );
}
