// Searching a space by meaning from the browser. The web app has no embedding
// model of its own, so the query is embedded by the Ollama on this computer,
// with the same model the space declares, then the backend asks the appview
// for the nearest memories. Ollama only answers pages from other origins when
// OLLAMA_ORIGINS allows them.

import type { SpaceConfig } from "../api";
import { normalizeDigest, normalizeModelName, OLLAMA } from "./ollama";

export type RecallErrorKind = "ollama-unreachable" | "model-missing" | "model-changed" | "embed-failed";

// RecallError is something a person can fix; its message says how.
export class RecallError extends Error {
  kind: RecallErrorKind;
  constructor(kind: RecallErrorKind, message: string) {
    super(message);
    this.kind = kind;
  }
}

// What gets embedded for a query: the space's query prefix, then the query.
export function queryText(config: Pick<SpaceConfig, "queryPrefix">, query: string): string {
  return (config.queryPrefix ?? "") + query;
}

// Six significant digits are plenty for a query vector, and keep the request small.
export function roundVector(v: number[], digits = 6): number[] {
  return v.map((x) => Number(x.toPrecision(digits)));
}

// Models already checked against the space's, by name and digest.
const verifiedModels = new Set<string>();

export type EmbedOptions = {
  fetch?: typeof fetch;
  base?: string;
  origin?: string;
  verified?: Set<string>;
};

// embedQuery embeds a question with the space's model, after checking the
// Ollama here has that exact model. The query prefix is added here.
export async function embedQuery(config: SpaceConfig, question: string, opts: EmbedOptions = {}): Promise<number[]> {
  const doFetch = opts.fetch ?? fetch.bind(globalThis);
  const base = opts.base ?? OLLAMA;
  const verified = opts.verified ?? verifiedModels;
  const origin = opts.origin ?? (typeof window !== "undefined" ? window.location.origin : "this site");
  const unreachable = () =>
    new RecallError(
      "ollama-unreachable",
      `Couldn't reach Ollama at ${base}. Start it with OLLAMA_ORIGINS=${origin} so this page may ask it.`,
    );

  const key = `${config.model}@${config.modelDigest}`;
  if (!verified.has(key)) {
    let resp: Response;
    try {
      resp = await doFetch(`${base}/api/tags`);
    } catch {
      throw unreachable();
    }
    if (!resp.ok) throw unreachable();
    const data = (await resp.json()) as { models?: { name?: string; model?: string; digest?: string }[] };
    const local = (data.models ?? []).filter((m) => (m.name || m.model) && m.digest);
    const sameName = local.filter((m) => normalizeModelName(m.name || m.model || "") === config.model);
    if (sameName.length === 0) {
      throw new RecallError(
        "model-missing",
        `This space embeds with ${config.model}, which your Ollama doesn't have. Run: ollama pull ${config.model}`,
      );
    }
    if (!sameName.some((m) => normalizeDigest(m.digest!) === config.modelDigest)) {
      throw new RecallError(
        "model-changed",
        `Your copy of ${config.model} isn't the one this space uses, so its vectors wouldn't match. Pull the same version the space's agents use.`,
      );
    }
    verified.add(key);
  }

  let resp: Response;
  try {
    resp = await doFetch(`${base}/api/embed`, {
      method: "POST",
      body: JSON.stringify({ model: config.model, input: queryText(config, question) }),
    });
  } catch {
    throw unreachable();
  }
  if (!resp.ok) throw new RecallError("embed-failed", `Ollama couldn't embed the query with ${config.model} (${resp.status}).`);
  const out = (await resp.json()) as { embeddings?: number[][] };
  const v = out.embeddings?.[0];
  if (!v || v.length !== config.dims) {
    throw new RecallError(
      "embed-failed",
      `${config.model} returned a vector of ${v?.length ?? 0} numbers; this space expects ${config.dims}.`,
    );
  }
  return v;
}
