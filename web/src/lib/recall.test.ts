import { describe, expect, it } from "vitest";
import { embedQuery, queryText, RecallError, roundVector } from "./recall";

const config = {
  model: "nomic-embed-text",
  modelDigest: "sha256:0a109f422b47e3a30ba2b10eca18548e944e8a23073ee3f3e947efcf3c45e59f",
  dims: 3,
  queryPrefix: "search_query: ",
};

type Call = { url: string; body?: unknown };

function fakeOllama(handlers: {
  tags?: unknown;
  embed?: unknown;
  tagsStatus?: number;
  embedStatus?: number;
  down?: boolean;
}) {
  const calls: Call[] = [];
  const f = (async (url: string, init?: RequestInit) => {
    calls.push({ url, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    if (handlers.down) throw new TypeError("Failed to fetch");
    const json = (data: unknown, status = 200) => new Response(JSON.stringify(data), { status });
    if (url.endsWith("/api/tags")) return json(handlers.tags ?? { models: [] }, handlers.tagsStatus);
    if (url.endsWith("/api/embed")) return json(handlers.embed ?? { embeddings: [[0.1, 0.2, 0.3]] }, handlers.embedStatus);
    return json({}, 404);
  }) as unknown as typeof fetch;
  return { f, calls };
}

const localCopy = { models: [{ name: "nomic-embed-text:latest", digest: "0a109f422b47e3a30ba2b10eca18548e944e8a23073ee3f3e947efcf3c45e59f" }] };

describe("queryText", () => {
  it("puts the space's query prefix in front", () => {
    expect(queryText(config, "deploys")).toBe("search_query: deploys");
    expect(queryText({ ...config, queryPrefix: undefined }, "deploys")).toBe("deploys");
  });
});

describe("roundVector", () => {
  it("keeps six significant digits so the request stays small", () => {
    expect(roundVector([0.123456789, -0.000123456789, 12345.6789, 0])).toEqual([0.123457, -0.000123457, 12345.7, 0]);
  });
});

describe("embedQuery", () => {
  it("embeds the prefixed text with the space's model after checking the model is the same one", async () => {
    const { f, calls } = fakeOllama({ tags: localCopy });
    const v = await embedQuery({ ...config, model: "nomic-embed-text" }, "deploys", { fetch: f, verified: new Set() });
    expect(v).toEqual([0.1, 0.2, 0.3]);
    expect(calls.map((c) => c.url)).toEqual(["http://localhost:11434/api/tags", "http://localhost:11434/api/embed"]);
    expect(calls[1].body).toEqual({ model: "nomic-embed-text", input: "search_query: deploys" });
  });

  it("checks the model once, not for every search", async () => {
    const { f, calls } = fakeOllama({ tags: localCopy });
    const verified = new Set<string>();
    await embedQuery(config, "one", { fetch: f, verified });
    await embedQuery(config, "two", { fetch: f, verified });
    expect(calls.filter((c) => c.url.endsWith("/api/tags"))).toHaveLength(1);
    expect(calls.filter((c) => c.url.endsWith("/api/embed"))).toHaveLength(2);
  });

  it("says how to let this page reach Ollama when it can't", async () => {
    const { f } = fakeOllama({ down: true });
    const err = await embedQuery(config, "x", { fetch: f, verified: new Set(), origin: "https://engram.garden" }).catch((e) => e);
    expect(err).toBeInstanceOf(RecallError);
    expect(err.kind).toBe("ollama-unreachable");
    expect(err.message).toContain("OLLAMA_ORIGINS=https://engram.garden");
    expect(err.message).toContain("http://localhost:11434");
  });

  it("says to pull the model when Ollama doesn't have it", async () => {
    const { f } = fakeOllama({ tags: { models: [{ name: "llama3:latest", digest: "abc" }] } });
    const err = await embedQuery(config, "x", { fetch: f, verified: new Set() }).catch((e) => e);
    expect(err.kind).toBe("model-missing");
    expect(err.message).toContain("ollama pull nomic-embed-text");
  });

  it("refuses a model with the same name but a different digest, since its vectors wouldn't match", async () => {
    const { f, calls } = fakeOllama({ tags: { models: [{ name: "nomic-embed-text:latest", digest: "ffff" }] } });
    const err = await embedQuery(config, "x", { fetch: f, verified: new Set() }).catch((e) => e);
    expect(err.kind).toBe("model-changed");
    expect(calls.some((c) => c.url.endsWith("/api/embed"))).toBe(false);
  });

  it("refuses a vector of the wrong size", async () => {
    const { f } = fakeOllama({ tags: localCopy, embed: { embeddings: [[1, 2]] } });
    const err = await embedQuery(config, "x", { fetch: f, verified: new Set() }).catch((e) => e);
    expect(err.kind).toBe("embed-failed");
  });

  it("reports an embedding error from Ollama", async () => {
    const { f } = fakeOllama({ tags: localCopy, embedStatus: 500 });
    const err = await embedQuery(config, "x", { fetch: f, verified: new Set() }).catch((e) => e);
    expect(err.kind).toBe("embed-failed");
  });
});
