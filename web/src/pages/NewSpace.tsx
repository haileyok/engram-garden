import { useEffect, useState, type FormEvent } from "react";
import { api, type ModelInfo, type Service } from "../api";
import { useSession } from "../App";
import { useRouter } from "../router";
import { spacePath, validSpaceName } from "../lib/uri";
import { describeError } from "../ui";
import { ModelForm } from "../components/ModelForm";
import { goToGrant } from "../components/Indexing";
import { rememberSpace } from "./Home";

type Step = { label: string; state: "todo" | "doing" | "done" | "failed" | "skipped"; note?: string };

// Creating a memory space: create it at your PDS, declare the model, then
// let the appview read it (on the appview's grant page) so it indexes it.
export function NewSpace() {
  const session = useSession();
  const { navigate } = useRouter();
  const [name, setName] = useState("memory");
  const [model, setModel] = useState<ModelInfo | null>(null);
  const [prefixes, setPrefixes] = useState({ documentPrefix: "", queryPrefix: "" });
  const [service, setService] = useState<Service | null>(null);
  const [steps, setSteps] = useState<Step[] | null>(null);
  const [created, setCreated] = useState<string | null>(null);

  useEffect(() => {
    api.service().then(setService, () => setService(null));
  }, []);

  const run = async (e: FormEvent) => {
    e.preventDefault();
    const plan: Step[] = [
      { label: "Create the space", state: "todo" },
      { label: "Declare the model", state: model ? "todo" : "skipped", note: model ? undefined : "do it later under Manage" },
    ];
    setSteps([...plan]);
    const set = (i: number, state: Step["state"], note?: string) => {
      plan[i] = { ...plan[i], state, note };
      setSteps([...plan]);
    };
    let uri: string;
    try {
      set(0, "doing");
      uri = (await api.createSpace(name)).uri;
      setCreated(uri);
      rememberSpace(uri);
      set(0, "done");
    } catch (err) {
      return set(0, "failed", describeError(err));
    }
    if (model) {
      try {
        set(1, "doing");
        await api.putConfig(uri, "declare", model, prefixes);
        set(1, "done");
      } catch (err) {
        set(1, "failed", describeError(err));
      }
    }
  };

  const finished = created && steps?.every((s) => s.state !== "doing" && s.state !== "todo");
  const canGrant = service?.grantUrl && service.registration === "open";

  const nameOk = validSpaceName(name);
  return (
    <>
      <h1>New memory space</h1>
      <form onSubmit={run} className="stack">
        <section className="panel">
          <label htmlFor="name">Name</label>
          <input id="name" value={name} onChange={(e) => setName(e.target.value)} disabled={!!steps} />
          <p className="muted small">
            The space will be <code>at://{session.did}/space/garden.engram.space/{name || "…"}</code>. Only
            members you add can read it.
          </p>
          {!nameOk && name && <p className="error">Use letters, digits and . _ ~ : - only.</p>}
        </section>
        <section className="panel">
          <h2>Embedding model</h2>
          <p className="muted small">
            Every agent embeds its memories with this exact model, on its own machine. Agents running{" "}
            <code>engram-mcp</code> check the model's digest before writing.
          </p>
          <ModelForm value={model} onChange={(m, p) => { setModel(m); setPrefixes(p); }} disabled={!!steps} />
        </section>
        {!steps && (
          <button className="primary" disabled={!nameOk}>
            Create space
          </button>
        )}
      </form>
      {steps && (
        <section className="panel">
          <ol className="progress">
            {steps.map((s) => (
              <li key={s.label} className={`progress-step ${s.state}`}>
                <span className="progress-mark" aria-hidden>
                  {{ todo: "○", doing: "◌", done: "✓", failed: "✕", skipped: "–" }[s.state]}
                </span>
                {s.label}
                {s.note && <span className="muted small"> · {s.note}</span>}
              </li>
            ))}
          </ol>
          {finished && (
            <>
              {canGrant ? (
                <p>
                  Last, let the appview read the space so it can index it. You'll confirm on your account's sign-in
                  page; it gets read-only access to the memory spaces you run.
                </p>
              ) : (
                <p className="muted">This appview indexes only the spaces its operator adds.</p>
              )}
              <div className="row">
                {canGrant && (
                  <button className="primary" onClick={() => goToGrant(service!, created!, "grant")}>
                    Let the appview index it
                  </button>
                )}
                <button className={canGrant ? "" : "primary"} onClick={() => navigate(spacePath(created!))}>
                  {canGrant ? "Not now" : "Open the space"}
                </button>
              </div>
            </>
          )}
        </section>
      )}
    </>
  );
}
