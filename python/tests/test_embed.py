"""Embedding providers: the OpenAI-compatible endpoint, Ollama's digest check, and the hashing embedder."""

from __future__ import annotations

import json

import httpx
import pytest

import engram_garden as eg
from engram_garden import embed, lex, vec

NOMIC = lex.ModelInfo("nomic-embed-text", "sha256:0a109f422b47e1b5", 4)


def ollama(models: list[dict], calls: list[str] | None = None, dims: int = 4):
    def handler(req: httpx.Request) -> httpx.Response:
        if calls is not None:
            calls.append(f"{req.method} {req.url.path}")
        if req.url.path == "/api/tags":
            return httpx.Response(200, json={"models": models})
        if req.url.path == "/v1/embeddings":
            body = json.loads(req.content)
            # Return out of order, to check the index is honored.
            data = [{"index": i, "embedding": [float(i + 1)] + [0.0] * (dims - 1)} for i in range(len(body["input"]))]
            return httpx.Response(200, json={"data": list(reversed(data))})
        return httpx.Response(404)

    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


async def test_openai_embedder_batches_and_orders():
    calls: list[str] = []
    http = ollama([], calls)
    e = embed.OpenAIEmbedder("http://ollama/v1", "m", 4, batch_size=2, http=http)
    out = await e.embed(["a", "b", "c"])
    assert [v[0] for v in out] == [1.0, 2.0, 1.0]  # batches of 2 then 1, each ordered by index
    assert calls.count("POST /v1/embeddings") == 2


async def test_openai_embedder_checks_dimensions_and_errors():
    http = ollama([], dims=3)
    with pytest.raises(eg.EngramError, match="returned 3 dimensions, configured for 4"):
        await embed.OpenAIEmbedder("http://ollama/v1", "m", 4, http=http).embed(["x"])
    bad = httpx.AsyncClient(transport=httpx.MockTransport(lambda r: httpx.Response(500, text="boom")))
    with pytest.raises(eg.EngramError, match="500: boom"):
        await embed.OpenAIEmbedder("http://x/v1", "m", 4, http=bad).embed(["x"])
    assert await embed.probe_dims("http://ollama/v1", "", "m", ollama([], dims=7)) == 7


async def test_openai_embedder_sends_key_and_dimensions():
    seen = {}

    def handler(req: httpx.Request) -> httpx.Response:
        seen["auth"] = req.headers.get("authorization")
        seen["body"] = json.loads(req.content)
        return httpx.Response(200, json={"data": [{"index": 0, "embedding": [1.0, 0.0]}]})

    http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    e = embed.OpenAIEmbedder("http://x/v1", "m", 2, api_key="k", send_dimensions=True, http=http)
    await e.embed(["x"])
    assert seen["auth"] == "Bearer k" and seen["body"] == {"model": "m", "input": ["x"], "dimensions": 2}


async def test_ollama_digest_forms():
    models = [
        {"name": "nomic-embed-text:latest", "model": "nomic-embed-text:latest", "digest": "0a109f422b47e1b5"},
        {"name": "other:7b", "model": "other:7b", "digest": "sha256:ff"},
        {"name": "nodigest:1", "model": "nodigest:1"},
    ]
    http = ollama(models)
    assert await embed.ollama_digest(http, "http://ollama/v1", "nomic-embed-text") == "sha256:0a109f422b47e1b5"
    assert await embed.ollama_digest(http, "http://ollama/v1/", "nomic-embed-text:latest") == "sha256:0a109f422b47e1b5"
    assert await embed.ollama_digest(http, "http://ollama", "other:7b") == "sha256:ff"
    assert await embed.ollama_digest(http, "http://ollama/v1", "nodigest:1") == ""
    assert await embed.ollama_digest(http, "http://ollama/v1", "missing") == ""


