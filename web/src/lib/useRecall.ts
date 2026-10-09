import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError, type Memory, type SpaceStatus } from "../api";
import { describeError } from "../ui";
import { embedQuery, RecallError, roundVector } from "./recall";

export type RecallState =
  | { phase: "idle" }
  | { phase: "searching"; query: string }
  | { phase: "done"; query: string; hits: Memory[]; approximate: boolean }
  | { phase: "error"; query: string; message: string };

export type Recall = {
  state: RecallState;
  // Why searching isn't possible, when it isn't.
  unavailable: string | null;
  run: (query: string) => void;
  clear: () => void;
};

const LIMIT = 20;

// The model searches use is the one the index is serving; the prefixes come
// from the space's config.
export function searchConfig(status: SpaceStatus | null) {
  const cfg = status?.config;
  const model = status?.active ?? cfg;
  if (!cfg || !model) return null;
  return { ...cfg, model: model.model, modelDigest: model.modelDigest, dims: model.dims };
}

// useRecall searches one space by meaning, from the browser. A search that
// has been overtaken by a newer one, or by switching space, is dropped.
export function useRecall(uri: string, status: SpaceStatus | null): Recall {
  const [state, setState] = useState<RecallState>({ phase: "idle" });
  const seq = useRef(0);
  const config = searchConfig(status);
  const configRef = useRef(config);
  configRef.current = config;

  useEffect(() => {
    seq.current++;
    setState({ phase: "idle" });
  }, [uri]);

  const run = useCallback(
    (raw: string) => {
      const query = raw.trim();
      const cfg = configRef.current;
      if (!query || !cfg) return;
      const mine = ++seq.current;
      setState({ phase: "searching", query });
      (async () => {
        try {
          const vector = roundVector(await embedQuery(cfg, query));
          const r = await api.search({
            space: uri,
            vector,
            model: cfg.model,
            modelDigest: cfg.modelDigest,
            limit: LIMIT,
          });
          if (mine === seq.current) setState({ phase: "done", query, hits: r.memories, approximate: !!r.approximate });
        } catch (e) {
          if (mine !== seq.current) return;
          let message: string;
          if (e instanceof RecallError) message = e.message;
          else if (e instanceof ApiError && e.code === "ModelMismatch") message = "This space switched models. Reload the page to search with the new one.";
          else message = describeError(e);
          setState({ phase: "error", query, message });
        }
      })();
    },
    [uri],
  );

  const clear = useCallback(() => {
    seq.current++;
    setState({ phase: "idle" });
  }, []);

  const unavailable = !status
    ? "Loading…"
    : !config
      ? "Search needs an embedding model, and this space hasn't declared one."
      : status.memories === 0
        ? "Nothing to search yet."
        : null;
  return { state, unavailable, run, clear };
}
