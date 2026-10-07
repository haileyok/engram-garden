// Reads a model's exact identity from the Ollama on this computer, the way
// engram-config does, so the space records what agents will check against.
// Ollama only answers pages from other origins when OLLAMA_ORIGINS allows
// them.

import type { ModelInfo } from "../api";

export const OLLAMA = "http://localhost:11434";

export type LocalModel = { name: string; digest: string };

// Names are recorded without Ollama's default ":latest" tag.
export function normalizeModelName(name: string): string {
  return name.endsWith(":latest") ? name.slice(0, -":latest".length) : name;
}

export function normalizeDigest(digest: string): string {
  return digest.startsWith("sha256:") ? digest : `sha256:${digest}`;
}

export function knownPrefixes(model: string): { documentPrefix: string; queryPrefix: string } {
  if (model.startsWith("nomic-embed-text")) {
    return { documentPrefix: "search_document: ", queryPrefix: "search_query: " };
  }
  return { documentPrefix: "", queryPrefix: "" };
}

export async function listLocalModels(base = OLLAMA): Promise<LocalModel[]> {
  const resp = await fetch(`${base}/api/tags`);
  if (!resp.ok) throw new Error(`Ollama answered ${resp.status}`);
  const data = (await resp.json()) as { models?: { name?: string; model?: string; digest?: string }[] };
  return (data.models ?? [])
    .filter((m) => (m.name || m.model) && m.digest)
    .map((m) => ({ name: normalizeModelName(m.name || m.model || ""), digest: normalizeDigest(m.digest!) }));
}

// The model's vector size, from embedding a probe.
export async function probeDims(model: string, base = OLLAMA): Promise<number> {
  const resp = await fetch(`${base}/api/embed`, {
    method: "POST",
    body: JSON.stringify({ model, input: "dimension probe" }),
  });
  if (!resp.ok) throw new Error(`Ollama couldn't embed with ${model} (${resp.status})`);
  const data = (await resp.json()) as { embeddings?: number[][] };
  const n = data.embeddings?.[0]?.length ?? 0;
  if (!n) throw new Error(`${model} returned no vector; is it an embedding model?`);
  return n;
}

export async function describeLocalModel(m: LocalModel, base = OLLAMA): Promise<ModelInfo> {
  return { model: m.name, modelDigest: m.digest, dims: await probeDims(m.name, base) };
}