async def test_provider_accepts_the_declared_digest_and_caches():
    calls: list[str] = []
    models = [{"name": "nomic-embed-text:latest", "model": "nomic-embed-text:latest", "digest": "0a109f422b47e1b5"}]
    p = embed.OpenAIProvider(base_url="http://ollama/v1", http=ollama(models, calls))
    e = await p.for_model(NOMIC)
    assert e.dimensions() == 4 and e.model() == "nomic-embed-text"
    assert (await p.for_model(NOMIC)) is e
    assert calls.count("GET /api/tags") == 1


async def test_provider_refuses_other_builds():
    models = [{"name": "nomic-embed-text:latest", "model": "nomic-embed-text:latest", "digest": "ffff"}]
    p = embed.OpenAIProvider(base_url="http://ollama/v1", http=ollama(models))
    with pytest.raises(eg.ModelMismatchError, match="has digest sha256:ffff; pull the declared model"):
        await p.for_model(NOMIC)
    missing = embed.OpenAIProvider(base_url="http://ollama/v1", http=ollama([]))
    with pytest.raises(eg.ModelMismatchError, match="couldn't be checked locally"):
        await missing.for_model(NOMIC)


async def test_provider_digest_override_and_model_name():
    seen = {}

    def handler(req: httpx.Request) -> httpx.Response:
        seen["model"] = json.loads(req.content)["model"]
        return httpx.Response(200, json={"data": [{"index": 0, "embedding": [1.0, 0, 0, 0]}]})

    http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    p = embed.OpenAIProvider(base_url="http://api/v1", digest=NOMIC.model_digest, model_name="local-name", http=http)
    e = await p.for_model(NOMIC)  # no /api/tags call: the digest is declared
    await e.embed(["x"])
    assert seen["model"] == "local-name"


async def test_hashing_embedder_is_deterministic_and_word_based():
    e = embed.HashingEmbedder(64)
    a, b, c = await e.embed(["deploy the app", "deploy the app", "bananas"])
    assert a == b
    dot = lambda x, y: sum(p * q for p, q in zip(x, y, strict=True))  # noqa: E731
    assert abs(dot(a, a) - 1) < 1e-9 and dot(a, c) < 0.5
    (empty,) = await e.embed(["!!!"])
    assert empty[0] == 1.0
    p = embed.HashingProvider()
    assert (await p.for_model(lex.ModelInfo("hashing-64", embed.HASHING_DIGEST, 64))).dimensions() == 64
    with pytest.raises(eg.ModelMismatchError):
        await p.for_model(NOMIC)


def test_f16_edges():
    assert vec.decode_f16(vec.encode_f16([0.0, -0.0, 1.0, 0.5, 65504.0, 6e-8]))[:5] == [0.0, -0.0, 1.0, 0.5, 65504.0]
    assert vec.encode_f16([1e9]) == bytes.fromhex("007c")  # overflow is infinity, as in Go
    with pytest.raises(ValueError, match="not finite"):
        vec.decode_f16(bytes.fromhex("007c"))
    with pytest.raises(ValueError, match="odd"):
        vec.decode_f16(b"\x00")
    v = [3.0, 4.0]
    assert vec.normalize(v) and v == pytest.approx([0.6, 0.8])
    assert not vec.normalize([0.0, 0.0])


async def test_describe_model_with_ollama():
    models = [{"name": "nomic-embed-text:latest", "model": "nomic-embed-text:latest", "digest": "0a109f422b47e1b5"}]
    s = eg.Settings(embed=eg.Embed(url="http://ollama/v1"))
    m = await eg.describe_model(s, "nomic-embed-text", http=ollama(models, dims=768))
    assert m == lex.ModelInfo("nomic-embed-text", "sha256:0a109f422b47e1b5", 768)
    with pytest.raises(eg.EngramError, match="isn't installed in Ollama"):
        await eg.describe_model(s, "missing", http=ollama(models))
    with pytest.raises(eg.EngramError, match="which model"):
        await eg.describe_model(s, " ")
